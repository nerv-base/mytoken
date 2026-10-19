# 0.4.0 预算核心（internal/budget）验证（周期身份与回拨拒绝修复轮）

日期：2026-10-13 19:46 CST
环境：go1.27.1 darwin/arm64；工作目录 `/Users/zzstar/Developer/Programs/MyToken!!!!!`；HEAD `2e696f2`
写入范围：仅 `internal/budget/**`（`budget.go` / `evaluate.go` / `state.go` / `budget_test.go` / `state_test.go` / 新增 `period_spend_test.go`、`watermark_test.go`）与本文件。未改动 `model` / `store` / `query` / `app` / `gui` / `cli` / `reporttime` 及任何共享规划文件：

```console
$ git status --porcelain -- internal/budget docs/verification/v040-budget-core.md
?? docs/verification/v040-budget-core.md
?? internal/budget/
```

`internal/budget/` 与 `docs/verification/` 在本工作区尚未纳入 git 跟踪，因此没有可供 `git diff` 的基线；工作区里其余 `M` 文件是其它工作流的既有改动，本次没有触碰。

本轮未提交、未联网、未使用真实数据、未跑 fullsuite，只跑了 `./internal/budget/` 与只读依赖 `./internal/reporttime/`。全文引用行号除特别标注"修复前"外，均指本轮修复后的文件。

## 结论与范围

本次交付的是**预算功能的纯内核**：规则/估算的校验、按作用域+周期身份+币种的一次性去重、一个可序列化的预留账本，以及一个**持久、单调的评估水位线**。它是**纯函数 + 注入 now/location**，不读时钟、不读全局、不读文件、不发通知。

它**不是**预算功能本身。下面这些全部不存在，因此本次不改变任何运行时行为，也不声称预算可用：

- **没有任何运行时调用方**：仓库里没有任何代码 import `internal/budget`；
- **没有真实估算数据接入**：`Input.Spends` 由调用方构造，没有从 `pricing`/采集链路计算出的本地估算；
- **没有通知投递**：只产出 `Notification` 值，没有系统通知、GUI 弹窗、CLI 输出；
- **没有落盘**：`EncodeState`/`DecodeState` 只提供文档格式，没有 store 表、没有读写时机、没有原子替换；
- **统计时区尚未持久化**：`task_plan.md:15` 要求的"首次运行取系统时区并固定保存"没有实现，`Input.Location` 完全由调用方注入；
- **没有 CLI/GUI、没有配置入口**：规则从哪来、怎么改版本、怎么禁用，都还没有决定；
- **没有 schema 迁移**：只接受 `version == 2`；`version == 1` 一律 `ErrStateVersion`（见"未关闭的门"第 3 条）。

## 本次修复：四个缺陷与修法

### 1. 估算只按作用域索引，global 的 day/week/month 读同一个金额

**缺陷**（修复前 `evaluate.go:48-58`、`:107`）：唯一键是 `s.Scope.String()`，查找也是 `spends[r.Scope.String()]`。后果是同一作用域在一次输入里只能有一个估算，而 global 的 day 规则、week 规则、month 规则会读到**同一个** `Amount`；把三个周期的 global 估算放进一次调用，直接撞 `ErrDuplicateSpend`。旧注释声称"一个 scope 一条估算"，但规则是按周期判定的，于是周期维度被静默压平。

**修法**：

- `EstimatedSpend` 增加必填字段 `Period reporttime.Period`（`budget.go:371`），携带 kind + 时区 + 精确的半开区间 `[Start, End)`；`EstimatedSpend.PeriodID()`（`budget.go:393`）直接返回 `reporttime` 的 `Period.ID()`，**派生而不单独存储**，调用方无法"声明一个身份、携带另一段区间"。
- `EstimatedSpend.Validate()`（`budget.go:396`）现在也校验周期，落在 `validateSpendPeriod`（`budget.go:423`）：零值、未知 kind、反向区间、起点不是边界、截断、超长、kind 与区间不符（把一天标成一周）、把 DST 的 23/25 小时日伪造成 24 小时，全部 `ErrInvalidPeriod`。判定方式是拿 `p.Start` 回问 `reporttime.Resolve`，要求它复现出**一模一样**的 kind 与区间；没有任何默认值、推断或放宽。调用方说不出周期，就没有可用估算。
- 索引键改为 `scopePeriodKey`（`evaluate.go:236`）= `Scope.String() + "\x00" + PeriodID()`，值是该周期下的 `币种 → 估算`（`evaluate.go:67-82`）；**只有精确三元组 `(scope, periodID, currency)` 重复才算 `ErrDuplicateSpend`**。
- 规则查找改为 `(r.Scope, 规则解析出的当前 periodID, r.Currency)`（`evaluate.go:156-158`）。

### 2. "更早周期不可能再被跨越"的裁剪假设，在时钟回拨/时区切换下是错的

**缺陷**（修复前 `evaluate.go:88-95`）：每次评估都把该规则非当前周期的预留删掉，理由写在注释里——"earlier period can never be crossed again"。这只在评估时钟单调前进时成立。时钟回拨或统计时区被改回旧周期后，旧周期的预留**已经被删掉**，重新进入该周期就会二次提醒，直接违反"每规则/每周期/每阈值只提醒一次"。

**修法**：为每个规则世代增加持久、单调的水位线。

- `RuleState.Watermark time.Time`（`budget.go:534`）记录该世代**最近一次评估周期的 `Start`**；只有显式版本升级（新世代）才会重置。
- 评估时若解析出的周期 `Start` 早于水位线 → `ErrEvaluationRegressed`（`evaluate.go:118-128`），返回**零 `Result`**，`prev` 一个字节都不动。
- 裁剪（`dropOtherPeriodReservations`，`evaluate.go:252`）只在通过水位线检查之后执行，"有界账本"因此重新变得安全：删掉的那些周期在语义上已经不可能再被评估到。
- 水位线的粒度是**周期起点，不是墙上时刻**：同一周期内把时钟往回拨（12:00 → 06:00）不是回退，该周期的预留还在，重放是幂等的、不会二次提醒。这样既堵住了"回到昨天重新提醒"，又不会把"崩溃后重试当前周期"误判成回退。
- 水位线对**禁用规则同样推进**：裁剪在禁用分支之前执行，所以禁用期间跨过的周期也必须被水位线记住，否则"禁用 → 跨周期 → 回拨"仍会二次提醒。

### 3. 版本**回退**被当成新世代

**缺陷**（修复前 `evaluate.go:74` 的 `case !seen || known.Version != r.Version:`）：任何版本差异都走"新世代"分支，于是持久化的 v2 遇到输入 v1（旧配置被恢复、配置回滚、多副本版本不一致）会清掉 v2 的预留并**重新武装**通知。

**修法**（`evaluate.go:101-114`）：拆成四种情况，版本回退在指纹比较**之前**就被拒绝：

| 情况 | 处理 |
| --- | --- |
| 未见过的规则 ID | 清掉该 ID 的残留预留 |
| `r.Version < known.Version` | `ErrRuleVersionDecrease`（`budget.go:133`），零 `Result`，账本不动 |
| `r.Version > known.Version` | 显式新世代：清预留、水位线重置 |
| `r.Version == known.Version` | 比指纹，不一致 → `ErrRuleVersionConflict` |

### 4. schema 装不下水位线

`SchemaVersion` 由 `1` 升到 `2`（`budget.go:84`），状态文档的 `rules.<id>` 新增 `watermark`（`state.go:25`）：

- `EncodeState` 拒绝写出一条没有水位线的规则条目（`state.go:53`），`ErrInvalidState`——否则回拨保护会被静默关掉；
- `DecodeState` 拒绝缺失或零值 `watermark`（`state.go:113`），`ErrInvalidState`；
- `version == 1` 的文档现在是 `ErrStateVersion`（因为 `DisallowUnknownFields`，v1 文档本来也读不进 v2）。

## 契约映射

| 契约（`task_plan.md:14-15`） | 实现 | 测试 |
| --- | --- | --- |
| 只提醒、不阻断 | 包内无阻断路径，只返回 `Result`；无 IO、无 error 之外的副作用 | 全部评估测试 |
| 作用域 global/project/provider，稳定 ID | `Scope{Kind,ID}`；global 必须不带 ID，project/provider 必须带非空、无包围空白、无控制字符的稳定 ID | `TestScopeStringAndParseScopeKind`、`TestRuleValidateRejectsInvalidFields` |
| day/week/month，固定持久化统计时区，周一为周首 | 完全交给 `internal/reporttime.Resolve(now, kind, loc)`，周期身份直接用 `Period.ID()`；**估算也必须带同一种周期身份**，否则不算这个周期的数据 | `TestEvaluatePeriodBoundariesFollowReporttime`、`TestEvaluateAllPeriodKinds`、`TestEvaluateTimezoneIsExplicit`、`TestEvaluateRoutesSpendByScopeAndPeriod` |
| 默认 80%/100%，可调整 | `DefaultThresholds()` 返回 `[80 100]`；`Rule.Thresholds` 为空时用默认值，非空时完全替换 | `TestEvaluateDefaultAndCustomThresholds` |
| 每个规则/周期/阈值只提醒一次 | `State.Reservations` 以确定性 `NotificationID` 为键；命中即不再产出；**并由持久水位线保证在时钟回拨/时区切换/重启后仍然成立** | `TestEvaluateCrossingFiresOncePerRulePeriodThreshold`、`TestEvaluateExactThresholdBoundary`、`TestStatePersistenceRestartSimulation`、`TestEvaluateRefusesRegressedEvaluationAfterRollover`、`TestEvaluationWatermarkSurvivesSerializedRestart`、`TestEvaluateTimezoneChangeDoesNotSilentlyResetDedup`、`TestEvaluateTimezoneChangeBackwardsIsRefused`、`TestWatermarkRefusesRollbackAcrossEveryPeriodKind` |
| 未知价格显式标记 | `EstimatedSpend.UnpricedRequests` 计数，`Evaluation.UnknownPricing` / `Notification.UnknownPricing` 标记；`Estimated`/`Percent` 是下界 | `TestEvaluateUnpricedRequestsAreFlagged` |
| 只用本地估算成本，绝不用真实账单/余额 | 输入只有 `EstimatedSpend`；包内没有任何余额、账单、provider 调用 | 结构上不存在该路径 |
| 绝不混合币种 | 规则与估算各带 3 位大写币种；同作用域同周期但币种不符 → `ErrCurrencyMismatch`，不换算、不相加、不相加不同周期的量 | `TestEvaluateNeverCombinesCurrencies`、`TestEvaluatePeriodScopedCurrencyMatching` |
| 无数据不等于 0 | 该作用域**在该周期身份下**没有估算时 `DataAvailable=false` + `SkipNoData`，`Percent` 保持 0 且不判交叉；别的周期/别的币种的估算都不算替代品 | `TestEvaluateMissingSpendIsNotFakeZero`、`TestEvaluateOtherPeriodIsMissingNotZero` |
| 规则修改必须显式换版本 | 身份指纹不含 `Label`/`Disabled`；同版本但指纹变化 → `ErrRuleVersionConflict`；版本**升高** → 新世代并清掉旧预留；版本**降低** → `ErrRuleVersionDecrease` | `TestRuleEditRequiresExplicitVersion`、`TestLabelEditDoesNotResetDedup`、`TestEvaluateRejectsRuleVersionDecrease` |

## 状态文档（schema version 2）

`EncodeState` 写出的就是下面这个形状（字段名即线上的键，未知字段一律拒绝）：

```json
{
  "version": 2,
  "rules": {
    "rule-1": {
      "version": 1,
      "fingerprint": "…64 位 hex…",
      "last_period_id": "period:v1:day:UTC:2026-03-09T00:00:00Z",
      "watermark": "2026-03-09T00:00:00Z"
    }
  },
  "reservations": {
    "budget:v1:rule-1:1:global:period:v1:day:UTC:2026-03-09T00:00:00Z:80": {
      "id": "budget:v1:rule-1:1:global:period:v1:day:UTC:2026-03-09T00:00:00Z:80",
      "rule_id": "rule-1",
      "rule_version": 1,
      "period_id": "period:v1:day:UTC:2026-03-09T00:00:00Z",
      "threshold": 80,
      "reserved_at": "2026-03-09T12:00:00Z"
    }
  }
}
```

- 版本不符（含 `{}` 这种 `version` 缺失即 0 的情况，以及 `version: 1`）→ `ErrStateVersion`，调用方必须显式迁移或丢弃，不会被悄悄当成空账本；
- 无法解析、未知字段、尾部多余数据 → `ErrInvalidState`；
- 规则条目缺少 `watermark` 或水位线为零 → `ErrInvalidState`：没有水位线的账本无法证明回拨保护还在，因此宁可拒绝也不静默信任；
- 解码时会校验预留项的键与 `id` 一致、`rule_id`/`period_id` 非空、阈值合法——键就是去重身份，键和内容不一致会静默破坏去重，因此直接拒绝；
- 返回值的 `Rules`/`Reservations` 一定非 nil，可以直接喂给 `Evaluate`。

`Reservation` 里的 `reserved_at` 只用于审计；去重只看键。`EncodeState` 只拒绝"零水位线"，`Evaluate` 对手工构造的、没有水位线的 `prev` 是宽容的（不检查回拨），但这样的状态**无法被持久化**，所以宽容路径不会变成长期漏洞。

## 周期身份与估算查找

估算与规则能否匹配，取决于**四元组是否全部一致**：`Scope`、周期身份（`Period.ID()`，内含 kind、时区名、真实起点）、`Currency`，加上规则自己的当前周期。因此：

- 同一次调用里，global 的 day/week/month 三条规则各自只看自己周期的估算（`TestEvaluateRoutesSpendByScopeAndPeriod`：day 9/10 → 80%，week 50/100 → 不触发，month 1050/1000 → 80%+100%，三条通知各自带自己的金额与 `PeriodID`）；
- 同一作用域只有"昨天"的估算、规则要的是"今天" → `DataAvailable=false` + `SkipNoData`，`Estimated`/`Percent` 保持 0，**不是 0 元、也不判交叉**；该周期真的有一条 0 元估算时才 `DataAvailable=true`、`Percent=0%`、不触发；
- 同作用域同周期只有 EUR、规则是 USD → `ErrCurrencyMismatch`（中止整次评估）；同周期同时有 EUR 和 USD → 取 USD，EUR 既不参与也不报错；只有**别的周期**的 EUR → 是 `SkipNoData` 而不是币种错误；
- 同作用域同周期同币种出现两次 → `ErrDuplicateSpend`；同周期两种币种不算重复。

## 水位线、回拨拒绝与有界账本

`Evaluate` 是纯函数：`prev State` 原值绝不被改动（`cloneState` 深拷贝，`TestEvaluateIsRaceFree` 用 32 个 goroutine 共用同一个 `prev` 验证），返回的 `Result.State` 才是下一份账本。任何 error 都在返回前中止，`Result` 是零值，账本保持原样。

`State` 是**预留/去重账本，不是投递回执**。包内不做投递，因此先落盘还是先投递由接入方决定：

- **先落盘再投递**：每个周期至多一次；进程在"已落盘、未投递"之间崩溃会**丢一次**提醒；
- **先投递再落盘**：至少一次；崩溃后重放会**重复**投递。

两条都不能保证跨崩溃的 exactly-once。本包**不声称** exactly-once，也**不声称**任何 OS 级投递成功；去重之外的投递重试、用户可见的二次去重都属于接入层。

### 回拨与重启语义（本轮修复的核心）

| 场景 | 行为 |
| --- | --- |
| 同一周期内时钟小幅回退（12:00 → 06:00） | 允许，幂等：该周期预留还在，不产生新通知 |
| 已跨到新周期后回拨到旧周期 | `ErrEvaluationRegressed`，零 `Result`，账本不变 |
| 序列化重启后重放**当前**周期 | 允许，幂等（预留随文档一起回来了） |
| 序列化重启后、已进入新周期，再回放旧周期 | `ErrEvaluationRegressed` |
| 统计时区从 UTC 改成 America/New_York | 新时区在当前时刻对应的周期起点更晚，是**真正的新周期**，可以提醒；此时再切回 UTC 会落在水位线之前 → `ErrEvaluationRegressed`，**不会被静默重置** |
| 规则被禁用期间跨过的周期 | 水位线照样推进（裁剪在禁用分支之前发生），因此禁用后回拨也不会二次提醒 |
| 版本升高（新世代） | 预留清空、水位线重置；这是唯一允许"忘记历史"的路径 |
| 版本降低 | `ErrRuleVersionDecrease`，不是新世代 |

### 有界性（没有 tombstone）

本方案**不引入任何 tombstone**，也没有"删除标记"这种无界结构。每个规则世代的账本上界是：

- 1 个水位线（`time.Time`，每世代一个）；
- 当前周期内、每个生效阈值最多 1 条预留（`len(r.EffectiveThresholds())` 条，默认 2 条）。

裁剪只在通过水位线检查后删除"非当前周期"的预留，被删周期在语义上已经不可能再被评估到，所以状态大小与历史长度无关，且不需要保留历史就能拒绝回拨。

## 关键判定语义

- **交叉判定用乘法而不是除法**：`amount*100 >= threshold*limit`。这样"恰好 80%"（8/10）和"恰好 100%"（10/10）不会被 `amount/limit` 的舍入挪位，`TestEvaluateExactThresholdBoundary` 覆盖 7.999 / 8 / 9.999 / 10。
- **百分比不封顶**：150% 就报 150，便于显示超支程度。
- **阈值升序返回**：`Crossed` 与 `Notifications` 每个阈值只出现一次，且按（规则 ID，版本，周期，阈值）排序，输入顺序不影响输出（`TestEvaluateOutputIsDeterministicAndSorted`）。
- **单条规则出错会中止整次评估**：`Evaluate` 在写任何状态之前就完成全部校验，返回 error 时账本保持原样，不会出现"一半规则生效"。
- **禁用规则仍记账**：禁用只跳过评估（`SkipDisabled`），世代、水位线与预留都保留/推进，因此同一周期内重新启用不会二次提醒（`TestEvaluateDisabledRule`）。
- **无数据不等于 0**：`SkipNoData` 的字符串值仍是 `"no spend data for scope"`（未改 API 值），但语义收窄为"该作用域在该周期身份下没有估算"。

## 导出 API

```go
const SchemaVersion = 2
const SkipDisabled = "rule disabled"
const SkipNoData   = "no spend data for scope"

var ErrInvalidRule, ErrInvalidScope, ErrInvalidPeriod, ErrInvalidCurrency,
    ErrInvalidAmount, ErrInvalidThreshold, ErrDuplicateRule, ErrDuplicateSpend,
    ErrRuleVersionConflict, ErrRuleVersionDecrease, ErrEvaluationRegressed,
    ErrCurrencyMismatch, ErrInvalidInput, ErrStateVersion, ErrInvalidState error

func DefaultThresholds() []float64

type ScopeKind string
const (ScopeGlobal ScopeKind = "global"; ScopeProject ScopeKind = "project"; ScopeProvider ScopeKind = "provider")
func (ScopeKind) Valid() bool
func ParseScopeKind(string) (ScopeKind, error)

type Scope struct{ Kind ScopeKind; ID string }
func (Scope) IsGlobal() bool
func (Scope) Validate() error
func (Scope) String() string // "global" / "project:<id>" / "provider:<id>"

func ValidCurrency(string) bool
func ParseCurrency(string) (string, error)

type Rule struct {
    ID string; Version int; Scope Scope; Period reporttime.Kind
    Currency string; Limit float64; Thresholds []float64
    Disabled bool; Label string
}
func (Rule) Validate() error
func (Rule) EffectiveThresholds() []float64 // 升序副本；空 => [80 100]
func (Rule) Fingerprint() string            // sha256，含 ID/Version/Scope/Period/Currency/Limit/阈值；不含 Label/Disabled

type EstimatedSpend struct {
    Scope Scope
    Period reporttime.Period   // 必填：kind + 时区 + 精确 [Start,End)
    Currency string; Amount float64; UnpricedRequests int64
}
func (EstimatedSpend) PeriodID() string   // == Period.ID()，派生
func (EstimatedSpend) Validate() error    // 含周期合法性 -> ErrInvalidPeriod

type Input struct {
    Now time.Time; Location *time.Location
    Rules []Rule; Spends []EstimatedSpend
}

type Evaluation struct {
    RuleID string; RuleVersion int; Label string; Scope Scope
    Period reporttime.Kind; PeriodID string; Currency string; Limit float64
    Estimated float64; UnpricedRequests int64; UnknownPricing bool
    Percent float64; DataAvailable bool; SkippedReason string; Crossed []float64
}

type Notification struct {
    ID string; RuleID string; RuleVersion int; Label string; Scope Scope
    Period reporttime.Kind; PeriodID string; Threshold float64
    Currency string; Limit float64; Estimated float64; Percent float64
    UnknownPricing bool; ReservedAt time.Time
}

type RuleState struct {
    Version int; Fingerprint string; LastPeriodID string
    Watermark time.Time   // 该世代最近评估周期的 Start
}
type Reservation struct{ ID, RuleID string; RuleVersion int; PeriodID string; Threshold float64; ReservedAt time.Time }
type State struct{ Version int; Rules map[string]RuleState; Reservations map[string]Reservation }
type Result struct{ State State; Evaluations []Evaluation; Notifications []Notification }

func NotificationID(r Rule, periodID string, threshold float64) string
func Evaluate(prev State, in Input) (Result, error)
func EncodeState(State) ([]byte, error)
func DecodeState([]byte) (State, error)
```

**相对上一轮的 API 变化**（包内无外部调用方，因此不构成跨包破坏，但接入方需要知道）：

| 变化 | 说明 |
| --- | --- |
| `EstimatedSpend` 新增必填 `Period reporttime.Period` | 破坏性：构造点必须提供周期 |
| 新增 `EstimatedSpend.PeriodID()` | 派生，不可单独设置 |
| `EstimatedSpend.Validate()` 语义扩大 | 周期非法 → `ErrInvalidPeriod` |
| 去重身份由 `scope` 收窄/精确为 `(scope, periodID, currency)` | 同 scope 多周期/多币种不再误报重复 |
| 币种不匹配的触发条件收窄 | 只有"同 scope 同周期、币种不同"才是 `ErrCurrencyMismatch`；不同周期 → `SkipNoData` |
| `RuleState` 新增 `Watermark time.Time` | 破坏性：组合字面量构造点必须补字段 |
| 新增 `ErrRuleVersionDecrease` | 版本降低不再被当成新世代 |
| 新增 `ErrEvaluationRegressed` | 回拨/时区回退不再被静默接受 |
| `SchemaVersion` 1 → 2，文档新增 `rules.<id>.watermark` | 线格式变化；v1 文档 → `ErrStateVersion` |
| `SkipNoData` 常量值不变 | 语义收窄为"该周期身份无数据" |

**未变**：只提醒不阻断、只用本地估算、未知价格显式标记、绝不混合币种、`DefaultThresholds`、通知 ID 形状、`crosses` 的乘法判定。

通知 ID 形状（`TestNotificationIDIsDeterministic` 钉住的黄金值）：

```
budget:v1:rule-1:1:global:period:v1:day:UTC:2026-03-09T00:00:00Z:80
```

即 `budget:v1` + 规则 ID + 规则版本 + `Scope.String()` + `reporttime` 的 `Period.ID()` + 阈值。`Label` 不在其中，改名不会重置去重；`PeriodID` 已经带上 period kind、时区名和真实起点，所以换时区或跨 DST 都是不同身份。

## TDD 证据（先红后绿）

**红**：本轮先把新契约写进测试，同时只把新 API（`EstimatedSpend.Period`、`RuleState.Watermark`、两个新 sentinel）声明进 `budget.go`，行为保持旧实现。`go test ./internal/budget/ -count=1` → exit 1，39 个顶层测试中 **15 个失败**、29 条失败断言（含 14 个子测试），日志 `/tmp/budget-red.txt`。代表性失败都是缺陷本身的直接签名：

```
budget_test.go:647: Evaluate() error = budget: duplicate spend scope: global, want nil
period_spend_test.go:200: Validate({Kind: Start:0001-01-01 00:00:00 +0000 UTC End:0001-01-01 00:00:00 +0000 UTC}) = <nil>, want ErrInvalidPeriod
watermark_test.go:40: rolled-back Evaluate() error = <nil>, want ErrEvaluationRegressed
watermark_test.go:189: version decrease error = <nil>, want ErrRuleVersionDecrease
state_test.go:136: encoded rule entry omits the watermark: {"version":1,"rules":{"rule-1":{…}}}
```

（行号取自红跑当时的日志。）

**实现过程中改掉了我自己写错的 1 处断言、补了 1 个用例**（诚实记录，不是实现迁就测试）：

1. `TestEvaluationWatermarkSurvivesSerializedRestart` 原本要求"重启后重放**同一**周期"也返回 `ErrEvaluationRegressed`。这是过强的断言：水位线的粒度是周期起点，当前周期的预留随文档一起回来了，重放该周期是幂等的；把它判成错误会让"崩溃后重试当前周期"这种合法重试硬失败。改为：重启后重放当前周期 → 幂等无通知；重启后进入下一周期 → 正常提醒并推进水位线；此后回放旧周期 → `ErrEvaluationRegressed`。
2. 首次全绿后补了 `TestWatermarkRefusesRollbackAcrossEveryPeriodKind`（day/week/month 各自跨周期再回拨都必须被拒）和 `TestEvaluateTimezoneChangeBackwardsIsRefused`（时区往"更早的日历周期"切同样要拒），因为原用例只覆盖了 day 与"新周期确实更晚"这一个方向。

**绿**（全部在 `2e696f2` 工作区上跑）：

```console
$ gofmt -l internal/budget/          # 无输出
$ go vet ./internal/budget/          # 通过
$ go test ./internal/budget/ -count=1
ok  	github.com/zzstar101/mytoken/internal/budget	0.270s
$ go test -race ./internal/budget/ -count=1
ok  	github.com/zzstar101/mytoken/internal/budget	1.348s
$ go test ./internal/reporttime/ -count=1     # 只读依赖健康检查
ok  	github.com/zzstar101/mytoken/internal/reporttime	2.489s
$ go test ./internal/budget/ -count=1 -cover
ok  	github.com/zzstar101/mytoken/internal/budget	0.347s	coverage: 95.5% of statements
$ go build ./internal/budget/ ./internal/reporttime/   # 本修复范围（唯一相关的构建面）
$ go build ./internal/...                     # 通过（仓库内无其它包 import internal/budget）
```

规模：**41 个顶层测试、130 个 `=== RUN`（含 90 个子测试），全部通过**。行数：`budget.go` 585、`evaluate.go` 287、`state.go` 141、`budget_test.go` 793、`state_test.go` 186、`period_spend_test.go` 228、`watermark_test.go` 274。

> 验证说明（并发写入）：本工作区同时有其它写入者在改 `internal/harness/**`（与本次修复无关）。验证过程中曾观察到 `go build ./internal/...` 因 `internal/harness/{codebuddy,cline}` 正处于半成品状态（`"os" imported and not used`）而瞬时失败，随后即恢复通过。因此本轮的**权威构建/测试面**是 `./internal/budget/` 与只读依赖 `./internal/reporttime/`，全树构建仅作参考。

本轮新增/改写的用例（按题目）：

- 周期路由：`TestEvaluateRoutesSpendByScopeAndPeriod`（global day/week/month 一次调用各自金额）；
- 错周期不是 0：`TestEvaluateOtherPeriodIsMissingNotZero`；
- 周期内币种匹配：`TestEvaluatePeriodScopedCurrencyMatching`（5 个子用例）；
- 周期必须是真周期：`TestEstimatedSpendRequiresACanonicalPeriod`（9 个拒绝 + 4 个接受，含 NY 的 23 小时 DST 日）、`TestEvaluateRejectsSpendWithForgedPeriod`；
- 回拨拒绝：`TestEvaluateRefusesRegressedEvaluationAfterRollover`、`TestWatermarkRefusesRollbackAcrossEveryPeriodKind`、`TestWatermarkAllowsBackwardsClockInsideTheSamePeriod`、`TestWatermarkAdvancesForDisabledRules`；
- 重启：`TestEvaluationWatermarkSurvivesSerializedRestart`、`TestEncodeStateCarriesTheEvaluationWatermark`、`TestEncodeStateRejectsRuleEntryWithoutWatermark`、`TestStatePersistenceRestartSimulation`（新增水位线断言）、`TestDecodeStateRejectsBadDocuments`（新增缺水位线/零水位线/`version:1` 用例）；
- 时区切换不静默重置：`TestEvaluateTimezoneChangeDoesNotSilentlyResetDedup`（切到更晚的新周期：允许并提醒；切回来：拒绝）、`TestEvaluateTimezoneChangeBackwardsIsRefused`（切到更早的日历周期：拒绝）、`TestEvaluateTimezoneIsExplicit`；
- 版本回退：`TestEvaluateRejectsRuleVersionDecrease`。

## 未关闭的门（接入方必须自己补的）

1. **估算来源 + 周期解析**：需要一个把本地已计费/已计价请求按 `Scope` 聚合成 `EstimatedSpend` 的组件，包括"价格未知的请求数"，并且**必须用持久化的统计时区去 `reporttime.Resolve` 出 `Period`**。若估算层用了别的时区（例如进程本地时区），它的 `PeriodID` 与规则解析出的不一致，结果是**静默 `SkipNoData`**——不会报错，只会不提醒。这是本轮之后最容易踩的接入坑。
2. **统计时区**：`task_plan.md:15` 要求固定持久化、默认取首次运行系统时区。现在只是 `Input.Location` 参数，落盘、迁移、GUI 修改都没有。时区一旦改变，旧周期的水位线会导致"回到旧时区"被 `ErrEvaluationRegressed` 拒绝，这是有意的；接入方需要决定这是报错、告警还是需要人工确认。
3. **持久化与 v1→v2 迁移**：`EncodeState`/`DecodeState` 有了，但没有任何调用方：没有 store 表/文件、没有写入时机（建议"先落盘再投递"）、没有原子替换与失败处理。另外**已存在的 `version == 1` 文档现在一律 `ErrStateVersion`**；因为 v1 里没有水位线，正确做法是拒绝并重建（从当前周期重新开始记账），**不要**给 v1 文档补一个假水位线——那正好会把本轮修掉的回拨漏洞装回去。
4. **投递与拒绝的上报**：没有通知通道，没有重试。特别是 `ErrEvaluationRegressed` / `ErrRuleVersionDecrease` **必须被上报告警而不是在循环里静默吞掉**：静默吞掉的表现是"规则从此不再评估"，而用户只会觉得预算提醒莫名停了。
5. **规则来源与变更入口**：规则集合怎么配置、改字段时怎么自动 bump `Version`（否则会撞 `ErrRuleVersionConflict`）、回滚配置时如何避免 `ErrRuleVersionDecrease`、禁用怎么表达，都还没有。
6. **`ErrRuleVersionConflict` / `ErrRuleVersionDecrease` 的运维体验**：现在同版本改身份是硬错误，版本回退也是硬错误，需要接入层保证"改规则必带新版本、回滚必须显式处理"，否则用户会看到失败而不是被静默重置——这是有意的，但需要 UI/CLI 配合。
7. **已删除规则的账本条目不会被回收**：`Evaluate` 只处理本次 `in.Rules` 里的规则，配置里删掉的规则其 `Rules`/`Reservations` 条目会一直留着（既有行为，本轮未改，也未加界）。状态上界是"当前配置的规则数 ×（1 个水位线 + 每阈值 1 条预留）"，不是历史长度；但如果规则集合长期增删，接入层可能需要自己的回收策略。
8. **币种白名单**：只校验"3 位大写字母"的形状，没有 ISO 4217 列表；`US$`、数字会被拒，但 `ZZZ` 会被接受。这是"不猜、不换算"的取舍，若产品要选单需要额外一层校验。
9. **验收状态**：预算功能在真实数据接入与通知投递打通之前**不算交付**；本包只是它的可测试内核。本轮修掉的是内核里两个"会静默给出错误答案"的缺陷（周期压平、回拨二次提醒）和一个"静默重新武装"的缺陷（版本回退），并为此把状态 schema 升到 v2。
