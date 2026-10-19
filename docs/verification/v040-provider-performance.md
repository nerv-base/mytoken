# 0.4.0 provider 端点感知查询性能验证

日期：2026-10-13。环境：Go 1.27.1、darwin/arm64（Apple M5）。分支基线：2e696f2 加工作区未提交的 provider 身份工作。
写入范围：`internal/query/**`、`internal/store/**`、本文件。未改 provider/gui/共享规划文件。
本轮目标：让 `TestQueryPerformanceBudget` 的全部 22 个用例在本机回到 100ms 预算以内，且不改写任何会计/身份语义。

这不是 0.4.0 完成声明。未验证项见文末"未关闭的门"。

## 结论

四个超预算用例全部回到预算内，且没有放宽预算、没有删用例、没有缩小夹具、没有把真实端点换成平凡端点：

| 用例 | 基线（红） | 本轮（绿） | 变化 |
| --- | --- | --- | --- |
| `TotalsIDFilter` | 651.801ms | 3.637ms | 约 179× |
| `ProviderGroups` | 685.760ms | 4.198ms | 约 163× |
| `ProviderGroupsIDFilter` | 637.750ms | 3.672ms | 约 174× |
| `CacheTotalsIDFilter` | 152.796ms | 0.598ms | 约 255× |

其余用例同样低于预算（原基线 ≤15ms）。峰值分配 3.50MiB，远低于 50MiB 预算。

做法：新增两张**端点感知的派生汇总表**（`provider_daily`、`provider_cache_daily`），在 `daily_usage`/`cache_daily` 的分组键上追加 `base_url`；只有带端点身份的查询改走新表，其它查询仍走原表。原表主键、触发器与全部消费方未动。派生表是"事实的另一种汇总"，不承载新语义，也不重写历史归属与价格。

## 红门（基线，由 lead 独立测量）

```sh
MYTOKEN_STRICT_PERF=1 go test ./internal/query -run '^TestQueryPerformanceBudget$' -count=1 -v
```

基线结果：`TotalsIDFilter` 651.801ms、`ProviderGroups` 685.760ms、`ProviderGroupsIDFilter` 637.750ms、`CacheTotalsIDFilter` 152.796ms，其余 ≤15ms。预算 100ms，夹具 200000 条事件、8 个真实端点变体（其中两个归一化为同一端点 ID）。

## 分析机制与证据

新增可选的诊断测试 `internal/query/profile_test.go`，由 `MYTOKEN_PROFILE_PERF=1` 守护，输出每个用例的 `EXPLAIN QUERY PLAN` 与逐用例墙钟时间：

```sh
MYTOKEN_PROFILE_PERF=1 go test ./internal/query -run '^TestProfileProviderQueries$' -count=1 -v
```

关键计划（200K 夹具）：

- `TotalsIDFilter`（`eventAggregateSQLMode`）：`SCAN e USING INDEX sqlite_autoindex_event_data_1` + `SEARCH d USING INTEGER PRIMARY KEY (rowid=?)` + `USE TEMP B-TREE FOR GROUP BY`。**`base_url` 过滤完全没有下推到事实扫描。**
- `CacheTotalsIDFilter`（`cacheRollupFromEventsWhere`）：`SCAN e`（无索引）+ 同样的 join + 临时 B 树。
- `daily_usage` 汇总扫描：`SCAN daily_usage`。

关键计时与归因：

| 测量 | 结果 |
| --- | --- |
| `SELECT count(*),sum(input) FROM event_data` | 18.9–21.4ms |
| `events` 视图（join，不分组） | 15.9–17.3ms |
| `SELECT dimension_id,sum(input) FROM event_data GROUP BY dimension_id` | 60.6–65.1ms（610 组） |
| `SELECT count(*) FROM daily_usage` | 65–92µs |
| 原样事件聚合（日桶 + 全列 GROUP BY） | 619.8–631.2ms（710 行） |

即：扫描与 join 都便宜，开销在**逐行 Go 标量 `mytoken_day(timestamp)` 加 7 列 TEXT 的临时 B 树 GROUP BY**。

被实测排除的纯改写候选：

- (A) 按 `dimension_id` 分组 + join 取维度 + 11 个指标表达式：176.8ms（不 join 版本 181.4ms）。已低于 631ms，但仍超预算约 77%——11 个指标表达式在 65ms 基线上再加约 115ms。
- (A2) 同 (A) 追加 `WHERE d.base_url IN (?,?)`：85.3ms（160 行）。SQLite 在 join 之后、聚合之前应用该谓词，B 树因此变小；但仍要扫全部 200K 事件行，且不稳定的调度噪声足以越过 100ms。
- (B) 去掉日桶的纯 TEXT GROUP BY：535.8ms；加 `base_url` 过滤后 185.7ms。
- (C) `count(*)` + `base_url` 过滤：32.3ms。

结论：单靠查询改写无法稳定达标，因此选择**持久化派生汇总**；`ProviderGroups`（无范围、无 SQL 谓词）这一用例只有读汇总表才能到毫秒级。

排除的另一条捷径：仅在"没有哪个日汇总键跨多个端点身份"时才回退到按字典消歧 `daily_usage` 行。它恰好在本任务点名的多租户场景（同一原始 provider 指向多个租户）退化，等于迎合基准而不是解决问题，故不采用。

## 设计与不变量

- `providerDailyDimensions = "day,harness,session_id,resolved_provider,model,project,base_url,attrib"`，`providerCacheDimensions = "day,harness,session_id,resolved_provider,model,project,base_url,cause"`：即 `dailyDimensions`/`cacheDimensions` 插入 `base_url`。
- 指标表达式与 `daily_usage`/`cache_daily` **逐字一致**（`providerDailyValues`/`providerCacheValues` 只是 `dailyValues`/`cacheValues` 插入 `base_url`）；因此任何消费方对两张表求和的结果都相同。
- 触发器由共享的 `rollupChange(...)` 生成，维度引用通过 `event_dimensions d` 解析（`prefix+"."+col` → `d."+col`），与 `migrateDaily`/`migrateCacheDaily` 的 `change()` 同构；插入累加、删除先减再清理 `requests=0` 的行、更新 = 删旧 + 加新。
- `base_url` 取自字典 `event_dimensions`，事件视图已经暴露它，回填不引入新状态、不写事件。
- 使用方路由（`internal/query/aggregate_sql.go`、`internal/query/cache_query.go`）：
  - 用量路径：`scanUsage` 在 `withEndpoint && zone == store.LocalDayZone()` 时走 `providerAggregateSQL`（中间用 `provider_daily`，边界日仍扫事实）；身份谓词**仍然不**下推到 SQL，`base_url` 随行返回、由 `load` 的 Go 循环 `selector.match(v)` 过滤——与基线语义一致。
  - 缓存路径：谓词**照旧**下推，但落到 `provider_cache_daily`（`providerCacheRangeSQL`）；未受限查询仍读 `cache_daily`。
  - 时区非持久日（`daily_timezone != LocalDayZone()`）时全部退回纯事实路径，保证 `UNION ALL` 的日边界与桶定义完全一致。
- `daySplit` 从 `aggregateSQL` 中提取并被三条路径共用，`eventsOnly` 语义不变（整段落在同一个不完整本地日内时直接扫事实）。

## 绿门（本轮，精确命令与数字）

```sh
MYTOKEN_STRICT_PERF=1 go test ./internal/query -run '^TestQueryPerformanceBudget$' -count=1 -v
```

结果：`ok github.com/zzstar101/mytoken/internal/query 7.703s`，22/22 PASS（无其它本地测试在跑）。逐用例（耗时 / 分配）：

```
Totals                 3.045625ms  2597368 B
TotalsIDFilter         3.636625ms  2785880 B
ProviderGroups         4.197541ms  3497136 B
ProviderGroupsIDFilter 3.672167ms  2910552 B
Daily                  2.713084ms  2518400 B
Hourly24h             14.554375ms   734656 B
HourlyHarness24h      14.419333ms   735024 B
ByModel                2.900083ms  2593048 B
ByProvider             2.817125ms  2571008 B
ByHarness              3.138542ms  2598240 B
ByProject              2.931917ms  2619464 B
Sessions6              3.333708ms  3505208 B
SessionsAll             3.145ms    3505208 B
Session                5.022334ms  3129208 B
CacheTotals             802.791µs    5384 B
CacheTotalsIDFilter       598µs      10760 B
CacheDaily              929.042µs   54616 B
CacheByProvider         891.167µs    7448 B
CacheByModel            867.791µs    8056 B
CacheByHarness          819.666µs    5952 B
CacheByProject          967.583µs   22408 B
SessionCache            892.208µs  178336 B
```

`CacheTotalsIDFilter` 的分配从基线的数 MiB 降到 10.7KiB，说明它确实在汇总表上聚合，而不是把事实行读进 Go。

### 重复运行（噪声边界）

同一命令连续跑三次，三次全部 PASS（22/22），但幅度随机器负载变化：

| 用例 | 空闲机器 | 载入机器（load avg ≈ 11） |
| --- | --- | --- |
| `TotalsIDFilter` | 3.637ms | 10.022ms |
| `ProviderGroups` | 4.198ms | 14.284ms |
| `ProviderGroupsIDFilter` | 3.672ms | 32.582ms |
| `CacheTotalsIDFilter` | 0.598ms | 3.334ms |
| `Hourly24h`（纯事实路径，未改动） | 14.554ms | 39.082ms |

空闲机器上最大余量约 25×；载入机器上四个目标用例的最差一次仍有约 3× 余量，但**未改动的 `Hourly24h` 从 14.5ms 涨到 39.1ms（约 2.7×）**，说明本机噪声可达 2–3×。四个目标用例（3.7–32.6ms）在此噪声下仍达标，但 `ProviderGroupsIDFilter` 在 32.6ms 时若再放大 3× 就会贴近 100ms。已按"不改预算、不删用例、不缩夹具"的要求保留该风险，见"未关闭的门"第 2 条。

## 迁移与备份安全

新增行为**只创建派生表与触发器**，不新增/修改任何列，不重写事件、会话、端点字典或价格。

- 版本标记分两颗：`provider.identity.v1`（既有）与 `provider.endpoint-rollup.v1`（本轮新增）。分开的理由：身份索引可由任意 pre-0.4 索引建立，而汇总表只有在 `event_dimensions` 与 cache 列存在后才正确；合成一颗会把"已完成身份迁移但还没有汇总"的库静默视为完整。
- 预检 `checkProviderRollupIndex`：标记为空却存在 `provider_daily`/`provider_cache_daily`（无论 table/view/index/trigger）→ 报 `provider endpoint rollup is inconsistent: ...`；标记为 `1` 但表缺失 → 同样报错；标记为其它值 → 报错。绝不静默跳过，也绝不"当作完整"。
- 不变式：`before` 摘要现在在 `identityMigration || rollupMigration` 时于**任何写操作之前**取；`after` 与 `before` 不等即返回 `provider migration history invariant failed; transaction rolled back`，整个事务回滚。摘要覆盖 `harness,dedup_key,timestamp`、五类 token、`cost`/`log_cost`/`priced`。
- 新库 / 身份迁移待执行：汇总与身份索引共用同一个事务、同一份备份（`path + ".pre-provider-v1.db"`），不额外备份。
- 已有身份标记的库：汇总单独一个事务，写自己的备份 `path + ".pre-provider-endpoint-rollup-v1.db"`——身份备份已不再覆盖其后累积的历史。备份名不同，因此不会复用/覆盖既有备份；备份写入失败即中止，事务从未开始，标记不写。
- 备份策略沿用既有实现：不覆盖、不复用、失败不删除。
- 与既有重建路径的集成：
  - `rebuildLedger`（价格指纹变更后的全量重算）的 `DROP TRIGGER` 列表加入 6 个新触发器；批量 `UPDATE event_data` 之后重建 `cache_daily` 与 `provider_cache_daily`，并 `migrateProviderDaily`/`migrateProviderCacheDaily` 复原触发器。`provider_daily` 的指标不依赖 cache 列，因此只需重建触发器、无需重算数据。
  - `EnsureLocalDays`（本地日/时区键变化）在重建 `daily_usage` 时同步重建 `cache_daily` 与两张新表。
  - 顺带修正：`EnsureLocalDays` 此前只重建 `daily_usage`，在本地时区键变化时会把 `cache_daily` 留成旧日界；这会让"带端点过滤"的缓存查询与"不过滤"的缓存查询对不上。本批一并重建。
- `replacePricing`/`recomputeMissCost` 走 `UPDATE event_data`，`AFTER UPDATE` 触发器自动维护两张新表，无需额外代码。

## 测试证据

```sh
go test ./internal/store -count=1 -run 'TestProviderRollup' -v   # ok 0.824s
go test ./internal/query -count=1 -run 'TestProviderRollup' -v   # ok 0.685s
go test ./internal/query ./internal/store -count=1               # ok 3.585s / ok 26.524s
```

`internal/store/provider_rollup_test.go`（6 项）：`providerDailyOracle`/`providerCacheOracle` 是事实侧的汇总定义（按相同分组键与相同会计表达式对 `events` 视图 GROUP BY），`assertRollupMatchesFacts` 对每张表做**双向 `EXCEPT`**（汇总多出的行、事实多出的行都必须为 0）。

- `TestProviderRollupMigrationMatchesFacts`：回填与事实一致、标记为 `1`、`count/sum(input)/sum(cost) FROM events` 与 `count(*),count(DISTINCT provider_id) FROM provider_endpoints` 前后不变、二次 `Open` 幂等。
- `TestProviderRollupMigrationOnIdentityMarkedIndex`：只删汇总、保留身份标记 → 生成 `.pre-provider-endpoint-rollup-v1.db`，不生成第二个 `.pre-provider-v1.db`，回填后一致。
- `TestProviderRollupMaintenanceTracksEvents`：插入 → 一致；`UPDATE events SET project=...`（维度换桶）→ 一致；`EnsurePrices` 重定价 → 一致；`DELETE FROM events` → 一致且不留 `requests=0` 的空行。
- `TestProviderRollupInvariantFailureRollsBack`：在标记写入处挂 `AFTER INSERT ON settings` 触发器改写 `event_data.cost` → `Open` 失败，事实不变，两颗标记均为 0，两张表都不存在。
- `TestProviderRollupPartialStateRefused`：删标记留表 → `Open` 报 `inconsistent`。
- `TestProviderRollupBackupFailurePreservesExisting`：目标备份已存在 → `Open` 失败，既有备份字节不变。

`internal/query/provider_rollup_parity_test.go`（4 项）：用 `withFactsOracle` 把 `daily_timezone` 改成非本地时区，强制同一查询走**纯事实路径**，再与走汇总路径的答案做逐字节 JSON 对比（`Totals/Daily/Hourly/ProviderGroups/ByProvider/ByModel/ByProject/ByHarness/Sessions/CacheTotals/CacheDaily/CacheBy*`）。夹具沿用 `idFixtureEvents()`：`name-a` 同时出现在两个端点、`name-a`/`name-b` 共用同一端点、`name-c` 同会话不同租户、原始 URL 不可识别 → 未解析身份。

- `TestProviderRollupParityWithFacts`：19 个探针，覆盖整日汇总段、单个不完整日、前导/尾随边界与整日混合、同日内窄区间、空区间、反向区间、未知 ID、大量未知 ID、未解析身份、harness/价格名/项目与端点组合。
- `TestProviderRollupParityOnDSTBoundary`：`America/New_York`，2026-03-08 02:00 EST → 03:00 EDT 的 23 小时日；`floor`/`step` 用 `AddDate`，故整日段与边界段在 DST 交界处对齐，5 个区间探针全部一致。
- `TestProviderRollupParityAcrossManySessions`：40 事件 / 7 会话 / 3 模型 / 4 名称 / 3 原始 URL。
- `TestProviderRollupIsActuallyRead`：篡改 `provider_daily.input` 与 `provider_cache_daily.requests` 后，带端点过滤的答案必须改变（否则说明测试是空转），而**不带**端点过滤的答案必须不变（说明它仍读 `daily_usage`/`cache_daily`）。

### 变异检验（证明测试真的能抓住本批要防的缺陷）

把 `providerDailyValues`/`providerCacheValues` 中的 `base_url` 换成 SQL 空字面量 `''`（等价于"两租户被合并进同一汇总行"），重新运行：

- `go test ./internal/query -run TestProviderRollupParityWithFacts` → **FAIL**：`ProviderGroups` 从两个 `endpoint:v1:` 组退化成 5 个 `unresolved:` 组，正是端点身份丢失。
- `go test ./internal/store -run TestProviderRollupMaintenanceTracksEvents` → **FAIL**：`provider_daily: rollup has rows the facts do not (5 rows)`。
- `go test ./internal/store -run TestProviderRollupMigrationMatchesFacts` → 通过。**边界说明**：该用例只覆盖回填（`providerDailyRollupInsert` 是独立的手写 SQL），触发器路径由 `TestProviderRollupMaintenanceTracksEvents` 覆盖；两条路径分别有测试，因此并非覆盖缺口。

变异随后按备份字节还原（`diff` 为空），并重新构建通过。

## 变更路径

新增：

- `internal/store/provider_rollup.go`
- `internal/store/provider_rollup_test.go`
- `internal/query/provider_rollup_parity_test.go`
- `internal/query/profile_test.go`（`MYTOKEN_PROFILE_PERF=1` 守护的分析工具）
- `docs/verification/v040-provider-performance.md`（本文件）

修改：

- `internal/store/migration.go`：在 `prepareProviderMigration` 后调用 `prepareProviderRollup`；摘要门与回滚门扩展为 `identityMigration || rollupMigration`；事务内追加 `migrateProviderRollup`。
- `internal/store/aggregate.go`：`EnsureLocalDays` 在时区键变化时同步重建 `cache_daily` 与两张新表。
- `internal/store/cache.go`：`rebuildLedger` 增补 6 个新触发器的 drop 与两张新表的重建。
- `internal/query/aggregate_sql.go`：`providerUsageColumns`、`daySplit`、`providerAggregateSQL`，`scanUsage` 改为按 `hourly`/`withEndpoint`/`persistentDays` 三轴路由。
- `internal/query/cache_query.go`：`providerCacheSubColumns`、`cacheRollupFromEventsEndpoint`、`providerCacheRangeSQL`，`cacheRangeSQL` 改用 `daySplit`，`cacheSource` 改为先读时区再路由。

不属于本批：`internal/query/query.go`、`internal/query/service.go`、`internal/query/bench_test.go`、`internal/store/store.go` 的未提交改动来自本批之前的 provider 身份工作。

## 未关闭的门

1. 工作区在本批期间有**其它 agent 的并发改动**（`docs/ROADMAP.md`、`go.mod`/`go.sum`、`internal/attrib`、`internal/cli`、`internal/gui`、`internal/harness/**`、`internal/provider` 等，均在本批写入范围之外），因此全仓状态是移动目标：
   - 本批早期 `go build ./...` 曾在 `internal/gui` 失败（`internal/gui/mainwin.go:271:33`、`internal/gui/pages.go:25:10` 把 `[]query.ProviderGroup` 当 `[]query.Bucket`；`internal/gui/providers.go:371:55` 对 `int` 调用 `max`），当时 `git show HEAD:internal/gui/data.go` 不含 `ProviderGroup`，属未提交的半截 GUI 迁移。
   - 同期一次 `go test ./...` 曾在 `internal/harness` 失败：`TestNewRootedExplicitEmptyRootsStayEmpty/codebuddy`、`/workbuddy` → `NewRooted(codebuddy, empty entries) produced 2 roots ["" "  "]; explicit empty roots must not fall back to defaults`。
   - 稍后完整 `go test -count=1 ./...` **全部通过**，含 `internal/gui`（35.6s）与 `internal/harness`（0.413s），说明上述两次都是并发编辑过程中的瞬时状态，不是本批引入。
   - 本批自身只验证了 `go build ./internal/store/... ./internal/query/...`、`go vet ./internal/store/... ./internal/query/...` 与这两个包的全部测试；全仓结论需由 lead 在冻结写入后复跑。
2. 严格性能门是单机测量，未做分位数统计（既有测试本身用硬截止，注释已说明共享 CI 上不适合）。三次运行全部 PASS，但机器载入时 `ProviderGroupsIDFilter` 观测到 32.6ms、未改动的 `Hourly24h` 观测到 39.1ms（空闲时为 14.5ms），说明本机噪声可达 2–3×；四个目标用例在该噪声下仍有约 3× 余量，但没有在更高并发负载下验证过。
3. `TestProviderRollupMigrationMatchesFacts` 不覆盖回填以外的触发器路径（见变异检验的边界说明）；两条路径分别由不同用例覆盖，但没有单一用例同时覆盖"回填 + 后续触发维护"。
4. 未做真实磁盘库的端到端迁移演练；所有迁移覆盖都基于 `t.TempDir()` 中的夹具库。
5. 未验证 `internal/query/provider_identity_cache_test.go`（另一位 agent 的未跟踪文件）是否与本批路由假设一致；它当前 `gofmt` 不干净但编译、测试通过。
