# 0.4.0 只读 store/路径基础验证（修正版）

日期：2026-10-13；修正：同一工作区只读可用性修复轮。环境：Go 1.27.1、darwin/arm64。分支基线：2e696f2 加工作区未提交修改。
写入范围：`internal/store/**`、`internal/snapshot/**`、`internal/cli/**` 与本文件。**未改** `internal/app`、`internal/query`、`internal/gui`、任何 schema 迁移文件或共享规划文件，未改任何持久化数据库内容、已提交 WAL 帧、迁移/重定价/凭据/网络路径。

> **本文件修正了上一版的错误契约。** 上一版把只读打开的安全声明写成"绝不创建、绝不删除任何文件（含 `-wal`/`-shm`）"，并据此让默认入口在 `-shm` 缺失或不属于本进程时一律以 `wal_sidecar_missing` / `wal_writer_required` 拒绝。这个契约既做不到（§4 实测），也不是需求：它使 `mytoken snapshot --once`、`mytoken report`、`mytoken statusline`、`mytoken doctor` 在任何写者进程持有索引时、以及在任何干净关闭的 WAL 库上全部不可用。上一版与之绑定的 API（`OpenReadOnlyAllowForeignWriter`）、写者注册表（`registerLiveWriter`/`unregisterLiveWriter`/`liveWALWriters`/`sidecarGuard`/`writerKey`）和"缺边车即拒绝"的分支已全部删除，写路径恢复原样。

## 1. 修正后的契约

`store.OpenReadOnly` 的承诺是**不做任何应用写入**，而不是"不产生任何文件系统写入"：

1. **不做应用写入**：`mode=ro` + `PRAGMA query_only(1)`。不建表、不迁移、不重定价、不重建 rollup、不写迁移标记、不写备份、不联网、不扫描，也不创建数据库文件或其父目录（缺失即 `missing_database`，从不创建）。
2. **绝不隐藏已提交的 WAL 帧**：`immutable=1`/`nolock` 永不出现在 DSN 中。实测 `immutable=1` 会把活跃 WAL 库读成 `count=0` 并把 `journal_mode` 报成 `delete`，即静默丢数据。
3. **绝不改动数据库主文件与已提交的 `-wal` 帧**，绝不删除任何文件，不改权限，不复制源文件，也不打开任何可写连接。
4. **允许 SQLite 自己的 WAL 读协调簿记，并明确披露**：
   - WAL 库且 `-shm` 不存在时，SQLite 在 attach 时会创建 `<db>-shm` 与 0 字节的 `<db>-wal`；这两个文件可能在该次读取结束后继续留存。
   - `-shm` 已存在时，读者会改写 wal-index 的 read mark（实测恰好 1 字节，偏移 104，落在 `[96,120)` 区间）。
   - 这是共享内存索引与锁簿记，不是索引数据；它不修改数据库文件，也不修改已提交的 WAL 帧。

保留的 typed 拒绝（一律不返回句柄）：`missing_database`、`not_regular_file`、`permission_denied`、`corrupt`、`locked`（超过 `readOnlyBusyTimeoutMS`，默认 5000ms）、`schema_upgrade_required`、`schema_newer`、`schema_unreadable`、`unavailable`。非 ok 的 schema 判断绝不返回空数据集。

README/usage/envelope 注释现在都区分"无应用写入"与"SQLite 的临时协调写入"，不再声称整个文件系统逐字节不变，也不再声称所有实际文件系统写入为空。

## 2. 精确 API

```go
func OpenReadOnly(ctx context.Context, path string) (*Store, error)
func InspectSchemaReadOnly(ctx context.Context, path string) (SchemaReport, error)
func (s *Store) ReadOnly() bool
func (s *Store) ReadOnlySchema() SchemaReport
func ReadOnlyReasonOf(err error) (ReadOnlyReason, bool)

type SchemaState string   // "ok" | "upgrade_required" | "newer" | "unreadable"
type SchemaReport struct { State SchemaState; Markers map[string]string; Missing []string; Detail string }
type ReadOnlyReason string
type ReadOnlyError struct { Reason ReadOnlyReason; Path string; Schema SchemaReport; Detail string; Err error }
// ReadOnlyError 实现 Error() / Unwrap() / Is()；Is 按 Reason 比较
var ErrReadOnlyMissingDatabase, ErrReadOnlyNotRegularFile, ErrReadOnlyPermissionDenied,
    ErrReadOnlyCorrupt, ErrReadOnlyLocked, ErrReadOnlySchemaUpgrade, ErrReadOnlySchemaNewer,
    ErrReadOnlySchemaUnreadable, ErrReadOnlyUnavailable error
```

`Reason` 取值：`missing_database`、`not_regular_file`、`permission_denied`、`corrupt`、`locked`、`schema_upgrade_required`、`schema_newer`、`schema_unreadable`、`unavailable`。`wal_sidecar_missing` 与 `wal_writer_required` 已删除。

`store.Store` 只新增 `readOnly bool`；`schema SchemaReport` 由只读打开填充。`OpenReadOnly` 产出的句柄是普通 `*Store`，`query.NewService(st)` 与既有只读消费者无需改动。`internal/store/backup.go:84` 用 `sqlitedsn.URI(destination, "mode=ro")` 校验自己刚写出的备份产物，不在本契约内。

## 3. schema 校验（数据驱动，非伪造全局版本号）

校验表 `readOnlyMarkers`（`internal/store/readonly.go`）：

| 标记 | 本构建已知值 | 形状检查 | 缺失时 |
| --- | --- | --- | --- |
| `provider.identity.v1` | `"1"` | `checkProviderIndex` | `upgrade_required` |
| `provider.endpoint-rollup.v1` | `"1"` | `checkProviderRollupIndex` | `upgrade_required` |
| `instance_schema_version` | `"1"` | `checkInstanceSchema` | `upgrade_required` |
| `ledger_version` | `ledger.Version` | 无 | **允许缺失** |

规则：值 == 已知值 → 跑形状检查，失败即 `upgrade_required`（并在 `Missing` 里点名标记）；值缺失 → 除 `ledger_version` 外一律 `upgrade_required`；值非空且不是已知值 → `newer`。`settings` 不是表 → `upgrade_required`；`events` 读面缺失 → `upgrade_required`。

`ledger_version` 允许缺失：它由 `EnsurePrices → ensureLedgerVersion`（`internal/store/pricing.go:55`、`internal/store/cache.go:150`）写入，是**派生缓存**版本而非 schema 迁移标记；只读打开永不调用 `EnsurePrices`，因此从未定价过的库天然没有这一行。缓存列由 rollup 触发器增量维护，读取值仍然正确；但它一旦存在且值未知，仍是 `newer`。

`upgrade_required` 的 Detail 现在只指向**正常可写启动**（`mytoken scan`）。上一版建议"跑 `mytoken doctor` 完成迁移"是错的：doctor 自身只读，无法迁移。

## 4. 为什么必须允许 SQLite 的边车簿记（实测）

darwin/arm64、modernc.org/sqlite v1.60.1 实测：干净关闭的 WAL 库在可写目录下用 `mode=ro` 打开会**创建** `<db>-shm`（32768 字节）和 `<db>-wal`（0 字节），且只读连接关闭后仍然留存；`cache=private`、`psow=0`、`_pragma=query_only(1)` 都无法避免；`vfs=unix-excl` 同样创建；`vfs=unix-none` 直接报 `unable to open database file (14)`；`_pragma=locking_mode(EXCLUSIVE)` 报 `disk I/O error (3850)`（=SQLITE_IOERR_LOCK，只读 fd 无法取 EXCLUSIVE 锁）；驱动自带的只读 VFS `modernc.org/sqlite/vfs.New(os.DirFS(dir))`（`io_methods` iVersion=1，没有 `xShmMap`/`xWrite`/`xCreate` 路径）对 rollback-journal 库完全无创建地可读，但对任何 WAL 库报 `unable to open database file (14)`。结论：**没有任何 DSN/驱动开关能既读 WAL 又不创建 `-shm`**，所以"缺 `-shm` 即拒绝"只会把正常用法一并拒绝，而不是提高安全性；正确做法是接受并披露这项 SQLite 簿记。

`-shm` 已存在时的 read mark 写入同样是 wal-index 的共享内存状态，不是数据库内容。§5 给出对应断言：数据库与 `-wal` 的 sha256 不变，`-shm` 大小不变，差异偏移必须落在 `[96,120)`。

## 5. 本轮验证（定向测试 + 一次真实跨进程冒烟）

定向测试（无广泛 race/性能循环）：

```sh
go test -count=1 ./internal/store/ ./internal/snapshot/ ./internal/cli/
go vet ./internal/store/ ./internal/snapshot/ ./internal/cli/
gofmt -l internal/store internal/snapshot internal/cli
```

结果：`ok internal/store`、`ok internal/snapshot`、`ok internal/cli`；vet 与 gofmt 无输出。

只读测试清单（`internal/store/readonly_test.go`，27 个 + 外部包 2 个）：`TestOpenReadOnlyMissingDatabaseCreatesNothing`（3 子）、`TestOpenReadOnlyRejectsNonRegularFile`、`TestOpenReadOnlyCorruptDatabaseIsTypedAndUntouched`、`TestOpenReadOnlyTruncatedDatabaseIsTyped`、`TestOpenReadOnlyUnclassifiableSchemaIsUnreadable`、`TestOpenReadOnlyLegacySchemaIsUpgradeRequiredAndUntouched`、`TestOpenReadOnlyPreSettingsSchemaIsUpgradeRequired`、`TestOpenReadOnlyNewerSchemaIsReportedWithoutTouchingBytes`、`TestOpenReadOnlyUnknownProviderMarkerIsNewer`、`TestOpenReadOnlyUnknownLedgerMarkerIsNewer`、`TestOpenReadOnlyAppliesReadOnlyPragmas`、`TestOpenReadOnlySeesCommittedWALFromLiveWriter`、`TestOpenReadOnlyReadsCleanlyClosedWALDatabase`、`TestOpenReadOnlyReadsWALWithoutSharedMemory`、`TestOpenReadOnlyReadsForeignWriterWAL`、`TestOpenReadOnlyReadsSidecarsWithNoLiveWriter`、`TestOpenReadOnlyReadOnlyReaderWritesOnlyTheWALReadMark`、`TestOpenReadOnlyHandleSurvivesWriterClose`、`TestOpenReadOnlyReadsRollbackJournalDatabase`、`TestOpenReadOnlyWithReadOnlyFilePermissions`、`TestOpenReadOnlyUnreadableDatabaseIsTyped`、`TestOpenReadOnlyLockedDatabaseIsBoundedAndTyped`、`TestOpenReadOnlyPerformsNoApplicationWrite`、`TestInspectSchemaReadOnlyCurrentSchema`、`TestInspectSchemaReadOnlyMissingDatabase`、`TestOpenReadOnlyInconsistentInstanceSchemaIsUpgradeRequired`、`TestReadOnlyErrorIsTyped`；`internal/store/readonly_query_test.go`：`TestQueryServiceOverReadOnlyHandleMatchesWritableHandle`、`TestReadOnlyHandleSubscribeIsUsable`。

本轮新增的四个正向用例替代了上一版的拒绝用例：`TestOpenReadOnlyReadsCleanlyClosedWALDatabase`（干净关闭、无 `-shm`）、`TestOpenReadOnlyReadsWALWithoutSharedMemory`（只有库与 `-wal`）、`TestOpenReadOnlyReadsForeignWriterWAL`（外部写者持有边车，读到其已提交帧且库/`-wal` 字节不变）、`TestOpenReadOnlyReadsSidecarsWithNoLiveWriter`（第一次只读打开创建 `-shm`，第二次在没有活跃写者的情况下仍可读）。`TestOpenReadOnlyNeverWrites` 改名为 `TestOpenReadOnlyPerformsNoApplicationWrite`：在活跃写者已持有边车的前提下，文件集合、大小与库摘要都不变。

跨进程冒烟：`internal/cli/readonly_smoke_test.go:TestReadOnlyCommandsCrossProcessWALSmoke`。它把测试二进制自身再执行为夹具写者进程（`TestFixtureWALWriterProcess`），该进程 `store.Open` 一个 WAL 索引、提交两个已定价事件（$1.25 / $0.75）并写入一个过期的 `pricing_fingerprint`，打印 `ready` 后保持可写连接（含 `-wal`/`-shm`）打开，直到父进程关闭其 stdin。写者存活期间：

- `snapshot --once --json` → `sources.index.available=true`、`schema.state=ok`、`requests=2`、`tokens=58`、`costUsd=2`；
- `statusline --json` → `index.available=true`，`session.requests=1` 且 cost 为已提交的 1.25 或 0.75；
- `report --json` → `current.totals.requests=2`；
- `doctor --json` → `readOnly=true`、`dbUnavailable=false`、`schema.state=ok`（修正前这里是 `dbUnavailable=true` + `wal_writer_required`）；
- 读完后数据库镜像与已提交 `-wal` 帧的 sha256 与读前一致（`-shm` 允许被写 read mark，故不断言其摘要）；
- 关闭写者（checkpoint 并删除边车）后再跑 `snapshot --once --json` → 仍是 `requests=2`、`tokens=58`、`costUsd=2`；
- 只读复读：`settings.pricing_fingerprint` 仍为那个过期值，事件的 `cost` 仍为 1.25——读路径既不重定价也不改元数据；
- 缺库场景：指向空目录运行 `snapshot --once --json` → `reason=missing_database`，目录内容指纹不变且 `mytoken.db` 不存在（只读打开不创建库与父目录）。

## 6. 未关闭的门（已知限制）

1. **SQLite 会在缺 `-shm` 的 WAL 库上创建 `-shm` 与 0 字节 `-wal`，且可能留存**（§4）。这是契约允许并披露的簿记，不是索引数据；需要文件系统绝对零新建的调用方不能用此路径。
2. **`immutable=1` 永不使用**：实测会对活跃 WAL 库静默丢帧。DSN 中不存在该参数，由 `TestOpenReadOnlyAppliesReadOnlyPragmas` 与 `TestOpenReadOnlySeesCommittedWALFromLiveWriter` 双重守护。
3. **读者会改写已存在 `-shm` 的 read mark（1 字节）**：`TestOpenReadOnlyPerformsNoApplicationWrite` 的目录/摘要断言与 `TestOpenReadOnlyReadOnlyReaderWritesOnlyTheWALReadMark` 的区间断言共同刻画真实边界。
4. **跨进程竞态不再被拒绝，而是被披露**：在"读者 attach"与外部写者 "checkpoint + 关闭"之间仍存在竞态，最坏结果是只读进程在 attach 时重建 `<db>-shm` 与 0 字节 `<db>-wal`；数据库内容不会被写、已提交帧不会被隐藏、不会删除文件。这正是修正后契约接受的行为。
5. **跨表不是单一时点快照**：`internal/query` 每次调用各自 `BeginTx(ReadOnly)`，单次调用内部一致；一次快照里的多次 Service 调用不是同一个事务。`query.ReadEnvelope` 单次调用是一致读事务。
6. **只读打开不做 `EnsureLocalDays`**：若 `settings.daily_timezone` 与当前 `LocalDayZone()` 不一致，`daily_usage`/`cache_daily` 行是旧的；`internal/query/aggregate_sql.go:127`、`cache_query.go:130` 已有只读回退，数字正确但更慢。
7. **只读打开不重定价**：`EnsurePrices` 不跑，库里的 `cost`/`log_cost` 与 `pricing_fingerprint` 原样返回（冒烟测试断言了这一点）。调用方必须在快照元数据里说明定价指纹与定价时间，而不是假装价格最新。
8. **不支持的 schema 直接拒绝**：`upgrade_required` / `newer` / `unreadable` 都不返回句柄，因此不存在"半迁移 schema 上读出错误数字"的路径；升级只能由正常可写启动（`mytoken scan`）完成，`mytoken doctor` 只读、不能升级。
9. **平台覆盖**：实测仅在 darwin/arm64 + modernc.org/sqlite v1.60.1 上完成；Windows 与网络文件系统（NFS/SMB）的 WAL/`-shm` 语义未实测。

## 7. 明确不在本轮范围

`internal/app` 的接线（含把 `App.OpenLocal` 的 `EnsurePrices` 从只读路径拆出）、`internal/gui` 快照页、以及任何 schema 迁移/重定价/凭据/网络改动均未做。GUI 与其他不在上述写入范围内的包由对应 owner 决定是否跟进。
