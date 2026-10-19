# 0.4.0 迁移与备份安全验证

日期：2026-10-13。环境：Go 1.27.1、darwin/arm64（Apple M5）。分支基线：2e696f2 加工作区未提交修改。
写入范围：`internal/store/**`、本文件。未改 query/provider/gui 及任何共享规划文件。
本轮更新：复核指出基线摘要晚于增量初始化，已改为"任何写操作之前取基线"，并补了缺失 `priced` 归一化、`provider_endpoints` 非表对象预检、备份失败措辞四项修正（见第 4 节）。

## 结论与范围

第一轮修复了三个可复现缺陷、补了一组此前完全缺失的真实 legacy 物理表迁移覆盖，并对 workflow-9 报告中不成立的条目给出证据反驳；本轮（复核后的定向修正）修复了基线顺序、缺失 `priced` 归一化、`provider_endpoints` 非表对象预检与备份失败措辞四项，并新增对应回归。备份/迁移的保守策略保持不变：失败备份保留、拒绝覆盖 `path + ".pre-provider-v1.db"`、备份失败不迁移、不变式失败回滚、绝不自动删除/复用备份或恢复真实库。本轮没有扩大迁移范围，也没有加入任何自愈逻辑。

这不是 0.4.0 完成声明。未验证项见文末"未关闭的门"。

## 已修复缺陷（均先红后绿）

| 缺陷 | 影响 | 修复位置 |
| --- | --- | --- |
| `historyDigest` 不含 `priced` | 迁移过程中定价标志被改写（等于静默重定价）不会被不变式发现 | `internal/store/provider_migration.go` `historyDigest` |
| 基线摘要晚于增量初始化 | 真实 legacy 表在 `UPDATE events SET raw_project=project` / `SET priced=...` 时会触发 legacy 触发器改写历史 token/cost；基线在加列循环之后取，等于拿"已改写状态"与自身比较，不变式必然通过 → 静默改写历史并提交 | `internal/store/migration.go:18-31` |
| 摘要把缺失 `priced` 当 0 | 真实 legacy 表没有 `priced` 列；若基线直接按列取，`log_cost IS NOT NULL OR cost!=0` 的合法初始化（0→1）会被误判为改写 | `internal/store/provider_migration.go` `legacyPricedExpression`、`historyDigest` |
| `provider_endpoints` 非表对象 + 标记缺失被放行 | `checkProviderIndex` 在标记为空时只拒绝 `kind=='table'`；名为 `provider_endpoints` 的 view/index/trigger 会通过预检，迁移先写备份、再在 `CREATE TABLE` 处以原始 DDL 错误失败 | `internal/store/provider_migration.go:35-43` |
| 标记/表不一致被静默接受 | `provider.identity.v1='1'` 但 `provider_endpoints` 缺失时，迁移被"跳过"，之后无法再记录端点身份；反向不一致（表在标记缺失）则在 DDL 深处抛出原始 SQLite 错误，且已经写过备份 | `internal/store/provider_migration.go` `providerIndexInconsistent`、`checkProviderIndex` |
| 空备份产物被当作有效备份 | `PRAGMA quick_check` 对 0 字节目录库返回 `ok`（已实测），因此仅靠 quick_check 会接受一个未写入内容的产物 | `internal/store/backup.go:17`、`:58` |
| 备份目录项未落盘 | 备份文件内容 fsync 后，其目录项（文件名）仍可能丢失，崩溃后留下不可达文件 | `internal/store/backup.go:35`、`:39` |
| 备份失败报错夸大 preflight 范围 | 原文声称 `stopped before any database changes` / `The live index was not modified`，但 `Open` 在 `migrate()` 之前已执行 bootstrap DDL（journal mode、按需建表/索引），真实 legacy 库此时并非字节未改 | `internal/store/provider_migration.go` `prepareProviderMigration` |

关键的顺序修正：**基线摘要在任何写操作之前取**（`internal/store/migration.go:18-31`，紧接 `tx.Begin()`）。真实 legacy 表的加列初始化本身就是对历史表的 UPDATE，legacy 触发器可以在此改写 token/cost；基线若晚于它，改写值已被纳入基线，比较结果永远相等，缺陷会被静默提交。列尚不存在时，`historyDigest` 用 `legacyPricedExpression`（`CASE WHEN log_cost IS NOT NULL OR cost!=0 THEN 1 ELSE 0 END`，与初始化谓词逐字一致）把缺失的 `priced` 归一化为"合法初始化后应有的值"，而不是当 0，避免把合法初始化误报为改写；已有 `priced` 列时按实际值取，保留显式标志。

摘要覆盖范围（明确边界）：`historyDigest` 覆盖请求标识、timestamp、五类 token、`cost`/`log_cost` 与 `priced`，即用户契约中的历史请求/token/费用；不覆盖 `raw_project` 这类派生展示列，也不覆盖 `project`。本批没有扩大摘要范围。

## 先红后绿证据（精确命令与结果）

### 1. 真实 legacy 物理表覆盖（首轮即绿，属覆盖补齐）

新增 `internal/store/migration_legacy_test.go`，用 `writeLegacyPhysicalDatabase` 造与 0.4 之前 `store.go` 引导 schema 完全一致的物理 `events` 表（无 `priced`、无 `raw_project`），而非现有 `legacyProviderFixture` 那种"先建新 schema 再删一张表"的近似夹具。

```sh
go test ./internal/store/ -run 'TestLegacyPhysical' -v
```

结果（首次运行即通过，是既有行为的守护测试，不是新功能）：
`TestLegacyPhysicalMigrationPreservesFactsAndBacksUp`、`TestLegacyPhysicalMigrationKeepsExistingPricedFlags`、`TestLegacyPhysicalMigrationInvariantFailureRollsBack`、`TestLegacyPhysicalBackupFailureStopsBeforeChanges` 全部 PASS；第 5 个 `TestLegacyPhysicalMigrationDetectsTriggeredInitRewrite` 是本轮新增的回归（见第 4 节）。

断言内容：请求数 3、input/output/cache_read/cache_write/reasoning = 33/36/39/42/45、`sum(cost)=3.25`、`sum(log_cost)=2.5` 迁移前后一致；`pricing_fingerprint` 检查点值不变；备份 `path+".pre-provider-v1.db"` 必须是**迁移前**的 legacy 快照（`events` 仍是 `table`、不含 `event_data`）；`priced` 逐行等于 `req-log=1, req-est=1, req-free=0`；二次 `Open` 幂等；不变式失败或备份失败时物理表、事实、增量列、`event_data`、`provider_endpoints`、标记全部回到原状，遗留备份仍保留。

### 2. 不变式覆盖与标记一致性

```sh
go test ./internal/store/ -run 'TestProviderMigration' -v
```

红（修复前）：
```
--- FAIL: TestProviderMigrationDetectsPricingFlagCorruption
    migration_integrity_test.go:30: pricing flag corruption during migration was committed
--- PASS: TestProviderMigrationDetectsDroppedRowWithoutRowcount
--- FAIL: TestProviderMigrationMarkerWithoutEndpointsFailsActionably
    migration_integrity_test.go:95: inconsistent index (marker without provider_endpoints) was accepted
--- FAIL: TestProviderMigrationEndpointsWithoutMarkerFailsActionably
    migration_integrity_test.go:142: failure is not actionable: SQL logic error: table provider_endpoints already exists (1)
```

绿（修复后，同一命令）：上述全部 PASS，连同既有 `TestProviderMigrationBacksUpAndPreservesFacts`、`TestProviderMigrationInvariantFailureRollsBack`、`TestProviderMigrationBackupFailureStopsOpen`。

新增的一致性校验在**取备份之前**执行，返回可行动错误并拒绝猜测：
`provider identity index is inconsistent: <具体不一致>; refusing to guess, to reprice, or to skip the migration. Nothing was rewritten and no backup was replaced. Recovery needs an explicit operator decision`。
测试同时断言此时**没有**生成 `path+".pre-provider-v1.db"`：既然不迁移，就不该留下一个以后会挡住真正迁移的产物；也断言标记没有被"自愈"写入、历史费用未被改写。

修复过程中出现过一次自己引入的中间态：给摘要查询加了 `priced` 但 `dest` 切片仍写死 10，`go test` 报 `panic: runtime error: index out of range [10] with length 10`；改为 `len(values)` 后消失。该中间态未进入任何提交，也未影响其他批次。

### 3. 备份产物与目录 fsync

新增 `internal/store/backup_durability_test.go`。

红（修复前即编译失败，属新增单元的预期红）：
```
internal/store/backup_durability_test.go:30:15: undefined: verifyBackupArtifact
internal/store/backup_durability_test.go:62:12: undefined: syncDirectory
internal/store/backup_durability_test.go:99:11: undefined: syncParentDirectory
```

```sh
go test ./internal/store/ -run 'TestVerifyBackupArtifact|TestSyncDirectory|TestBackupArtifactIsNonEmpty|TestProviderMigrationBackupFailureErrorIsActionable|TestBackup' -v
```

绿（修复后）：4 个新测试 + 3 个既有备份测试全部 PASS。其中 `TestVerifyBackupArtifactRejectsEmptyArtifact` 先记录证据——对 0 字节文件执行 `PRAGMA quick_check` 返回 `ok` 且无错误——再断言 `verifyBackupArtifact` 以 "empty (0 bytes)" 拒绝该产物、拒绝目录（"not a regular file"）、接受非空普通文件。

实测平台事实（darwin/arm64，APFS）：`os.Open(dir).Sync()` 返回 nil；`os.OpenFile(...,O_CREATE|O_EXCL|O_RDWR,0600)` + `f.Sync()` 返回 nil，权限 `-rw-------`。

`TestProviderMigrationBackupFailureErrorIsActionable` 断言失败信息包含：`stopped before any history was migrated`、`the migration transaction never started`、`does not promise a byte-identical file`、备份绝对路径、`never overwritten, reused or deleted`、`Stop every MyToken process`、`-wal and -shm`、`verify both independently`、`Do not restart while this state is unverified`；并断言既有备份内容未被修改、live 库费用未变、标记未被提前写入。

### 4. 基线顺序、非表对象预检与措辞修正（本轮新增，先红后绿）

红（修复前，两个新回归都失败）：

```sh
go test ./internal/store/ -run 'TestLegacyPhysicalMigrationDetectsTriggeredInitRewrite|TestProviderMigrationNonTableEndpointsObjectFailsBeforeBackup' -v
```
```
--- FAIL: TestProviderMigrationNonTableEndpointsObjectFailsBeforeBackup
    migration_integrity_test.go:184: failure is not actionable: SQL logic error: view provider_endpoints already exists (1)
--- FAIL: TestLegacyPhysicalMigrationDetectsTriggeredInitRewrite
    migration_legacy_test.go:294: history rewritten while initializing additive columns was committed
```

绿（修复后，同一命令）：两个测试 PASS，连同 `TestLegacyPhysical*`、`TestProviderMigration*`、`TestVerifyBackupArtifact*`、`TestSyncDirectory*`、`TestBackup*` 全部 PASS（`ok github.com/zzstar101/mytoken/internal/store 0.845s`）。

- `TestLegacyPhysicalMigrationDetectsTriggeredInitRewrite`：在真实 legacy 物理表上建 `AFTER UPDATE ON events` 触发器，同时改写 `cost=cost+1` 与 `input=input+1`；它只会被 `raw_project`/`priced` 初始化 UPDATE 触发，不会被身份迁移步骤触发。断言 `Open` 必须报 `invariant` 失败；live 库回滚到原 legacy 表与原始事实（`assertFactsUnchanged`、`assertNoIdentityMigrationArtifacts`）；保留的 `path+".pre-provider-v1.db"` 仍是迁移前 legacy 快照（`events` 是 table、事实一致）。这正是"基线必须早于任何写操作"的守护测试。
- `TestProviderMigrationNonTableEndpointsObjectFailsBeforeBackup`：把 `provider_endpoints` 造为 view 且标记缺失。断言报 `provider identity index is inconsistent`、含 `view` 与 `provider_endpoints`，且**没有**生成备份，标记与历史均未被改写。若预检只拒绝 `kind=='table'`，该库会先写备份、再以 `SQL logic error: view provider_endpoints already exists` 失败（见上红证据）。

Preflight 范围（如实记录，不夸大）：`prepareProviderMigration` 在 `migrate()` 的事务开始前运行，保证的是**历史行不变**——没有 event/session/price 行被改写、身份标记未写入、迁移事务未开始。它不保证文件字节一致：`Open` 在此之前已执行 bootstrap DDL（`PRAGMA journal_mode=WAL`、`CREATE TABLE IF NOT EXISTS`、`CREATE INDEX IF NOT EXISTS`），真实 legacy 库此时已非字节未改。错误文案已改为 `provider migration stopped before any history was migrated` + `the migration transaction never started` + `does not promise a byte-identical file`，并由测试逐句断言，防止再次出现"未改动数据库/未修改 live 索引"的过度声明。本批没有借此扩大迁移范围，也没有加入任何自愈逻辑。

### 5. 包级回归

```sh
gofmt -l internal/store/                  # 无输出
go vet ./internal/store/                  # exit 0
go test -count=1 ./internal/store/        # ok github.com/zzstar101/mytoken/internal/store 18.428s（本轮）
```

本轮按委托要求只跑 store 定向测试 + 非 race 的 store 全量；上一轮记录的 `go test -race -count=1 ./internal/store/`（582.098s）未重复执行，race 结论沿用上一轮，**不代表本轮新增/修改路径已获得 race 覆盖**。

## 恢复流程（需操作者显式决定）

以下步骤是**人工决策流程**，本批没有实现任何自动恢复或自动清理。失败时 MyToken 不会启动完成迁移，也不会改动 live 库。

1. 停掉所有 MyToken 进程（托盘/CLI/GUI 全部），确认没有进程持有该库。
2. 保留证据：把数据库文件**连同 `-wal`、`-shm`** 一起复制/保留；同时保留 `path + ".pre-provider-v1.db"`，无论它看起来是否完整（0 字节或极小者很可能是上次中断的产物，不是可用备份——但不要删除，它是现场证据）。
3. 独立核验两边，而不是假设任一边正确：
   - 数据库：只读打开后 `PRAGMA quick_check`，并核对事件数与 token 合计；
   - 备份：以 `mode=ro` 打开，确认 `PRAGMA quick_check` 为 `ok`，且其中的请求数/token/历史费用与预期一致。
4. 在以下选项中**显式选择**，不要依赖程序自动猜测：修好备份失败的原因后重试（需先自行决定如何处理已存在的备份文件，例如改名另存）；或从已独立核验的备份恢复。程序本身不会覆盖、删除、复用该备份文件，也不会自动重试不安全的迁移。
5. 核验完成并做出选择后才重启；状态未核验期间不要重启。

若启动报错为 `provider identity index is inconsistent: ...`，说明标记与 `provider_endpoints` 不一致（部分应用的迁移，或该名字下是 view/index/trigger 等非表对象）：同样按上述流程停进程、保留现场、独立核验后人工修复或恢复，不要手工补写标记来"绕过"。

## 对 workflow-9 报告条目的判定

- **"historyDigest 没有显式 row count，所以检测不到丢行"：不成立。** `historyDigest` 是有序遍历并对每行做 JSON 编码进 SHA-256 流；丢行/重复行会直接改变流。`TestProviderMigrationDetectsDroppedRowWithoutRowcount` 用 `AFTER INSERT ON settings` 触发器删掉一行，迁移即报 `invariant` 并回滚（该测试在修复前就是绿的）。故未按报告建议加 row count。
- **priced 等字段"直接加进摘要"：不能直接加。** 真实 legacy 表没有该列；直接加会让旧库在取基线时失败，或把合法初始化误判为改写。本批的处理是"基线在任何写操作之前取 + 摘要包含 `priced`，列缺失时用 `legacyPricedExpression` 归一化为合法初始化值"，并新增 `TestLegacyPhysicalMigrationKeepsExistingPricedFlags` 固定"已存在的 priced 标志（含显式 0 且 cost 非零、cost=0 却 priced=1 的边界）必须原样保留，不得按 cost/log_cost 重算"。
- **目录 fsync / Windows**：已在 macOS 实测并实现；Windows 分支是显式 no-op（`syncDirectory` 的 `goos=="windows"` 早返回），**未在 Windows 上执行过任何测试，不声称 Windows 已验证或已持久**。
- 报告其他条目（端点 registry、SQL 替换、性能倍数等）不在本写入范围，未在本批处理或判定。

## 未关闭的门

1. **恢复体验仍是手工流程**：没有 `mytoken doctor` 之类的备份核验/会话恢复入口，错误只把步骤说清楚。
2. **磁盘故障语义未验证**：没有做真实 ENOSPC/EIO 注入；"空产物"检查是防御性校验 + 已实测的 quick_check 盲区，不是在真实磁盘故障下观测到的行为。
3. **Windows 未测**：目录 fsync 在 Windows 上为 no-op，该平台的备份崩溃持久性未验证。
4. **未做崩溃/杀进程注入**：备份写入过程中 kill 进程的行为、以及 `-wal` 在极端断电下的可见性，均未模拟。
5. **未做超大库/长时间 VACUUM 的耗时与空间测量**。
6. **标记与表都齐全但内容部分回填**无法被当前校验发现：一致性检查只看标记值与表是否存在，没有逐端点核对 `provider_endpoints` 是否已把历史 `base_url` 全部登记。正常路径下两者写在同一事务内，不会出现该状态；若将来出现，需要额外的版本/计数元数据才能判定。
7. **本批改动所在的 `internal/store/backup.go`、`provider_migration.go` 及新增测试在 git 中仍是未跟踪文件**（整个 provider 身份迁移特性尚未提交），因此没有提交基线可对比；复核应以工作区快照为准。
8. `docs/verification/v040-review-triage.md` 与本文件均为待复核的中间记录，不构成发布许可。
9. **摘要不覆盖派生展示列**：`historyDigest` 不包含 `raw_project`/`project`；只改写这些列、且不改动请求/token/cost/priced 的触发器不会被不变式发现。本批按用户契约只保护历史事实与费用，未扩大摘要或迁移范围。
