# 0.4.0 多实例数据层前置（internal/store + internal/query）验证

日期：2026-10-13 19:50 CST
环境：go1.27.1 darwin/arm64；工作目录 `/Users/zzstar/Developer/Programs/MyToken!!!!!`；HEAD `2e696f2`（未提交、未打标签）
写入范围：仅 `internal/store/**`、`internal/query/**`、`internal/model/**`（**实际未改动 model**）与本文件。`internal/instance/**` 属于并行的配置代理，本次**没有读写、没有依赖其未完成的 API**；`internal/scan` / `internal/app` / `internal/cli` / `internal/gui` / `main.go` 一行未改。

```console
$ git status --porcelain internal/model/
（空）
$ git status --porcelain internal/store/ internal/query/ docs/verification/ | head
 M internal/query/aggregate_sql.go
 M internal/query/bench_test.go
 M internal/query/cache.go
 M internal/query/cache_query.go
 M internal/query/query.go
 M internal/query/service.go
 M internal/store/aggregate.go
 M internal/store/aggregate_test.go
 M internal/store/cache.go
 M internal/store/compact.go
 M internal/store/migration.go
 M internal/store/store.go
?? internal/query/instance_test.go
?? internal/store/instance_migration.go
?? internal/store/instance_migration_test.go
?? internal/store/instance_test.go
?? docs/verification/
```

`internal/query/provider_identity.go` 及若干 provider 测试、`internal/store` 的 backup/migration 测试在本工作区尚未纳入 git 跟踪，因此没有可供 `git diff` 的基线；上面这些 `M` 里也混有其它工作流的既有改动，本次只触碰与实例维度相关的部分。未提交、未联网、未使用真实数据、未执行真实迁移、未启动任何服务。

## 结论与范围

本次交付的是**多实例的数据层前置**：一个数据库内把"实例"做成显式维度，覆盖事实表主键、维度唯一键、会话身份、游标、四张汇总表、维护触发器、连接与按会话聚合；查询层增加可选的实例过滤与实例溯源字段，并让旧签名在"多实例歧义"时**显式报错而不是取第一个匹配**。旧的空实例输入映射到保留实例 `default`，语义与 0.4.0 之前逐字节一致。

它**不是**多实例功能本身。以下全部不存在，因此本次不声称"多实例已完成"，也不改变任何默认行为：

- **没有采集侧接线**：`internal/scan` 仍只走默认实例路径，没有任何代码读取 `sources.json`，没有实例 id 从配置流到 `Write`；
- **没有配置层**：`internal/instance`（并行代理负责）的 `Entry.ID` 尚未被数据层消费，本次只约定 `store.DefaultInstanceID = "default"` 这一字符串兼容点；
- **没有 CLI/GUI 入口**：没有任何命令或界面能创建/选择实例；
- **没有执行真实迁移**：所有迁移证据来自内存 fixture 与 `t.TempDir()` 磁盘 fixture，真实用户库没有被打开过；
- **没有提交/发布**：工作区未提交，未打标签，未跑 fullsuite。

## 公开 API（全部为增量，旧调用方一行不用改）

### 写入与游标（`internal/store`）

```go
const DefaultInstanceID = "default"                    // store.go:28
func NormalizeInstanceID(instance string) string       // store.go:40；"" → DefaultInstanceID

type Write struct {                                    // store.go:182
    Harness     model.Harness
    InstanceID  string   // 新增；"" 表示保留实例 default
    Path        string
    Batch       harness.Batch
    Resolutions []Resolution
}

func (s *Store) Commit(ctx, h model.Harness, path string, b harness.Batch, res []Resolution) error
func (s *Store) CommitMany(ctx context.Context, writes []Write) error   // 签名未变

func (s *Store) Cursor(ctx, h model.Harness, path string) (harness.Cursor, error)      // → CursorIn(..., DefaultInstanceID, path)
func (s *Store) Cursors(ctx, h model.Harness) (map[string]harness.Cursor, error)        // → CursorsIn(..., DefaultInstanceID)
func (s *Store) CursorIn(ctx, h model.Harness, instance, path string) (harness.Cursor, error)
func (s *Store) CursorsIn(ctx, h model.Harness, instance string) (map[string]harness.Cursor, error)
```

**采集侧接线说明（留给后续 scanner/config 切片，本次不实现）**：`CommitMany` 的签名已经是 `[]Write`，所以接线**不需要任何签名改动**——scanner 只需在构造 `Write` 时把该源所属实例的 id 填进 `Write.InstanceID`（默认实例留空即可），游标读取改为 `CursorIn(ctx, h, entry.ID, path)` / `CursorsIn(ctx, h, entry.ID)`。实例是**列**，绝不写进 dedup key、session id 或 path 前缀；`path` 仍是今天的原始路径字符串，游标主键升级为 `(harness, instance_id, path)`，因此"同一路径属于两个实例"天然拥有两条独立游标。

### 查询（`internal/query`）

```go
type Filter struct {                                   // query.go:19
    Range Range; Harnesses []model.Harness
    Providers, ProviderIDs, Models []string
    InstanceIDs []string   // 新增：可选、正交；空 = 全部实例
    Project string
}

type AttributedEvent struct { ..., InstanceID string `json:"instanceId"` }   // 新增溯源字段
type SessionRow struct { model.SessionMeta; InstanceID string `json:"instanceId"`; ... }

var ErrAmbiguousInstance = errors.New("query: session exists in more than one instance; use SessionIn or SessionCacheIn")

type InstanceService interface {                       // 与 Service 分离，避免强制所有 GUI/demo 实现改动
    Service
    SessionIn(ctx, h model.Harness, instance, id string) (SessionRow, []SessionRow, []AttributedEvent, error)
    SessionCacheIn(ctx, h model.Harness, instance, session string) ([]CacheStep, error)
}
func SessionIn(ctx, s Service, h model.Harness, instance, id string) (...)      // 自由函数，非 InstanceService 时报错
func SessionCacheIn(ctx, s Service, h model.Harness, instance, session string) ([]CacheStep, error)
func CacheSessionKeyIn(instance, harness, session string) string               // default/"" → 旧 key；否则 "instance:<id>:<旧 key>"
```

`Service` 接口本身**没有新增方法**：`Session(ctx, h, id)` / `SessionCache(ctx, h, session)` 保持原签名。只有一个实例持有该会话时照常返回；多个实例都持有时返回 `ErrAmbiguousInstance`（绝不做 first-match）；显式实例查询走 `SessionIn` / `SessionCacheIn`（`*service` 实现 `InstanceService`）。

`Filter.InstanceIDs` 正交作用于 `Totals`、`Daily`、`Hourly`、`ByProvider`、`ProviderGroups`、`ByModel`、`ByProject`、`ByHarness`、`Sessions`、`CacheTotals`、`CacheDaily`、`CacheBy`、`SessionCache`，并同时进入 `daily_usage` 汇总臂与 `events` 事实臂（部分日走事实臂）。**序列语义保持不变**：`Daily`/`Hourly` 的时间轴在无实例过滤时是两个实例的并集（同一小时/同一天合并为一个点），`Filter.InstanceIDs` 才是把它们分开的手段；`Point` 不新增字段。

## Schema 变更（`instance.v1`）

`event_data` 的主键无法原地修改，因此迁移在**一个事务内**重建下列对象；重建后的 DDL 与旧 DDL 逐字段相同，只在 `harness` 之后（事实表在 `dedup_key` 之后）多出 `instance_id TEXT NOT NULL DEFAULT 'default'`，主键/唯一键带上它：

| 对象 | 旧 | 新 |
| --- | --- | --- |
| `event_data` | PK `(harness,dedup_key)` | PK `(harness,instance_id,dedup_key)` |
| `event_dimensions` | `UNIQUE(harness,session_id,parent_id,project,model,provider,base_url,resolved_provider,attrib,raw_project)` | `UNIQUE(harness,instance_id,…)`（`dimensionColumns` 由 `withInstance` 派生） |
| `sessions` | PK `(harness,session_id)` | PK `(harness,instance_id,session_id)` |
| `cursors` | PK `(harness,path)` | PK `(harness,instance_id,path)` |
| `daily_usage` | `(day,harness,session_id,project,model,resolved_provider,attrib)` | `+ instance_id`（`dailyDimensions`） |
| `cache_daily` | `(day,harness,session_id,project,model,resolved_provider,cause)` | `+ instance_id` |
| `provider_daily` | 端点维度 | `+ instance_id` |
| `provider_cache_daily` | 端点维度 | `+ instance_id` |

索引：`dimensions_session` 与 `sessions_parent` 变为 `(harness,instance_id,session_id)` / `(harness,instance_id,parent_id)`；`events_time`、`events_session`、`dimensions_raw_project`、`sessions_raw_project` 不变（事实表主键变化不影响它们，且 `events_time` 仍必须被 `(harness||'') IN` 的写法保持可用）。`events` 视图保持原有列并新增 `instance_id`；`INSTEAD OF` 触发器按 `instanceRollupSchema` 生成，把新行的 `instance_id` `coalesce(…, 'default')`，四个汇总表的维护触发器（`daily_`、`cache_`、`provider_daily_`、`provider_cache_` × insert/update/delete）都带上实例维度，删除分支按 `(dims) = (SELECT … FROM event_dimensions WHERE id=OLD.dimension_id)` 匹配。标记：`settings.instance_schema_version = "instance.v1"`。

`internal/store/instance_migration.go` 的 `instanceTables` 只列本次迁移自己拥有的**六张**表（`event_data`/`event_dimensions`/`sessions`/`cursors`/`daily_usage`/`cache_daily`）；两张 provider 汇总表故意排除，因为它们由各自的 `provider.endpoint-rollup.v1` 标记治理，"只掉这两张表"是可恢复形态而不是实例 schema 损坏。

## 迁移语义（`instance.v1`）

- **先备份后变更**：仅当 `events` 有数据且是磁盘库时，先把**变更前**的物理文件复制到固定产物 `path + ".pre-instance-v1.db"`；备份失败即 `Open` 失败，不做任何改动；产物固定、不覆盖、不删除、不复用（已存在同名文件时按失败处理，见 `TestInstanceMigrationBackupFailureStopsBeforeChanges`）。
- **全事务**：marker 写入、视图/触发器删除与重建、六张表重建、索引、四张汇总表重建与回填都在同一个 `tx` 内；任何一步失败一起回滚（facts + DDL + marker）。
- **逐请求不变量**：`instanceFactStamps(tx)` 对 `SELECT harness,dedup_key,timestamp,input,output,cache_read,cache_write,reasoning,cost,log_cost,priced FROM event_data ORDER BY harness,dedup_key` 求 sha256 并计数，迁移前后必须相等，**不是 SUM 对比**（`TestInstanceFactStampsIsPerRequest` 逐字段证明该摘要对单请求的 token 类别/时间戳/cost/log_cost/priced/dedup_key 任一变化都敏感）。
- **旧行全部归 default**：重建用 `'default'` 字面量插入，`TestInstanceMigrationLegacyDiskBacksUpAndPreservesFacts` 断言旧事实逐请求保留且 `instance_id='default'`。
- **空库跳过磁盘备份**：新建文件与 `:memory:` 都不产生备份产物。
- **残缺状态响亮失败**：`checkInstanceSchema` 在动手前拒绝"有 marker 无列"、"列存在但无 marker"、"未知 marker 值"，错误信息明确说明不做猜测、不重算、不跳过、备份未被替换。本次加固后它还逐项校验主键/唯一键/对象类型/`events` 视图/维护与写入触发器，见下文《实例 schema 预检加固（本次修复）》。
- **保留既有迁移守卫与端点汇总**：迁移顺序仍是 `prepareProviderMigration → prepareProviderRollup → prepareInstanceMigration → tx → (historyDigest) → 加列 → migrateCompact → migrateSignals → migrateCache → migrateEventIndexes → migrateDaily → relay → migrateProviderEndpoints → migrateProviderRollup → migrateInstance`；`internal/store/migration.go` 先探测 `event_data` 是否已有 `instance_id` 再选 `legacyRollupSchema` / `instanceRollupSchema`，所以已经带实例维度的库会用自己的世代修复增量步骤，而真正的旧库仍走逐字节的旧世代。

## 实例 schema 预检加固（本次修复）

**缺陷**：`checkInstanceSchema` 在 marker 为 `"1"` 时只检查六张表是否存在 `instance_id` **列**，而 `tableExists` 用的是 `SELECT count(*) FROM sqlite_master WHERE name=? AND type IN ('table','view')` —— 视图被当成表。于是"所有表都带 `instance_id` 列、但主键/唯一键还是旧世代、维度的 `UNIQUE` 丢了、`events` 不是实例视图、维护/写入触发器缺失或停留在旧世代"的**残缺库会被静默接受**：迁移以"已升级"跳过实例重建，实例维度停在半成品状态，且不会有任何备份或报错。

**加固后的预检不变量**（仅在 `marker == "instance.v1"` 时执行；全部只读，发生在备份与事务之前，任一项不满足即 `instanceSchemaInconsistent` 失败）：

1. 六张 `instanceTables`（`event_data`/`event_dimensions`/`sessions`/`cursors`/`daily_usage`/`cache_daily`）必须真实存在、`sqlite_master.type='table'`（缺失或"是视图"都拒绝）且带 `instance_id` 列；
2. `event_data`、`sessions`、`cursors`、`daily_usage`、`cache_daily` 的**主键必须包含** `instance_id`（按 `pragma_table_info` 的 `pk` 序号读取，不再只看列存在）；
3. `event_dimensions` 必须存在一个**包含** `instance_id` 的 `UNIQUE` 索引（`pragma_index_list` 中 `unique=1 AND origin<>'pk'`，再与 `pragma_index_info` 交叉比对）；
4. `events` 必须是**视图**而不是表，且暴露 `instance_id`；
5. 四组维护触发器与汇总表匹配：汇总表存在时，其 `<prefix>_insert/_update/_delete` 必须存在，且触发器 SQL 必须含 `instance_id`（实例世代从字典行读 `d.instance_id`，旧世代触发器文本里完全没有 `instance_id` 字样）；
6. `events_insert`、`events_update` 必须存在且引用 `NEW.instance_id`（写入分支写入 `coalesce(nullif(NEW.instance_id,''),'default')`）；`events_delete` 只要求存在——其正确实现是 `DELETE FROM event_data WHERE rowid=OLD.rowid`，**不含任何实例引用**，要求它也带 `NEW.instance_id` 会误杀正确 schema。

**故意豁免**（避免把可恢复形态一刀切拒绝）：

- 两张 provider 汇总表**不存在不是缺陷**：它们由 `provider.endpoint-rollup.v1` 独立治理，`prepareProviderRollup` 会在本次打开时重建；因此只在它们存在时才校验键与触发器（`TestInstancePreflightAllowsProviderRollupRebuild` 守着这条：删掉两张表 + 其 6 个触发器 + 清掉 rollup marker，`Open` 必须成功、重建后键带 `instance_id`、备份产物出现、四张汇总与事实一致、新写入按实例落表）。
- 维护触发器只在"其汇总表存在"时才要求。
- **marker 为空（尚未升级）时保持宽松**：只要求"没有表带 `instance_id`"且"没有对象用视图冒充表"，旧世代库照常走增量升级。

**红灯（先观察，后实现）**：新增 `internal/store/instance_preflight_test.go`，21 个 fixture 直接对**真实迁移产物**做单点损坏（改一处键/唯一键/对象类型/触发器，其余全对）。在旧实现上 21 个全部失败：

```console
$ go test ./internal/store/ -run TestInstancePreflight -count=1
--- FAIL: TestInstancePreflightRejectsLegacyKeys (1.31s)
    --- FAIL: .../event-data (0.17s)
        instance_preflight_test.go:194: Open accepted a malformed instance schema; want rejection mentioning [event_data instance_id]
    --- FAIL: .../sessions              → [sessions instance_id]
    --- FAIL: .../cursors               → [cursors instance_id]
    --- FAIL: .../daily-usage           → [daily_usage instance_id]
    --- FAIL: .../cache-daily           → [cache_daily instance_id]
    --- FAIL: .../provider-daily        → [provider_daily instance_id]
    --- FAIL: .../provider-cache-daily  → [provider_cache_daily instance_id]
    --- FAIL: .../event-dimensions-unique → [event_dimensions instance_id]
--- FAIL: TestInstancePreflightRejectsViewSubstitutions
    --- FAIL: .../event-data-is-a-view: Open error = SQL logic error: no such column: e.rowid (1); want an instance schema inconsistency
    --- FAIL: .../daily-usage-is-a-view: Open accepted a malformed instance schema; want rejection mentioning [daily_usage]
    --- FAIL: .../events-is-not-a-view: Open error = SQL logic error: no such column: project (1); want an instance schema inconsistency
--- FAIL: TestInstancePreflightRejectsNonInstanceEventsView
        instance_preflight_test.go:262: Open accepted a malformed instance schema; want rejection mentioning [events instance_id]
--- FAIL: TestInstancePreflightRejectsMissingMaintenanceTriggers（daily_delete/cache_update/provider_daily_insert/provider_cache_delete 4/4）
--- FAIL: TestInstancePreflightRejectsNonInstanceMaintenanceTriggers
--- FAIL: TestInstancePreflightRejectsMissingEventsWriteTriggers（events_insert/events_update/events_delete 3/3）
FAIL	github.com/zzstar101/mytoken/internal/store	3.450s
```

18 个 fixture 的旧行为是"静默接受"；2 个（`event-data-is-a-view`、`events-is-not-a-view`）连预检都没拦住，是在**迁移中途**以无关 SQL 错误炸掉。两者都证明"列存在"完全不能代表 schema 正确。每个 fixture 还断言 `events` 事实总量（count/sum(input)/sum(cost)）在失败前后不变——预检失败**不改写任何历史**。

**绿灯**（修复后的本轮实测，`-count=1`）：

```console
$ gofmt -l internal/store/
（空）
$ go vet ./internal/store/
（exit 0）
$ go test ./internal/store/ -run 'TestInstancePreflight|TestInstanceMigrationFaultRollsBackFactsAndDDL|TestSignalsUpgradeExistingCompactDatabase' -count=1
ok  	github.com/zzstar101/mytoken/internal/store	2.052s
$ go test ./internal/store/ -count=1
ok  	github.com/zzstar101/mytoken/internal/store	45.478s
```

21 个 fixture（20 个拒绝 + 1 个 provider 重建守卫）全部 PASS。

**连带修正（测试 fixture 本身是残缺形态）**：`TestSignalsUpgradeExistingCompactDatabase` 原先的造法是"先 `Open` 当前库，再手工把 `events` 视图降级成旧世代，同时把 `instance_schema_version='1'` 留在 `settings` 里"——那正是本次要拒绝的"marker 说已升级、视图却不是实例世代"的残缺形态（而且 `event_data` 仍带旧实例列/键），加固后被正确拒绝。该 fixture 已改为**真实的前置世代**：用原始 SQL 建 `event_data(PK harness,dedup_key)` + `event_dimensions(UNIQUE 旧维度)` + 旧 `events` 视图（无 `instance_id`、无 marker、无 provider 汇总表、`sessions` 无 `raw_project`），再由 `Open` 走完整的"压缩世代 → 信号列 → 缓存列 → `instance.v1`"升级路径（含四张汇总回填），用例意图（"升级既有 compact 库"）不变且更忠实于真实旧库。

## 验证用例（新增）

`internal/store/instance_test.go`：`TestInstanceSeparatesEqualDedupKeys`（同一 dedup key 在两个实例各存一条，总量 2 vs 旧 1）、`TestInstanceReimportIdempotent`（同实例重复导入不增行）、`TestInstanceCursorsIsolated`（同路径两实例游标独立）、`TestInstanceSessionsIsolated`（同 session id/同 parent 在两实例不串、无跨实例子会话）、`TestInstanceRollupsSeparated`（四张汇总按实例分组）、`TestInstanceMaintenanceTriggerKeepsInstance`（维护触发器带实例）、`TestInstanceMoveRetainsDedupKey`（移动后保留原 id 且 dedup key 不变）。

`internal/store/instance_migration_test.go`：`TestInstanceMigrationLegacyDiskBacksUpAndPreservesFacts`、`TestInstanceMigrationSkipsBackupOnFreshDatabase`、`TestInstanceMigrationRejectsPartialState`、`TestInstanceMigrationFaultRollsBackFactsAndDDL`、`TestInstanceFactStampsIsPerRequest`、`TestInstanceMigrationBackupFailureStopsBeforeChanges`、`TestInstanceRollupParityAfterUpdateDeleteAndReprice`（视图 UPDATE / DELETE / `RecomputeCosts` 后四张汇总与事实双向 EXCEPT 为空）、`TestInstanceEnsureLocalDaysKeepsInstances`（`daily_timezone` 设为陈旧值后 `EnsureLocalDays` 仍保持实例分离与汇总一致）。

`internal/store/instance_preflight_test.go`（本次新增）：`TestInstancePreflightRejectsLegacyKeys`（8 个旧主键/唯一键 fixture）、`TestInstancePreflightRejectsViewSubstitutions`（`event_data`/`daily_usage` 被替换成视图、`events` 被替换成表）、`TestInstancePreflightRejectsNonInstanceEventsView`、`TestInstancePreflightRejectsMissingMaintenanceTriggers`、`TestInstancePreflightRejectsNonInstanceMaintenanceTriggers`、`TestInstancePreflightRejectsMissingEventsWriteTriggers`、`TestInstancePreflightAllowsProviderRollupRebuild`（绿守卫）。

`internal/query/instance_test.go`：`TestInstanceTotalsSeparateIdenticalLogs`、`TestInstanceFiltersAcrossSeriesCacheGroupsAndSessions`（含部分日/小时的**事实臂**断言：alpha 部分日 = 1 请求 100 input，不过滤 = 2 请求 500 input）、`TestInstanceSessionLookupIsAmbiguousNotFirstMatch`、`TestInstanceUnresolvedIdentityIsolatedKnownMerged`。

**未解析端点身份**：`displayProvider` 的哈希输入现在是 `[harness, instance_id, session_id, resolved_provider, base_url]`，`providerIdentityPredicate` 扫描 `SELECT DISTINCT harness,instance_id,session_id,resolved_provider,base_url` 并生成 5 元组 `IN (VALUES …)`（每身份 5 个绑定参数）。因此**已知同一端点仍跨实例合并展示组**（同 URL 的两个实例进入一个 `endpoint:v1:` 组，Requests/Sessions 相加），而**未解析身份按实例隔离**（两个实例里同 session id、同定价名、同未知 URL 的未知端点保持两个 `unresolved:v1:` 组）。**计费 `Origin`+`KeyID`、定价身份/指纹与算法未改动**（`internal/pricing`、`internal/store/provider_alias.go` 均未触碰）。

## 红/绿证据

最终绿色（本轮最后一遍，全部 `-count=1`）：

```console
$ go build ./...
（exit 0）
$ go vet ./...
（exit 0）
$ go vet ./internal/store/ ./internal/query/ ./internal/model/
（exit 0）
$ go test ./internal/model/ -count=1
?   	github.com/zzstar101/mytoken/internal/model	[no test files]
$ go test ./internal/query/ -count=1
ok  	github.com/zzstar101/mytoken/internal/query	4.165s
$ go test ./internal/store/ -count=1
ok  	github.com/zzstar101/mytoken/internal/store	30.933s
$ go test ./internal/cli/ -count=1
ok  	github.com/zzstar101/mytoken/internal/cli	6.543s
```

`go vet ./...` 会连所有测试文件一起类型检查，`go build ./...` 通过说明**没有任何范围外调用方需要改签名**；`internal/cli`（真实的 `query.Service` 消费方）在实例改动后仍然全绿。

（过程记录：终检期间曾观察到一次范围外失败——`internal/harness/kilo/kilo.go:33` 的 `"os" imported and not used`，使 `go build ./...` / `go vet ./...` 短暂失败。那是并行 harness 工作流的在途编辑，不在本切片写入范围，我**没有改动它**；其负责方随后自行修好，重跑 `go build ./...` 与 `go vet ./...` 均为 exit 0。本切片自身包始终 `go vet` 干净、测试全绿。）

**诚实说明**：查询层的声明与行为是在同一段连续编辑里落盘的，因此**没有捕获"先声明后实现"式的原始首次红灯**。下面是随后做的**变异红灯**（每次改坏一处实现、跑测试、再用备份还原，并用 `diff` 确认文件逐字节复原）：这证明新测试确实能失败，而不是恒真。

1. 从 `filterSQL` 去掉 `add("instance_id", f.InstanceIDs)`：

```console
--- FAIL: TestInstanceTotalsSeparateIdenticalLogs (0.04s)
    instance_test.go:103: alpha totals = {Tokens:{Input:2800 …} CostUSD:127 Requests:7 Sessions:3 …}, want 3 requests over 1 root session
```

2. 事实臂单独丢掉 `Filter.InstanceIDs`（只改 `aggregateSQL` 的 `eventsOnly` 分支）：

```console
--- FAIL: TestInstanceFiltersAcrossSeriesCacheGroupsAndSessions (0.04s)
    instance_test.go:223: alpha partial-day totals = {Tokens:{Input:500 …} Requests:2 Sessions:2 …}, want 1 request / 100 input
```

3. 从 `displayProvider` 哈希元组去掉 `v.InstanceID`：

```console
--- FAIL: TestInstanceUnresolvedIdentityIsolatedKnownMerged (0.04s)
    instance_test.go:319: groups = [endpoint:v1:b6332d8e… Requests:5 Sessions:3, unresolved:v1:0708245f… Requests:2 Sessions:2], want 1 merged endpoint group and 2 isolated unresolved groups
```

4. 把三处歧义判定（load 的 meta、load 的事实、`sessionInstance`）改成取第一个匹配：

```console
--- FAIL: TestInstanceSessionLookupIsAmbiguousNotFirstMatch (0.04s)
    instance_test.go:221: Session(codex, s1) err = <nil>, want ErrAmbiguousInstance
```

存储侧的红灯是**真实观察到的**，不是事后补的（修复前的原始观察）：

```console
--- FAIL: TestInstanceMigrationFaultRollsBackFactsAndDDL
    instance_migration_test.go:239: want an invariant error: SQL logic error: error in trigger corrupt_instance_migration: no such table: main.events (1)
```

那次注入用的是任意触发器名（`corrupt_instance_migration`），根因经探针二分确认：SQLite（≥3.25，默认 `legacy_alter_table=OFF`）在 `ALTER TABLE … RENAME TO` 时会**重新解析并改写库内每条触发器的 body**，所以只要某条触发器 body 引用了此刻不存在的表，重建的 rename 步骤就会以 `SQL logic error: error in trigger …: no such table: main.<table>` 中止——**发生在逐请求摘要比较之前**。这解释了为什么"任意名 + `UPDATE events`/`UPDATE event_data`"的注入从未真正改写历史（`WHEN` 键换成 `providerIdentityVersion` 也一样失败），也解释了为什么既有的 `TestLegacyPhysicalMigrationInvariantFailureRollsBack` 从未察觉：它只断言 `err != nil`、从不断言错误文案。

**本次把该注入改成真正能改写历史的形态**（`TestInstanceMigrationFaultRollsBackFactsAndDDL`，现在同时断言不变量文案与回滚证据）：

- 触发器在**旧物理库**上创建：`CREATE TRIGGER provider_cache_delete AFTER INSERT ON settings WHEN NEW.key='instance_schema_version' BEGIN UPDATE event_data SET cost=cost+1 WHERE dedup_key='req-log'; END;`
- 它**借用了 12 个维护触发器之一的名字**：`migrateInstance` 写入 instance marker 后（`instance_migration.go:476`）立刻 `DROP TRIGGER` 同一批 12 个名字（`instance_migration.go:487-490`），所以注入的触发器**在标记阶段改写事实、随后被迁移自身清掉**——重建的 rename 步骤不会重新解析到它，逐请求摘要因此能走到比较点。
- 选 `provider_cache_delete` 而不是 `daily_insert`：后者在更早的 `migrateCompact`（`compact.go:46`）里就被删掉，根本不会触发（实测 `Open` 返回 `nil`、req-log 的 cost 仍是 2.5）。
- 实测错误文案（正是任务要求的"由逐请求不变量、而非 SQL 解析错误拒绝"）：

```console
$ go test ./internal/store/ -run TestInstanceMigrationFaultRollsBackFactsAndDDL -count=1 -v
--- PASS: TestInstanceMigrationFaultRollsBackFactsAndDDL (0.04s)
# 断言到的 Open 错误：
instance migration history invariant failed (3 facts before, 3 after); transaction rolled back
```

  事实数不变、值被改写，摘要据此拒绝；随后 `assertNoIdentityMigrationArtifacts` / `assertFactsUnchanged` / 无 marker / `events` 无 `instance_id` 列 / 备份产物仍在，全部成立。

**残留限制（如实记录，而不是把断言削弱成假证明）**：该 fixture 依赖"注入的触发器名恰好属于 `migrateInstance` 会在标记阶段删除的那 12 个名字"——这是让改写存活到摘要比较点的唯一途径；任意命名且 body 引用后续会被 rename 的表时，SQLite 的 rename 重解析会先以解析错误中止，此时被证明的是**回滚路径**而非摘要路径。`TestInstanceFactStampsIsPerRequest` 仍以逐字段 UPDATE（token 类别/时间戳/cost/log_cost/priced/dedup_key）独立证明摘要对单请求变化敏感，DELETE 则改变计数。

## 未关闭的门与范围外事项

1. **采集/配置接线未做**（本次范围外）：`Write.InstanceID` 已就位且 `CommitMany` 无需改签名，但没有任何代码把 `internal/instance` 的 `Entry.ID` 填进去，`CursorIn`/`CursorsIn` 也无人调用。多实例端到端（扫描两个根、各自游标、GUI/CLI 展示实例）**尚未可用**。
2. **严格性能门已跑过一次（在无其它测试运行时，事后执行）**：`MYTOKEN_STRICT_PERF=1 go test ./internal/query/ -run TestQueryPerformanceBudget -count=1`（200000 事件 fixture，单查询 ≥100ms 或 ≥50MiB 即失败）→ `ok 7.652s`，23 个子用例全部 PASS，最慢 `SessionCache 1.041291ms / 192048 bytes`，其余多在 1ms 以下（`TestQueryPerformanceBudget` 在 `internal/query/bench_test.go:145`，`BenchmarkService` 在 `:127`）。新增的 `instance_id` 进入 `GROUP BY`/`UNIQUE`/索引只带来常量级开销。**并发代理可能同时占用机器，建议 Lead 在最终集成时复跑一次**。
3. **`docs/verification` 里两处示例哈希已陈旧**（范围外，不改）：`docs/verification/v040-provider-cli.md:182-184` 与 `docs/verification/v040-provider-gui.md:330` 记录的 `unresolved:v1:<sha256(harness,session,provider,baseURL)>` 示例，其哈希元组现在多了 `instance_id`。没有任何 Go 测试硬编码未解析哈希（只有 `internal/cli/provider_identity_test.go:227-228` 的 `strings.HasPrefix(…, "unresolved:v1:")` 与 `internal/query/provider_ids_test.go:187` 的伪造串 `"unresolved:v1:00"`）。
4. **序列不按实例拆分**是设计选择而非遗漏：`Point` 没有实例字段，`Daily`/`Hourly` 无过滤时是选中集合的时间并集；需要按实例分离时用 `Filter.InstanceIDs`（或将来在 GUI 层按实例分面查询）。如果产品要求"同图两条实例曲线"，那需要给 `Point` 增加实例字段并改变返回粒度——**这是新决策，不在本次范围**。
5. **未跑真实迁移**：`path + ".pre-instance-v1.db"` 的备份、200K 级事实表重建耗时（计划 §12 R1 预计单事务内重建）都只在 fixture 上验证过；真实库首次打开的表现未测。
