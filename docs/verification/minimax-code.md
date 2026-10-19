# MiniMax Code 0.4.x 离线适配验证（internal/harness/minimax）

日期：2026-10-16 10:19 CST；环境：go1.27.1 darwin/arm64；工作目录 `/Users/zzstar/Developer/Programs/MyToken!!!!!`；分支 `feat/0.4x-harnesses`；HEAD `a9684c7`。**未提交**。

写入范围（严格限制）：仅 `internal/harness/minimax/**` 与本文件。**未改动** `internal/model/model.go`、`internal/app/app.go`、`internal/harness/rooted_test.go`、`internal/harness/provenance_test.go`、`internal/gui/**`、`internal/store`、`internal/query`、其他 harness（`kimi/**`、`pi/**`、`zcode/**`、`mimocode/**`、`stepcode/**`）及任何共享规划文件（`task_plan.md`、`findings.md`、`progress.md`、`docs/ROADMAP.md`）。注册接线由 Lead 完成（已完成，见文末「Lead 集成更新」；`internal/harness/minimax` 现已被 blank import，注册数 32）。

```console
$ git status --porcelain -- internal/harness/minimax docs/verification/minimax-code.md
?? internal/harness/minimax/
?? docs/verification/minimax-code.md
```

## 结论与范围

新增离线 harness 包 `internal/harness/minimax`：只读解析 MiniMax Code 的本地运行时账本 `<data dir>/v2/sqlite/runtime-state.sqlite` 中的 `local_runtime_token_usage` 表，**一行 → 一个 `model.UsageEvent`**（官方写入器就是每提交一条带 usage 的 assistant 消息写一行），并汇总出 `model.SessionMeta`。

**做**：官方 schema 的必需列**亲和性探测**；可选列（model / reasoning / cache read / cache write / cost）缺列或缺类型时在 SQL 中替换为常量（0 / `''` / `NULL`），不猜列名；`mode=ro` + 连接级 `query_only(1)` + busy timeout 只读打开，绝不写用户库、不留 journal 副作用；文件签名（主库 + `-wal`/`-shm`）+ 行 id 水位线 + 水位行摘要 的增量/幂等游标；稳定 `DedupKey = minimax-code:<session_id>:<id>`；隐私最小权限 SELECT；严格 TDD（RED → GREEN → refactor）；合成 SQLite fixture 覆盖 golden、incremental、cursor-stable、idempotent、privacy、discover-missing 六项一致性检查。

**不做**（证据不足就不臆造）：不改 `model`/`app`/registry/UI（注册由 Lead 接线，见文末）；不读运行时的 config、auth、API key（本包只 `SELECT` 用量列，`Discover` 只认 `runtime-state.sqlite` 文件名）；不读 `raw` 列、`agent_name`、`framework_type`、`turn_id` 及其他任何表；不区分 v1/v2 写入来源——官方 v1 与 v2 **共用同一文件、同一张表**（见风险 3），所以 v1 写入的行同样会被读到，只是本包没有与真实 v1 日志对拍；不推断 `Provider`/`BaseURL`/`ProjectPath`/`Title`/`ParentID`/`GitBranch`/`Boundary`；`cost_usd` 为 NULL/负/NaN/±Inf 时 `CostUSD` 为 `nil`，**有限 0 保留为可信的 $0**（官方写入器只在该值为有限数时写数，其中 0 是"provider 报了 0 美元"，见风险 6）；profile 根 `~/.minimax-<name>` 不自动枚举（见风险 2）。

## 上游依据（官方一手，仅作证据，本轮不联网）

一手来源 <https://github.com/MiniMax-AI/minimax-code>：

- `packages/config/src/data-dir.ts`：默认数据目录 `~/.minimax`（`NEW_DATA_DIR_BASENAME`），兼容旧名 `~/.mavis`（`LEGACY_DATA_DIR_BASENAME`）；profile 目录为 `<basename>-<profile>`（`basenameForProfile`，例如 `~/.minimax-work`）。`resolveDataDir` 在 primary/legacy 之间迁移：primary 为空而 legacy 有数据时把 legacy 迁入 primary 并留下 **legacy → primary 的兼容符号链接**；primary 有数据而 legacy 是真实目录时把它改名备份为 `<legacy>.backup-<nowMs>` 再建链接。→ 两个默认根可能是**同一个目录**，必须按文件同一性去重（`Discover` 用 `os.SameFile`）。
- `packages/tui/src/runtime/data-dir.ts`：`readDataDirOverride` 先读 `MINIMAX_DATA_DIR`（trim），再读 `MAVIS_DATA_DIR`（trim）；否则用 `getPrimaryDataDirPath(undefined, getProfile())`。
- `packages/config/src/config.ts`：数据目录优先级为 显式 profile 的 data dir > git 分支推断 profile > `__MAVIS_RUNTIME_DATA_DIR` > profile env > `resolveDataDir()` 默认。**当前活动 profile 只存在于进程环境（`--profile` / `__MAVIS_RUNTIME_PROFILE` / git 推断），不落盘**，所以 profile 根无法离线枚举。
- `packages/local-runtime-v2/src/infra/db/client.ts`：`DB_FILE = "runtime-state.sqlite"`，`DB_RELATIVE_PATH = join("v2","sqlite",DB_FILE)` → `<root>/v2/sqlite/runtime-state.sqlite`。
- `packages/local-runtime-v2/src/infra/db/schema/usage.ts`：`local_runtime_token_usage` 列与索引（本包探测的权威依据）：
  `id INTEGER PRIMARY KEY AUTOINCREMENT`、`session_id TEXT NOT NULL`、`agent_name TEXT NOT NULL`、`framework_type TEXT NOT NULL`、`turn_id TEXT`（可空）、`model TEXT`（可空）、`ts INTEGER NOT NULL`、`input_tokens/output_tokens/reasoning_tokens/cache_read_tokens/cache_write_tokens INTEGER NOT NULL`、`cost_usd REAL`（可空）、`raw TEXT`（可空）。索引：`(session_id, ts, id)`、`(agent_name, ts, id)`、`(ts, id)`、`(model, ts, id)`、`(CAST(ts/86400000 AS INTEGER), ts, id)`。
- `packages/local-runtime-v2/src/service/session-system/usage/pi-usage.ts`：`committedPiUsageRows` 对每条**有 usage 对象**的已提交 assistant 消息写一行；`frameworkType` 硬编码 `'pi-agent'`；`turnId = input.turnId`；`model = input.model ?? null`；`ts = readTimestamp(message) ?? nowMs`（**毫秒**）；`costUsd = usage.cost.total`（有限数）否则 `null`；`raw = safeJson(usage)`（**只有 usage 对象**）；`normalizePiUsage` 把 `usage.input`（或退化用 `total_tokens`）、`usage.output`、`usage.reasoning`、`usage.cacheRead ?? usage.cache?.read ?? usage.cache_read`、`cacheWrite` 同类**分别**归一化到各自的列；五类计数全 0 的行不写。→ 官方列与 `model.Tokens` 五类**一一对应，不存在重复计数**。
- 官方自己的聚合口径：`totalTokens = input + output + reasoning`（**cache 两类不计入**），按天分组用 `CAST(ts/86400000 AS INTEGER)`，按模型分组用 `coalesce(model,'unknown')`。本包 `Model` 为空时输出 `unknown` 即对齐该口径。

| 决策 | 依据 |
| --- | --- |
| 默认根 = `$HOME/.minimax` + `$HOME/.mavis`（builtin tier，两者都读、按文件同一性去重） | `data-dir.ts` 的 primary/legacy 双目录与迁移链接 |
| `MINIMAX_DATA_DIR` 优先于 `MAVIS_DATA_DIR`，且**替换**而非叠加默认根 | `readDataDirOverride` 的先后顺序与 `config.ts` 的优先级 |
| 多根只走 `MYTOKEN_MINIMAX_DIRS`（平台 `ListSeparator`） | MyToken 既有 override 惯例（`harness.SplitEnvList`） |
| profile 根不自动枚举，只能显式指定 | 活动 profile 只在进程环境，不落盘 |
| 只认 `runtime-state.sqlite` 文件名 | 避免把同目录/同根无关 SQLite 当账本 |
| `ts` 为毫秒，`time.UnixMilli(...).UTC()`；`ts<=0` 用零值时间 | `pi-usage.ts` 写 `nowMs`/消息时间戳；官方按 `ts/86400000` 分天 |
| 无 `Provider`/`BaseURL`/`ProjectPath`/`Title`/`ParentID`/`GitBranch`/`Boundary` | 账本没有这些列；`docs/SPEC.md` 禁止虚构 |
| cost NULL/负/非有限 → `nil`；有限 0 → `$0` | 上游 `pi-usage.ts`/`api.ts`：只有 `usage.cost.total` 是有限数才写 `costUsd`，否则写 `null`；官方注释是"把缺失的 provider 用量与可信的零成本区分开"。`docs/HARNESS.md` §4 的通用约定（0 表示未知）在这里与上游语义冲突，按上游语义取真，见风险 6 |

## 实现要点

行数与函数位置（第二轮修复后）：`minimax.go` 785 行；`minimax_test.go` 1032 行；`conformance_test.go` 67 行；seed SQL 79 行。

常量与结构：`minimax.go:75-120`（`Harness model.Harness = "minimax-code"`、`kindSQLite="sqlite"`、`usageTable="local_runtime_token_usage"`、`dbName="runtime-state.sqlite"`、`dbRelPath="v2/sqlite/runtime-state.sqlite"`、`dirsEnv="MYTOKEN_MINIMAX_DIRS"`、`primaryDataDirEnv`/`legacyDataDirEnv`、`primaryDirName=".minimax"`/`legacyDirName=".mavis"`、`unknownModel="unknown"`、`fingerprintBytes=4096`、`busyTimeoutMS=5000`、`openTimeout=5s`）。

Root provenance（`rootProvenance`，`minimax.go:140`）：`MYTOKEN_MINIMAX_DIRS`（`harness.Getenv` + `harness.SplitEnvList`，畸形时返回 `*harness.EnvListError` 且**不回退**）→ `MINIMAX_DATA_DIR` → `MAVIS_DATA_DIR` → `harness.BuiltinRoots(builtinRoots()...)`；`builtinRoots`（`minimax.go:157`）在 home 未知时返回空而不是相对路径。`DefaultRoots`（`minimax.go:170`）在 override 畸形时返回空。`WithRoots`（`minimax.go:191`）显式空列表保持为空、**不回退**默认根，`RootProvenance` 报 `OriginExplicit`；`NewWithRoots`（`minimax.go:133`）只 trim 去空、不展开 `~`/env。

Discover（`minimax.go:206`）：每个根，目录 → 拼 `<root>/v2/sqlite/runtime-state.sqlite`；文件且 basename 为 `runtime-state.sqlite` → 直接采用；缺失/不可读根跳过且**不报错**；候选按 `os.SameFile` 去重（覆盖 legacy 符号链接）；结果按路径排序。空根列表 → 0 source。

Parse（`minimax.go:257`）顺序：`ctx.Err()` → `signatureOf`（`minimax.go:529`：主库 + `-wal` + `-shm` size 之和、最新 mtime）→ `fingerprint`（`minimax.go:551`：前 4096 字节 sha1，SQLite 头部含 change counter，因此任何提交都会改变指纹）→ **未变化快速路径**（`Fingerprint`+`Size`+`ModTime` 全等则原样返回 `cur`，不打开数据库）→ `openReadOnly`（`minimax.go:566`）→ 一个 `BeginTx(ReadOnly: true)` 事务 → `tableColumns`（`minimax.go:600`，返回 `(map, error)`）→ `supportedSchema`（`minimax.go:630`）→ 水位行校验（`watermarkIntact`，`minimax.go:746`）→ 扫 `id > watermark ORDER BY id ASC` → `ledgerRow.event`（`minimax.go:395`）+ `sessionMetas`（`minimax.go:457`）→ `cursorFor`（`minimax.go:761`）。

**失败一律不推进游标**（第二轮修复的核心）：`ctx` 已取消、`signatureOf`/`fingerprint` 失败、`openReadOnly` 失败（含被锁到 busy timeout 用尽）、`BeginTx` 失败、**schema 探测出错**、水位校验/查询/扫描/`rows.Err()` 出错 —— 全部走 `keepCursor(cur)`（`minimax.go:370`）返回**调用方原样的 cursor**，不记录文件签名，下一轮重试同一份库。只有两种情况会推进：**完成一次读取**（含"库可读但没有账本列"这一刻意情形，`minimax.go:291-293`）与**未变化的快速路径**（原样返回 `cur`）。

只读访问（`openReadOnly`，`minimax.go:545`）：`sqlitedsn.URI` 拼 `mode=ro&immutable=0&_pragma=busy_timeout(5000)&_pragma=query_only(1)`，`SetMaxOpenConns(1)`，5s `PingContext`；全程只有 `SELECT` 与 `PRAGMA table_info`，没有写语句、没有 `journal_mode` 变更、不创建 `-wal/-shm/-journal`。

schema 探测而非假设（`supportedSchema`/`hasAffinity`/`affinityOf`，`minimax.go:607-654`）：必需列 `id INTEGER`、`session_id TEXT`、`ts INTEGER`、`input_tokens INTEGER`、`output_tokens INTEGER`（按 SQLite 自身的类型亲和性判定，声明类型不认识 → 不视为支持）。可选列只在**亲和性匹配**时才读（`model` 需 TEXT；`reasoning/cache_read/cache_write` 需 INTEGER；`cost_usd` 需 REAL/NUMERIC），否则在 `selectList`（`minimax.go:656`）里替换为 `''` / `0` / `NULL` 常量。`CAST(...)` 包裹所有列，使同一扫描路径兼容不同代 schema。

游标（增量 + 幂等）：`Cursor.Extra` 是 `ledgerState` 的 JSON `{"id":<已消费的最大行 id>,"h":<该行摘要>}`（`minimax.go:705`）。`id` 是 `AUTOINCREMENT` 主键 → 严格单调，追加只增不减；`digest()`（`minimax.go:402`）对 id/session/ts/五类计数/model/cost 做 sha1。读取时先校验 `WHERE id = 水位` 仍在且摘要一致，不一致（库被替换/重写）就**丢弃水位、从 0 重扫**并靠稳定 `DedupKey` 保证幂等；无 session_id 的行不产出事件但**仍推进水位**，避免每轮重扫。会话元数据取本批次各 session 的 min/max 时间戳（与 store 的 `min(started_at)/max(updated_at)` 合并语义一致，因此增量结果与全量解析相同）。

隐私：`selectList` 只含 `id, session_id, ts, input_tokens, output_tokens` + 可选用量列，**绝不 SELECT `raw`、`agent_name`、`framework_type`、`turn_id` 或任何其他表**；`Discover` 只认固定的账本文件名，因此 config/auth/API key 文件既不会被列为 source 也不会被打开。

## 严格 TDD 过程

**RED**：先写 `minimax_test.go`、`conformance_test.go` 与 `testdata/conformance/minimax/seed/*.sql`，对只有 API 骨架的 stub（`minimax.go` 全部方法返回零值）跑出真实失败：

```text
--- FAIL: TestConformance (0.08s)
    --- FAIL: TestConformance/golden (0.02s)
        harnesstest.go:124: harnesstest: minimax-code discovered no sources under testdata/conformance/minimax — check the fixture layout and Case.New
--- FAIL: TestParseMapsLedgerRows (0.00s)
    minimax_test.go:135: parsed 0 events, want 3: []
--- FAIL: TestParseIncompatibleSchema/no_ledger_table (0.00s)
    minimax_test.go:255: Discover returned 0 sources, want 1: []
--- FAIL: TestOpenReadOnlyRejectsWrites (0.00s)
    minimax_test.go:558: openReadOnly: file does not exist
--- FAIL: TestDiscoverLayouts/root_directory (0.00s)
    minimax_test.go:628: Discover = [], <nil>; want one source
--- FAIL: TestRootProvenance (0.00s)
    minimax_test.go:690: builtin roots = [], want .minimax and .mavis
--- FAIL: TestParseReadsWAL (0.00s)
    minimax_test.go:813: parsed 0 events from a WAL database, want 1
FAIL	github.com/zzstar101/mytoken/internal/harness/minimax	0.720s
```

（`gofmt -l` 与 `go vet` 在 RED 阶段即为空：stub 是合法 Go，失败全部来自断言。）

**GREEN**：实现后全部通过。测试抓出的**测试自身缺陷**（不是被放宽的断言）两处，均已按正确语义修正：

1. 幂等测试原本用 `first.Events[i] != second.Events[i]` 比较结构体，而 `UsageEvent.CostUSD` 是 `*float64` —— 结构体比较比的是**指针地址**，两次独立解析必然不等。改为比较 `json.Marshal` 后的字节（真正的幂等语义），并保留 `DedupKey` 逐项相等断言。
2. 并发测试在子 goroutine 里调用会 `t.Fatalf` 的 helper，属于 `testing` 的非法用法（且 stub 下 `srcs` 为空会 panic）。改为在 goroutine 内自行处理错误并经 channel 回传，主 goroutine 断言。

**refactor**：把扫描循环拆成 `ledgerRow`/`rowScanTargets`/`event`/`digest`、`sessionAggregate`/`sessionMetas`，SELECT 列表拆成 `selectList`/`intOrZero`/`textOrEmpty`/`realOrNull`/`affinityOf`；顺手修掉 `realOrNull` 里不可达的缺列分支。重构后重跑全部命令仍全绿。

fixture：`internal/harness/minimax/testdata/conformance/minimax/seed/{schema.sql,step0.sql,step1.sql,step2.sql}` 合成临时 SQLite（`Grow` 逐步重放 schema + step0..stepN，因为账本是原地增长的，不能靠复制文件）。`schema.sql` 里放了一张无关的 `runtime_message_text` 表并写入 `SENTINEL_PRIVATE_TEXT sk-minimax-DO-NOT-LEAK`（满足 `harnesstest` 的 fixture-tree 隐私要求，同时证明解析器不读其他表）。golden 覆盖 **7 events / 3 sessions**：两个模型（`MiniMax-M2.7`/`MiniMax-M3`）、cache read/write 与 reasoning 非零、一行 `model` 为 NULL → `unknown`、一行无 cost、一行 `cost_usd = 0`（保留为 **`$0`**，第二轮修正：官方写入器只在值为有限数时写数，0 是"provider 报了 0 美元"而不是未知）、一行 reasoning-only、session `sess-alpha` 跨 step2 继续追加。

## 命令与输出

```console
$ gofmt -l internal/harness/minimax/
（无输出）

$ go vet ./internal/harness/minimax/
（无输出）

$ go test ./internal/harness/minimax/ -count=1 -v
--- PASS: TestConformance (0.10s)        # golden/incremental/cursor-stable/idempotent/privacy/discover-missing
--- PASS: TestHarnessIdentity
--- PASS: TestParseMapsLedgerRows
--- PASS: TestParseSkipsRowsWithoutSession
--- PASS: TestParseIncompatibleSchema  # no_ledger_table / missing_required_columns / ts_is_not_an_integer
--- PASS: TestParseOptionalColumnsAbsent
--- PASS: TestParseIncompatibleOptionalColumnTypes
--- PASS: TestParseClampsUnusableNumbers
--- PASS: TestParseTimestampStaysUnknown
--- PASS: TestParseIncrementalByRowID
--- PASS: TestParseLedgerResetRescans
--- PASS: TestParseIsIdempotent
--- PASS: TestParseNeverReadsOtherTables
--- PASS: TestOpenReadOnlyRejectsWrites
--- PASS: TestTableColumnsAndSchemaProbe
--- PASS: TestDiscoverLayouts           # root_directory / root_names_the_database / legacy_symlink_is_the_same_file / missing_root / blank_root
--- PASS: TestRootProvenance
--- PASS: TestWithRootsKeepsExplicitRoots
--- PASS: TestParseReadsWAL
--- PASS: TestParseIsConcurrencySafe
--- PASS: TestParseKeepsCursorWhenLedgerIsNotADatabase   # 第二轮
--- PASS: TestParseKeepsCursorWhenSchemaProbeFails       # 第二轮
--- PASS: TestParseRetriesLedgerThatWasLocked            # 第二轮
--- PASS: TestParseKeepsCursorWhenContextIsCancelled     # 第二轮
ok  	github.com/zzstar101/mytoken/internal/harness/minimax	5.467s

$ go test ./internal/harness/minimax/ -count=1 -race
ok  	github.com/zzstar101/mytoken/internal/harness/minimax	7.317s

$ go test ./internal/harness/minimax/ -run '^$' -bench BenchmarkConformance -benchtime 20x
BenchmarkConformance-10   20   259973 ns/op   7.000 events/op   1.000 sources   31087 B/op   450 allocs/op
ok  	github.com/zzstar101/mytoken/internal/harness/minimax	0.228s

$ go test -cover ./internal/harness/minimax/
ok  	github.com/zzstar101/mytoken/internal/harness/minimax	coverage: 90.9% of statements

$ go test ./internal/harness/... -count=1
（全绿；根包注册数 32）

$ go build ./...
（成功）
```

`go test ./internal/harness/...` 现在**全绿**（第二轮复核时 `mimocode`/`stepcode` 已能构建）：`go test ./internal/harness/... -count=1` 无失败；根包注册数已随接线升到 32。本包不依赖、不修改这两个包。

## 第二轮审查修复（失败游标 + zero-cost 语义 + 错误路径审计）

第二轮审查提了 4 项，其中 1/2 是明确缺陷、3 是文档错误、4 是复审计。全部按 RED → GREEN 做，下面的输出是修复前的**精确**失败文本。

### 1（RED）失败时把新文件签名写进 `Next`，一行未消费

修复前的 `Parse` 在 `openReadOnly` 失败、`BeginTx` 失败、schema 探测出错时都返回 `Batch{Next: cursorFor(cur, sig, fp, state)}` —— 即"文件签名已更新、行 id 水位线未变"。`internal/scan/scanner.go:454` 的 `scanOne` 把 `out.advanced = b.Next != cur` 为真且事件为 0 的 batch **照常落盘**，于是下一轮命中指纹快速路径、**永远跳过这份库**（真实静默漏计）。

三个可复现的 fixture（写进 `minimax_test.go`）：

- 把账本文件覆盖成非数据库文本 → `openReadOnly` 报 `file is not a database (26)`；
- 用 `PRAGMA writable_schema` 注入 `CREATE VIRTUAL TABLE local_runtime_token_usage USING nonexistent_module` → 打开成功、`PRAGMA table_info` 报 `no such module: nonexistent_module`（探测错误与"没有账本列"因此可确定性区分）；
- 另一个连接持 `BEGIN EXCLUSIVE` 并写入 → `openReadOnly` 在 busy timeout 后失败：`database is locked (5) (SQLITE_BUSY)`（实测约 5.05–5.36s）。

精确 RED 文本：

```text
minimax_test.go:893: Next = {Offset:0 Size:37 ModTime:2026-10-16 11:02:22.748429931 +0800 CST Fingerprint:3e1af60a1b33ce775cde2ecbbf242f883ee1a56a Extra:}, want the caller's cursor {Offset:0 Size:0 ModTime:0001-01-01 00:00:00 +0000 UTC Fingerprint: Extra:}: an unread ledger must not be marked as read
minimax_test.go:917: Next = {Offset:0 Size:8192 ... Fingerprint:b505539d8687b59ed2d971f73b0d9438a82354f9 Extra:}, want the caller's cursor ... : a failed schema probe must not mark the ledger as read
parse while locked: took 5.357111083s, events=0, err=<nil>
after the writer left, parsed 0 events with the cursor the locked round returned ({Offset:0 Size:12288 ... Fingerprint:9a7707690693e9d81bb30e68e87be9864e0830a Extra:}), want the 1 committed row
```

**GREEN**：新增 `keepCursor(cur) (minimax.go:370)`，`ctx` 已取消、`signatureOf`/`fingerprint` 失败、**打开失败（含锁超时）**、`BeginTx` 失败、**schema 探测出错**、水位/查询/扫描/`rows.Err()` 出错全部返回调用方原样的 cursor（不记录签名、不改水位），下一轮重试同一份库。`TestParseRetriesLedgerThatWasLocked` 进一步验证：持锁轮返回原 cursor，写者 ROLLBACK 后**用该 cursor** 再解析能拿到那条已提交的行。`BeginTx` 分支在 modernc/sqlite 下无法用可控 fixture 触发（`database/sql` 只观察 `Done()`，不轮询 `Err()`），因此只由同一条 `keepCursor` 纪律覆盖、未单列 RED —— 这是被如实记录的覆盖缺口，没有假装有测试。

### 2（RED）`cost_usd = 0` 是可信的零成本，不是未知

官方语义：`pi-usage.ts` 的 `readCostUsd` 只在 `usage.cost.total` 为**有限数**时返回该值（含 0），否则 `null`，且源码注释写明 *"Keep missing provider usage distinct from a trustworthy zero-cost turn."*；v1 写入器 `usage/api.ts` 同样 `costUsd ?? null`（仅有限数写数），`sqlite-persistence.ts` 原样 INSERT。修复前 `costPointer` 把 `cost <= 0` 判为 `nil`。

RED 由"先按正确语义改 fixture"构成：`golden.json` 给 `minimax-code:sess-alpha:6` 加 `"costUsd": 0`，`step2.sql` 注释改掉，`TestParseMapsLedgerRows`（nullRow/zeroRow 拆开）与 `TestParseClampsUnusableNumbers`（4 行：`-0.5` → nil、`0` → 指向 0、`9e999` 与 `-9e999` → nil）加断言。精确 RED：

```text
harnesstest: minimax-code output does not match testdata/conformance/minimax/golden.json
minimax_test.go:172: event 3 CostUSD = <nil>, want a pointer to 0 for an explicit zero cost
minimax_test.go:377: CostUSD = <nil>, want a pointer to 0 for an explicit zero cost
```

（第一条后面附 diff，期望侧多出 `"costUsd": 0`。）

**GREEN**：`costPointer`（`minimax.go:510`）改为 `if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 { return nil }` —— 有限且 `>= 0` 一律保留指针，NULL/负/NaN/±Inf 为 nil。下游也已核对：`internal/pricing/rules.go:239` 的 `Pricer.Evaluate` 以 `if e.CostUSD != nil { return *e.CostUSD, true }` 开头，所以被保留的 0 **不会**被重新估价，语义在整个管道里一致。

### 3（文档错误）v1/v2 共用同一文件

详见风险 3、风险 6 的修正：`db.ts` 的 `openLocalRuntimeDb` → `resolveLocalRuntimeDbPath` → `resolveV2DirectoryContract(dataDir).db` = `<root>/v2/sqlite/runtime-state.sqlite`；迁移版本 10 在同一文件建同列表。原文档"v1 用另一个 DB 文件"的说法是错的，已删除；只保留"v1 writer 语义/真实日志尚未对拍"这一条真实风险（另附 v1 的 `DELETE ... WHERE session_id = ?` 会让水位行消失、触发一次全表重扫）。

### 4（复审计）两个可复现的真实缺陷 + 明确不改的部分

复审计先写复现、再改代码，只改有真实失败的两个：

- **D1**（静默漏计）：写入方持有超过 busy timeout 的排他锁时，本轮"打开失败但签名已记"导致下一轮快速路径永久跳过 —— 即上面的锁测试。
- **D2**（静默漏计）：`tableColumns` 原来用 `nil` 同时表示"探测出错"和"没有账本列"，两种情形无法区分，探测出错也会记录读位置 —— 由注入虚拟表确定性复现。

**审计后刻意不改**（无可信复现，不凭猜测改行为）：

- 可读但 schema 不被识别的库仍记录读位置（有意的漂移约定，`TestParseIncompatibleSchema` 覆盖；本轮只把"探测出错"从这条约定里摘出来，见 D2）；
- 指纹快速路径（前 4096 字节 sha1；SQLite 头部含 change counter，每次提交都会变）叠加主库/`-wal`/`-shm` 尺寸与最大 mtime，覆盖所有现实写入；要强制误命中，必须"只改 4KB 之外、且尺寸与 mtime 都刚好复原"；
- 增量查询是主键有界的（`WHERE id > ? ORDER BY id ASC`，含水位查询共 2 条语句），水位行被删只会触发**一次**全表重扫，不是每轮重扫；
- 冷启动会整表物化（100k 行实测：冷读 191ms、堆增量约 37.3 MiB；未变化快速路径 242µs；在 100k 行库上增量 +100 行 1.09ms；原地深改（in-place UPDATE）1.9ms 仍被检测到）—— 作为**特性/量级**报告，不作为缺陷。

### 验证（第二轮，全部在 `internal/harness/minimax` 与本文件写域内）

`gofmt -l internal/harness/minimax/` 空；`go vet ./internal/harness/minimax/` 空；`go test ./internal/harness/minimax/ -count=1 -v` 24 个顶层测试全 PASS（含 4 个新测试），`ok ... 5.467s`；`-race` `ok ... 7.317s`；bench `259973 ns/op  7.000 events/op  31087 B/op  450 allocs/op`；coverage `90.9%`；`go test ./internal/harness/... -count=1` 全绿。`git status --porcelain` 复核：本轮的改动只在 `internal/harness/minimax/` 与 `docs/verification/minimax-code.md`，未提交。

## 未解决的 schema 风险

1. **没有用真实 `~/.minimax` 数据库验证**：本轮按要求不联网、不读用户数据与凭据，全部结论来自 `t.TempDir()` 下的合成 fixture。schema 语义有官方源码背书，但"真实运行中的库"未被打开过。
2. **profile 根无法离线发现**：`~/.minimax-<name>` 只有在其 profile 生效时才被写入，活动 profile 不在盘上。默认根因此**不含**任何 profile 目录；多 profile 用户必须用 `MYTOKEN_MINIMAX_DIRS` 显式列出，否则会漏计该 profile 的用量。
3. **v1 与 v2 共用同一文件（已更正）；未与真实 v1 日志对拍**：官方 `packages/local-runtime/src/persistence/db.ts` 的 `openLocalRuntimeDb` 使用 `resolveLocalRuntimeDbPath(dataDir)`，它等于 `resolveV2DirectoryContract(dataDir).db` —— 与 v2 **同一个** `<root>/v2/sqlite/runtime-state.sqlite`；迁移版本 10 在同一文件里建 `local_runtime_token_usage`，列与本包探测的 14 列一致。所以本包也会读到 v1 写入的行，不存在"另一个 v1 库"（此前文档里"另一个文件"的说法是错的，已按官方源码更正）。仍然未验证的是 v1 写入路径（`packages/local-runtime/src/usage/api.ts` → `sqlite-persistence.ts`）产出的**真实**行没有被对拍过。另注：v1 暴露 `DELETE FROM local_runtime_token_usage WHERE session_id = ?`（删会话会删行），水位行消失会触发一次全表重扫，这是已有测试覆盖的行为。
4. **列名/表名演化**：必需列固定为 `id, session_id, ts, input_tokens, output_tokens`。若真实库改用别的拼写，该库会被判为"不是 MiniMax Code 账本"而**静默 0 计数**（不报错、也不误读）。这是刻意选择：宁可漏计，也不把不兼容库当支持。第二轮修复**没有**改这个约定：探测"成功回答但没有账本列"会记录读位置（不会每轮重开），只有探测**出错**才保留旧 cursor 下轮重试（见第二轮 D2）。
5. **`ts` 单位只按毫秒**：证据（`nowMs`、`ts/86400000` 分天）充分支持毫秒；本包**不做**秒级防御性缩放。若某代写入秒，时间会明显错误（不报错）。
6. **零成本语义与全局约定相反（刻意，已按上游取真）**：`docs/HARNESS.md` §4 的通用约定是"0 表示未知，不是免费"，但 MiniMax 官方写入器只在 `usage.cost.total` 为有限数（含 0）时才写 `cost_usd`，否则写 NULL，官方注释明确写着"把缺失的 provider 用量与可信的零成本区分开"（`packages/local-runtime-v2/src/service/session-system/usage/pi-usage.ts` 的 `readCostUsd`）。因此本包保留有限 0 为 `$0`，只把 NULL/负/NaN/±Inf 变成 `nil`。若上层按 §4 的旧约定解释 0，会与本 harness 不一致——需要 Lead 决定是否同步修订 `docs/HARNESS.md`（不在本任务写域）。
7. **`totalTokens` 口径差异**：官方 UI 的 total 是 `input+output+reasoning`（排除 cache 两类），而本包 `model.Tokens` 五类按官方列 1:1 保留。若上层用 `Tokens.Total()` 与官方数值对比，会把 cache 也算进去而偏大；这是上游口径差异，不是重复计数。
8. **`ParentID` 无法可靠得出**：账本虽有 `agent_name`/`framework_type`/`turn_id`，但没有父子会话外键；本包一律留空而不从 session id 命名（如 `sub-agent-01`）猜测。
9. **替换检测的边界**：水位行摘要只能发现"水位行内容变化或消失"。若库被替换成**恰好同 id、同行内容**的新库，无法区分（该情形对增量结果无影响）。
10. **harness id 是本地常量（接线已完成）**：`minimax.Harness = "minimax-code"`（对齐 `claude-code`/`claude-desktop` 的命名惯例）。Lead 已按文末清单接线：`model.MiniMaxCode` 存在（`internal/model/model.go:40`）、`internal/app/app.go:30` blank import、`internal/harness/rooted_test.go:59` 的 `wantRegistered = 32`、`provenance_test.go` 的 `overrideEnvSurface` 行已加，因此 `Harness.Known()` 为 true。若改用别的 id（例如 `minimax`），必须同步 `minimax.go` 常量、`minimax_test.go` 的断言与 `testdata/conformance/minimax/golden.json`。

## 未关闭的门（交接给 Lead 的接线清单）

本包 `init()` 已 `harness.Register(New())`；一旦被 blank import，`harness.All()` 即包含 `minimax-code`，下列 3/4 必须同步，否则公共套件会 fatal：

1. `internal/model/model.go:11-39` 常量区加 `MiniMaxCode Harness = "minimax-code"`，并在 `DisplayName()` 加 `"MiniMax Code"` 分支。
2. `internal/app/app.go:37-38`（现为 `zcode`、`zed`）加 `_ "github.com/zzstar101/mytoken/internal/harness/minimax"`。
3. `internal/harness/rooted_test.go:20-56`：加同一个 blank import，并把 `wantRegistered`（当时为 29；接线后为 32）加 1。
4. `internal/harness/provenance_test.go:37-70` 的 `overrideEnvSurface` 加一行：
   `{model.MiniMaxCode, []string{"MYTOKEN_MINIMAX_DIRS", "MINIMAX_DATA_DIR", "MAVIS_DATA_DIR"}}`（顺序即 `rootProvenance` 的优先级，表是双向断言的，漏标会失败）。
5. 若希望该 harness 默认关闭，可删除 `init()` 注册改为显式注册；本包按仓库惯例保留了 `init()`。

本轮未运行 GUI/UI（不在写域），未提交（任务要求）。

## Lead 集成更新（2026-10-16）

上述接线清单已完成：`model.MiniMaxCode`、app blank import、32-harness registry/provenance 断言、HARNESS/README 与 GUI 名称/颜色均已同步。首轮双审查随后要求修复 open/transaction 错误游标、zero-cost 语义并补直接回归；本文前面的写域说明保留为实现工作流当时的审计快照。最终状态以 `v04x-harness-matrix.md` 与全量验证结果为准。
