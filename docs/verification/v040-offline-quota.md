# 0.4.0 Codex 离线额度读取（offline quota reader）验证

日期：2026-10-13（同日第二轮：来源归因复评与修复）。环境：Go 1.27.1、darwin/arm64。写入范围：`internal/quota/**`（新包；第二轮只改包内文件并新增 `internal/quota/provenance_test.go`）、本文件。未改 `internal/harness/**`、`internal/model/**`、`internal/app/**`、`internal/store/**`、`internal/query/**`、`internal/gui/**`、`internal/cli/**`、`internal/scan/**`、`internal/budget/**`、`task_plan.md`、`docs/ROADMAP.md`、`docs/SPEC.md`、`go.mod`/`go.sum`，未新增依赖，未提交 git。

## 结论与范围

交付一个**完全离线、只读、无副作用**的 Codex 额度解析器新包 `internal/quota`：输入是调用方打开的 `io.Reader`，输出是一份带来源（provenance）与显式原因的 `*quota.Report`。

**这是额度功能的一个前置件，不是完整额度功能。** 本轮只交付"从字节流里如实读出 Codex 日志中已有的 `rate_limits` 观察值"，以及一个纯函数的消耗速度/预测 helper。未接线任何调用方，未落地存储/查询/GUI/CLI/扫描器集成，未做 Claude 侧（Keychain + OAuth 用量接口），未做缓存与刷新策略。

**第二轮（来源归因修复）**：复评发现旧实现的选择器会把流内**任意**记录的 `limit_id`/`limit_name`/`plan_type`/`credits` 拼到被选中窗口上（carry），于是"一条旧的有效窗口"会被贴上"另一条新记录的 limit 标签"并被当成当前值；同时纯函数 `EstimatePace` 不看额度身份，会把两个不同 limit 的观察拿来算速率；`readErr` 还会把读取器自身的文本直接带回错误。修复内容：删除 carry、`Report` 只描述被选中记录、新增 `OtherLimitIDs`；新增"最新带窗口记录决定当前值"的 `stale`/`newer_invalid`/`SupersededByLine` 语义；`Sample` 增加 `LimitID` 与 `limit_mismatch`；`readErr` 改为 `ReadError`（固定消息 + `Unwrap`）。复评测试中凡旧 API 能表达的断言在修复前**真实失败**（6 个测试、7 条断言，见"红 3"）；收紧到新 API 的断言在旧实现上无法编译，因此不冒充独立的红证据。

包内共 4 个源文件、4 个测试文件：

| 文件 | 行数 | 内容 |
| --- | --- | --- |
| `internal/quota/doc.go` | 43 | 包文档：六条报告原则 |
| `internal/quota/quota.go` | 322 | 类型、常量、`Report` 的 `Window`/`Available`/`Sample` 方法 |
| `internal/quota/codex.go` | 783 | `ReadCodex` 有界读取 + 逐字段宽松解码 + 观察值选择 + `ReadError` |
| `internal/quota/pace.go` | 221 | `Sample`/`PaceOptions`/`Pace` 与纯函数 `EstimatePace` |
| `internal/quota/codex_test.go` | 528 | 24 个读取器测试 |
| `internal/quota/pace_test.go` | 254 | 17 个速度/预测测试 |
| `internal/quota/privacy_test.go` | 134 | 4 个泄漏与离线不变量测试 |
| `internal/quota/provenance_test.go` | 371 | 12 个来源（provenance）与归因测试（含 2 个子测试） |

六条报告原则（逐字写在 `internal/quota/doc.go`）：缺失不等于 0；不从其它字段推断额度；每条观察都带来源，且 `Report` 只描述**被选中的那一条**记录、绝不混入其它记录的标签；**最新的带窗口记录决定"当前值"**，更新的记录不可用时旧值只能作为历史（`stale` + `newer_invalid`）；不支持的形状显式报原因；原始日志内容永不外泄（读取器自身的错误文本只经 `Unwrap` 可达，不进入 `Error()` 消息）。

## 输入形状与来源

形状来自仓库内**已有代码与合成样本**，未读取任何真实用户日志：

- `internal/harness/codex/codex.go`（既有解析器）的宽松解码约定：外层 `type`、`payload.type`、`timestamp`，以及"字段在版本间会改变 JSON 类型，所以逐字段兜底解码"。
- `internal/harness/codex/testdata/signals.jsonl:6`：`"rate_limits":{"credits":{"balance":"100","has_credits":true}}`。
- `testdata/codex/sessions/2026/01/02/rollout-2026-01-02T03-04-05-019aaa00-1111-7222-8333-444455556666.jsonl:6`：`"rate_limits":{"primary":{"used_percent":1.0}}`，同一行还有 `info.model_context_window:258400` 与带毫秒的时间戳 `2026-01-02T03:04:06.004Z`。
- `docs/SPEC.md:141`：`rate_limits.credits.balance` 是余额，不得当作"每次请求的费用"。
- `docs/ROADMAP.md:205`：0.4.0 额度窗口要求"Codex 直接读日志里的 `rate_limits`（离线）"，并列出"额度消耗速度、预计耗尽时间、均匀用完参考线"。

本包识别的键：`rate_limits.{primary,secondary}`、`plan_type`、`limit_id`、`limit_name`、`credits`；窗口内 `used_percent`、`remaining_percent`、`window_minutes`、`resets_in_seconds`、`resets_at`；`credits` 内 `has_credits`、`unlimited`、`balance`。

需要说明的是：仓库现有合成样本里**只出现过** `primary.used_percent` 与 `credits.{balance,has_credits}`。`secondary`、`plan_type`、`remaining_percent`、`window_minutes`、`resets_in_seconds`、`resets_at`、`limit_id/limit_name`、`credits.unlimited` 都是按"**若出现则解析**"实现的，测试用的是**据此构造的合成 fixture**，不构成对真实日志字段是否存在的验证。这一点在文末"未关闭项"重述。

## 选定 API 与理由

```go
// internal/quota/codex.go
type CodexOptions struct {
	Source       string        // 来源标签（通常是日志路径），会原样回填到 Report.Source
	MaxLineBytes int           // 单行上限；<=0 用 DefaultMaxLineBytes (1<<20)
	MaxBytes     int64         // 总输入上限；<=0 用 DefaultMaxBytes (1<<30)
	Now          time.Time     // 陈旧判定的参考时刻；零值用 time.Now
	StaleAfter   time.Duration // >0 时把过期观察标为 stale
}

func ReadCodex(ctx context.Context, r io.Reader, opts CodexOptions) (*Report, error)

// internal/quota/pace.go
func EstimatePace(prev, next Sample, opts PaceOptions) Pace
```

`Report` 携带：`Source`、`ObservedAt`、`Line`（被选中记录的行号）、`RecordType`、`LimitID`/`LimitName`（**只**来自被选中记录）、`OtherLimitIDs []string`（流中其它记录声明过的 limit ID，排序去重后最多 8 个，绝不并入 `LimitID`）、`PlanType`、`Credits`、`Primary`/`Secondary`、`Status`/`Reason`、`SupersededByLine`（更新的带窗口记录把本观察降级为历史时的行号，否则为 0）、`Issues []Issue`、`UnknownFields`、`LinesRead`、`BytesRead`、`Truncated`。便利方法：`Window(kind) *Window`、`Available() bool`（`ok` 或 `stale` 为 true）、`Sample(kind) (Sample, bool)`（仅在窗口 `ok` 且 `used_percent` 存在时为 true；`Sample` 会带上 `Report.LimitID` 作为身份，供 `EstimatePace` 判断两次观察是否同一额度）。

| 决策 | 选择 | 理由 |
| --- | --- | --- |
| 入口形态 | `io.Reader` 而非路径 | 打开文件是调用方（扫描器）的事，本包因此**不需要** `os`/`net/http`，隔离性可被静态测试证明 |
| 观察值选择 | 三层 tier + 时间戳/行号 tie-break | 有可用窗口的记录永远压过只有 plan/credits 的记录；同级内"有可信时间戳 > 无"，再比时间新，最后比行号大。选择器只回答"哪条记录是被选中的观察"，不回答"它是不是当前值" |
| 标签归因 | 只取被选中记录自己的 `limit_id`/`limit_name`/`plan_type`/`credits`；其它记录的身份另列 `OtherLimitIDs` | 旧的 carry 实现会把流内**任意**一行看到的标签拼到另一行的窗口上，导致"旧窗口被贴上另一个 limit 的标签、并被当成那个 limit 的当前值"这种错归因。`Report` 是单记录报告，多身份事实只能另行表达 |
| 当前值判定 | 最新的带窗口记录（append 顺序）决定 currency | rollout 是追加写，记录顺序即知识顺序。最新那条带窗口记录不可用时（形状坏、或没有 usage），更旧但可用的观察只作为**历史**：`stale` + `newer_invalid` + `SupersededByLine`，并保留它自己的行号、来源与精确时刻；绝不静默当成当前值 |
| 跨额度观察 | `Sample` 带 `LimitID`，身份不同就不比较 | 同一个 `WindowKind` 不等于同一个额度桶；两次观察的身份只要一侧已知且不相等就报 `limit_mismatch`，"一侧未知"同样拒绝——未知不是匹配证明 |
| 读取器错误 | `ReadError{Cause}`，`Error()` 返回固定文本 | 读取器错误是任意调用方文本，可能内嵌日志内容、路径或凭据，本包无法清洗。因此消息由本包生成，原始文本只经 `Unwrap()`/`Cause` 刻意取用；调用方若把它展示给用户，就由调用方负责该文本 |
| 缺失语义 | 指针 + `Has*` | `used_percent: 0` 是真实观测，不能被当成"没写"；缺失一律 `nil`，绝不填 0 |
| `balance` | 保留原始字符串 | 单位为日志所未定义，不做数值转换、不做除法 |
| 未知字段 | 只在本地按字段名去重计数（`UnknownFields`），字段名**不进入 `Report`** | 容忍未来字段，同时不让任意日志文本进入 `Report` |
| 自由文本标签 | `plan_type`/`limit_id`/`limit_name`/`balance` 经 `sanitize` 截断到 64 字节（按 rune 边界、去控制字符） | 日志是外部输入，不能让它撑大或污染 `Report` |
| 时间戳可信区间 | `[2000-01-01, 2100-01-01)` UTC | 区间外的数值报 `out_of_range`，**不**猜它是不是毫秒 |
| 取消 | 返回 `ctx.Err()` 错误 | 被取消的读取不是一个观察值，不伪装成 `Report`；因此枚举里没有 `canceled` |

## 取值与选择规则

- 行来源：外层 `type` 与 `payload.type` 只用于生成 `RecordType` 标签（如 `event_msg.token_count`），二者相同则不重复拼接；标签本身也过 `sanitize`。
- 无 `rate_limits` 的记录（包括 `token_count`、`token_usage_record` 的本地 token 计数）**不贡献任何额度**，也不会合成额度。这条被 `TestReadCodexDoesNotInferQuotaFromLocalUsage` 固定。
- `used_percent` 存在而 `remaining_percent` 缺失时，`RemainingPercent` 由 `100 - used` 派生并置 `RemainingDerived = true`；反之不派生 `used`。
- 窗口两个百分比都缺失时，该 `Window.Status` 为 `unknown`、`Reason` 为 `missing`，但 `window_minutes` 等其它字段照常保留。
- `resets_in_seconds` 只有在本记录**自己的**可信时间戳存在时才能换算成 `resets_at`；否则报 `no_trustworthy_timestamp` 并保持 `ResetsAt == nil`。显式 `resets_at` 优先。
- 陈旧：`Status == ok` 且 `StaleAfter > 0` 且 `now - ObservedAt > StaleAfter` 时降级为 `stale`；数值全部保留，`Available()` 仍为 true。
- 取代（currency）：选择器另外记住**最后一条带窗口的记录**（`isWindowAttempt`：`bad`、或带 `primary`/`secondary`）。若被选中的观察有可用 usage，而这条最新记录不是同一条、且它自身没有可用 usage，则 `Report` 变 `stale` + `newer_invalid`、`SupersededByLine` 记为它的行号。此时 `ObservedAt`/`Line`/`LimitID` 仍是**被保留那条记录**自己的值，`Available()` 为 true（历史值可用），但绝不报 `ok`。反向控制：最新一条带窗口记录自身可用时保持 `ok`。
- 归因：`LimitID`/`LimitName`/`PlanType`/`Credits` **只**从被选中记录取。其它记录声明过、且与选中记录 ID 不同的 limit ID 进入 `OtherLimitIDs`（排序、去重、最多 8 条）；选中记录自己没声明 ID 时 `LimitID` 就是空串，不会被别的行填上。
- 时间戳：`ObservedAt` 只来自被选中记录自己的 `timestamp`。另一条记录（哪怕是更"新"的、或只有元数据的）的时间戳不会被借来给这个窗口"续命"，因此"缺失时刻"不会被伪造成"有时刻"。

## 先红后绿证据（精确命令与结果）

57 个测试函数（24 读取器 + 17 速度 + 4 隐私/离线 + 12 来源/归因），加 2 个子测试。测试文件先于实现写入；第一轮的 45 个覆盖解析本身，第二轮的 12 个覆盖来源归因与当前值语义。

### 红 1：实现尚不存在（真实首跑，构建失败）

测试写完后、任何实现代码之前，`go test ./internal/quota/`（输出节选，逐字记录）：

```
undefined: CodexOptions
undefined: Report
undefined: ReadCodex
undefined: Issue
undefined: StatusOK
undefined: ReasonNone
undefined: WindowKind
undefined: Sample
(too many errors)
FAIL	github.com/zzstar101/mytoken/internal/quota [build failed]
```

退出码 1。这一步只证明测试引用的 API 不存在。

### 红 2：类型层 + 惰性 stub（行为红）

只写 `doc.go`/`quota.go`/`pace.go` 的类型与常量，加两个惰性实现——`ReadCodex` 恒返回 `&Report{Source: opts.Source, Status: StatusUnavailable, Reason: ReasonNoData}`（只转发 `ctx.Err()`），`EstimatePace` 恒返回 `Pace{Reason: ReasonNoData}`：

```sh
go test ./internal/quota/ -count=1
```

```
    privacy_test.go:63: ReadCodex = nil error, want the reader failure
--- FAIL: TestReadCodexCapsLabelsFromLog (0.00s)
    privacy_test.go:86: status = "unavailable"/"no_data", want a capped but usable observation
FAIL
FAIL	github.com/zzstar101/mytoken/internal/quota	0.262s
```

**第一轮的 45 个测试中 40 个 FAIL、5 个 PASS**（非 verbose 输出，目录里只打印失败项；完整输出存于 `/tmp/quota-evidence/red.txt`）。5 个当轮未失败的是：

| 测试 | 为什么在 stub 下也通过 | 是否算行为红证据 |
| --- | --- | --- |
| `TestQuotaPackageStaysOffline` | 静态 import 断言，与实现无关 | 否 |
| `TestPaceDefaults` | 只断言常量值 | 否 |
| `TestReadCodexDoesNotInferQuotaFromLocalUsage` | 断言 unavailable/no_data，正是 stub 的默认返回 | 否 |
| `TestReportSampleAbsentWindow` | 断言窗口缺席时 `Sample` 返回 false，stub 的 `Report` 没有窗口 | 否 |
| `TestReadCodexCanceledContext` | stub 本身就转发了 `ctx.Err()` | 否 |

即：**stub 阶段通过的 5 个都是静态/常量/默认不可用语义**；所有断言"解析出的数值"或"显式解释原因"的测试全部失败。上表由一次**不计入证据**的复现确认（在 `/tmp/quota-redcheck` 用同一 stub 重跑，得到同样的 40 FAIL / 5 PASS 与同样的 5 个名字），此复现仅用于分类，不作为 TDD 红证据引用。

### 红 3：来源错归因修复（真实首跑，行为红）

第二轮针对"观察值来源归因"复评后写 `internal/quota/provenance_test.go`。该文件**只用当时已存在的 API**（`Report.LimitID/LimitName/PlanType/Credits/ObservedAt/Line/Status/Pace.Reason`），因此在修复前可以编译、并按行为失败——这一点很重要：它证明失败不是因为引用了还不存在的字段，而是因为旧实现的**行为**错了。

```sh
go test -count=1 ./internal/quota/     # 修复前
```

真实输出（完整保存于 `/tmp/quota-provenance-evidence/red.txt`；`%+v` 打印指针地址的 `0x…` 已省略）：

```
--- FAIL: TestReadCodexDoesNotRelabelWindowWithOtherLimitID (0.00s)
    provenance_test.go:43: LimitID = "limit-B", want empty: line 1 never stated a limit ID and line 2 carried no window
    provenance_test.go:46: LimitName = "Beta", want empty for the same reason
--- FAIL: TestReadCodexPlanAndCreditsAreNotBorrowedAcrossRecords (0.00s)
    provenance_test.go:71: PlanType = "pro", want nil: only line 2 (a different limit) stated a plan
    provenance_test.go:74: Credits = &{HasCredits:0x… Unlimited:<nil> Balance:0x…}, want nil: only line 2 (a different limit) stated credits
--- FAIL: TestReadCodexNewerInvalidQuotaIsNotSilentlyCurrent (0.00s)
    --- FAIL: TestReadCodexNewerInvalidQuotaIsNotSilentlyCurrent/malformed_newer_window_after_a_rollover (0.00s)
        provenance_test.go:93: status = "ok"/"", want the old window marked not-current
    --- FAIL: TestReadCodexNewerInvalidQuotaIsNotSilentlyCurrent/newer_window_with_no_usage (0.00s)
        provenance_test.go:117: status = "ok"/"", want not ok: the newest window states no usage
--- FAIL: TestReadCodexMissingTimestampIsNotInventedFromAnotherRecord (0.00s)
    provenance_test.go:142: limit id/name = "limit-B"/"", want empty: the selected record stated none
    provenance_test.go:145: PlanType = "pro", want nil: the plan was on an untimestamped metadata-only record
--- FAIL: TestEstimatePaceRejectsSamplesFromDifferentLimits (0.00s)
    provenance_test.go:215: pace = {Available:true ... PercentPerHour:10 ...}, want unavailable: the samples name different limits
--- FAIL: TestReadCodexReaderErrorTextIsNotEmbedded (0.00s)
    provenance_test.go:229: error message echoed the reader's own text: quota: read codex rollout: reader exploded holding SYNTHETIC-SECRET-VALUE-4a71
FAIL
FAIL	github.com/zzstar101/mytoken/internal/quota	0.357s
```

退出码 1。6 个测试失败（其中 `NewerInvalidQuotaIsNotSilentlyCurrent` 含 2 个子测试），共 7 条断言，逐条对应复评提出的缺陷：

| 失败 | 对应缺陷 | 修复 |
| --- | --- | --- |
| `DoesNotRelabelWindowWithOtherLimitID` | carry 把"另一条记录"的 `limit_id`/`limit_name` 贴到被选中窗口上（**具体错归因**：第 1 行旧窗口无 ID，第 2 行另一个 limit 只有元数据 → 旧窗口被改标成 `limit-B`） | 删除 `carry`；标签只来自被选中记录；其它 ID 进 `OtherLimitIDs` |
| `PlanAndCreditsAreNotBorrowedAcrossRecords` | 同上，`plan_type`/`credits` 跨记录混入 | 同上（只取被选中记录） |
| `NewerInvalidQuotaIsNotSilentlyCurrent/malformed_…` | 更新的窗口形状坏时，旧的有效窗口仍被报成 `ok`（静默"当前值"） | 最新带窗口记录不可用 → `stale` + `newer_invalid` + `SupersededByLine` |
| `NewerInvalidQuotaIsNotSilentlyCurrent/newer_window_with_no_usage` | 更新的记录根本没写 usage，旧值仍被报成 `ok` | 同上（`isWindowAttempt` 覆盖"没写 usage"的窗口记录） |
| `MissingTimestampIsNotInventedFromAnotherRecord` | 无时间戳的记录借用了别的记录的标签，同样问题 | 标签归因修复；`ObservedAt` 只来自被选中记录 |
| `EstimatePaceRejectsSamplesFromDifferentLimits` | 纯函数只看 `WindowKind`，两个不同 limit 的观察被拿来算速率 | `Sample.LimitID` + `limit_mismatch` |
| `ReaderErrorTextIsNotEmbedded` | `readErr` 直接 `%w` 包装读取器文本，任意读取器文本随错误外泄 | 改为 `ReadError{Cause}`，`Error()` 固定文本，cause 只经 `Unwrap()` |

修复后把这 12 个测试**收紧**到断言新 API（`ReasonNewerInvalid`、`SupersededByLine`、`OtherLimitIDs`、`ReasonLimitMismatch`、`errors.As(*ReadError)`）。诚实说明：这些收紧断言在旧实现上无法编译，所以它们的"红"没有独立证据；旧实现上的行为红证据就是上表那 7 条。

### 绿：修复后的完整实现

```sh
go test -v -count=1 ./internal/quota/     # 57 PASS, 0 FAIL（末行 ok，退出码 0）
go test -race -count=1 ./internal/quota/  # ok, 退出码 0
go vet ./internal/quota/                  # 退出码 0
gofmt -l internal/quota/                  # 输出为空
go build ./...                            # 退出码 0
```

```
--- PASS: TestReadCodexDoesNotRelabelWindowWithOtherLimitID (0.00s)
...
ok  	github.com/zzstar101/mytoken/internal/quota	0.296s

ok  	github.com/zzstar101/mytoken/internal/quota	1.374s
```

第一轮的 24 个读取器测试、17 个速度测试、4 个隐私测试在删除 carry 后**全部原样通过**（例如 `TestReadCodexChoosesLatestTrustworthyObservation` 仍然通过：它 fixture 里第 2 行既带窗口、又自带 `limit_id`/`plan_type`/`credits`，所以"只取被选中记录"与"carry"结果相同）。

### 绿 2：第一轮遗留的实现缺口（历史记录）

首轮实现后仍有 4 项失败，其中 2 项是实现缺陷、2 项是测试自身的错：

1. `used_percent: "12.5"`（字符串）被当成合法数字接受——`encoding/json` 解码到 `json.Number` 时会接受**带引号的数字字符串**。修复为 `decodeNumber` 在首个非空白字节是 `"` 时返回 `unsupported_type`，`decodeFloat`/`decodeInt` 都建立在其上。**这条测试有牙。**
2. `rate_limits: "nope"` 得到 `unavailable/no_data` 而不是 `unavailable/malformed`——"没有 provenance"的提前返回把 `bad: true` 的回退快照一起丢掉了，导致"形状错误"降级成"没有数据"。修复为 `decodeLine` 末尾的 `if !snap.provenanced() && !snap.bad { return nil }`（`internal/quota/codex.go:417`）。**这条测试有牙。**
3. `TestReadCodexSkipsOverlongLines` 的 fixture 有误：`tokenCount` 总会塞进约 370 字节的 `info` 对象，于是把 `MaxLineBytes` 设成 256 时**两行都超长**，被测路径根本没走到。修正为第二行改用不含 `info` 的 `tokenCountSlim`，行长约 145 字节。这是测试自身的缺陷，不是实现缺陷。
4. `TestEstimatePaceDefaultMaxElapsedIsDisabled` 的算术写错：`2026-01-02T00:00:00Z` → `2026-01-05T00:00:00Z` 是 **72h**，断言却写了 96h。修正断言为 `72*time.Hour`。同上，测试自身缺陷。

第 3、4 项是在绿阶段修正的 fixture/断言，与红阶段记录的 40 FAIL 无关；此处如实记录，以免读者把绿阶段的改动误读成"为了让实现过而放宽测试"——两处都是让测试**更精确**（一个真正走到超长行路径，一个把区间算对）。

## 不支持的形状与显式原因

| 输入形状 | 结果 |
| --- | --- |
| 记录无 `rate_limits` | `Report` = `unavailable`/`no_data`，**无** issue（这不是错误，只是没有额度数据） |
| `rate_limits: null` | 同上，无 issue |
| `rate_limits` 不是对象（`"nope"`、数组、数字） | `unavailable`/`malformed`，issue `rate_limits` = `malformed` |
| `primary`/`secondary` 不是对象 | 该 `Window.Status` = `malformed`，`Report` = `unavailable`/`malformed`，issue `rate_limits.primary` = `malformed` |
| 窗口存在但无 `used_percent`/`remaining_percent` | 该 `Window` = `unknown`/`missing`；两个窗口都如此则 `Report` = `unavailable`/`missing` |
| `used_percent` 是字符串 | issue `…used_percent` = `unsupported_type`；该字段保持 `nil` |
| `used_percent` < 0 或 > 100 | issue `…used_percent` = `out_of_range`；该字段保持 `nil` |
| `used_percent` 无法表示为 float64（如 `1e999`） | issue `…used_percent` = `malformed`；该字段保持 `nil` |
| `window_minutes` 有小数（`300.5`） | issue `…window_minutes` = `malformed` |
| `window_minutes` 是字符串 | issue `…window_minutes` = `unsupported_type` |
| `timestamp` 不是 RFC3339 | issue `timestamp` = `malformed`；该记录无观测时刻 |
| `timestamp` 在可信区间外 | issue `timestamp` = `out_of_range` |
| `resets_at` 是 Unix 毫秒（如 `1767350400000`） | issue `…resets_at` = `out_of_range`（不猜单位） |
| `resets_in_seconds` 存在但本行无可信时间戳 | issue `…resets_at` = `no_trustworthy_timestamp` |
| `resets_in_seconds` < 0 或 > 100 年 | issue `…resets_at` = `out_of_range` |
| `plan_type` 不是字符串（如 `7`） | issue `rate_limits.plan_type` = `unsupported_type`，`PlanType` = `nil` |
| `plan_type: ""` | `PlanType` = `nil`，**无** issue（空串视为未声明） |
| `credits` 不是对象 | issue `rate_limits.credits` = `unsupported_type`，`Credits` = `nil` |
| `credits.has_credits`/`unlimited` 不是 bool | issue = `unsupported_type` |
| `credits.balance` 不是字符串 | issue = `unsupported_type` |
| 未知字段（当前 schema 之外） | `UnknownFields` 计数（按字段名去重）+1，无 issue，不假设单位，字段名不进入 `Report` |
| 单行超过 `MaxLineBytes` | 跳过整行（不缓存内容），issue `line` = `line_too_long`，`Count` 为被跳过的记录数 |
| 总输入超过 `MaxBytes` | `Truncated` = true，issue `input` = `byte_limit` |
| 末尾无换行的残行 | **不消费**（不完整输入不算一条记录） |
| `ctx` 已取消 | 返回 `context.Canceled` 错误，第二返回值非 nil |
| 读取器报错 | 返回 `*quota.ReadError`；`Error()` = `quota: read codex rollout: reader failed`（本包生成的固定文本），读取器原文只在 `Cause`/`Unwrap()` 中，需刻意取用 |
| 流中出现多个 limit ID | 被选中记录的 ID 作为 `LimitID`；其余排序去重后进 `OtherLimitIDs`（最多 8 条），**绝不**并入 `LimitID` |
| 被选中记录自己没声明 limit ID | `LimitID` = `""`；不会被流里别的记录填充 |
| 更新的一条带窗口记录不可用（形状坏、或无 usage），但存在更旧的可用的窗口 | `Report` = `stale`/`newer_invalid`，`SupersededByLine` = 那条更新记录的行号；被保留观察自己的 `ObservedAt`/`Line`/`LimitID` 不变，`Available()` 仍为 true（作为**历史**值），绝不报 `ok` |
| 更新的一条带窗口记录**可用** | 它就是当前值，`ok`，`SupersededByLine` = 0 |
| `r == nil` | 返回 `quota: nil reader` 错误 |

上表每一行都用一次性探针测试逐条核对过实际输出（探针是临时文件，已删除，不在交付物内）；其中 `1e999` 一行正是被探针纠正的——它报 `malformed` 而不是 `out_of_range`。第二轮新增的 4 行（多 limit ID、自己无 ID、被更新记录取代、更新记录可用）由 `internal/quota/provenance_test.go` 直接固定，不再依赖探针。

`Issue` 按 `字段 + 原因` 聚合计数，`Line` 记首次出现的行号，最终列表按 `字段`、`原因` 排序——因此**同一个损坏日志无论 map 迭代顺序如何，输出都逐字相同**。

## 隐私与离线不变量

`internal/quota/privacy_test.go` 用合成哨兵串（`SYNTHETIC-SECRET-PROMPT-9f2c`、`SYNTHETIC-SECRET-VALUE-4a71`）证明：

- `TestReadCodexReportNeverCarriesRawContent`：把哨兵串放进 `payload` 的自由文本位置与一个未知字段值，`Report` 序列化成 JSON 后两串都不出现，且未知字段只体现为 `UnknownFields == 1`。
- `TestReadCodexErrorsNeverCarryInput`：用一个"先给一行、再返回错误"的读取器，返回的 `error` 文本里不出现哨兵串，也不出现行内容。
- `TestReadCodexCapsLabelsFromLog`：超长 `plan_type` 被截断到 64 字节，且按 rune 边界（不会切出半个 UTF-8 字符）。
- `TestQuotaPackageStaysOffline`：用 `go/parser` 解析本包**所有非测试** `.go` 文件的 import，禁止 `net`、`net/http`（网络）、`os`、`os/exec`、`syscall`、`plugin`（读凭据/起进程）、`crypto/tls`，以及任何 `github.com/zzstar101/mytoken/` 前缀（避免与其它 package 耦合）。若一个 import 都没解析到就直接失败，防止"空跑即通过"。

结论：`internal/quota` 只能通过 `io.Reader` 拿到字节，自己不打开文件、不读环境变量、不读凭据、不联网、不依赖本模块其它包。

关于 `ReadError` 的**范围声明**（避免过度承诺）：本包保证的是"`ReadCodex` 返回的 error 消息里不含输入内容、也不含读取器自己的文本"，**不**保证调用方无法取得原始文本——`ReadError.Cause` / `errors.Unwrap` 是刻意保留的逃生口（上层需要 `errors.Is(err, io.EOF)` 之类的判断）。所以正确表述是"错误**消息**是生成的、可安全默认展示"，而不是"任意读取器错误都不可能泄漏文本"。调用方主动把 `Cause` 展示给用户时，那段文本的责任在调用方。`TestReadCodexErrorsNeverCarryInput` 固定前者，`TestReadCodexReaderErrorIsTyped` 固定后者（`errors.As` 可达、消息不含 cause 文本）。

## 消费速度与预测 helper 的支持条件

`EstimatePace(prev, next Sample, opts PaceOptions) Pace` 是纯函数：不读时钟、不做 I/O，只比较**两次**观察。它只在前置条件全部满足时返回 `Available = true`：

1. 两次观察是同一 `WindowKind`；
2. 两次观察的额度身份兼容：`LimitID` 相等，或两次都未知（`""`）。一侧已知而另一侧未知或不同 → `limit_mismatch`（未知不是匹配证明）；
3. 两次都带可信观察时刻；
4. 两次的 `used_percent` 都在 `[0, 100]` 且有限；
5. 关于 reset 的元数据**要么两次都有、要么两次都没有**——只有一次有就报 `reset_unknown`（两次都没有则仍可算速率，但不出"均匀用完参考线"）；
6. 若都有 reset：较早那次观察的 reset 必须晚于较晚那次观察的时刻（否则是 `reset_rollover`），两次 reset 必须相等（否则 `reset_mismatch`），且 reset 必须在较晚观察之后；
7. `elapsed > 0`、`elapsed >= MinElapsed`（默认 1s）、`elapsed <= MaxElapsed`（默认禁用）；
8. `used` 不下降（下降报 `decreasing_usage`）；
9. 速率不超过 `MaxPercentPerHour`（默认 3600，即 100%/分钟，超出报 `outlier`）。

满足后给出 `PercentPerHour`/`PercentPerDay`、`Elapsed`、`UsedDeltaPercent`、`RemainingPercent`；有 reset 时给出 `EvenPercentPerHour`（剩余量 / 到 reset 的小时数，即"均匀用完参考线"）；仅当 `rate > 0 && remaining > 0 && remaining/rate <= 100 年` 时才给 `TimeToExhaustion`/`ExhaustsAt`，并在预计耗尽晚于 reset 时置 `ResetsBeforeExhaustion = true`。任何一个前置条件不满足都返回 `Available = false` 加**具体** `Reason`，不会退回一个"差不多"的估计。

未做的部分：不维护滑动窗口/多观察拟合（只有两点）、不做跨会话合并、不做"多久刷新一次"的策略。这些属于后续集成。

## 未关闭项

1. **本包未被任何调用方引用**：扫描器集成（打开 `$CODEX_HOME/sessions/<YYYY/MM/DD>/rollout-*.jsonl`、选择最新文件、把 `Report` 接到 store/query/GUI/CLI）尚未落地。本轮只交付"从字节流读到 `Report`"这一层。
2. **字段覆盖未经真实日志验证**：`secondary`、`plan_type`、`remaining_percent`、`window_minutes`、`resets_in_seconds`、`resets_at`、`limit_id`/`limit_name`、`credits.unlimited` 在仓库现有合成样本里都没出现过，测试 fixture 是按预期形状构造的。**本文件不声明任何真实日志校验通过。**
3. **单位语义仍开放**：`credits.balance` 的单位（次数？金额？）由日志定义，本包按不透明字符串保留，展示层需要自行决定如何呈现。
4. **Claude 侧未动**：ROADMAP 要求的 Claude 授权额度（Keychain + OAuth 用量接口）不在本轮范围。
5. **消费速度与预测未接线**：`EstimatePace` 已可用且已测，但没有任何地方调用它；"需要几次观察才能显示预测"的产品决策未做。
6. **陈旧判定需要调用方给参数**：`StaleAfter` 默认关闭，扫描器需要自己定一个阈值，否则一个很旧但格式正确的观察仍会报 `ok`。
7. **"最新记录决定当前值"假设 append 顺序**：`SupersededByLine` 依赖"文件里靠后的行是更新的知识"。若调用方把多个 rollout 文件按时间倒序拼接、或读一个被重写过的文件，这个假设不成立。本包不做文件级排序，也不检查文件内的 `timestamp` 是否单调。
8. **`OtherLimitIDs` 上限是硬截断**：超过 8 个其它 limit ID 时多余的身份被丢弃，且**不**产生 issue/计数，因此调用方无法知道"还有更多"。若将来需要，应改成"计数 + 前 N 个"。
9. **历史值的产品语义未定**：`stale`/`newer_invalid` 时 `Available()` 为 true，GUI/CLI 到底该显示旧值并标注"非当前"，还是显示"额度不可用"，属于展示层决策，本包只保证不把它说成新鲜的。
10. **读取器错误文本的责任边界**：`ReadError.Cause` 不做清洗（本包也无法清洗）。任何把 cause 直接写进日志/UI 的调用方需要自己承担泄漏风险。
