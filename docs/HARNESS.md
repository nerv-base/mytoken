# Harness 接入与一致性规范

本文说明 MyToken 如何把一个代码 agent 的本地日志接进来，以及接入后必须满足的
一致性要求。所有 harness 都必须通过 `internal/harness/harnesstest` 套件。

相关文档：`docs/SPEC.md` §4（用量归一化）、`docs/CLI.md`（命令用法）。

## 1. 契约：Parser 接口

每个 harness 是 `internal/harness` 下的一个包，实现 `harness.Parser`
（`internal/harness/harness.go:41-51`）：

| 方法 | 责任 |
| --- | --- |
| `Harness() model.Harness` | 返回 harness id（与其它包、数据库里的值一致，且稳定不变） |
| `Roots() []string` | 返回要监听的目录，环境变量与 `~` 已展开；目录不存在是允许的，忽略即可。只返回**真正存放日志的最窄目录**（如 `~/.claude/projects` 而不是 `~/.claude`、`~/.codex/sessions` 而不是 `~/.codex`）：扫描器会为每个根及其下发现日志的祖先目录各加一个 fsnotify watch，根越宽，监听面越大 |
| `Discover(ctx) ([]Source, error)` | 枚举日志文件/数据库，`Source{Path, Kind}`，`Kind` ∈ `jsonl` / `json` / `jsonl.zstd` / `sqlite`。根目录不存在必须返回 `0` 个源 + `nil` 错误 |
| `Parse(ctx, src, cur) (Batch, error)` | 从游标 `cur` 增量读到文件末尾，返回 `Event`、`Session` 与新的 `Next` 游标 |

配套类型：

- `Source{Path, Kind}`：一个被发现的日志文件或数据库。
- `Cursor{Offset, Size, ModTime, Fingerprint, Extra}`（`internal/harness/harness.go:24-30`）：
  增量读取状态，由 store 按 `(harness, path)` 持久化（`internal/store/store.go:311`）。
  `Offset` 必须是「最后一条完整记录之后的字节位置」；`Fingerprint` 是文件前 4KB 的
  sha1，用来发现「被重写但没变长」的文件；`Extra` 放解析器私有状态（例如已发出的
  记录条数、sqlite 的 rowid）。
- `Batch{Events, Sessions, Next}`：一次 Parse 的产出。
- `Register(p)` 在包的 `init()` 里调用；`internal/app/app.go:7-16` 用空导入把包链进来。
  `harness.All()` / `Get(id)` 供扫描器使用。
- `Rooted`（别名 `Rooter`，`internal/harness/detect.go:14-16`）：
  `WithRoots(roots ...string) Parser`，返回一个只看这些目录的新实例，供设置页的手动目录
  覆盖自动探测根。每个 parser 都必须实现；`harness.NewRooted(id, roots...)` 按 id 取实例。
  实现要复制切片，不能改动原实例的根。

### 1.2 检测与目录校验

设置页对每个已编译进来的 harness 显示「已检测 / 未检测 / 已禁用 / 目录无效」，
由 `internal/harness/detect.go` 的纯函数支撑，全部只做 `stat` 和浅层 `ReadDir`，
不会遍历日志树：

| 函数 | 语义 |
| --- | --- |
| `Detect(p Parser) Status` | `Status{Harness, Detected, Roots, Problem}`。某个根存在、是目录且含非隐藏条目即视为已检测；`Problem` 只在未检测时给出英文原因（未配置 / 不存在 / 为空 / 不可读）。 |
| `ValidateRoot(p Parser, dir string) error` | 用户手选的目录是否是该 harness 的数据目录。用 `p.WithRoots(dir).Discover` 实际枚举；失败错误包裹 `ErrRootMissing`、`ErrRootNotDir`、`ErrRootNoData`（`errors.Is` 可区分），信息为英文。`~/.claude` 这类父目录会被拒绝，除非其中真有该 parser 能发现的源。 |

### 1.1 支持列表

「子代理」一列表示该工具本身有子代理/侧链概念、且解析器把它映射成
`Session.ParentSessionID`（golden 里能看到 `parentId`）。工具没有这个概念时留空
即可，不要求夹具硬造；有概念就必须覆盖：夹具里要有一个带父会话的子代理日志。

| harness | 子代理 | 备注 |
| --- | --- | --- |
| `amp` | 否 | 整文件重写的 thread JSON；credits 记为 `Bill`；未在真实数据上验证 |
| `antigravity` | 否 | 手写 protobuf 解码的 cascade `*.db`（`.pb` 不支持）；未在真实数据上验证 |
| `claude-code` | 是 | 追加式 JSONL；`isSidechain` / `agentId` / `subagents/` 目录 |
| `claude-desktop` | 是 | usage ledger + transcript。Code-tab 的 ledger 行在 `cliSessionId` 的 claude-code transcript 存在时丢弃（claude-code 已计）；Cowork 会话有 transcript 时以 transcript 为准、丢弃该会话的 turn 聚合行，无链接/无 transcript 时保留 ledger |
| `cline` | 是 | 原地重写的任务 JSON（经 `internal/harness/clinetask`） |
| `codebuddy` | 是 | 追加式 JSONL |
| `codebuff` | 否 | chat JSON；credits 记为 `Bill`；未在真实数据上验证 |
| `codex` | 是 | `thread_spawn.parent_thread_id` / `parent_thread_id` / `forked_from_id` |
| `crush` | 是 | sqlite；会话可互相引用 |
| `droid` | 否 | 工具无子代理概念 |
| `dsh` | 是 | JSONL（子代理侧可能是 `.jsonl.zstd`） |
| `forge` | 否 | sqlite；没有 cache write / reasoning / 父会话 / 请求 id 信号 |
| `gemini` | 是 | JSON / JSONL / zstd 三种源 |
| `goose` | 否 | 工具无子代理概念 |
| `grok` | 否 | 工具无子代理概念 |
| `hermes` | 是 | sqlite |
| `kilo` | 是 | 原地重写的任务 JSON（经 `clinetask`） |
| `kimi` | 是 | 旧 Kimi 与 Kimi Code 追加式 JSONL；新协议只计 `usage.record`，避开父/子 wire 汇总双计 |
| `kiro` | 否 | 五种布局；token 按字符/4 估算，无缓存信号；CLI `Clear` → `BoundaryClear`；未在真实数据上验证 |
| `mimo-code` | 是 | query-only SQLite；解析 assistant message usage，共享 OpenCode SQLite 核心但使用独立身份与去重命名空间 |
| `minimax-code` | 否 | query-only SQLite `local_runtime_token_usage`；兼容 `.minimax` / `.mavis` |
| `mux` | 是 | 追加式 JSONL；子代理 transcript 的父会话是所在 workspace；未在真实数据上验证 |
| `openclaude` | 是 | 追加式 JSONL |
| `openclaw` | 否 | 解析器不产出 `ParentID`（`internal/harness/openclaw/openclaw.go:45`） |
| `opencode` | 是 | SQLite；`parentID` 指向父会话；OMO plugin 模式复用此身份 |
| `pi` | 是 | JSONL；run-log 记录父会话；OMP 与 OMO/senpi 兼容路径复用此身份 |
| `qwen` | 否 | 工具无子代理概念 |
| `roo` | 是 | 原地重写的任务 JSON（经 `clinetask`） |
| `step-code` | 是 | Pi-compatible JSONL，但保持独立 root、身份与去重命名空间 |
| `workbuddy` | 是 | 追加式 JSONL |
| `zcode` | 否 | query-only SQLite；只计完成行，按 schema 探测可选 cache 列 |
| `zed` | 是 | sqlite `threads.db`；`parent_id`；累计用量与逐请求之差补一条余量事件；未在真实数据上验证 |

上面 32 个 harness 都已注册进 `internal/app/app.go` 的空导入，`mytoken doctor`
会列出它们。接入一个新 harness 的最后一步就是那次空导入：漏了它
`harness.All()` 里就没有这个 harness，`doctor`、`scan`、`stats` 都看不见它，
而单包测试仍然是绿的——最容易漏的一步。

## 2. 新增一个 harness

1. 建包 `internal/harness/<name>/`，包内定义记录结构体、`New()` / `NewWithRoots(roots ...string)`
   （`New()` 用默认根，测试用 `NewWithRoots` 指向临时目录）。
2. 默认根用 `harness.EnvOr("<ENV>", "相对路径", ...)`：环境变量优先，否则
   `$HOME/相对路径`。多根时全部返回；只返回日志所在的最窄目录（见 §1 的 `Roots`）。
3. 实现 `WithRoots`（`Rooted`），返回使用给定根的副本，Settings 的手动目录依赖它。
4. `Discover` 只匹配该 harness 真正使用的文件名/后缀，跳过目录、临时文件、非日志文件；
   根不存在返回空切片而不是错误。`ValidateRoot` 直接复用它判断用户目录。
5. `Parse` 按 §3 的游标规则增量读取；每条记录产出一个 `model.UsageEvent`
   （`DedupKey` 必须稳定且全局唯一，见 §4）。
6. 在 `internal/model/model.go` 的 `Harness` 常量里加 id 并在 `DisplayName` 里加名字
   （`Known()` 靠两者不同）；`Detect`/设置页不会显示没有名字的 harness。
7. 在包 `init()` 里 `harness.Register(New())`，并在 `internal/app/app.go` 加空导入。
8. 加 `conformance_test.go` 与 `testdata/conformance/<name>/` fixture，跑通 §6 的套件；
   `go test ./internal/harness` 会检查每个注册 parser 都有 `WithRoots` 和 DisplayName。
9. 重新生成 golden：`go test ./internal/harness/<name> -run Conformance -update`。

参考实现：`internal/harness/claude/claude.go`（追加式 JSONL）、
`internal/harness/crush/crush.go`（sqlite + 累计计数差分）、
`internal/harness/cline/cline.go`（原地重写的 JSON，经 `internal/harness/clinetask` 复用）。

## 3. 游标与增量语义

- `Parse` 只处理**完整记录**：末尾不完整的行不要消费，让 `Offset` 停在它之前，
  下次扫描再读。截断的尾行是常态（agent 正在写日志）。
- 不能增量读的格式（整文件 JSON、zstd、被原地重写的 JSON）可以每次整文件重解析，
  但必须：靠 `DedupKey` 去重，且返回准确的 `Next`（例如 `Fingerprint` 变化时
  `Extra` 里的「已发出条数」计数器决定哪些是新记录）。
- sqlite：游标放 `Extra`（rowid / 自增序号），并在事务边界读取；只读打开，
  不要写用户的数据库。
- 回到旧版本或一致性校验发现文件被重写时，从 0 重读并依靠 `DedupKey` 去重，
  绝不能发出两条相同 `DedupKey` 的记录。
- 累计计数器（如 codex 的 `total_token_usage`）要差分出单次用量；计数器回退
  （压缩/重写会话）时下调基线，而不是产出负值。
- 空产出是正常结果：文件没有新记录时返回 `Batch{Next: cur}`（`Next == cur`），
  扫描器据此跳过（`internal/scan/scanner.go:199`）。

## 4. 事件与会话的归一化

- `model.UsageEvent`：`Harness`、`DedupKey`、`SessionID`、`ParentID`、`ProjectPath`、
  `Timestamp`（**UTC**）、`Model`、`Provider`、`BaseURL`、`Tokens`、可选 `CostUSD`。
- `DedupKey` 的构造必须在同一日志被重复解析、被追加、被重写时都稳定：优先用日志里
  的请求/消息 id（claude 的 `message.id`+`requestId`、dsh 的 `compactionId`、
  clinetask 的 api_req_started 序号），否则用 `session + 序号`。不要用行号或时间戳。
- token 归一化见 `docs/SPEC.md` §4：claude 的 `usage.input_tokens` 已排除缓存命中，
  `cache_creation_input_tokens` → `CacheWrite`，`cache_read_input_tokens` → `CacheRead`；
  `output_tokens` 已包含 reasoning，不要再加。
- Anthropic 格式的 `usage.cache_creation.ephemeral_1h_input_tokens` 是 1h TTL 档，
  是 `CacheWrite` 的**子集**：`CacheWrite` 仍取 `cache_creation_input_tokens` 全量，
  `CacheWrite1h` 只放 1h 部分（5m 档是 `CacheWrite - CacheWrite1h`）。`Tokens.Add/Total`
  不会把 1h 再加一遍。同一请求被多行流式记录时，merge 必须一并取两者的最大值。
  只有真正带嵌套 `cache_creation` 的解析器能拆这一层；平铺字段的日志（zed/kimi/
  codebuff/workbuddy 等）没有 1h 信号，`CacheWrite1h` 留空。
- `model.SessionMeta`：`SessionID`、可选 `ParentID`（子 agent/侧链）、`Title`、
  `Project`、`StartedAt`、`UpdatedAt`。**`Title` 只能来自会话的第一条用户消息**，
  用 `harness.Title()` 归一化（空白折叠、最多 60 个 rune）。增量解析时只报告本批次
  知道的信息即可：store 的 upsert 会保留非空字段、`started_at` 取最小值、
  `updated_at` 取最大值（`internal/store/store.go:113`）。
- 有可信金额字段就用它，否则把 `CostUSD` 置为 `nil` 交给价格表估算。`CostUSD` 的三种状态在
  model 层是固定的：`nil` = 未知（走估价），非 nil 的 `0` = 日志/上游明确报告的零成本，
  非 nil 正数 = 报告金额（`internal/scan/scanner.go:489-493`；`docs/SPEC.md:217` 的
  log/estimate/unpriced 分类同样以 `log_cost != NULL`（含 0）为 log）。但**日志里的 0 是不是
  权威零，只能按上游 schema 各自解释，不能统一**：MiniMax Code 的官方 writer 只在 provider
  报了零价时才写 `cost_usd=0`（缺失写作 NULL），所以 0 是可信零
  （`internal/harness/minimax/minimax.go:504-518`）；OpenCode/MiMo Code 的 MessageV2 `cost` 里
  0 是「未知」哨兵，parser 把 0、负数与非有限数一并映射成 `nil` 再交给价格表
  （`sqlitecore.ReportedCost`，`internal/harness/opencode/sqlitecore/sqlitecore.go:636`）。
  缺失（NULL/没有该字段）、负数、非有限数在两个方向上都不是零成本，不得悄悄写成零。

### 4.1 压缩/清空边界（`model.Boundary`）

`model.Boundary`（`internal/model/model.go:131`）只在该 harness 的日志**明确记录**了上下文
替换时设置：`BoundaryCompact` 打在压缩后**第一条**请求上，`BoundaryClear`/`BoundaryResume`
同理。没有标记就留空——不要靠 token 骤降之类启发式猜测（判定优先序见 `docs/CACHE.md` §3）。

- 已上报边界：claude、codex、dsh、claudedesktop、grok、pi、openclaude、workbuddy。
- **pi**：`type=="compaction"` 的记录**既是压缩请求本身（其 usage 与日志成本照常计数），
  又是边界标记**：压缩行自己不带 `Boundary`，其后第一条带 usage 的记录打 `BoundaryCompact`。
  压缩记录不带 model/provider（日志里没有），所以沿用同一会话里**相邻请求**的值——优先前一条，
  前面没有请求时取后一条，都没有才用 `"unknown"`；`usage.cost` 是对象而 async 转录的 cost 是
  数字，`piAsyncUsage.UnmarshalJSON` 两种形状都接受（`pi.go` 里解码失败后的信封回退分支仍保留，
  作为没见过形状的安全网：至少把边界读出来，而不是整行丢弃）。
- **opencode**：v2 `session_message.type='compaction'` 的行**既是压缩请求本身（其 usage 照常
  计数），又是边界标记**。压缩行自己不带 `Boundary`（否则边界会落在压缩调用上），紧随其后
  第一条 timestamp ≥ 压缩行的请求打 `BoundaryCompact`。标记会随游标持久化
  （`dbState.Compact`），因为压缩行和它之后的请求常常落在两个增量批次里。
- 已核查但**没有可用标记**的 harness：crush（`messages.is_summary_message` 有标志，但用量
  来自 session 的累计计数器、没有每消息时间戳，挂不上去）、gemini（`summary` 是会话级
  元数据，仅用作标题）、openclaw/hermes（源码注释已说明不记录父会话与请求 id）、
  kimi/cline/roo/kilo/qwen/goose/droid/forge/codebuddy（日志格式与 codeburn 的对应
  provider 实现里都没有压缩标记）。这些包的事件 `Boundary` 保持为空是**正确结果**，
  不是遗漏；将来若上游加了标记，按本节的形状补 fixture。

## 5. 隐私边界

MyToken 只统计用量，不保存对话：

- **允许**进入数据库的：token 计数、模型/provider 名、时间戳、会话 id 与父子关系、
  项目路径、**截断到 60 rune 的首条用户消息标题**。
- **禁止**：用户消息正文、助手回复、工具调用参数与输出、文件内容、系统提示、
  推理文本、API key 等任何凭据。解析时读到它们就丢，不要写进任何字段。
- 扫描器只读日志，不修改用户目录；游标写在自己的数据库里。
- 一致性套件的 `privacy` 检查要求 fixture 里真的含 `harnesstest.SentinelText`
  （`SENTINEL_PRIVATE_TEXT`），且解析结果和 golden 文件里都找不到它。把 sentinel
  放在助手回复/工具输出/推理文本里，**不要**放在首条用户消息里——那会（正确地）
  变成标题并触发失败。
- 报告隐私问题时用「哪个 harness + 哪个字段」，不要贴日志原文。

## 6. 一致性套件

每个 harness 包一个 `conformance_test.go`：

```go
func TestConformance(t *testing.T) {
	harnesstest.Run(t, harnesstest.Case{
		Name:    "claude-code",
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: "testdata/conformance/claude",
	})
}

func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, harnesstest.Case{
		Name: "claude-code", New: func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: "testdata/conformance/claude",
	})
}
```

`harnesstest.Case`：

| 字段 | 含义 |
| --- | --- |
| `Name` | harness id，用于子测试与报错信息 |
| `New` | 用给定根目录构造 parser |
| `Fixture` | fixture 目录，如 `testdata/conformance/claude` |
| `Golden` | golden 文件，默认 `<Fixture>/golden.json` |
| `Steps` | 增量增长的步数，默认 3 |
| `Grow` | 自定义增长函数；追加式日志留空（套件按整行前缀增长），sqlite / 原地重写 JSON 必须提供 |
| `Rewrite` | 解析后、比较与写 golden 前对快照做归一化；输出合法依赖 fixture 绝对路径时用（如 crush 从 db 位置推导 project），把该路径换成 `harnesstest.RootPlaceholder`（`<ROOT>`） |
| `Skip` | 跳过某检查（最后手段，必须写注释说明原因） |

检查项（`harnesstest.Checks`）：

| 检查 | 要求 |
| --- | --- |
| `golden` | Discover+Parse 的输出与 `golden.json` 逐字节一致（`-update` 重新生成）；fixture 必须 discover 到至少一个源、解析出至少一个事件，否则直接失败（防止目录层级写错导致「空 golden 全绿」） |
| `incremental` | 分步增长、每步带上次游标解析，合并结果与「一次性完整解析」完全一致 |
| `cursor-stable` | 文件没变时再次解析产出 0 个事件，且 `Next` 与传入游标相等 |
| `idempotent` | 从零游标解析两次得到相同 `DedupKey` 集合，且单次解析内无重复键 |
| `privacy` | fixture 含 sentinel，但解析结果与 golden 里都不含它 |
| `discover-missing` | 根目录不存在时返回 0 个源且无错误 |

约定：

- fixture 要小（每个日志文件几条记录），但必须覆盖：两个会话（格式支持则含一个
  子 agent/父子关系）、两个模型、cache read/write 与 reasoning token、至少一条
  没有 usage 的记录、标题来自首条用户消息、固定 UTC 时间戳。
- `Case.New(root)` 传进去的 `root` 必须是该 parser 的 `Discover` 直接读取的那一层：
  例如 claude 的 root 就是 `projects` 目录本身，fixture 下直接放
  `<project-slug>/<session>.jsonl` 与 `<project-slug>/<session>/subagents/*.jsonl`，
  不要再套一层 `projects/`。`-update` 之后打开 `golden.json`，`sources` 必须是
  真实存在的相对路径。
- sqlite / 原地重写 JSON 的 `Grow` 用 `seed/` 目录放种子（sql 脚本、条目列表）；
  套件不会把 `seed/` 复制进被解析的根目录，`golden.json` 也不会。
- 如果 parser 的输出合法地依赖 fixture 的**绝对路径**（例如 crush 从 db 文件位置推导
  project，而 sqlite 必须走 `Grow`），用 `Case.Rewrite` 把 root 前缀替换成
  `harnesstest.RootPlaceholder`：baseline 与各个增量步骤用的是不同的临时目录，不归一化
  的话 `golden` 无法提交、`incremental` 必然失败。归一化只针对路径字段，不要用它
  掩盖真实差异。
- 不能增量增长的记录（例如每次全量重写的 JSON）用自定义 `Grow` 让每一步都写出
  「更大」的文件，从而真正走到 `Fingerprint` 变化后的重读路径。

运行：

```sh
go test ./internal/harness/<name> -run Conformance -update   # 重新生成 golden
go test ./internal/harness/<name> -run Conformance -count=1  # 校验
go test ./internal/harness/... -count=1                      # 全部 harness
go test ./internal/harness/<name> -run '^$' -bench Conformance -benchtime 200x
```

## 7. 第三方代码归属

- 从其它项目移植/改写代码（例如从 codeburn 移植的解析逻辑）必须：在源文件顶部注释
  写明来源项目与许可证，并把版权与许可证全文登记到仓库根目录的
  `THIRD_PARTY_NOTICES`。**MIT 等许可证要求保留原始版权声明与许可文本**，
  只写「参考了 X」不够。
- 新增移植代码时同步更新 `THIRD_PARTY_NOTICES`，同一个上游项目只登记一次。
- 只读别人的日志格式不算移植；照抄/改写其解析代码算。

## 8. 常见陷阱

- 用行号或字节偏移当 `DedupKey`：文件被重写/压缩后会重复计数。
- 消费了末尾不完整的行：下次解析丢记录或报 JSON 错误。
- 所有字段都从增量批次重建 `SessionMeta`：会把早先解析到的标题/开始时间抹掉
  （store 用「非空覆盖 + min/max」合并，套件也按这个语义比较）。
- 把累计 token 当单次用量：会话总量会被重复累加。
- 忘记 `.zstd` / `.jsonl.zstd`：dsh 显式识别 `session.v3.jsonl.zstd`，不认就会漏日志。
  `Kind` 只描述 parser 实际会发现的格式：目前只有 dsh 写 zstd；gemini-cli 上游只写
  `.jsonl`（旧版 `.json`），不压缩，gemini parser 只认这两种是对的。
- 包内已有「本地 testdata 优先」的测试 helper（形如 `fixture(t, parts...)`，用
  `exists("testdata/"+parts)` 判断）时，新增 `testdata/conformance/` 会让 `testdata`
  变成目录，从而劫持无参调用、让既有测试读到空目录（crush 就踩过）。判断要改成
  「同名**文件**优先」：`st, err := os.Stat(p); return err == nil && !st.IsDir()`。
- 根目录/文件不存在时返回错误：扫描器会把正常情况记成故障。
- 把正文塞进某个字段（包括 `Project`、`Title`）：触发隐私边界，套件隐私检查会失败。

## 9. 验收清单

- [ ] `gofmt -l .` 为空
- [ ] `go vet ./internal/harness/...`
- [ ] `go test ./internal/harness/... -count=1` 全绿（6 项检查 × 每个 harness）
- [ ] `TestConformance` 与 `BenchmarkConformance` 都在
- [ ] 新 fixture 已提交，`sentinel` 不在首条用户消息里
- [ ] 从第三方移植的代码已登记 `THIRD_PARTY_NOTICES`
