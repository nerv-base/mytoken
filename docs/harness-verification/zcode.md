# ZCode 0.4.x 离线适配验证（internal/harness/zcode）

日期：2026-10-16 10:09 CST（初版）；**复审轮 2026-10-16 10:51 CST**（见文末「复审修复」，本轮 `.md` 内所有 `zcode.go:NNN` 行号已更新到复审后的 1015 行文件）；环境：go1.27.1 darwin/arm64；工作目录 `/Users/zzstar/Developer/Programs/MyToken!!!!!`；分支 `feat/0.4x-harnesses`；HEAD `a9684c7`。**未提交**。

写入范围（初版）：`internal/model/model.go`、`internal/app/app.go`、`internal/harness/rooted_test.go`、`internal/harness/rooted_internal_test.go`、`internal/harness/provenance_test.go`、新增 `internal/harness/zcode/**` 与本文件。未改动 `internal/harness/kimi/**`、`internal/harness/pi/**`、`internal/gui/**`、`internal/store`、`internal/query` 及任何共享规划文件。**复审轮写入范围**只有 `internal/harness/zcode/zcode.go`、新增 `internal/harness/zcode/zcode_regression_test.go` 与本文件（见文末）。

```console
$ git status --porcelain -- internal/model internal/app internal/harness docs/harness-verification/zcode.md
 M internal/app/app.go
 M internal/harness/kimi/kimi.go            # 其它工作流
 M internal/harness/pi/pi.go                # 其它工作流
 M internal/harness/pi/pi_test.go           # 其它工作流
 M internal/harness/provenance_test.go
 M internal/harness/rooted_internal_test.go
 M internal/harness/rooted_test.go
 M internal/model/model.go
?? internal/harness/kimi/protocol11_test.go # 其它工作流
?? internal/harness/kimi/testdata/...       # 其它工作流
?? internal/harness/mimocode/               # 其它工作流（并发注册的新 harness）
?? internal/harness/minimax/                # 其它工作流（并发注册的新 harness）
?? internal/harness/pi/root_test.go         # 其它工作流
?? internal/harness/zcode/
```

`internal/harness/mimocode/`、`internal/harness/minimax/`、`kimi/**`、`pi/**` 是并发工作流的既有改动，本次没有触碰；但它们与 `rooted_test.go` 的注册数期望有耦合，见「未关闭的门」最后一条。全文行号均指本轮修改后的文件。

## 结论与范围

新增第 29 个 harness：`model.ZCode`（`internal/model/model.go:39`）与只读 SQLite 解析器 `internal/harness/zcode`，从 ZCode 的本地用量账本 `~/.zcode/cli/db/db.sqlite` 读出 `model_usage` 中 `status = "completed"` 的每一轮，映射为 `model.UsageEvent` 与 `model.SessionMeta`。

**做**：`model_usage` 必需列探测 + 可选 cache/provider 列探测；`input_tokens` 含 cache 时按存在的列扣出并输出 `CacheRead`/`CacheWrite`；`session.id → session.directory` 映射 `ProjectPath`；只读（`mode=ro` + `PRAGMA query_only`）打开，绝不改 journal；按 SQLite 文件签名 + per-row rowid 水位线增量、幂等；稳定 `DedupKey = zcode:<id>`；注册同步（`init()` 注册 + `internal/app/app.go:37` blank import + `rooted_test.go` 期望 28→29 + `provenance_test.go` 的 env surface 表加一行）；严格 TDD（RED → GREEN → refactor）；合成 fixture 覆盖缺失可选列、completed 过滤、cache 扣减、损坏/缺失 DB、隐私、游标稳定/幂等、discover-missing。

**不做**（明确不做，理由都是"证据不足就不臆造"）：不读 `~/.zcode/v2/credentials.json`（官方说明为设备加密，绝不读取）、不读 `config.json`（含 API Key/Base URL）、不使用 `ZCODE_HOME`（见下）、不读 `prompt`/标题等正文列、不推断 cost（`CostUSD` 恒为 `nil`）、不填 `BaseURL`/`Title`/`ParentID`/`GitBranch`/`Boundary`、不改 GUI 主题、不改 Kimi/Pi/roadmap。

## 上游依据

一手（官方，只作证据，未据此写入任何敏感读取）：

- <https://zcode.z.ai/cn/docs/qa>：ZCode 是**独立的 harness/ADE**；配置位于 `~/.zcode/v2/config.json`（模型 provider，含 API Key 与 Base URL）、`~/.zcode/cli/config.json`、`~/.zcode/AGENTS.md`、`~/.zcode/{agents,skills,commands}/`；`~/.zcode/v2/credentials.json` 为设备加密，绝不复制/读取；`~/.zcode/cli/exec` 是终端输出。
- <https://zcode.z.ai/cn/docs/usage-stats>：只描述应用内用量统计，**没有提到任何环境变量**。

第三方（schema 参考，非一手）：<https://ccusage.com/guide/zcode/> —— 账本在 `~/.zcode/cli/db/db.sqlite`；`model_usage` 必需列 `id, session_id, started_at, model_id, status, input_tokens, output_tokens`；`session` 必需列 `id, directory`；cache/provider/computed-total/session.version 为**逐列探测**的可选列；只统计 `status == "completed"`；`started_at` 为 epoch 毫秒；连接以只读 + busy timeout + 连接级 query_only 打开且不改 journal；`input_tokens` **已包含** cache-read 与 cache-creation，需要扣出并分别报告；没有 per-request cost 字段。

由此确定的语义（逐条对应上面的证据）：

| 决策 | 依据 |
| --- | --- |
| 默认根 = `$HOME/.zcode/cli/db`（builtin tier），各平台同一 join | 第三方文档给出该路径；ZCode 通过用户 home 解析，无平台分支证据 |
| `ZCODE_HOME` **不读** | 官方两页均未出现该变量，只有第三方文档提到；任务要求"无法确认就不臆造" |
| 多根只走 `MYTOKEN_ZCODE_DIRS`（平台 `ListSeparator`） | 任务指定的 MyToken override |
| 只认 `db.sqlite` 文件名 | 避免把同目录无关 SQLite 当账本 |
| 无 cost、无 BaseURL、无 Title | 账本无这些列；`docs/SPEC.md` 禁止虚构 |

## 实现要点

常量与结构：`zcode.go:92-171`（`dbName="db.sqlite"`、`cliDirName="cli"`、`dbDirName="db"`、`usageTable="model_usage"`、`sessionTable="session"`、`completedStatus="completed"`、`fingerprintBytes=4096`、`busyTimeoutMS=5000`、`maxRowsPerParse=20000`（**只作用于首次检查的 rowid 水位线路径，且是窗口不是截断：撞上窗口的扫描会在 cursor 里记 backlog，下一轮照样打开未变化的库继续读，直到读空才恢复未变化快速路径**）、`pendingBatch=500`、`cursorVersion=4`）。v3 会让旧 v2 cursor 做一次全量重扫，因为 v2 可能已经淘汰过无法恢复的 pending rowid；v4 让旧 v3 cursor 重扫，因为 v3 可能已经把水位线停在 20000 行窗口而此后不再前进（见「复审修复（第二轮）：cursor v4」）。

Root provenance：`rootProvenance`（`zcode.go:223`）先看 `MYTOKEN_ZCODE_DIRS`（`harness.Getenv` + `harness.SplitEnvList`，格式错误返回 `*harness.EnvListError` 且**不回退**），否则 `harness.BuiltinRoots(builtinRoots()...)`；`builtinRoots`（`zcode.go:237`）在 home 未知时返回空而不是相对路径；`defaultRoots`（`zcode.go:247`）在 override 畸形时返回空。`NewWithRoots`/`cleanRoots`（`zcode.go:196/201`）trim+Clean+去重；`WithRoots`（`zcode.go:268`）显式空列表保持为空，**不回退**默认根（`RootProvenance` 报 `OriginExplicit`）。注册入口：`init()`（`zcode.go:184`）。

Discover（`zcode.go:284`）：一个根可以是 ZCode home、`cli/`、`cli/db/` 或数据库文件本身（4 类布局、6 个测试用例）；缺失/非账本根跳过且不报错；结果按路径排序去重；只探测 `db.sqlite`。

Parse（`zcode.go:349`）顺序：`ctx.Err()` → `os.Stat` → `signatureOf`（`zcode.go:806`：主库 + `-wal` + `-shm` 的 size 之和、最新 mtime）→ `fingerprint`（`zcode.go:1183`：前 4096 字节 sha1，SQLite 的 change counter 就在头部，因此任何提交都会改变指纹）→ **未变化快速路径**（`Fingerprint`+`Size`+`ModTime`+`cursorVersion` 全等**且 cursor 没有记着 backlog（`!state.Drain`）**则原样返回 `cur`，不打开数据库）→ `openReadOnly`（`zcode.go:834`）→ `tableColumns`（`zcode.go:873`）→ 必需列校验（`hasColumns`）→ `collect`（`zcode.go:437`）→ `accumulator.batch`（`zcode.go:1164`）。缺失库、非 SQLite 文件、schema 不完整、数据库被锁，一律返回空 batch + 原 cursor，不报错；但 **schema 探测本身失败（`sqlite_master`/`PRAGMA table_info` 运行报错）会返回错误并保留原 cursor**——它只说明"这次没能探测到发生了什么"，不等于"库没有这些列"，不能借机推进 cursor。**同理，`collect` 里任何一次读取失败（含单行 `rows.Scan` 失败）也会返回错误并原样保留调用方的 cursor**：`Parse` 包成 `zcode: read usage from <path>: ...`，不产生部分 batch、不推进水位线（见「复审修复（第二轮）」I1）。

只读访问（`openWithPragmas`，`zcode.go:683`）：`sqlitedsn.URI` 只拼 `mode=ro`，`_pragma` 里是连接级 `busy_timeout(5000)` 与 `PRAGMA query_only`，`SetMaxOpenConns(1)`，5s `PingContext`。**不使用 `immutable=1`（也不再冗余写 `immutable=0`）**：旧版在热 WAL 打不开时回落到 `immutable=1` 去读"上次 checkpoint 的快照"，会静默丢掉仍只存在于 WAL 里的行；现在打开失败即返回空 batch + 原 cursor（`Parse` 的上层不报错地重试），**绝不读旧主库、绝不推进 cursor**。全程只有 `SELECT`/`PRAGMA table_info`，没有任何写语句、没有 `journal_mode` 变更、不创建 sidecar。

schema 探测而非假设：可选列按别名表探测（`cache_read_input_tokens`/`cache_read_tokens`/`cache_read`、`cache_creation_input_tokens`/`cache_creation_tokens`/`cache_write_input_tokens`/`cache_write`、`provider`），`pickColumn`（`zcode.go:846`）选中第一个存在的列；**缺列在 SQL 里被替换为常量**（`selectUsage` 及 `newRowsQuery`/`pendingQuery`/`fullScanQuery`，`zcode.go:857-976`），因此同一扫描路径覆盖所有 schema，缺失列的值保持 0，而不是报错或猜列名。

计数与扣减（`tokenSplit`，`zcode.go:554`）：只有 `status` 精确等于 `"completed"` 的行计数；`input_tokens` 视为含 cache，`Input = input - read - write`，每个 cache 桶以 `input_tokens` 为上限 clamp，三类之和恒等于记录的 `input_tokens`；缺 cache 列则 `CacheRead`/`CacheWrite` 为 0；不写 `CostUSD`。

游标（增量 + 幂等）：`Cursor.Extra` 是 `zcodeState`（`zcode.go:742`）的 JSON：`{Version, Rowid, Pending, Anchor, Drain}`。`Rowid` 是**已检查过的最大 rowid 水位线**；`Pending` 是“仍在飞”的行（非 completed 且不在 `terminalStatuses` 白名单内）；`Anchor` 是**水位线那一行自己的 `DedupKey`**（`{"a":...}`）；`Drain` 是**"水位线之后还有没读的行"的显式标记**（`{"d":true}`）。`Rowid`/`Pending`/`Anchor` 是同一个"续读点"的三个部分，一起失效（`forgetResumePoint`，`zcode.go:769`）：表被重建（`maxRowid` 回退）或水位线那一行已被替换/复用（`watermarkRowStillThere`，`zcode.go:595`）时三者一起清零并重扫。扫描把两条路径分开：先把**全部** pending rowid 按 `pendingBatch=500` 分块重读，再用 `rowid > watermark ORDER BY rowid LIMIT 20000` 读取新行。因此新行不会被大量长期 pending 饿死，pending 也不会受 SQLite 变量上限或总查询 LIMIT 影响。`normalizePending`（`zcode.go:710`）只排序/去重，**不再淘汰任何 rowid**；旧版 1024 上限会让被淘汰行在日后 completed 时永久漏计，故 v3 cursor 强制一次重扫。completed/明确终态会从 pending 移除；暂时消失的 rowid 仍保留，以防非 AUTOINCREMENT 表复用该 rowid。**读满 20000 行窗口而未读到表尾时置 `Drain=true`（`state.Drain = watermark < max`，`zcode.go:550`），下一轮即使文件签名完全没变也照样打开数据库把剩余行读完；读空后 `Drain=false`，未变化快速路径才恢复**（见 C1）。**`WITHOUT ROWID` 表没有水位线，且主键不是单调游标（账本 id 可能随机或乱序插入），因此读全部 `completed` 行、不设 LIMIT，由稳定 `DedupKey` 保证幂等**（该分支没有窗口，`Drain` 恒为 false）。`DedupKey` 为 `zcode:<id>`（主键，`dedupKey`，`zcode.go:655`），id 为空时退化为 `sha256(session|started_at|rowid)`。会话元数据由 `readSessionDirs`（`zcode.go:1087`）把 `session.id → directory` 映射成 `ProjectPath`（无 session 行则留空）；`zcodeTime`（`zcode.go:665`）把秒级时间戳防御性 ×1000。代价是 cursor 与每轮重读成本随同时未终态行数线性增长；这是避免静默漏计的显式正确性取舍，并有 1500/2500/10000 pending 的回归与 benchmark 覆盖。

隐私：只 SELECT 标识、时间、status、token、model、provider 与 `session.directory`，绝不 SELECT `prompt`/正文/标题；`Discover` 只认 `db.sqlite` 文件名，因此 `config.json`/`credentials.json` 既不会被列为 source 也不会被打开。

## 严格 TDD 过程

**RED**：先写 `zcode_test.go`（22 个测试）、`conformance_test.go` 与 `testdata/conformance/zcode/seed/*.sql`，对 stub 实现跑出真实失败（节选，完整输出见本轮会话）：

```text
--- FAIL: TestConformance (0.06s)
    --- FAIL: TestConformance/golden (0.01s)
        harnesstest.go:124: harnesstest: read golden testdata/conformance/zcode/golden.json: open testdata/conformance/zcode/golden.json: no such file or directory
            (run `go test ./internal/harness/zcode -run Conformance -update`)
--- FAIL: TestParseCountsOnlyCompletedStatus (0.00s)
    zcode_test.go:265: DedupKeys = [zcode:c zcode:u], want exactly the completed row [zcode:c]
--- FAIL: TestParseDoesNotTouchTheDatabaseOrItsJournal (0.00s)
    zcode_test.go:604: stat .../TestParseDoesNotTouchTheDatabaseOrItsJournal631535555/001/db.sqlite-wal: no such file or directory
--- FAIL: TestIncrementalWatermarkEmitsOnlyNewRows (0.00s)
    zcode_test.go:659: incremental DedupKeys = [zcode:u1 zcode:u2 zcode:u3], want only the new row [zcode:u3]
FAIL	github.com/zzstar101/mytoken/internal/harness/zcode	0.758s
```

**GREEN**：测试抓到两个真实缺陷，都不是测试写错：

1. `status` 比较用了 `strings.ToLower`，于是 `"COMPLETED"` 也被计数（`TestParseCountsOnlyCompletedStatus` 期望它**不**计数）。改为对 `completedStatus` 做精确 `TrimSpace` 比较——文档给的是枚举值，大小写不折叠。
2. 查询写成了 `(status = ? OR rowid > ?)`：`status = 'completed'` 这一支会把**每一条历史 completed 行**都重新读出来，增量扫描因此把 u1、u2 又发了一遍。改为 `(rowid > watermark OR rowid IN (pending))`，并补上 `maxRowid` 的表重建检测。

第三个失败是**测试自身的 fixture 缺陷**：辅助函数 `execDB` 干净关闭连接后 SQLite 会删除 `-wal`/`-shm`，于是"WAL 不被改动"的断言在文件不存在时先失败了。改为在测试里保留一个持有连接的 `openWALDB`，在 parse 前后记录主库与 `-wal` 的字节并逐一比对——现在断言的是**内容不变**，而不是"文件存在"。

**refactor**：把 `Parse` 的扫描循环抽成 `decodeState`（`zcode.go:384`）与 `collect`（`zcode.go:399`），`Parse` 现在只负责"签名 → 快速路径 → 打开 → schema → collect → batch"。重构后重跑全部命令仍全绿。

fixture：`internal/harness/zcode/testdata/conformance/zcode/seed/{schema.sql,step0.sql,step1.sql,step2.sql}` 合成临时 SQLite；`schema.sql` 内含 `SENTINEL_PRIVATE_TEXT`（`harnesstest` 的隐私检查要求它出现在 fixture 树里、但绝不进入解析输出或 golden）。golden 覆盖 6 events / 3 sessions，其中包含一行 `provider` 为 NULL、cache 扣减（1200→`input 700`+`cacheRead 400`+`cacheWrite 100`；500→`input 350`+`120`+`30`）、以及 `running → completed` 原地翻转（step1/step2）。

## 命令与输出

```console
$ gofmt -l internal/harness/zcode internal/model/model.go internal/app/app.go \
    internal/harness/rooted_test.go internal/harness/rooted_internal_test.go internal/harness/provenance_test.go
（无输出）

$ go vet ./internal/harness/zcode ./internal/harness ./internal/model ./internal/app
（无输出）

$ go build ./...
（成功）

$ go test ./internal/harness/zcode ./internal/harness ./internal/model ./internal/app
ok  	github.com/zzstar101/mytoken/internal/harness/zcode	0.704s
ok  	github.com/zzstar101/mytoken/internal/harness	0.645s
?   	github.com/zzstar101/mytoken/internal/model	[no test files]
ok  	github.com/zzstar101/mytoken/internal/app	3.445s

$ go test -cover ./internal/harness/zcode/
ok  	github.com/zzstar101/mytoken/internal/harness/zcode	0.590s	coverage: 88.9% of statements

$ go test ./internal/harness/zcode/ -race
ok  	github.com/zzstar101/mytoken/internal/harness/zcode	5.834s

$ go test ./internal/harness/zcode/ -run TestConformance -v
--- PASS: TestConformance (0.06s)
    --- PASS: TestConformance/golden / incremental / cursor-stable / idempotent / privacy / discover-missing

$ go test ./internal/harness/ -run 'TestProvenance|TestNewRooted' -v   # zcode 子用例
--- PASS: TestProvenanceHonoursEveryDocumentedOverride/zcode/MYTOKEN_ZCODE_DIRS
--- PASS: TestProvenanceBlankOverrideIsTreatedAsAbsent/zcode/MYTOKEN_ZCODE_DIRS
--- PASS: TestProvenanceMalformedOverrideIsErrorNotDefault/zcode/MYTOKEN_ZCODE_DIRS
--- PASS: TestProvenanceExplicitRootsNeverFallBackToDefaults/zcode
--- PASS: TestNewRootedSupportsEveryRegisteredHarness/zcode
--- PASS: TestNewRootedExplicitEmptyRootsStayEmpty/zcode
--- PASS: TestNewRootedDoesNotMutateSingleton/zcode
--- PASS: TestNewRootedParsersAreIsolated/zcode

$ go test ./internal/harness/...
（全部 ok）
```

代码量（初版）：`zcode.go` 923 行、`zcode_test.go` 948 行、`conformance_test.go` 85 行、seed SQL 62 行；22 个顶层测试 + conformance 6 个子检查。复审后：`zcode.go` 1015 行、新增 `zcode_regression_test.go` 251 行（4 个回归测试，其中 1 个正对照）。

## 复审修复：三项静默少计（2026-10-16 10:51 CST）

ZCode 审查指出三项会让用量**静默少计**的缺陷。复审轮仍按 RED → GREEN：先写 `internal/harness/zcode/zcode_regression_test.go`，确认 3 个真实 RED 后才动 `zcode.go`。约束照旧：只改 `internal/harness/zcode/**` 与本文件，不读真实用户数据，不提交。

### RED（改 `zcode.go` 之前）

```console
$ go test ./internal/harness/zcode/ -run 'TestWithoutRowidCountsRowsPastTheRowCap|TestFailedSchemaProbeIsReportedAndKeepsTheCursor|TestGenuinelyMissingUsageColumnsStillAdvance|TestUnreadableWALNeverFallsBackToTheStaleSnapshot' -v -count=1
--- FAIL: TestWithoutRowidCountsRowsPastTheRowCap (0.09s)
    zcode_regression_test.go:96: WITHOUT ROWID parse read 20000 of 20025 completed rows (last row zcode:u20025 present: false): every row past maxRowsPerParse=20000 is silently undercounted
--- FAIL: TestFailedSchemaProbeIsReportedAndKeepsTheCursor (0.00s)
    zcode_regression_test.go:152: a failed PRAGMA table_info probe was swallowed as "incompatible schema": Parse returned 0 events and advanced the cursor to {Offset:0 Size:12288 ModTime:2026-10-16 10:46:11.401563574 +0800 CST Fingerprint:d7589bc5e86311f01ee9b38a29dd4974fe85c22e Extra:{"v":1}}; want an error and the unchanged cursor {Offset:0 Size:11 ModTime:1970-01-01 08:00:01 +0800 CST Fingerprint:previous-scan Extra:}
--- PASS: TestGenuinelyMissingUsageColumnsStillAdvance (0.00s)
--- FAIL: TestUnreadableWALNeverFallsBackToTheStaleSnapshot (0.01s)
    zcode_regression_test.go:237: cursor advanced to {Offset:0 Size:61520 ModTime:2026-10-16 10:46:11.407346684 +0800 CST Fingerprint:16f57fa2c772c47bee1da8ce84766f2a66f6123e Extra:{"v":1}} while the write-ahead log was unreadable (parse error: <nil>): the log-only row would be skipped for good
FAIL	github.com/zzstar101/mytoken/internal/harness/zcode	0.744s
```

（这份 RED 快照拍摄于后来加入 GREEN 里那行 `t.Logf` 之前，所以断言的行号比当前 `zcode_regression_test.go` 小 1。）

三条 RED 都是实现缺陷而不是测试写错；正对照 `TestGenuinelyMissingUsageColumnsStillAdvance`（库**真的**缺必需列）在旧实现下就 PASS，说明修复没有把"缺列"和"探测失败"混为一谈。

复现手法（都是 `t.TempDir()` 合成库，无真实数据）：

- **20025 行**：`CREATE TABLE model_usage (... PRIMARY KEY(id)) WITHOUT ROWID`，id 形如 `u00001…u20025`，按插入顺序排序，于是 `WHERE status='completed' ORDER BY "id" LIMIT 20000` 每次返回同一批前缀。
- **探测失败**：`CREATE VIEW model_usage AS SELECT * FROM a_table_that_does_not_exist` —— SQLite 接受该 DDL（视图体在查询时才解析），`PRAGMA table_info("model_usage")` 才报 `SQL logic error: no such table: main.a_table_that_does_not_exist (1)`。这是确定性、无注入的"探测运行错误"。
- **热 WAL 不可读**：keeper 连接保持 WAL，u1 只写进 `-wal`；先断言前提（`openWithPragmas(path,"immutable=1")` 看不到 u1），再 `chmod 000 <db>-wal`。`chmod` 只改 ctime，不改 size/mtime，而 `signatureOf` **只看** size 与 mtime，所以旧实现推进的 cursor 会让 u1 永久消失（恢复权限后的下一次扫描命中"未变化快速路径"）。

### 修复

1. **WITHOUT ROWID 不再被 20000 行截断**：`usageQuery`（`zcode.go:852`）现在只在 rowid 水位线分支拼 `LIMIT maxRowsPerParse`；无 rowid 分支读全部 `completed` 行、不设 LIMIT。没有改用 keyset 分页（`id > lastID`）——账本 id 可能随机或乱序插入，那只是把一种漏行换成另一种；幂等由稳定 `DedupKey` 保证。`collect`（`zcode.go:399`）在无 rowid 分支显式清空水位线/pending，并注明该批从未被截断。
2. **探测失败不再被吞成"不兼容 schema"**：`tableColumns`（`zcode.go:718`）改为返回 `(map, error)`，`sqlite_master` 查询、`rows.Scan`、`rows.Err`、`PRAGMA table_info`（抽成 `tableInfo`，`zcode.go:749`）的任何失败都向上传播；`Parse`（`zcode.go:323`）把它包成 `zcode: probe schema of <path>: ...` 并**原样返回 `cur`**，不推进 cursor。只有"`model_usage` 不存在或缺必需列"才继续"空 batch + 推进 cursor"。`usageHasRowid`（`zcode.go:808`）与 `maxRowid`（`zcode.go:832`）同样返回 error，只把 SQLite 的 `no such column`（`*sqlite.Error`、code 1）判为"没有 rowid"，其余错误当探测失败。
3. **删除 `immutable=1` 陈旧快照兜底**：`openReadOnly`（`zcode.go:679`）现在只做 `mode=ro`（顺带去掉冗余的 `immutable=0`），不再回落 `immutable=1`。热 WAL 打不开时打开失败 → `Parse` 返回空 batch + 原 cursor（上层不报错、下轮重试），**绝不读旧主库、绝不推进 cursor**。
4. **`cursorVersion` 1 → 2**（`zcode.go:145`）：`decodeState` 会丢弃旧 revision 的 cursor 并触发一次全量重读，使已被前两个缺陷漏计过的库在下次扫描自愈，而不是等文件再变化。

### GREEN（本轮，命令与输出）

```console
$ gofmt -l internal/harness/zcode
（无输出）

$ go vet ./internal/harness/zcode/
（无输出）

$ go test ./internal/harness/zcode/ -count=1
ok  	github.com/zzstar101/mytoken/internal/harness/zcode	1.035s

$ go test ./internal/harness/zcode/ -race -count=1
ok  	github.com/zzstar101/mytoken/internal/harness/zcode	11.678s

$ go test ./internal/harness/zcode/ -run 'TestWithoutRowidCountsRowsPastTheRowCap|TestFailedSchemaProbeIsReportedAndKeepsTheCursor|TestGenuinelyMissingUsageColumnsStillAdvance|TestUnreadableWALNeverFallsBackToTheStaleSnapshot' -v -count=1
--- PASS: TestWithoutRowidCountsRowsPastTheRowCap (0.31s)
--- PASS: TestFailedSchemaProbeIsReportedAndKeepsTheCursor (0.00s)
--- PASS: TestGenuinelyMissingUsageColumnsStillAdvance (0.00s)
    zcode_regression_test.go:233: parse with an unreadable write-ahead log: 0 events, error = <nil>, cursor = {Offset:0 Size:0 ModTime:0001-01-01 00:00:00 +0000 UTC Fingerprint: Extra:}
--- PASS: TestUnreadableWALNeverFallsBackToTheStaleSnapshot (0.01s)
PASS
ok  	github.com/zzstar101/mytoken/internal/harness/zcode	0.501s

$ go build ./...
（成功）

$ go test ./internal/harness/... ./internal/scan/...
（zcode 与 scan 全部 ok；唯一失败是 `internal/harness/minimax` 的
 TestProbeMalformedSchema / TestProbeBrokenVirtualTable —— 那是并发工作流的
 未跟踪包、不 import zcode、不在本轮写入范围内，本轮没有触碰）
```

WAL 用例的那行日志是本轮的关键证据：不可读时走的是"**0 events + 原 cursor**"路径（`error = <nil>`，cursor 全零），恢复权限后再扫描发出 `zcode:u1` —— WAL-only 行没有被静默跳过。

保留的不变量（复审未改）：只读 `mode=ro` + 连接级 `PRAGMA query_only`、不创建/改写 sidecar 与 `journal_mode`、`status` 精确匹配 `completed`、token/cache 分桶与 clamp、隐私（不 SELECT 正文/标题列）、Rooted/Provenance 的"显式空根不回退默认"。**未提交**。

## 复审修复（第二轮）：cursor v4 —— 行窗口 backlog、水位行锚点、扫描失败上报（2026-10-16）

第二轮 ZCode 复审指出三项问题，其中 C1（>20000 行静止账本永久漏计）与 I1（单行 `rows.Scan` 失败被 `continue` 且仍推进水位线）是静默少计，M1（`MAX(rowid)` 恰等于水位线但旧行已被替换/复用）要求先评估再决定修或写明边界。约束照旧：只改 `internal/harness/zcode/**` 与本文件，全部 fixture 都在 `t.TempDir()` 下合成，不读真实用户数据库，不提交。三轮仍是 RED → GREEN → REFACTOR：先写 `internal/harness/zcode/zcode_backlog_test.go`（6 个测试，其中 1 个正对照），确认 5 个真实 RED 之后才动 `zcode.go`，最后做只改结构的 REFACTOR 并复跑全部测试。

### RED（改 `zcode.go` 之前）

```console
$ go test ./internal/harness/zcode/ -run 'TestStaticLedgerLargerThanRowCapDrainsAcrossScans|TestVersionMigrationDrainsAStaticLedgerPastTheRowCap|TestScanFailureIsReportedAndKeepsTheCursor|TestReplacedRowidsAtTheWatermarkAreNotSkipped|TestVanishedWatermarkRowStillCountsRowsAppendedAboveIt|TestWatermarkRowStillCountsAppendedRowsIncrementally' -v -count=1
--- FAIL: TestStaticLedgerLargerThanRowCapDrainsAcrossScans
    after reading 20000 of 20123 rows of an unchanged ledger the cursor state is {"v":3,"r":20000} with no backlog marker: the next scan takes the unchanged fast path and rows 20001..20123 are never counted
--- FAIL: TestVersionMigrationDrainsAStaticLedgerPastTheRowCap
    a v2 cursor migrating off a static 20077-row ledger reported 20000 distinct rows; first missing [zcode:u20001 zcode:u20002 zcode:u20003 zcode:u20004 zcode:u20005]
--- FAIL: TestScanFailureIsReportedAndKeepsTheCursor
    a row that failed to scan was skipped and the cursor advanced to {Offset:0 Size:20480 ... Extra:{"v":3,"r":3}} (events [zcode:u2 zcode:u3]): the row is now below the watermark and no later scan can reach it
--- FAIL: TestReplacedRowidsAtTheWatermarkAreNotSkipped
    a ledger replaced in place (MAX(rowid)=5 == watermark) reported 0 events and missed [zcode:n00001 zcode:n00002 zcode:n00003 zcode:n00004 zcode:n00005]: `rowid > watermark` cannot see rows that reuse a rowid at or below it
--- FAIL: TestVanishedWatermarkRowStillCountsRowsAppendedAboveIt
    the row appended above a vanished watermark row was skipped: keys = []
--- PASS: TestWatermarkRowStillCountsAppendedRowsIncrementally
FAIL	github.com/zzstar101/mytoken/internal/harness/zcode	1.128s
```

（上面每段只摘录该 `t.Fatalf` 的消息文本；路径/时间等细节与当时快照一致。5 个 RED 全为实现缺陷，正对照 `TestWatermarkRowStillCountsAppendedRowsIncrementally` 在旧实现下就 PASS —— 它证明修复不能靠把每次扫描都变成全量重扫来蒙混。）

复现手法（全部确定性、无注入、无真实数据）：

- **20123 行静止账本（C1）**：`baseSchema` 下插 20123 行 completed（id `u00001…u20123`），同一份文件反复 Parse。旧实现第一轮读满 20000 行、`Rowid=20000`，第二轮文件签名没变直接命中未变化快速路径，第 20001..20123 行**再也不会有任何扫描去读**。
- **v2 cursor 迁移（C1 的"已经中过招"形态）**：同一份 20077 行账本，把第一轮的 `b.Next` 改写成 `Extra={"v":2,"r":20000}`（版本号不同即触发旧 v3 一次全量重扫），仍然只读到 20000 行。
- **坏行（I1）**：`started_at` 列声明为 INTEGER，但插入字符串 `'not-a-number'`；SQLite 的列亲和性允许存 TEXT，`sql.NullInt64` 扫描它必然失败（`typeof(started_at)='text'`，确定性、无注入）。旧 `readUsage` 对 `scanUsage` 的错误 `continue`，于是这一行被跳过而水位线仍然前进到 3。
- **原地重写（M1）**：先插 5 行读满，再 `DELETE FROM model_usage` 后插入 5 行**新 id**（`n00001…`）。非 AUTOINCREMENT 表在表空时从 rowid 1 重新分配，于是 `MAX(rowid)=5` 恰等于旧水位线，`rowid > watermark` 返回 0 行。（`TestReusedWatermarkRowTriggersAnIdempotentRescan` 里还断言了 `maxRowidOf == rows` 这个前提，避免 fixture 悄悄漂移。）

### 修复

1. **C1：cursor 增加持久 backlog 标记 `Drain`（`{"d":true}`）**。`zcodeState`（`zcode.go:742`）新增 `Drain bool \`json:"d,omitempty"\``；未变化快速路径（`zcode.go:368`）从"签名相等 + 版本相等"改为"签名相等 + 版本相等 + **`!state.Drain`**"。`collect`（`zcode.go:437`）在读完新行后置 `state.Drain = watermark < max`（`zcode.go:550`）：读满窗口而没到表尾就是 backlog，下一轮**即使文件完全没变**也会打开库继续读，直到读空后 `Drain=false`、快速路径恢复。`max` 与本次读取来自同一个快照，所以并发提交要么被这次读到、要么改变签名；早一拍置 false 也不会漏行。没有改成 keyset 分页，也没有把 LIMIT 调大——窗口本身没错，错的是"窗口结束"被当成了"账本结束"。
2. **C1 的迁移面：`cursorVersion` 3 → 4**（`zcode.go:171`）。`decodeState` 会丢弃旧 revision 的 cursor（`Rowid`/`Pending`/`Anchor` 一起归零）并触发一次全量重读，所以一个**已经被停在 20000 行的 v3 cursor** 下次扫描就自愈：重扫第一轮读满窗口并记 `Drain`，第二轮把 20001 行之后读完；任何更旧的 v2 cursor 也一样。稳定 `zcode:<id>` 去重保证重扫只重复发同一批 `DedupKey`，不会多计（测试逐 key 断言恰好 1 次）。
3. **I1：单行扫描失败不再被吞**。`readUsage`（`zcode.go:571`）把 `scanUsage` 的错误改成 `return fmt.Errorf("scan usage row: %w", err)`（`zcode.go:583`），不再 `continue`；`Parse`（`zcode.go:349`）把 `collect` 的失败包成 `zcode: read usage from <path>: ...`（`zcode.go:415`）并**原样返回调用方的 `cur`**（`ctx` 取消仍优先返回 `ctx.Err()`）。因此一轮失败**不留部分推进**：`collect` 只在成功路径上把状态写回 `next()`，失败时 `Parse` 用的是进来的 `cur`；重试时坏行与其他行一起被重新读到（测试断言修好后一次拿到全部 3 行，且失败轮 events 为 0）。
4. **M1：给水位线加"行锚点" `Anchor`（`{"a":"zcode:<id>"}`）**。`collect` 在 `Rowid>0` 时先用 `watermarkRowStillThere`（`zcode.go:595`；`anchorQuery` 即 `WHERE rowid = ?`，`zcode.go:1032`）读回水位线那一行，把它的 `dedupKey` 与 cursor 记的 `Anchor` 比较：行不见了、或该 rowid 现在装着别的行、或 anchor 为空/损坏，一律 `forgetResumePoint`（`zcode.go:769`）后从 0 重扫（幂等靠稳定 `DedupKey`）。捕获时机在同一轮读新行时完成（`row.rowid > watermark` 时同步更新 anchor）。`MAX(rowid) == watermark` 之所以不能证明"水位线以下都读过"，正是因为 SQLite 会把刚腾出来的 rowid 立刻发给下一行；只有那一行自己能作证。锚点校验每轮只多一条单行查询，读新行的代价仍是 O(新增行)。

### REFACTOR（结构整理，行为不变，测试复跑仍全绿）

三处"续读点失效"（表重建、水位行被替换/复用、`WITHOUT ROWID` 分支）原本各写一遍 `Rowid=0; Pending=nil; Anchor=""`，收敛为 `(*zcodeState).forgetResumePoint`（`zcode.go:769`），并在 `zcodeState` 的文档注释里写明**`Rowid`/`Pending`/`Anchor` 属于同一个续读点，必须一起失效**（用新表去读旧 anchor 等于对错误的行下断言）；`Drain` 刻意不在其中：它由续读后的读取结果重算，唯一没有窗口可重算的分支自己清掉它。`gofmt`/`vet`/测试在 REFACTOR 后复跑结果与 GREEN 相同。

### GREEN（本轮，命令与输出）

```console
$ gofmt -l ./internal/harness/zcode/
（无输出）

$ go vet ./internal/harness/zcode/
（无输出）

$ go test ./internal/harness/zcode/ -run 'TestStaticLedgerLargerThanRowCapDrainsAcrossScans|TestVersionMigrationDrainsAStaticLedgerPastTheRowCap|TestScanFailureIsReportedAndKeepsTheCursor|TestReplacedRowidsAtTheWatermarkAreNotSkipped|TestReusedWatermarkRowTriggersAnIdempotentRescan|TestWatermarkRowStillCountsAppendedRowsIncrementally' -v -count=1
--- PASS: TestStaticLedgerLargerThanRowCapDrainsAcrossScans (0.10s)
--- PASS: TestVersionMigrationDrainsAStaticLedgerPastTheRowCap (0.22s)
--- PASS: TestScanFailureIsReportedAndKeepsTheCursor (0.01s)
--- PASS: TestReplacedRowidsAtTheWatermarkAreNotSkipped (0.01s)
--- PASS: TestReusedWatermarkRowTriggersAnIdempotentRescan (0.01s)
--- PASS: TestWatermarkRowStillCountsAppendedRowsIncrementally (0.00s)
PASS
ok  	github.com/zzstar101/mytoken/internal/harness/zcode	0.624s

$ go test ./internal/harness/zcode/ -count=1
ok  	github.com/zzstar101/mytoken/internal/harness/zcode	1.436s

$ go test -race ./internal/harness/zcode/ -count=1
ok  	github.com/zzstar101/mytoken/internal/harness/zcode	15.150s

（上面两条是 REFACTOR 之前的 GREEN；下面是 REFACTOR 之后的最终冻结状态，同一套命令复跑）
$ gofmt -l ./internal/harness/zcode/ && go vet ./internal/harness/zcode/
（均无输出）
$ go test ./internal/harness/zcode/ -count=1
ok  	github.com/zzstar101/mytoken/internal/harness/zcode	1.536s
$ go test -race ./internal/harness/zcode/ -count=1
ok  	github.com/zzstar101/mytoken/internal/harness/zcode	14.975s

$ go test ./internal/harness/... -count=1
ok  	.../internal/harness/zcode	1.398s
（minimax/openclaude/openclaw/pi/qwen/roo/stepcode/workbuddy/zed 等包 ok；
 失败的是并发工作流正在改的 opencode / opencode/sqlitecore / mimocode
 —— 三个包都未跟踪或已修改、都不 import zcode、都不在本轮写入范围内，本轮未触碰）

$ go test ./internal/harness/zcode/ -run TestUnreadableWALNeverFallsBackToTheStaleSnapshot -v -count=1
    zcode_regression_test.go:233: parse with an unreadable write-ahead log: 0 events, error = <nil>, cursor = {Offset:0 Size:0 ModTime:0001-01-01 00:00:00 +0000 UTC Fingerprint: Extra:}
--- PASS: TestUnreadableWALNeverFallsBackToTheStaleSnapshot (0.01s)
```

关键一行：不可读 WAL 时 `mode=ro` 打开本身失败，仍是"**0 events + 原 cursor（全零）+ error=nil**"，恢复权限后下一次扫描发出 `zcode:u1`；I1 改动没有把这条只读/WAL 安全路径变成"读到陈旧主库"或"推进 cursor"。

测试规模：`zcode_backlog_test.go` 418 行 / 6 个测试；连同既有套件，`zcode` 包内共 6 个测试文件 + conformance 子检查，`-race` 全绿。改动文件：`internal/harness/zcode/zcode.go`、新增 `internal/harness/zcode/zcode_backlog_test.go`、本文件。`zcode.go` 1093 → 1194 行。

### M1 的边界（锚点覆盖不到的部分，写清楚而不是假装没有）

锚点解决的是复审点名的那种形态：**rowid 被复用/替换**。只要水位线那一行的身份变了（行没了、或同一 rowid 换了另一行、或 anchor 缺失），扫描就从 0 重来，因此"清空后原地重填、`MAX(rowid)` 恰等于旧水位线"不再漏行；`TestReplacedRowidsAtTheWatermarkAreNotSkipped` 与 `TestReusedWatermarkRowTriggersAnIdempotentRescan` 覆盖了这两种入口。

真正的残余边界只有一条，且本轮**不修**：

> 一次账本重写同时满足：(a) 文件签名（`Size` + `ModTime` + 前 4KB sha1）与上一轮**完全一致**，因而未变化快速路径直接跳过；(b) 即便走到了扫描（签名变了），被替换的行都在水位线**以下**，而水位线那一行恰好被原样重建、`DedupKey` 一模一样。

要发现 (a) 只能把签名做到"任何一次提交都变"（`fingerprint` 已经是前 4KB sha1，SQLite 头部含 change counter，所以现实中要求文件内容与 mtime 都被复现）；要发现 (b) 只能每轮重读水位线以下的全部行，也就是把增量扫描退化成全量扫描——那正是水位线与 `maxRowsPerParse` 存在的理由。两者都不会静默吞掉**新增**的行（新增行 rowid 必然大于水位线，或改变签名），代价上限是"漏掉一次被覆盖写入的旧行重算"；真实账本要出现这一形态，意味着有人把历史行原地改写成另一条历史行。需要真机样本才能判定是否值得用全量扫描换。**已按"绝不臆造"处理：不写没有证据的兜底，把它作为显式边界留在文档里**（也列入下面"未解决的 schema 风险"第 9 条）。仓库里 `internal/harness/minimax/minimax.go:746` 的 `watermarkIntact` 用的是同一套"水位行自证"思路（键值型 `id` + 行 digest），可作旁证。

### 保留的不变量（本轮未改）

`mode=ro` + 连接级 `PRAGMA query_only`、绝不创建/改写 sidecar 与 `journal_mode`、**绝不回落 `immutable=1` 读陈旧快照**；`status` 精确匹配 `completed`；cache 分桶与 clamp；隐私（不 SELECT 正文/标题列）；Rooted/Provenance 的"显式空根不回退默认"；**全部 pending rowid 按 500 分块重读、不淘汰任何 id、暂时消失的 id 仍保留**（v4 只加 `Anchor`/`Drain` 两个字段，不动 pending 语义）；`WITHOUT ROWID` 全量读 `completed` 不设 LIMIT。**未提交**。

## 未解决的 schema 风险

1. **`ZCODE_HOME` 未被一手证据确认**，本轮刻意不实现。若官方后续确认，需要新增一个 env tier（并保持 `MYTOKEN_ZCODE_DIRS` 优先）+ 多根测试。
2. **默认路径只有第三方背书**：`~/.zcode/cli/db/db.sqlite` 出自 ccusage 指南，官方 QA 页面没有列出该文件。若真实布局不同，默认根会 Discover 到 0 个 source（静默无输出）；`Discover` 已同时探测 home/cli/db/文件四种候选布局来缓解，但消除不了该风险，用户可用 `MYTOKEN_ZCODE_DIRS` 覆盖。
3. **列名仍可能演化**：必需列集合按第三方文档固定为 `id, session_id, started_at, model_id, status, input_tokens, output_tokens`；若真实表用别的拼写（例如 `tokens_input`），该库会被判为"不是 ZCode 账本"而**静默 0 计数**（不报错）。cache/provider 的别名表也只覆盖已知拼写。
4. **`input_tokens` 是否真含 cache 只有第三方背书**（也是本轮最关键的一条）：若实际不含，`CacheRead`/`CacheWrite` 会被扣成 0 或偏小，`Input` 也随之偏小。需要真机核对一次。
5. **status 枚举大小写**：按文档精确匹配 `"completed"`；若真实写入 `"COMPLETED"`/`"Completed"`，会 0 计数。这是刻意选择——宁可漏计，也不把未知状态当用量（原先的 case-fold 版本正是被测试抓到的缺陷）。
6. **秒级 `started_at` 的 ×1000 是防御性推断**：若真实基准不是毫秒，时间会错（不报错）。
7. **`credentials.json` 的加密格式无法验证**，因此解析器完全不接触该文件，也从不读 `config.json`。
8. **`rooted_test.go` 的 `wantRegistered` 已 28→29**：工作区里已出现并发工作流新增的 `internal/harness/mimocode/`、`internal/harness/minimax/`。它们注册后该常量与 `provenance_test.go` 的 env surface 表必须再同步一次，否则 public 套件会 fatal（`len(overrideEnvSurface) != len(harness.All())`）。本轮没有触碰那两个包。
9. **水位行以下的"原地覆盖写入"无法在不放弃增量的前提下发现**（M1 的残余边界，细节与理由见「复审修复（第二轮）→ M1 的边界」）：锚点只能证明**水位线那一行**仍是原来那行。若一次重写把水位线以下的旧行换成另一批行、而水位线那一行恰好被原样重建（且文件签名一致或看不出差异），这些被替换的旧行不会重算。新增行不受影响。判定是否需要用全量扫描换这一形态，需要真机样本。

## 未关闭的门

- **没有用真实 `~/.zcode` 数据库验证**：本轮不联网、不读真实用户数据，全部结论来自 `t.TempDir()` 下的合成 fixture。上面第 2/3/4/5 条风险只有真机才能关闭。
- **GUI 无专属配色**：`internal/gui/theme.go` 的 harness 颜色/图标表没有 `zcode` 条目（该文件不在允许写入范围内）；既有回退路径会让新 harness 以默认颜色与圆点显示，不影响计数。

## Lead 集成更新（2026-10-16）

上述注册数与 GUI 接线项已由 Lead 完成：`model.ZCode`、app blank import、32-harness registry/provenance 断言，以及专属名称/颜色均已接入。本文保留原工作流边界作为审计记录；尚未关闭的发布门只剩脱敏真实数据库对拍及其 schema 风险。
