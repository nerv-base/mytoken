# MiMo Code 离线适配验证（internal/harness/mimocode）

日期：2026-10-16 10:27 CST（首轮）；第二轮（五项审查修正）见文末，同日 11:2x CST；环境：go1.27.1 darwin/arm64；工作目录 `/Users/zzstar/Developer/Programs/MyToken!!!!!`；分支 `feat/0.4x-harnesses`；HEAD `a9684c7`。**未提交**（工作区有并发改动，本次不提交、不 reset、不覆盖他人内容）。

写入范围（只此三处）：

```console
$ git status --porcelain internal/harness/opencode internal/harness/mimocode
 M internal/harness/opencode/opencode.go
 M internal/harness/opencode/opencode_test.go
?? internal/harness/mimocode/
?? internal/harness/opencode/sqlitecore/
```

未改动 `internal/model`、`internal/app`、`README`、`ROADMAP`、规划文件，也未改动并发工作流正在写的 `internal/harness/kimi/**`、`internal/harness/pi/**`、`internal/harness/zcode/**`、`internal/harness/stepcode/**`、`rooted_test.go`、`provenance_test.go`、`docs/verification/*`（除本文件）。

## 结论与范围

新增第 N 个离线 harness：MiMo Code（`github.com/XiaomiMiMo/MiMo-Code`，OpenCode 的 fork）。它保持**独立来源身份** `model.Harness("mimo-code")`（`internal/harness/mimocode/mimocode.go:53`）与**独立事件命名空间** `mimo-code:`（`internal/harness/mimocode/mimocode.go:58`），因此同一份对话即使同时被 OpenCode 与 MiMo Code 记录，也仍是两个 source、两套 dedup key，永不碰撞。

**复用而非复制**：MiMo Code 沿用 OpenCode 的 MessageV2 存储引擎，所以只读 SQLite 深模块被抽出到 `internal/harness/opencode/sqlitecore`（902 行，只读连接 + schema 探测 + 水位线 + usage 累加器），`mimocode.go`（420 行）只剩 MiMo 特有的部分：数据目录解析、数据库发现、身份与 key 前缀、以及两条有意的隐私收紧。OpenCode 自身也迁移到该模块，其 `opencode.go` 从 1145 行降到 517 行（`-751/+123`），并且 conformance golden 逐字节未变。

**做**：`MIMOCODE_HOME`/`MIMOCODE_DB`/`MYTOKEN_MIMO_CODE_DIRS` 的显式优先级与硬失败；XDG 默认数据目录（`$XDG_DATA_HOME/mimocode`，否则 `~/.local/share/mimocode`，全平台一致）；只发现 `mimocode.db` 与 `mimocode-*.db`；`mode=ro` + `PRAGMA query_only` 的只读快照读取活跃 WAL 库；`(time_updated,id)` 水位线增量、cursor 稳定、幂等；`session.parent_id` → `ParentID` 且每条 message 只计一次；usage 映射与 OpenCode v1 完全一致；`ProjectPath` 只存目录 basename、`Title` 永不存储；严格 RED→GREEN→REFACTOR；合成 fixture 覆盖 conformance 六项 + channel DB、多 DB、环境优先级、敏感文件排除、父子不双计、WAL/query_only。

**不做**（都是"没有证据就不臆造/不越权"）：不读 `auth.json`、`.credentials.json`、`config`、日志（Discover 只报数据库文件，`mimocode.go:270`）；不读 `part` 表与 `session.title`（都是对话正文）；不迁移或改写源库（只读连接 + 测试断言源库字节不变）；不推断 cost/BaseURL/GitBranch/Boundary；不动 `internal/model` 的共享常量与 `internal/app` 的注册（见「未关闭的门」）。

## 上游依据

一手官方源码（只作事实依据，未据此读取任何凭据）：

- <https://raw.githubusercontent.com/XiaomiMiMo/MiMo-Code/main/packages/shared/src/global.ts>：`MIMOCODE_HOME` 非空且为绝对路径时 `data = <home>/data`；否则 `mode: "xdg"`、`data: path.join(xdgData!, APP)`（`APP = "mimocode"`）；`MIMOCODE_HOME` 非绝对时 **抛错**（不是回退）。
- <https://unpkg.com/xdg-basedir@5.1.0/index.js>（`packages/cli/package.json:188`、`packages/shared/package.json:37` 都锁定 `"xdg-basedir": "5.1.0"`）：`export const xdgData = env.XDG_DATA_HOME || (homeDirectory ? path.join(homeDirectory, '.local', 'share') : undefined)`。这是**唯一**的分支：该库没有 darwin/windows 特例，所以 `path.join(xdgData, "mimocode")` 在 macOS 上同样是 `~/.local/share/mimocode`，不是 `~/Library/Application Support/mimocode`；空 `XDG_DATA_HOME` 走 `||` 回退。
- <https://raw.githubusercontent.com/XiaomiMiMo/MiMo-Code/main/packages/cli/src/storage/db.ts>：`MIMOCODE_DB` 为 `:memory:` 或绝对路径时原样使用，否则相对 data 目录；`getChannelPath()` 的通道文件名规则见下；`journal_mode = WAL`、`busy_timeout 5000`。
- 通道文件名（`storage/db.ts` 的 `getChannelPath` + `flag/flag.ts:422` 的 `MIMOCODE_DISABLE_CHANNEL_DB: !falsy("MIMOCODE_DISABLE_CHANNEL_DB")`，`falsy(key)` 为 `value === "false" || value === "0"`）：**默认 true** —— 所有通道共用 `mimocode.db`。只有 `MIMOCODE_DISABLE_CHANNEL_DB=false`/`0` **且** `InstallationChannel` 不属于 `latest|beta|prod`（未设 `MIMOCODE_CHANNEL` 时为 `"local"`）才产生 `mimocode-<safe-channel>.db`（`replace(/[^a-zA-Z0-9._-]/g,'-')`）。所以 `mimocode-*.db` 是少数派形态，不是"正式通道外的其他通道"通例。
- <https://raw.githubusercontent.com/XiaomiMiMo/MiMo-Code/main/packages/cli/src/session/session.sql.ts>：`session(id, parent_id, directory, …)`、`message(id, session_id, time_created, time_updated, data)`、`part(...)`；**没有** OpenCode v2 的 `session_message` 表。`message.data` 存 `MessageV2.Info`。
- <https://raw.githubusercontent.com/XiaomiMiMo/MiMo-Code/main/packages/cli/src/session/message-v2.ts>：assistant JSON 含 `role, modelID, providerID, path{cwd,root}, cost, tokens{total?,input,output,reasoning,cache{read,write}}`，与 OpenCode v1 message schema 相容。
- 官方 README（**文档漂移，不作为行为依据**）：`README.md:405` 写 data 为 `~/.local/share/mimocode/`，`README.md:409` 写「On macOS, XDG data defaults to `~/Library/Application Support/mimocode/`」，`README.md:372` 写「On Windows, XDG paths fall under `%LOCALAPPDATA%\mimocode\`」（中文版 `README.zh.md:363`）。`LOCALAPPDATA` 在整个仓库里只出现在这两处 README 正文，实现里没有任何对应分支；macOS 那句也与 xdg-basedir@5.1.0 的源码矛盾。**以代码为准**：默认目录全平台都是 `$XDG_DATA_HOME/mimocode` 或 `~/.local/share/mimocode`。`MIMOCODE_HOME` 可覆盖；凭据/config/auth 不在本次读取范围。

## 实现要点

### 身份与命名空间

| 项 | 值 | 位置 |
| --- | --- | --- |
| Harness identity | `model.Harness("mimo-code")`（本地常量 `HarnessID`，待 Lead 补 `model.MimoCode`） | `internal/harness/mimocode/mimocode.go:53` |
| 事件 key 前缀 | `mimo-code:`（与 `opencode:` 无交集） | `internal/harness/mimocode/mimocode.go:58` |
| 来源 kind | 仅 `sqlite` | `internal/harness/mimocode/mimocode.go:318` |

### 环境优先级与默认目录

`rootProvenance`（`internal/harness/mimocode/mimocode.go:148`）严格按序：

1. `MYTOKEN_MIMO_CODE_DIRS`（`harness.SplitEnvList`，与 `MYTOKEN_OPENCODE_DIRS` 同格式）——替换整个根集合；
2. `MIMOCODE_DB`——单个数据库文件；
3. `MIMOCODE_HOME`——数据目录 `<home>/data`；
4. XDG 默认：`XDG_DATA_HOME/mimocode`（`XDG_DATA_HOME` 为空串视同未设，与 xdg-basedir 的 `||` 一致），否则 `~/.local/share/mimocode`。**没有按 GOOS 分支**：macOS/Windows 也走同一条规则。

**显式坏值不静默回退**：`MIMOCODE_HOME` 非绝对、`MIMOCODE_DB` 不以文件命名（`:memory:` 或任何 `:` 前缀）→ `RootProvenance` 返回 `*envError`，`Roots()` 为空，绝不用低优先级默认顶上（`mimocode.go:194-226`、`defaultRoots` `:175`、`envError` `:228`）。`MIMOCODE_DB` 绝对路径原样使用，相对路径拼到 data 目录下（`:211`）。

### Discover

`Discover`（`internal/harness/mimocode/mimocode.go:270`）只做两件事：

- 根本身是普通文件 → 直接当成一个显式 sqlite source（这是 `MIMOCODE_DB` 指向任意文件名时的读取路径）；
- 根是目录 → 只取**直接子项**中 `isDBName` 命中的文件，即 `mimocode.db` 或 `mimocode-*.db`（`mimocode.go:310`），不递归、不匹配 `mimocode.db-wal`/`-shm`、不碰 `opencode.db`、不碰 `auth.json`/`config.json`/`.credentials.json`/`*.log`。缺失根 → 0 source、nil error。

### 只读 SQLite 深模块（`internal/harness/opencode/sqlitecore`）

从 `opencode.go` 抽出（`sqlitecore.go:160`）：`sql.Open("sqlite", file:…?mode=ro&immutable=0&_pragma=busy_timeout(5000)&_pragma=query_only(1))`、`SetMaxOpenConns(1)`、5s ping；签名 `Stat`（主库 + `-wal`/`-shm` 的 size/mtime，`sqlitecore.go:81`）、`Fingerprint`（前 4KB sha1，`:101`）、`Unchanged`/`Next`（`:116`/`:122`）；每表独立 `Watermark`（`:133`）与 `Reset`（`:413`）；`Spec`（`:224`）、`requiredColumns`/`kindColumn`/`scannable`（`:237`/`:254`/`:403`）；`Sessions`（`:285`，`session`+`session_v2` 按 id 合并）；`Messages`（`:356`，列探测 + keyset 分页 + 富化水位线）；`MaxUpdated`（`:390`）；`Profile`/`Accumulator`（`:651`/`:681`，identity、key 前缀、Project/Title 映射、按 dedupKey 字段级 max 合并、compaction boundary）。

**水位线契约（`Reset` 与 `Messages` 必须回答同一个 schema 问题）**：`Reset` 丢弃水位线的条件是 `wm.Updated < 0`、`!scannable(spec)`、或表内 `MAX(time_updated)` 取不到/小于水位线（回退或换库）。`scannable` = `HasColumns(spec.Table, requiredColumns()...)`，而 `requiredColumns` 正是 `Messages` 的读取列（`id, session_id, data, time_updated`，再加 `KindExpr` 是裸列名时的该列）。**旧实现是错的**：`Reset` 只检查 `time_updated`，比读取谓词弱，于是"表在但缺 `data`/`type` 列"时水位线被保留，而那一轮什么都读不到，之后即使列回来了，`time_updated` 不增长的行会被永久跳过（永久漏计）。修复是把同一个 schema 判据交给两边，并新增 `internal/harness/opencode/sqlitecore/sqlitecore_test.go`（286 行）做直接回归：水位线超前/回退、表缺失与重现、缺列三种形态、`Reset` 状态、`(time_updated,id)` 排序与幂等重扫。

**为什么不用 `immutable=1`**：`immutable=1` 会告诉 SQLite 文件不会变化，从而可能跳过 WAL，活跃库最近提交的行会被静默漏读。因此固定 `immutable=0` + `query_only`，由 `TestActiveWALDatabaseIsReadFromASafeSnapshot` 证明活跃 WAL 能读全且源库/`-wal` 不被改写。

**`PRAGMA query_only(1)` 是安全收紧，不是"完全行为不变"**：OpenCode 迁移后**读到的行**与旧实现逐字节一致（同一 spec、同一 keyset 分页、golden 不变，原实现即 `mode=ro&immutable=0`，没有这条 PRAGMA），但连接多了一层只读约束——任何试图写库的语句（包括误加的临时表/attached 库写入）会以 `SQLITE_READONLY` 直接失败，而不是依赖文件以只读方式打开的间接保护。这是**刻意保留的行为差异**，不是纯重构：如果将来需要在该连接上写临时表，这条 PRAGMA 必须显式放开。`TestSourceIsOpenedReadOnly` 同时断言只读打开与 `query_only` 生效。

### usage 映射（与 OpenCode v1 完全一致）

`input`/`output`/`reasoning`/`cache.read`/`cache.write` 一一对应 `model.Tokens` 的 Input/Output/Reasoning/CacheRead/CacheWrite，**不做扣减**（OpenCode 的 `input` 本就不含 cache 类）；负数与浮点截断沿用 `Num.Int64()`；`cost` 只保留有限正值（0 = unknown → nil）；时间戳取 `time.created`，缺失时回落到行 `time_updated`，秒/毫秒按 `<1e12` 归一；同一 dedup key 在一次解析内重复出现按类别取 max（OpenCode 会在 row 首次出现后追加 usage）。MiMo 只有 `message` 一张表，因此只用一个 spec（`mimocode.go:349`），不存在 v2 compaction 语义。

`TestUsageMappingMatchesOpenCodeV1` 用同一个数据库同时喂两个 parser，逐字段比对 Model/Provider/Tokens/Cost/Timestamp/SessionID/ParentID，`projectPath` 按各自契约不同（MiMo 存 basename，OpenCode 存绝对目录）。

### 隐私与父子关系

- `session.parent_id` → `UsageEvent.ParentID`/`SessionMeta.ParentID`；每条 message 只 emit 一次（key = message id），父子不会双计（`TestParentChildSessionsCountOnce`）。
- `ProjectPath` = `baseName(FirstNonEmpty(session.directory, path.cwd, path.root))`（`mimocode.go:88`、`:410`），只留最后一段；绝不存完整路径。
- `Profile.Title` 为 nil（`mimocode.go:88`）——MiMo 的 `session.title` 是对话正文，永不落盘到输出；`part` 表与 assistant 正文同样从不读取。
- 全部 fixture 为手写合成数据，凭据类文件只以哨兵文本存在，用于断言"永不出现"。

## 严格 TDD 过程

### RED（先失败）

先写测试与 fixture（`internal/harness/mimocode/{mimocode_test.go,conformance_test.go}`、`testdata/conformance/mimocode/seed/step{0,1}.sql`），此时包内没有任何实现文件：

```console
$ go test ./internal/harness/mimocode
# github.com/zzstar101/mytoken/internal/harness/mimocode [github.com/zzstar101/mytoken/internal/harness/mimocode.test]
internal/harness/mimocode/mimocode_test.go:174:25: undefined: HarnessID
internal/harness/mimocode/mimocode_test.go:177:5: undefined: HarnessID
internal/harness/mimocode/mimocode_test.go:180:12: undefined: New
internal/harness/mimocode/mimocode_test.go:180:36: undefined: HarnessID
internal/harness/mimocode/mimocode_test.go:181:50: undefined: HarnessID
internal/harness/mimocode/mimocode_test.go:183:23: undefined: HarnessID
internal/harness/mimocode/mimocode_test.go:218:36: undefined: Parser
internal/harness/mimocode/mimocode_test.go:240:39: undefined: Parser
internal/harness/mimocode/conformance_test.go:60:19: undefined: HarnessID
internal/harness/mimocode/conformance_test.go:61:54: undefined: NewWithRoots
internal/harness/mimocode/mimocode_test.go:183:23: too many errors
FAIL	github.com/zzstar101/mytoken/internal/harness/mimocode [build failed]
FAIL
```

（同一次 RED 也暴露了测试自身的一个缺陷：隐私断言里把 `secret-project` 当禁词，而 basename `secret-project-worktree` 合法包含它；改为禁 `SENTINEL_PRIVATE_TEXT`/用户名/完整路径/路径分隔符，并保留"输出必须等于 basename"的正向断言。）

### GREEN（实现到通过）

实现 `mimocode.go`（在 `sqlitecore` 之上），生成 golden：

```console
$ go test ./internal/harness/mimocode -run Conformance -update
ok  	github.com/zzstar101/mytoken/internal/harness/mimocode	0.546s
$ go test ./internal/harness/mimocode
ok  	github.com/zzstar101/mytoken/internal/harness/mimocode	0.327s
```

### REFACTOR（把 OpenCode 也搬到同一深模块）

`opencode.go` 改为使用 `sqlitecore`：删除自带的只读连接、signature/fingerprint、tableColumns/quoteIdent、loadSessions/sessionColumns/mergeSession、resetWatermark/maxTableUpdated、readMessages、整个 accumulator 与 num/epochMS/reportedCost 等 751 行重复实现；只保留 `Profile`、`dbState`（Extra 线格式，含 legacy `{"u","i"}` 兼容）、`messageTables()` 两个 generation 的 spec、`parseDB`、legacy JSON 路径与发现逻辑。测试侧只有一处机械改名（`p.classes()` → `p.Classes()`），断言未动。

回归证据：OpenCode 的测试与 golden **完全未改且全绿**，`git status` 里 `internal/harness/opencode/testdata` 无任何改动——即迁移后**读到的行**是等价的。唯一的行为差异见上一节的 `PRAGMA query_only(1)`：连接比旧实现更严，属安全收紧而非"完全行为不变"。

## 命令与输出

```console
$ gofmt -l internal/harness/opencode internal/harness/mimocode
（无输出）

$ go vet ./internal/harness/opencode/... ./internal/harness/mimocode/...
（无输出）

$ go test -count=1 ./internal/harness/opencode/... ./internal/harness/mimocode/...
ok  	github.com/zzstar101/mytoken/internal/harness/opencode	0.586s
ok  	github.com/zzstar101/mytoken/internal/harness/opencode/sqlitecore	0.436s
ok  	github.com/zzstar101/mytoken/internal/harness/mimocode	0.662s

$ go test -race -count=1 ./internal/harness/opencode/... ./internal/harness/mimocode/...
ok  	github.com/zzstar101/mytoken/internal/harness/opencode	2.220s
ok  	github.com/zzstar101/mytoken/internal/harness/opencode/sqlitecore	1.945s
ok  	github.com/zzstar101/mytoken/internal/harness/mimocode	2.263s

$ go test -count=1 ./internal/harness/opencode/sqlitecore -v
--- PASS: TestResetKeepsOnlyAWatermarkTheTableStillCovers (0.06s)
    --- PASS: .../at_the_newest_row (0.02s)
    --- PASS: .../behind_the_newest_row (0.01s)
    --- PASS: .../without_an_id_yet (0.01s)
    --- PASS: .../newer_than_the_table,_a_rewound_or_replaced_database (0.01s)
    --- PASS: .../negative (0.01s)
    --- PASS: .../an_emptied_table_drops_every_watermark (0.01s)
    --- PASS: .../a_rewound_watermark_re-reads_every_row (0.01s)
--- PASS: TestResetForgetsAWatermarkWhenTheTableVanishes (0.01s)
--- PASS: TestResetDropsAWatermarkForATableMessagesCannotRead (0.02s)
    --- PASS: .../without_the_data_column (0.01s)
    --- PASS: .../without_the_id_column (0.01s)
    --- PASS: .../without_the_kind_column (0.01s)
--- PASS: TestAHalfMigratedTableDoesNotStrandTheRowItHeld (0.01s)
--- PASS: TestMessagesResumesByUpdatedThenID (0.01s)
ok  	github.com/zzstar101/mytoken/internal/harness/opencode/sqlitecore	0.360s

$ go test -count=1 ./internal/harness/mimocode -v
--- PASS: TestConformance (0.06s)
    --- PASS: TestConformance/golden (0.01s)
    --- PASS: TestConformance/incremental (0.02s)
    --- PASS: TestConformance/cursor-stable (0.01s)
    --- PASS: TestConformance/idempotent (0.01s)
    --- PASS: TestConformance/privacy (0.01s)
    --- PASS: TestConformance/discover-missing (0.00s)
--- PASS: TestIdentityKeepsMiMoCodeSeparate (0.00s)
--- PASS: TestDedupKeyPrefixIsIndependent (0.00s)
--- PASS: TestDefaultDataDirectoryFollowsXdgBasedir (0.00s)
    --- PASS: TestDefaultDataDirectoryFollowsXdgBasedir/without_XDG_DATA_HOME_it_is_<home>/.local/share/mimocode (0.00s)
    --- PASS: TestDefaultDataDirectoryFollowsXdgBasedir/an_empty_XDG_DATA_HOME_counts_as_unset,_like_xdg-basedir's_|| (0.00s)
    --- PASS: TestDefaultDataDirectoryFollowsXdgBasedir/MIMOCODE_HOME_beats_it (0.00s)
    --- PASS: TestDefaultDataDirectoryFollowsXdgBasedir/the_macOS_and_Windows_defaults_are_never_used (0.00s)
--- PASS: TestRootsEnvironmentPriority (0.00s)
    --- PASS: TestRootsEnvironmentPriority/platform_default (0.00s)
    --- PASS: TestRootsEnvironmentPriority/XDG_DATA_HOME (0.00s)
    --- PASS: TestRootsEnvironmentPriority/MIMOCODE_HOME (0.00s)
    --- PASS: TestRootsEnvironmentPriority/MIMOCODE_HOME_beats_the_platform_default (0.00s)
    --- PASS: TestRootsEnvironmentPriority/MIMOCODE_DB_absolute (0.00s)
    --- PASS: TestRootsEnvironmentPriority/MIMOCODE_DB_relative_to_MIMOCODE_HOME_data (0.00s)
    --- PASS: TestRootsEnvironmentPriority/MIMOCODE_DB_relative_to_the_platform_data_dir (0.00s)
    --- PASS: TestRootsEnvironmentPriority/MYTOKEN_MIMO_CODE_DIRS_wins (0.00s)
    --- PASS: TestRootsEnvironmentPriority/a_bad_MYTOKEN_MIMO_CODE_DIRS_is_not_replaced (0.00s)
    --- PASS: TestRootsEnvironmentPriority/a_relative_MIMOCODE_HOME_is_rejected (0.00s)
    --- PASS: TestRootsEnvironmentPriority/an_in-memory_MIMOCODE_DB_is_rejected (0.00s)
    --- PASS: TestRootsEnvironmentPriority/a_bad_MIMOCODE_DB_does_not_fall_back_to_the_data_dir (0.00s)
--- PASS: TestExplicitRootsOverrideEnvironment (0.00s)
--- PASS: TestWithRootsBuildsAFreshParser (0.00s)
--- PASS: TestDiscoverOnlyMiMoDatabases (0.00s)
--- PASS: TestDiscoverExplicitDatabaseFile (0.00s)
--- PASS: TestDiscoverMissingRootIsEmpty (0.00s)
--- PASS: TestMultipleDatabasesAndChannelDatabase (0.01s)
--- PASS: TestUsageMappingMatchesOpenCodeV1 (0.01s)
--- PASS: TestParentChildSessionsCountOnce (0.00s)
--- PASS: TestNoUsageRowsEmitNoEvent (0.00s)
--- PASS: TestPrivacyKeepsOnlyTheDirectoryBasename (0.00s)
--- PASS: TestSourceIsOpenedReadOnly (0.00s)
--- PASS: TestActiveWALDatabaseIsReadFromASafeSnapshot (0.00s)
--- PASS: TestIncrementalResumesFromTheWatermark (0.00s)
--- PASS: TestForeignFileAndUnknownSchemaAreNotFatal (0.00s)
ok  	github.com/zzstar101/mytoken/internal/harness/mimocode	0.300s

$ go test -count=1 ./internal/harness/ ./internal/app/
ok  	github.com/zzstar101/mytoken/internal/harness	0.453s
ok  	github.com/zzstar101/mytoken/internal/app	3.146s

$ git diff --stat internal/harness/opencode
 internal/harness/opencode/opencode.go      | 870 +++++-------------------------
 internal/harness/opencode/opencode_test.go |   4 +-
 2 files changed, 123 insertions(+), 751 deletions(-)

$ git status --porcelain internal/harness/opencode internal/harness/mimocode
 M internal/harness/opencode/opencode.go
 M internal/harness/opencode/opencode_test.go
?? internal/harness/mimocode/
?? internal/harness/opencode/sqlitecore/
```

首轮同步全量回归时，唯一失败的包是**并发工作流正在写的** `internal/harness/stepcode`（`TestConformance/golden` 缺 golden、`TestBadDirsListIsHardError`、`TestParseCountsOnlyRealModelCalls`、`TestIncrementalAppendAndPartialLine`）；它与本次改动无关（不 import `opencode`/`sqlitecore`/`mimocode`），本次没有触碰该包，且该包随后已由作者收口（见文末）。第二轮（XDG 修正 + `sqlitecore` 直接单测）只跑本节列出的两个包及其子包 + `-race`，未再跑全量。

## 验收覆盖矩阵

| 验收项 | 证据 |
| --- | --- |
| 独立 identity + display-ready `mimo-code` | `TestIdentityKeepsMiMoCodeSeparate`（`Harness()==mimo-code`，与 `harness.Get(model.OpenCode)` 并不同源） |
| 独立 key 前缀不碰撞 | `TestDedupKeyPrefixIsIndependent`（`mimo-code:` 前缀、与 `opencode:` 无交集） |
| Rooted / Provenance / Discover / Parse 完整 | `TestRootsEnvironmentPriority`、`TestExplicitRootsOverrideEnvironment`、`TestWithRootsBuildsAFreshParser`、`TestConformance/discover-missing` |
| XDG 默认数据目录全平台一致（无 darwin/windows 分支） | `TestDefaultDataDirectoryFollowsXdgBasedir` 4 个子用例（无 `XDG_DATA_HOME` → `<home>/.local/share/mimocode`；空串视同未设；`MIMOCODE_HOME` 优先；macOS `Library/Application Support` 与 Windows `LOCALAPPDATA` 永不被使用） |
| 只发现 `MIMOCODE_DB` 显式文件或 roots 下 `mimocode.db`/`mimocode-*.db` | `TestDiscoverExplicitDatabaseFile`、`TestDiscoverOnlyMiMoDatabases`（可见 `TestDiscoverOnlyMiMoDatabases`） |
| 不扫 auth/config/日志 | `TestDiscoverOnlyMiMoDatabases`（`auth.json`/`config.json`/`.credentials.json`/`*.log`/`mimocode.db-wal`/`opencode.db`/nested 全部排除，且不产出事件） |
| `MIMOCODE_HOME` 与 `MYTOKEN_MIMO_CODE_DIRS` 优先级 | `TestRootsEnvironmentPriority` 12 个子用例（含 `MYTOKEN_MIMO_CODE_DIRS` 压过 `MIMOCODE_DB`/`MIMOCODE_HOME`） |
| `MIMOCODE_DB` 绝对/相对、拒绝 `:memory:` | 同上（绝对、相对 data 目录、相对 XDG data 目录、`:memory:` 拒绝） |
| 显式坏 env 不静默回退 | `TestRootsEnvironmentPriority/a_bad_*`（`RootProvenance` 报错且 `Roots()` 为空） |
| SQLite query-only/read-only | `TestSourceIsOpenedReadOnly`（`PRAGMA query_only=1`，`INSERT`/`DELETE` 被拒，源库字节不变）——安全收紧，非"完全行为不变" |
| WAL 活跃库安全快照 | `TestActiveWALDatabaseIsReadFromASafeSnapshot`（db 保持打开、WAL 未 checkpoint 时读全用量，`-wal` 与主库均未被改写） |
| assistant usage 与 OpenCode v1 一致 | `TestUsageMappingMatchesOpenCodeV1` |
| `parent_id` 父子关系、每条只计一次 | `TestParentChildSessionsCountOnce` + golden 中 `ses_mc_sub` 带 `parentId` |
| 只存目录 basename、不存正文/标题/完整路径 | `TestPrivacyKeepsOnlyTheDirectoryBasename` + `TestConformance/privacy`（哨兵文本不出现在输出与 golden） |
| conformance 六项 | `TestConformance/{golden,incremental,cursor-stable,idempotent,privacy,discover-missing}` |
| channel DB / 多 DB | `TestMultipleDatabasesAndChannelDatabase` |
| 增量水位线 | `TestIncrementalResumesFromTheWatermark` + `TestConformance/incremental`，以及 `sqlitecore` 直接单测 `TestMessagesResumesByUpdatedThenID`（按 `(time_updated,id)` 排序、空转不推进、越界新行被读到） |
| 无关/损坏文件不致命 | `TestForeignFileAndUnknownSchemaAreNotFatal`、`TestNoUsageRowsEmitNoEvent` |
| 水位线语义（`Reset`/读取共用同一 schema 判据） | `internal/harness/opencode/sqlitecore/sqlitecore_test.go`：`TestResetKeepsOnlyAWatermarkTheTableStillCovers`（8 子用例）、`TestResetForgetsAWatermarkWhenTheTableVanishes`、`TestResetDropsAWatermarkForATableMessagesCannotRead`（缺 `data`/`id`/kind 列各一）、`TestAHalfMigratedTableDoesNotStrandTheRowItHeld` |
| OpenCode 不回归 | `go test ./internal/harness/opencode/...` 全绿（含新增 `sqlitecore` 包），`testdata/**` 未改，`internal/app` 全绿；连接层多一条 `query_only` 收紧 |

## 未解决的 schema 风险

1. **无真实日志验证**：所有 fixture 均为手写合成数据，未使用任何真实 MiMo Code 数据目录；`message.data` 与 OpenCode v1 相容这一结论来自上游源码阅读，而不是对真实库的抽样。若真实库存在旧 schema 变体（缺列），`sqlitecore` 的列探测会降级跳过而非报错，可能静默少读——这继承 OpenCode 的既有策略。**本轮已修掉其中最坏的一类**：过去"表在但读取列缺失"会保留水位线，列回来后被跳过的行永久漏计；现在 `Reset` 与 `Messages` 用同一 `scannable(spec)` 判据，读不到的表一律清空水位线（`TestResetDropsAWatermarkForATableMessagesCannotRead`、`TestAHalfMigratedTableDoesNotStrandTheRowItHeld`）。**仍存**：keyset 水位线本身无法回头——若某行在扫描之后、以**早于水位线**的 `time_updated` 才写入（乱序回填），它仍会被跳过；去重（`dedup`/字段级 max）能挡住重复计数，但挡不住这一次漏计。
2. **`MIMOCODE_DISABLE_CHANNEL_DB` 的语义（已按上游更正）**：默认 **true**，即所有通道共用 `mimocode.db`；只有显式 `false`/`0` 且通道不属于 `latest|beta|prod` 时才出现 `mimocode-<channel>.db`。本实现离线发现同时接受 `mimocode.db` 与 `mimocode-*.db`，是安全超集，不会漏读也不会误报（其他文件名不会被当成 source）；flag 本身只影响在线进程选哪个文件名，离线无需建模。
3. **`:` 前缀一律拒绝**：MiMo 的 `db.ts` 只对 `:memory:` 特判，其余非绝对路径都会拼到 data 目录。本实现对任何 `:` 开头（含 `file:` URI 式）的 `MIMOCODE_DB` 都判为坏覆盖并硬失败——这是有意的保守选择（宁可显式报错，不把 `data/file:...` 这种离奇文件名当真），已由测试固定。
4. **Windows 未在真机验证（但已无 GOOS 分支）**：上一版的 `%LOCALAPPDATA%/mimocode` 分支已删除，默认目录现在只由 `XDG_DATA_HOME`/`os.homedir()` 决定，因此在 Windows 上也是 `%USERPROFILE%\.local\share\mimocode`（与 xdg-basedir@5.1.0 的 `os.homedir()` 一致）。风险从"分支未验证"降级为"这套路径规则未在 Windows 上实跑"，且本机测试用的 `HOME` 覆盖不覆盖 Windows 的 `USERPROFILE` 解析。
5. **session_v2 / compaction 语义不适用于 MiMo**：MiMo 的 schema 没有 `session_message`，因此本 parser 不产出 `model.BoundaryCompact`（无证据表明 MiMo 有该存储形态）。若上游后续引入 v2 表，需要新增 spec 而不是改 identity。
6. **`requiredColumns` 是结构判据，不解析 SQL**：它把 `Spec.KindExpr` 为裸列名的情况算进必需列，但 `Spec.Filter` 里额外引用的列不在检查范围内。若未来的 spec 把列写进 `WHERE`/`json_extract` 而不放 `KindExpr`，缺列时仍可能落到"表在、读取报错"的分支——此时 `Messages` 会 `return wm`（不清水位线）。当前两个 harness 的 spec 都不属于这种情况（过滤表达式只用 `data` 与裸 `type`/`role`）。

## 未关闭的门

1. **共享常量与注册 wiring（Lead 负责，本次被明确禁止改共享文件）**：`internal/model/model.go` 需补 `MimoCode Harness = "mimo-code"` 与 `DisplayName()` 分支，`internal/app/app.go` 需 blank import `internal/harness/mimocode`；在那之前 `harness.All()` 里没有 MiMo Code，CLI 无法选中它，`HarnessID` 只是本地常量（值与前缀已按最终契约固定）。`internal/harness/rooted_test.go`、`provenance_test.go` 的期望集合、`docs/HARNESS.md` 的支持表也随注册一并更新。**（该项随后已在 Lead 集成更新中完成，见文末。）**
2. ~~**`sqlitecore` 的独立单测**~~ **已关闭（本轮）**：新增 `internal/harness/opencode/sqlitecore/sqlitecore_test.go`（286 行），只覆盖该模块自身的水位线/schema 契约，不复制两个 harness 的集成测试；它先以 `Reset(ctx, table, wm)` 旧签名跑出行为性 RED，再随实现一起改为 `Reset(ctx, spec, wm)`。
3. **并发工作流**：`internal/harness/stepcode` 首轮同步全量回归时自测失败，属于其他工作流的进行中状态；本次不做任何改动也不提交。**（该作者随后已独立收口，见文末。）**

## Lead 集成更新（2026-10-16）

上述共享接线已完成：最终常量为 `model.MiMoCode`，app blank import、32-harness registry/provenance 断言、HARNESS/README 与 GUI 名称/颜色均已同步；Step Code 也已独立收口。首轮双审查随后要求修正 macOS XDG 默认路径，并为 `sqlitecore` 增加直接回归覆盖；最终状态以 `v04x-harness-matrix.md` 与全量验证结果为准。

## 第二轮：五项审查修正（同日，RED→GREEN→REFACTOR）

只改 `internal/harness/mimocode/**`、`internal/harness/opencode/**`、`docs/verification/mimo-code.md`；未提交（HEAD 仍 `a9684c7`）。每条都先独立核对上游源码，再让测试（或新增直接测试）以旧实现 RED。

### 1（Critical）默认数据目录：macOS 也是 XDG，不是 `~/Library/Application Support`

上游核对：`packages/{cli,shared}/package.json` 都锁 `xdg-basedir@5.1.0`，其 `index.js` 只有 `env.XDG_DATA_HOME || path.join(os.homedir(), '.local', 'share')` 一条分支，**没有 darwin/windows 特例**；`packages/shared/src/global.ts` 在 `MIMOCODE_HOME` 未设时 `data: path.join(xdgData!, APP)`（`APP = "mimocode"`）。README 里的 macOS/Windows 路径是文档漂移。

RED（先把测试改成可证伪的 XDG 规则，实现未动；`go test -count=1 ./internal/harness/mimocode`）：

```console
--- FAIL: TestDefaultDataDirectoryFollowsXdgBasedir (0.00s)
    --- FAIL: TestDefaultDataDirectoryFollowsXdgBasedir/without_XDG_DATA_HOME_it_is_<home>/.local/share/mimocode (0.00s)
        mimocode_test.go:289: RootProvenance paths = [/Users/zzstar/Library/Application Support/mimocode], want [/Users/zzstar/.local/share/mimocode]
    --- FAIL: TestDefaultDataDirectoryFollowsXdgBasedir/an_empty_XDG_DATA_HOME_counts_as_unset,_like_xdg-basedir's_|| (0.00s)
        mimocode_test.go:295: RootProvenance paths = [/Users/zzstar/Library/Application Support/mimocode], want [/Users/zzstar/.local/share/mimocode]
    --- FAIL: TestDefaultDataDirectoryFollowsXdgBasedir/the_macOS_and_Windows_defaults_are_never_used (0.00s)
        mimocode_test.go:323: default data directory = "/Users/zzstar/Library/Application Support/mimocode", want the xdg-basedir path "/Users/zzstar/.local/share/mimocode"
        mimocode_test.go:327: default data directory = "/Users/zzstar/Library/Application Support/mimocode", want "/Users/zzstar/.local/share/mimocode"
--- FAIL: TestRootsEnvironmentPriority (0.00s)
    --- FAIL: TestRootsEnvironmentPriority/platform_default (0.00s)
        mimocode_test.go:361: RootProvenance paths = [/var/folders/v6/s5l8nzv52hs283p18y_0x3l80000gn/T/TestRootsEnvironmentPriority517385710/001/Library/Application Support/mimocode], want [/var/folders/v6/s5l8nzv52hs283p18y_0x3l80000gn/T/TestRootsEnvironmentPriority517385710/001/.local/share/mimocode]
FAIL
FAIL	github.com/zzstar101/mytoken/internal/harness/mimocode	0.354s
```

FIX（`internal/harness/mimocode/mimocode.go:194`）：删掉 `switch runtime.GOOS`，改为「`MIMOCODE_HOME`（必须绝对，否则 `envError`）→ `XDG_DATA_HOME`（空串视同未设）→ `os.homedir()/.local/share/mimocode`」，并删掉 `runtime` import；测试侧的 `platformData` helper 同步改成 XDG 规则，另加 `TestDefaultDataDirectoryFollowsXdgBasedir` 4 子用例把「macOS/Windows 默认永不被使用」钉死。GREEN：`ok .../mimocode 0.412s`。

### 2 文档更正 `MIMOCODE_DISABLE_CHANNEL_DB`

上游核对（`storage/db.ts` 的 `getChannelPath` + `flag/flag.ts:422` + `falsy()`）：flag 默认 **true**（`!falsy(...)`，只有 `"false"`/`"0"` 算 falsy）⇒ 所有通道共用 `mimocode.db`；只有显式 `false`/`0` **且** 通道不属于 `latest|beta|prod` 时才产生 `mimocode-<safe-channel>.db`。文档原来写成「正式通道 `mimocode.db`，其他通道 `mimocode-<channel>.db`」，漏掉了默认 true 这一层，已改写（见「上游依据」第 4 条与「未解决的 schema 风险」第 2 条）。Discover 同时接受 `mimocode.db` 与 `mimocode-*.db` 是安全超集，不需要按 flag 建模。

### 3 `sqlitecore` 水位线语义审计（表缺失/重现、缺列、回退、Reset）

审计结论：**`Reset` 的 schema 判据比 `Messages` 弱**。旧 `Reset(ctx, table, wm)` 只看 `time_updated` 列是否存在；`Messages` 还需要 `id, session_id, data`（以及 v2 的 `type`）。于是「表在、但缺 `data`/`id`/`type` 列」时，`Messages` 什么也读不到却 `return wm`，而 `Reset` 又保留了水位线 ⇒ 该表的水位线**再也无法被降低**；等列回来（迁移补列、备份回退），`time_updated <= wm` 的行会被永久跳过（永久漏计）。表**整个缺失**的分支（`MaxUpdated` 的 `!ok`）反而是安全的，`MaxUpdated` 的列检查与 `Reset` 一致。

RED（先写直接测试，`go test -count=1 ./internal/harness/opencode/sqlitecore/...`；测试对着旧签名 `Reset(ctx, table, wm)` 写，所以失败是行为性的而不是编译错误）：

```console
--- FAIL: TestResetDropsAWatermarkForATableMessagesCannotRead (0.01s)
    --- FAIL: .../without_the_data_column (0.00s)
        sqlitecore_test.go:201: Reset = {Updated:2000 ID:b}, want zero: the matching Messages call cannot read this table, so the watermark can never be lowered again and a row at or before time_updated=2000 is skipped once the columns return
    --- FAIL: .../without_the_id_column (0.00s)
        sqlitecore_test.go:201: Reset = {Updated:2000 ID:b}, want zero: the matching Messages call cannot read this table, so the watermark can never be lowered again and a row at or before time_updated=2000 is skipped once the columns return
    --- FAIL: .../without_the_kind_column (0.00s)
        sqlitecore_test.go:201: Reset = {Updated:2000 ID:b}, want zero: the matching Messages call cannot read this table, so the watermark can never be lowered again and a row at or before time_updated=2000 is skipped once the columns return
--- FAIL: TestAHalfMigratedTableDoesNotStrandTheRowItHeld (0.01s)
    sqlitecore_test.go:242: the scan after the migration finished emitted [], want [c a b]
FAIL
FAIL	github.com/zzstar101/mytoken/internal/harness/opencode/sqlitecore	0.252s
```

FIX（`internal/harness/opencode/sqlitecore/sqlitecore.go`）：新增 `Spec.requiredColumns()`（`:237`）、`kindColumn(expr)`（`:254`）、`(*Snapshot).scannable(spec)`（`:403`）；`Messages`（`:356`）与 `Reset`（`:413`）改用同一判据，`Reset(ctx, spec Spec, wm Watermark)` 在 `!scannable(spec)` 时清空水位线。选 spec-aware 的 `Reset` 而不是只在 `Messages` 里兜底，是为了让 `Reset` 自己的答案无论调用顺序都正确；两处生产调用点（`opencode.go`、`mimocode.go`）同步改名。REFACTOR 无重复实现。

### 4 `sqlitecore` 最小直接测试

新增 `internal/harness/opencode/sqlitecore/sqlitecore_test.go`（286 行）：水位线**超前**（`wm.Updated > MAX(time_updated)`，含换库/回退）与**回退**、表**缺失**与**重现**、缺列三种形态、`Reset` 状态保持/清空、表空、`(time_updated,id)` 排序与幂等空转。只用合成 fixture 直连该模块，不复制两个 harness 的集成测试（集成覆盖仍由两侧 conformance 提供）。

### 5 文档单列 `PRAGMA query_only(1)` 是安全收紧

原文档写「OpenCode 迁移后行为不变……额外获得 `query_only`」，措辞把差异说没了。已改为单列一段（见「只读 SQLite 深模块」）：**读到的行**与旧实现逐字节一致（golden 未变），但连接多一层只读约束——任何写库语句（含临时表/attached 库）会 `SQLITE_READONLY` 直接失败，这是刻意保留的行为差异而非纯重构；`TestSourceIsOpenedReadOnly` 同时断言 `PRAGMA query_only=1`、`INSERT`/`DELETE` 被拒、源库字节不变。

### 第二轮验证

`gofmt -l` 无输出、`go vet` 无输出、`go test -count=1 ./internal/harness/opencode/... ./internal/harness/mimocode/...` 三包全绿、`-race` 三包全绿（见「命令与输出」）。**仍存风险**：keyset 水位线无法回头读「事后以更早 `time_updated` 写入」的行（去重只防重复计数，不防这一次漏计）；`requiredColumns` 是结构判据、不解析 `Spec.Filter`；Windows 未真机验证；无真实日志抽样。

## 第三轮：容错投影——一个错类型的可选字段不再吃掉整行（同日，RED→GREEN→REFACTOR）

只改 `internal/harness/opencode/**`、`internal/harness/mimocode/**`、`docs/verification/mimo-code.md`；HEAD 仍 `a9684c7`，未提交，无新依赖。原文的证据来自上游源码阅读与合成 fixture，未读任何真实用户日志或凭据。

### 1 缺陷：严格解码 + 水位线推进 = 永久漏计

`sqlitecore.Decode` 过去对 `Payload` 做**一次**严格 `json.Unmarshal`（`encoding/json` 遇到第一个类型不符的字段就整体报错），调用点 `if !ok { return }` 于是丢掉**整行**，而 `Messages` 的 `(time_updated,id)` keyset 水位线照常推进 ⇒ 这一行的 token 永远不会被读第二次。触发条件只需要 `data` 里**任一非核心可选字段**的 JSON 类型不符，与 token 数字本身无关：

- `"cost":"0.0123"`（上游 `Schema.Finite`/`z.number()`，字符串是客户端或中间层的序列化变形）
- `"path":"/tmp/x"`（上游是 `{cwd,root}` 对象）
- `"time":{"created":"whenever"}`、`"role":7`、`"modelID":7`、`"providerID":7`、`"sessionID":7`
- 任何未知字段只要是合法 JSON 都不该影响解码（这一条旧实现本来也满足，新实现必须保持）

三条读取路径都在同一类里：OpenCode sqlite（`internal/harness/opencode/opencode.go:424`）、MiMo sqlite（`internal/harness/mimocode/mimocode.go:391`）、以及 legacy JSON 生成（`parseLegacyJSON` 里的严格 `json.Unmarshal(raw,&file)`，失败即 `return harness.Batch{Next: next}, nil`）。

### 2 上游依据：usage 权威地写在 message 上，step-finish `part` 只是同一次 usage 的副本（因此永不读 `part`）

本轮同时把「为什么不读 `part`」钉进文档与测试，因为它是容错修复的相邻风险：一旦改成「`message` 读不出来就去 `part` 里找」，同一次请求就会被计两次。

OpenCode（`anomalyco/opencode`，`dev`；`sst/opencode` 已 301 到该仓库，旧 `packages/opencode/src/session/message-v2.ts` 已在 `44b6938b2ad2` 删除）：

- `packages/schema/src/v1/session.ts:453-483` — `Assistant`：`role: Literal("assistant")`、`time:{created,completed?}`、`path:{cwd,root}`、`cost: Schema.Finite`（:471）、`tokens: {total?,input,output,reasoning,cache:{read,write}}`（:472）。
- `packages/schema/src/v1/session.ts:240-256` — `StepFinishPart`：`type: Literal("step-finish")`、`cost: Schema.Finite`（:245）、**同一个** `tokens` 结构（:246）。
- `packages/core/src/session/sql.ts:79-98` — `SessionMessageTable("session_message", {id, session_id, type, seq, Timestamps, data: text({mode:"json"})})`，与本仓库 v2 spec 的表/列一致。

MiMo Code（`XiaomiMiMo/MiMo-Code`，`main`）：

- `packages/cli/src/session/message-v2.ts:579-637` — `Assistant`：`cost`（:613）、`tokens`（:614）；`:639` `Info = discriminatedUnion("role",[User,Assistant])`。
- `packages/cli/src/session/message-v2.ts:373-390` — `StepFinishPart`：`cost: z.number()`（:377）、**同一个** `tokens` 结构（:378）。
- `packages/cli/src/session/session.sql.ts:86-102` — `MessageTable("message", {id, session_id, agent_id, Timestamps, data: text json})`；`:104-120` — `PartTable("part", {id, message_id, session_id, Timestamps, data})`；`:12` — `type PartData = Omit<MessageV2.Part,"id"|"sessionID"|"messageID">`，即 `part` 行确实带着 `StepFinishPart` 的 usage 副本。

结论：**两侧上游都把同一份 `cost`+`tokens` 同时写在 assistant message（权威）与 step-finish part（副本）上**。本解析器只读 message 表（v1 `message` / v2 `session_message` / MiMo `message`），MiMo 的 `part` 一律不打开，因此同一次 usage 恰好计一次。已固定的证据与守卫：

- `internal/harness/mimocode/mimocode_test.go:30` 的 `mimoDDL`（`part` 表在 `:43`）建出 `part` 表并在注释里声明「`part` 只放会话正文，parser 不许看」。
- `TestPrivacyKeepsOnlyTheDirectoryBasename`（`mimocode_test.go:859`）把哨兵文本写进 session title、assistant message 与一行 `part`，断言**恰好 1 个事件**且输出里没有哨兵、绝对路径。
- 本轮新增 `TestStepFinishPartIsNeverCountedTwice`（`mimocode_test.go:827`）：message 与 `part` 写入**完全相同**的 usage（`input:10,output:2,cache{read:4,write:1}`、`cost:0.0123`，`reason`/`type:"step-finish"` 齐备），断言事件数为 1、token 类逐项等于 `{10,2,4,1}`（而不是 20/4/8/2）、`cost` 只出现一次。这是**守卫测试**：它钉住既有正确行为，在修复前后都通过——若哪天有人让 `part` 参与计数，它立刻红。

### 3 RED（先失败；实现未动一行）

四层 RED，全部用合成临时 SQLite / 合成 legacy 目录，`go test -count=1`（行号是当次运行的行号，随后新增断言使同文件行号顺移）：

sqlitecore 深模块直接单测（19 例表驱动，只测 `Decode` 本身）：

```console
--- FAIL: TestDecodeKeepsTheRowWhenOptionalMetadataDoesNotFit (0.01s)
    ...
        sqlitecore_test.go:502: Decode reported nothing to project: one wrong-typed optional field must not cost the row
--- FAIL: TestDecodeRejectsOnlyWhatIsNotAMessageObject (0.00s)   # 只差 `null`：它也不是消息对象
--- FAIL: TestDecodeTreatsANonObjectUsageBlockAsNoUsage (0.00s)   # tokens 为 "12"/12/[]/true 时应为「无 usage」而非丢行
--- FAIL: TestDecodeKeepsEveryLegibleTokenClass (0.00s)
        sqlitecore_test.go:...: Decode = {...}, false; want a usage block
FAIL	github.com/zzstar101/mytoken/internal/harness/opencode/sqlitecore	0.252s
```

OpenCode sqlite 集成（真实临时库 + fixture 库副本，`INSERT` 恶意 `data`）：

```console
--- FAIL: TestMalformedOptionalMetadataStillCountsTheRow (0.01s)
    opencode_test.go:729: no event with DedupKey "opencode:msg_quoted_cost" (have [opencode:msg_a1 opencode:msg_a2 opencode:msg_sub1 opencode:msg_dual])
FAIL	github.com/zzstar101/mytoken/internal/harness/opencode	0.354s
```

MiMo sqlite 集成（`message` 表被污染，`part` 表另含正文哨兵）：

```console
--- FAIL: TestMalformedOptionalMetadataStillCountsTheRow (0.01s)
    mimocode_test.go:787: events = [], want [mimo-code:msg_path_string mimo-code:msg_quoted_cost mimo-code:msg_time_text]
FAIL	github.com/zzstar101/mytoken/internal/harness/mimocode	0.354s
```

legacy JSON 生成：

```console
--- FAIL: TestLegacyJSONWithAMalformedOptionalFieldStillCounts (0.01s)
    opencode_test.go:572: no event with DedupKey "opencode:msg_bad_path" (have [])
FAIL	github.com/zzstar101/mytoken/internal/harness/opencode	0.406s
```

legacy `time.created` 的兜底时间（在实现改 `sig.Mod.UnixMilli()` 之前重跑一次，确认断言可证伪）：

```console
--- FAIL: TestLegacyJSONWithAMalformedOptionalFieldStillCounts (0.01s)
    opencode_test.go:593: timestamp = 0001-01-01 00:00:00 +0000 UTC, want the file's modification time 2026-10-16 05:35:39.271 +0000 UTC
FAIL	github.com/zzstar101/mytoken/internal/harness/opencode	0.406s
```

### 4 FIX：两段式容错投影（`internal/harness/opencode/sqlitecore/sqlitecore.go`）

`Decode`（`:585`）改成两段：第一段只问「这是不是一个消息对象」——`var fields map[string]json.RawMessage; json.Unmarshal(data,&fields)`，`err != nil || fields == nil` 才 `return Payload{}, false`（于是 `null`/`[]`/`"x"`/`12`/`true`/截断的 `{` 都是「不是消息」，`{}` 是「是消息但什么都没说」）；第二段**逐字段**投影，一个字段解不出来只丢它自己：

- `project[T]`（`:615`）——单字段投影，失败即零值；`object`（`:633`）——取嵌套对象，缺失/`null`/非对象都返回 `false`。
- `projectTokens`（`:649`）——`tokens` 逐类投影（`input/output/reasoning` + `cache.read/cache.write`），`cache` 非对象只丢 cache 两类；**`tokens` 本身非对象 ⇒ `Tokens == nil` = 无 usage**，行仍然可解码。
- `projectModel`（`:670`）、`projectPath`（`:684`）——同样逐字段，所以 `"model":{...}` 内层畸形不会连带丢掉能读的 `providerID`。
- `projectCost`（`:701`）——先按 JSON number 解，失败再去掉引号 `strconv.ParseFloat`。**故意不复用 `Num`**：`Num` 是 `int64`，`cost:"0.0123"` 会被截成 0。这一点是第一版 GREEN 的实测失败暴露的（`quoted cost = <nil>, want 0.0123`），也说明「字符串数字兼容」必须按字段类型分别给。
- `Num.UnmarshalJSON`（`:744`）改为**全函数（total）**：读不出的数字记 0 并返回 `nil`，不再返回 `sqlitecore: cannot decode %q as a number`。理由有二：①`encoding/json` 里自定义 `Unmarshaler` 报错会中止**外层对象**剩余字段的解码，逐类容错会因此变成「顺序相关」（`input` 畸形会连带清掉 `output`）；②仓库既有house pattern 就是「垃圾数字记 0、好值之后的垃圾不破坏好值」（`internal/harness/clinetask/clinetask_test.go:134-147`）。

legacy 生成同步容错（`internal/harness/opencode/opencode.go`）：新增 `decodeLegacy`（`:468`，同样是 map 门控，再把文档当扁平 `MessageV2.Info` + session 字段交给 `sqlitecore.Decode` 填充内嵌 `messagePayload`）与 `legacyString`（`:486`，逐字段读 `directory/parentID/title/projectID/version`）；`parseLegacyJSON` 改用 `file, ok := decodeLegacy(raw)`。原有的「半写文件」门控（`Role != "assistant" || Tokens == nil || Tokens.Cache == nil || Time.Created == nil`）保持不变；但 `Num` 全函数化后，`"time":{"created":"whenever"}` 会给出非 nil 的 `Num(0)` 而穿过这道门，于是 `Emit` 的兜底时间从 `0` 改为 `sig.Mod.UnixMilli()`（`:541`），即**用文件自身的修改时间**给一个自己说不清时间的消息定年（`Stat` 把 `-wal`/`-shm` 的 mtime 一起折进去，`sqlitecore.go:81-98`）。没有这一改，这类行会以 1970 年入库——比丢行好，但仍然错。既有 legacy golden 全部自带 `time.created`（如 `testdata/opencode/legacy/.../msg_leg4.json`），所以该兜底对 golden 完全惰性。

### 5 水位线策略（刻意保持不变：跳过即推进）

`!ok` 的两处调用点仍保留「跳过并让水位线前进」（`opencode.go:424`、`mimocode.go:391` 各加了一段解释性注释）。理由：

1. **滞留会永久卡死**：`Messages` 的 keyset 是 `(time_updated,id)` 单调扫描，若为一行「读不出」的行不推进水位线，那么每次扫描都要重读这一行及其之后的所有行，永远没有进展（旧实现把「读不出」等同于「不是消息」，本来就不该滞留）。
2. **上游 `time_updated` 是 `$onUpdate`**：OpenCode `packages/core/src/database/schema.sql.ts` 与 MiMo `packages/cli/src/storage/schema.sql.ts` 都是 `time_updated: integer().notNull().$onUpdate(() => Date.now())`，任何 `UPDATE` 都会刷新它。于是被修复/重写的行会以**新的** `(time_updated,id)` 重新进入扫描窗口，并由 `DedupKey` 合并——这正是仓库里既有的回填测试所依赖的机制（`opencode_test.go` 的 `TestIncrementalAppendAndBackfill` 用 `UPDATE message SET time_updated = 1742302010000, data = '...' WHERE id = 'msg_a2'` 模拟上游重写）。因此「跳过 + 推进」不会造成永久漏计，只要那次修复是一次真正的写入。
3. **修好之后 `!ok` 几乎不可达**：v1 过滤（`opencode.go:367`）、v2 过滤（`:374`）、MiMo 过滤（`mimocode.go:346`）都要求 `json_valid(data)` **且** `$.role='assistant'`（或 `type IN ('assistant','compaction')`）**且** `$.tokens IS NOT NULL`。能穿过这些条件却仍不是 JSON 对象的行，等于「自称有 usage 却什么都没说」，跳过一次是正确结局。

> **第四轮（见文末）对本节的限定**：上面三条只适用于「行是合法消息、只是**调用点**读不出内容」的情形——那时水位线列本身读得出来，跳过一次是对的。**驱动层连整行都物化不了**（`rows.Scan` 失败）是另一回事：连 `time_updated` 都没读到，水位线会越过一个位置未知的行，keyset 从此不再匹配它。那种情况第四轮改成 fail-closed（返回调用时水位线），与本节策略并存、互不覆盖。

### 6 边界与代价（有意为之，已写进注释）

1. **`Num` 的错误信号没了**：一个真正损坏的计数器现在读作 0 而不是报错。若某行的所有 token 类都读不出且没有 cost，`Emit` 的既有判据（`sqlitecore.go:891` `tk.IsZero() && cost == nil`）会跳过该事件——与「非对象 `tokens`」同一条路径。这是刻意取舍：换来的是「错类型的可选字段不再吃掉整行」，而代价是损坏值不再响亮。
2. **非对象 `tokens` 仍会永久漏掉这一行**（事件层面）：它不可能被计入（没有可解释的数字），水位线依旧推进。这是本轮唯一保留的「跳过即推进」形态，也正是上游 SQL 过滤已经排除的形态。
3. **legacy `time.created` 不可读**：事件保留、按文件 mtime 定年（见上一节）。
4. **未改动的既有契约**：字符串数字兼容（`tokenPayload` 的 `Num` 字段、`{"input":"12"}`、小数截断、负值夹紧、`null` reasoning）由 `TestTokenClassMapping` 原样守住；`query_only`、水位线语义、privacy 哨兵、v1/v2 legacy golden、`testdata/**` 全部未动。

### 7 第三轮验证

```console
$ gofmt -l internal/harness/opencode/ internal/harness/mimocode/        # 无输出
$ go vet ./internal/harness/opencode/... ./internal/harness/mimocode/... # VET_OK
$ go build ./...                                                         # BUILD_OK
$ go test -count=1 ./internal/harness/opencode/... ./internal/harness/mimocode/...
ok  .../internal/harness/opencode          0.758s
ok  .../internal/harness/opencode/sqlitecore 0.519s
ok  .../internal/harness/mimocode          0.767s
$ go test -race -count=1 ./internal/harness/opencode/... ./internal/harness/mimocode/...
ok  .../internal/harness/opencode          2.686s
ok  .../internal/harness/opencode/sqlitecore 2.039s
ok  .../internal/harness/mimocode          2.585s
$ go test -count=1 ./...            # 全仓库：无任何 FAIL/构建失败行
```

**本轮仍存风险**：①上面第 6 节的三条取舍；②keyset 水位线依旧无法回头读「事后以更早 `time_updated` 写入」的行（与上一轮同）；③`cost` 的字符串兼容是本仓库的宽容扩展，上游写的是 JSON number——若上游某天改成语义不同的字符串（如带货币符号），会静默读 0 而不是报错；④无真实日志抽样，`part` 不读这一结论仍来自上游源码而非样本比对。

## 第四轮：`Messages` 的 Scan 失败不再静默越过（可选 NULL 宽容读入，其余 fail-closed）

只改 `internal/harness/opencode/sqlitecore/**` 与本文档；HEAD 未动、未提交未推送、无新依赖、未接触真实用户库（全部证据出自合成临时 SQLite fixture）。`Messages` 与 `Row` 的公开签名未变。

### 1 缺陷：Scan 失败的 `continue`，被**随后的行**推进水位线越过（永久漏计）

`Messages`（`sqlitecore.go:394`）逐行读五列，旧代码是：

```go
		var row Row
		if err := rows.Scan(&row.ID, &row.SessionID, &row.Data, &row.Updated, &row.Kind); err != nil {
			continue
		}
```

水位线只在成功扫描的行之后推进（`sqlitecore.go:430`：`if row.Updated > wm.Updated || (row.Updated == wm.Updated && row.ID > wm.ID)`）。失败行本身不会被**显式**写进水位线——但只要它后面还有一行能扫描成功，水位线就落在那行上，keyset 谓词 `time_updated > ? OR (time_updated = ? AND id > ?)`（`sqlitecore.go:404`）之后再也不匹配失败行，它的 usage **永久**漏计。这恰好违反该函数自己写下的契约（`sqlitecore.go:370-380`：一次失败的 pass 必须返回调用时的水位线，靠 `DedupKey` 幂等重读，丢行则不可接受）。

两个触发形态都已在复审中复现：

1. **`session_id IS NULL` 而 `data` 是合法 assistant usage**。`database/sql` 不能把 NULL 扫进 `string`（`sql: Scan error ... converting NULL to string is unsupported`）。而 `session_id` 确实是**可选**元数据：payload 自己就写 `sessionID`，MiMo 调用点本来就有兜底链 `sid := sqlitecore.FirstNonEmpty(row.SessionID, payload.SessionID, payload.SnakeSessionID)`（`internal/harness/mimocode/mimocode.go:401`）。旧代码里这一行直接消失，且若它在 keyset 序后面还有可读行，水位线就被推过去——不是"下次再读"，是"永远不再读"。
2. **`time_updated` 以 SQLite TEXT 动态类型存储**。列声明 `INTEGER`，但表不是 STRICT，动态类型照样接受 `'not-a-number'`，扫进 `int64` 失败。这一形态最险：`time_updated` **就是**水位线列，读不出它等于"这行的位置未知"，可旧代码仍允许后面的行把水位线推过它。合成库里 TEXT 按类型序排在所有整数之后，于是症状是"前面的行看起来都读到了"，直到其后再追加一行并成功扫描，水位线一步跨过畸形行。

### 2 RED（先失败；测试与实现都不动）

同包新增两条测试（`internal/harness/opencode/sqlitecore/sqlitecore_test.go`，白盒，复用既有 `newFixture/v2DDL/v2Payload/v2Spec`）：

- `TestMessagesReadsARowWhoseSessionIDIsNull`（`sqlitecore_test.go:374`）——`('a',NULL,'assistant',1000,payload)` 与 `('b','s1','assistant',2000,payload)`，要求两行都交付、`rows[0].SessionID == ""`、`rows[1].SessionID == "s1"`、水位线 `{2000,b}`。旧代码只交付 `[b]`。
- `TestMessagesHoldsTheWatermarkWhenARowCannotBeScanned`（`sqlitecore_test.go:418`）——四条断言钉住整条链路：①畸形 `time_updated` 的那次 pass 返回调用时水位线 `{0,""}`（而不是 `{2000,later}`）；②该 pass 里畸形行**之前**的可读行照常交付；③永久畸形的代价是**进度停滞而非丢行**：在畸形行后追加可读行 `('behind',2500)` 再跑一次，仍交付 `[ok later behind]` 且水位线保持不动；④`UPDATE session_message SET time_updated = 3000 WHERE id='bad'` 修复该值后，用被保持的水位线再跑一次，从区间头部重读并交付 `[ok later behind bad]`、水位线 `{3000,bad}`——**重试即恢复**，已 visit 的行重复交付（`DedupKey` 幂等）。

RED 证据是对**最终版测试文件**重新取的：把 `Messages` 的循环临时还原成修复前的 `Scan + continue`，跑出下面的输出，再恢复修复版并用 `shasum -a 256 -c` 校验 `sqlitecore.go: OK`（所以 RED/GREEN 这对证据不依赖任何陈旧副本）。

```console
$ python3 - <<'PY'   # 临时把 Messages 的循环还原为修复前的 Scan+continue
...
REVERTED_TO_PREFIX_CODE
$ go test -count=1 -run 'TestMessagesReadsARowWhoseSessionIDIsNull|TestMessagesHoldsTheWatermarkWhenARowCannotBeScanned' ./internal/harness/opencode/sqlitecore/
--- FAIL: TestMessagesReadsARowWhoseSessionIDIsNull (0.02s)
    sqlitecore_test.go:383: the scan over a row with no session emitted [b], want [a b]
--- FAIL: TestMessagesHoldsTheWatermarkWhenARowCannotBeScanned (0.01s)
    sqlitecore_test.go:437: Messages returned {Updated:2000 ID:later} after a row failed to scan; want the caller's {Updated:0 ID:} so the next pass re-reads the range instead of resuming past a row it never read
    sqlitecore_test.go:451: a pass stalled by the malformed timestamp delivered [behind], want the readable rows [ok later behind]: the stall must cost progress, never rows
    sqlitecore_test.go:454: the stalled pass returned {Updated:2500 ID:behind}, want the held {Updated:2000 ID:later}: a row it never read is still in the range
    sqlitecore_test.go:461: the pass after the malformed timestamp was repaired emitted [behind bad], want [ok later behind bad]
FAIL
FAIL	github.com/zzstar101/mytoken/internal/harness/opencode/sqlitecore	0.419s
```

RED 输出要按缺陷指纹来读，尤其 `:461` 那行：**修复前**的"重试"只交付 `[behind bad]`——`ok`/`later` 这两行在上一轮只被**扫描**过、从未被持久化为已读，却从这次重试里消失了，而水位线声称 `{2000,later}`。这就是永久性：水位线一旦落盘，之后任何 pass 都拿不回那两行，账本永久少了这些行的 usage。

### 3 FIX（两处，都在 `Messages` 循环内；公开签名未变）

```go
	for rows.Next() {
		var (
			row Row
			// session_id is optional metadata the payload also states, and
			// SQLite happily stores NULL in the column. Scanning NULL into a
			// string fails, so the row would never be delivered; a NullString
			// accepts it and normalises to the empty string the callers already
			// treat as "ask the payload".
			session sql.NullString
		)
		if err := rows.Scan(&row.ID, &session, &row.Data, &row.Updated, &row.Kind); err != nil {
			return before
		}
		row.SessionID = session.String
		if row.ID == "" {
			continue
		}
		visit(row)
		...
	}
	if err := rows.Err(); err != nil {
		return before
	}
```

- `sqlitecore.go:420` 声明 `session sql.NullString`；`:422` 用它接收第 2 列；`:425` 归一 `row.SessionID = session.String`（NULL → `""`）。这不是新风格：本包的 `MaxUpdated` 早已用 `sql.NullInt64` 读 `COALESCE(MAX(time_updated),0)`（`sqlitecore.go:445-449`），NULL↔零值归一在本包属于既有做法。
- `sqlitecore.go:423` Scan 失败 → `return before`（调用前水位线）；`sqlitecore.go:435` `rows.Err()` 同规则。**不 continue、不推进**，与函数原有注释的契约一致。
- 为什么两个形态分开处理：`session_id` 是**可选**列，NULL 有明确定义（"没说"），且调用点有 payload 兜底链能接住；而 `id`/`data`/`time_updated`/`kind` 是**驱动整行**的必需列，读不出时行不可物化，任何形态的"猜"都会让水位线说谎。所以前者宽容读入、后者 fail-closed。
- 空 `id` 行仍跳过（不 fail）：keyset 排序键都没有的行无法被水位线命名，而 `Accumulator.Emit` 本就忽略空 message id 的行（`messageID == "" || p.Tokens == nil` 直接 return），跳过它不产生漏计。这条也写进了 `Messages` 的注释（`sqlitecore.go:391-393`）。

### 4 保守代价：永久畸形的 `time_updated` 让**该表**进度停滞，而不是静默漏计

这是本轮唯一新增、需要明确记录的代价，已写进 `Messages` 的注释（`sqlitecore.go:382-389`）并被测试第 ③ 条断言钉住：

- 只要该行的值永久不可读，水位线就停在它前面，每次 pass 都从同一位置重扫这一段——**进度停滞、整段重读**（`Reset` 会因 `MaxUpdated` 变大而清空水位线，于是变成全表重读），CPU/IO 有开销，但**行不丢**：所有可读行每轮重复交付，由 `DedupKey` 幂等合并。
- 这是**可检测的内部停滞**：水位线不动、数据库再次变化时该区间会重读；诊断若暴露 cursor 可定位它，但当前用户界面不保证直接提示。它仍优于把该行永久越过后悄悄少计。
- 解除条件很短：`time_updated` 是上游 `$onUpdate` 列，任何一次真实 `UPDATE` 都会写入新时间戳（第三轮 §5.2 的依据），修复值即解除；测试用 `UPDATE ... SET time_updated = 3000 WHERE id='bad'` 模拟这一步，证明下一次 pass 自动恢复。
- 若某行**永久**畸形且**永不**被重写，该表就停在它那里：这是有意的 fail-closed，而不是"继续推进并悄悄少计"。调用点在失败 pass 里不会被 `visit` 到畸形行之后的部分——因为整段会在下一次 pass 从同一水位线重来，不会跳过。

### 5 与第三轮 §5「调用点 `!ok` 跳过即推进」的关系（保留，不冲突）

两者作用于不同的层，判定标准也不同：

| | 失败位置 | 已知条件 | 结论 |
| --- | --- | --- | --- |
| 第三轮 §5（保留） | 调用点 `Decode(data) !ok` | 整行读出来了，只是内容"不是消息" | 跳过并推进是对的：它按定义不该计入 |
| 第四轮（本轮） | `sqlitecore.Messages` 内 `rows.Scan` / `rows.Err` | **连水位线列都没读出来**，行位置未知 | 必须 fail-closed：推进等于永久越过它 |

`rows.Scan` 失败不是"读出了内容但不是消息"，而是"驱动无法物化这一行"；旧代码把它与 `Decode` 的 `!ok` 同等对待（都 `continue`），正是本次漏计的根因。所以第三轮的调用点语义原样保留（`opencode.go:424`、`mimocode.go:391` 的注释仍然准确），第四轮只修 sqlitecore 里那把水位线。

### 6 第四轮验证

```console
$ gofmt -l internal/harness/opencode/sqlitecore/   # 无输出
$ go vet ./internal/harness/opencode/...           # 无输出
$ go test -count=1 ./internal/harness/opencode/... ./internal/harness/mimocode/...
ok  .../internal/harness/opencode            0.414s
ok  .../internal/harness/opencode/sqlitecore 0.357s
ok  .../internal/harness/mimocode            0.448s
$ go test -race -count=1 ./internal/harness/opencode/... ./internal/harness/mimocode/...
ok  .../internal/harness/opencode            1.842s
ok  .../internal/harness/opencode/sqlitecore 1.652s
ok  .../internal/harness/mimocode            1.911s
$ go test -count=1 ./...                           # 全仓库：无 FAIL / 构建失败行
```

两条新测试各覆盖一个形态；既有 sqlitecore 测试（`Reset` 三条、`MessagesResumesByUpdatedThenID`、`MessagesKeepsTheCallersWatermarkWhenTheScanIsInterrupted`、`AHalfMigratedTableDoesNotStrandTheRowItHeld`、`Decode` 四条）与 opencode/mimocode 全部 golden 原样通过，说明"宽容 NULL + fail-closed"没有改动任何既有契约——尤其 `TestMessagesKeepsTheCallersWatermarkWhenTheScanIsInterrupted`（`cancel()` 制造的中断）现在与 Scan 失败共享同一条返回路径，语义一致。

### 7 本轮仍存风险

1. **永久畸形的 `time_updated` 阻塞该表进度**（第 4 节）：有意取舍、可恢复但不是零代价；内部 cursor 可显示停滞，当前用户界面不保证直接告警。若上游真的写出永久畸形值，适配器保持未越过它的水位线，而不是永久少计。
2. **`Sessions()` 内的 Scan 失败仍是 `continue`（未改）**：`session`/`session_v2` 的读取没有水位线，每次 pass 都从表头重读，所以那里的跳过只影响本轮、下一轮自动重试，不具备 `Messages` 的永久性。属相邻风险，本轮刻意不动（收窄改动面）。
3. **`data` 列为 NULL 时同样无法扫进 `string`**：现在会 fail-closed 而非跳过。v1/v2 过滤都要求 `json_valid(data)`，NULL 过不了过滤，所以实际不可达；但若上游改了过滤条件，症状会是进度停滞而非漏计。
4. **keyset 无法回头读"事后以更早 `time_updated` 写入"的行**（与上一轮同）；`Reset` 用 `MaxUpdated` 兜底，但一行若以**更早**时间戳写进已读区间且水位线没变，仍不会被看到。上游 `$onUpdate` 让这类写入实践中都以新时间戳出现。
5. **无真实日志抽样**：两个触发形态的判定来自 `database/sql` 的类型规则与上游源码（`session_id` 可空、`time_updated` 在非 STRICT 表可存 TEXT），并已在合成库上复现；仍未对真实用户库做形态统计。

## 第五轮交叉引用：OpenCode 复审缺陷修复把 fail-closed 规则收窄了一条（同日，RED→GREEN→REFACTOR）

第四轮的规则是「`sqlitecore.Messages` 里 `rows.Scan` 失败一律 fail-closed」。同一天对 OpenCode 的最终只读复审在合成库上
复现了 `id` 为 NULL 的 `TEXT PRIMARY KEY` 行，这暴露了该规则的一个真实例外；完整过程、RED 原文与测试清单见
[`docs/verification/opencode-sqlitecore.md`](opencode-sqlitecore.md)。本节只做两条文档之间的对齐说明，mimocode 本身未改。

| | 第四轮 `time_updated` TEXT | 第五轮 `id` 为 NULL |
| --- | --- | --- |
| Scan 结果 | 失败（`string` 装不下 TEXT 值） | **失败**（`string` 装不下 NULL）→ 现改为 `sql.NullString` **成功读入** |
| 行的可用性 | 未知：连水位线列都没读出来 | 已知：该行**可以**定位，只是没有稳定键 |
| 后果 | 推进 = 永久越过一行，属漏计 | 不推进 = 整张表永久停滞，属完全不可用 |
| 结论 | fail-closed：返回调用者水位线，等待上游重写 | **跳过并推进**：`row.ID == ""` 不入账（`Accumulator.Emit` 本来也会 return），水位线照常前移 |

所以第四轮的第 1 条风险在第五轮被**有意缩小**：`id` 从「Scan 失败」变成「成功读出但不可物化」。
边界仍然清晰——`internal/harness/opencode/sqlitecore/sqlitecore.go:423` 只把 `id` 换成 `sql.NullString`，
`time_updated`/`data`/`kind` 的 Scan 错误仍在同一处 `return before`，第四轮的
`TestMessagesHoldsTheWatermarkWhenARowCannotBeScanned` 与 `TestMessagesKeepsTheCallersWatermarkWhenTheScanIsInterrupted`
原样通过（`docs/verification/opencode-sqlitecore.md` §4）。mimocode 与 opencode 共享这个深模块，因此本轮同时重跑了
`./internal/harness/mimocode/...`（含 `-race`）：20 条测试全过，无契约漂移。

第五轮另有两处只影响「不写坏数据」的收紧，同样不改合法输入的语义：`Num.UnmarshalJSON` 不再把
1e30/Inf/超 int64 饱和成 `MaxInt64`（否则两条事件的 `SUM(input)` 会 `SQL logic error: integer overflow`），
`EpochMS` 不再把畸形时间解释成公元数亿年（超出可信窗口即 zero，调用点回落到行自身的 `time_updated`）。
细节、RED 原文与 5 条残留风险见 [`docs/verification/opencode-sqlitecore.md`](opencode-sqlitecore.md)。

