# 缓存账本（0.3）设计与接口契约

> 本文是 0.3 各模块之间的契约。改动接口先改本文，并通知 lead。
> 定位见 [ROADMAP §0.3](ROADMAP.md)：**只用 token 数**，所有 harness 通用，回答「因为缓存没命中，我多花了多少钱、为什么、是不是中转站的锅」。

## 1. 术语

| 名词 | 定义 |
| --- | --- |
| 提示长度 `P` | 一次请求的 `Input + CacheRead + CacheWrite`（本地事件里 `Input` 不含缓存读写，见 SPEC §2） |
| 应命中 `Expected` | 按会话历史推断，这次请求**本该**从缓存读到的 token 数 |
| 未命中 `Miss` | `max(0, Expected − CacheRead)`，扣掉容差后的值 |
| 多花 `MissUSD` | 实际费用 − 「未命中的 token 改记为 CacheRead」后的反事实费用；用和本地估价同一套价格（含中转站倍率、按时间生效的规则） |
| 原因 `Cause` | 每个有 `Miss > 0` 的请求只归一个原因，见 §3 |

不算未命中的：会话第一次请求（冷启动，`Expected = 0`）、`Expected` 低于最小可缓存长度的请求、日志里没有任何缓存字段的 harness/模型（整条会话标记 `uncacheable`，不进入账本分母）。

## 2. 应命中的推断（`internal/ledger`，纯函数，无 I/O）

- 按 `(harness, session_id)` 分链，调用方按时间排序；函数不修改输入。子代理是自己的会话，不和父会话混算。候选还须同 `Harness`、同 `Provider`（resolved provider），不把换中转站当成旧站缓存失效。
- **不能简单取「上一条」或历史最大 P**。真实 pi 会话：`(P,R)=(9102,4480),(9356,4480),(9556,9344),(15787,4480)`；末条是已观察到的短读分支，不记 4480 以上的损失。规则：
  1. 已完成请求同时贡献「可缓存前缀」和独立的历史 `CacheRead` 证据。OpenAI/兼容/DeepSeek 前缀用 `P_i`；**Anthropic 用 `CacheRead_i + CacheWrite_i` 断点**，不能把没有写入缓存的尾部 `Input` 当成后续应命中。真实 Claude 首条 `P=72974,W=60789`、下一条 `R=60789`，不能误报 12185。
  2. 候选 = TTL 内、同模型、长度 `≤ P + max(256, P/50)` 的最长前缀；`Expected = min(候选, P)`，再按家族粒度向下取整。
  3. 若当前非零 `CacheRead` 达到最小缓存长度，且在 `max(256, CacheRead/50)` 容差内匹配 TTL 内任一历史前缀或历史 read，视为已有分支证据。候选明显更长时，`Expected = CacheRead`，不算损失。宁可漏报反复出现的稳定低命中，也不把已知旁路冤枉成浪费。
  4. 找不到 live 候选时，可用同模型过期候选解释 TTL；只有完全没有同模型历史才尝试同站其他模型。提示缩短且没有可比前缀、也没有 §3 的强压缩证据时，按新分支冷启动（`Expected=0`），不能凭长度骤降指控。
- **粒度、容差**：OpenAI/兼容接口、DeepSeek 按 128 token 块，最少 1024；Anthropic 保留精确断点，Claude Opus 最少 2048，其他 Claude 最少 1024，只有 Anthropic provider 线索时最少 2048。先按模型家族、再按 provider 判断；未知用块 128、最少 1024。`Miss = max(0, Expected−CacheRead)`，小于 `max(256, Expected/50)` 才整笔记 0（不是所有大额 Miss 也减 256）。`Miss=0` 时 `Cause=""`。
- TTL 是启发式而非服务器承诺：Anthropic 5 min、OpenAI 系 10 min、DeepSeek 1 h、未知 5 min；间隔**等于** TTL 仍视为存活。当前 `Request` 没有 1 h 写入字段，不能仅凭 `CacheWrite>0` 宣称 Anthropic 延长 TTL。
- 全链无正 `CacheRead/CacheWrite` 时每条 `Uncacheable=true`；这表示没有缓存可观测性，不证明厂商不支持缓存。不能把可缓存链中某一条 `R=0` 排除，否则真正全失效也会被隐藏。
- 按长度离散化的最大序号树保留全部历史证据；只把已发生请求写入树，未来长度仅用于建立坐标。TTL/边界用序号下界过滤，`O(n log n)` 时间、`O(n)` 空间，不依赖固定窗口，因此不遗忘隔很久才重访的已知分支。

## 3. 原因（先排除冷启动、低于最小长度和已有分支；有 Miss 才归因）

| `Cause` | 判定 | 性质 |
| --- | --- | --- |
| `compact` | **本请求**带 `Boundary=compact/clear`；或无已知分支证据，当前 P < 上一请求 P 的 60%，且随后连续两条同模型同站请求均在 TTL 内、无新边界、P 单调不减但仍 < 原 P 的 60%，每条 read 都接近其上一条的新 P | 压缩后重付，显式边界可靠，长度推断仍属启发式 |
| `ttl` | 没有 TTL 内可比前缀，但同模型存在过期前缀 | 预期内，不等于中转站失效 |
| `model` | 同模型历史为空，而同站另一个模型有可比前缀 | 换模型，预期内 |
| `unexplained` | TTL 内有同模型可比前缀，排除已知分支后仍有超过容差的缺口 | **未解释，不是中转站有错的证明**；还可能是客户端改写、未记录的分支、缓存驱逐或路由差异 |

`Boundary` 在事件模型里表示「本请求之前发生」；**不能再次检查前一条的 Boundary**，否则同一压缩会重付两次。`resume` 不是压缩。`compact` 的 `Expected=P`，`Miss=P−CacheRead`（再适用最小长度和容差），不与旧提示长度相减。边界/已确认压缩之后旧证据失效，从本条建立新 epoch。第一次可观测请求仍按冷启动处理。

仅凭 token 长度无法证明前缀字节相同，也无法区分所有并行分支；没有后续两条确认的链尾骤降不推断 compact。健康度可用 `unexplained Miss / Expected` 做排查线索，报告同时展示流量与分母，不以它给中转站下因果定论。

## 4. Go 接口

```go
package ledger // internal/ledger

type Cause string

const (
	CauseNone        Cause = ""
	CauseCompact     Cause = "compact"
	CauseTTL         Cause = "ttl"
	CauseModel       Cause = "model"
	CauseUnexplained Cause = "unexplained"
)

// Request is one event of a session chain, in time order.
type Request struct {
	At                            time.Time
	Harness, Provider, Model      string // Provider = resolved provider
	Input, CacheRead, CacheWrite  int64
	Boundary                      model.Boundary
}

// Entry is the ledger verdict for the request at the same index.
type Entry struct {
	Expected, Miss int64
	Cause          Cause
	Uncacheable    bool // the chain shows no cache fields at all
}

// Version changes whenever Analyze's output for the same input can change;
// the store recomputes every ledger entry when it differs from the stored one.
const Version = 1

func Analyze(chain []Request) []Entry

// Counterfactual moves miss tokens into CacheRead, taking them from
// CacheWrite first, then Input. MissUSD = cost(e) − cost(Counterfactual(e, miss)).
func Counterfactual(e model.UsageEvent, miss int64) model.UsageEvent
```

## 5. 存储与维护（`internal/store`，step-ledger）

- `event_data` 增加 `cache_expected INTEGER NOT NULL DEFAULT 0`、`cache_miss INTEGER NOT NULL DEFAULT 0`、`miss_cause TEXT NOT NULL DEFAULT ''`、`miss_cost REAL NOT NULL DEFAULT 0`（`uncacheable` 用 `cache_expected = -1` 表示）。
- 汇总：**新表 `cache_daily`**（不并入 `daily_usage`），按 天 × harness × 会话 × resolved_provider × 模型 × 项目 × 原因 得到 `requests, cacheable, expected, read, miss, miss_cost`。选新表的理由：`daily_usage` 的 PK 与既有消费方不掺入「原因」维度；`cache_daily` 只收可缓存事件（`cache_expected >= 0`），由 `event_data` 上独立的 cache_insert/update/delete 触发器随缓存列增量维护（镜像 `daily_usage` 触发器）。因为是按维度分组的汇总行而非逐事件，逐事件库增长仍只有个位数 B。
- 维护时机：
  1. `CommitMany` 之后，只重算被写到的会话（整条链重算，链长通常 < 几千）；
  2. 价格或归因变化（`replacePricing` 及归因回填）时重算 `miss_cost`（只重算费用，不必重跑 Analyze）；
  3. `ledger.Version` 与 `settings.ledger_version` 不同 → 全量重建一次。
- 预算（20 万事件，Apple Silicon）：全量重建 < 3 s；单次提交的增量 < 50 ms（典型会话）；下面每个查询 < 50 ms；数据库增长 < 20 B/事件。
- 实测（216,036 events / 2,191 sessions 的真实库副本，含 astra-ledger 的 O(n log n) Analyze）：全量重建 3.14 s；单次提交增量 recomputeSessions 中位会话 0.24 ms、最大单会话（8,307 events）31–42 ms；cache 查询 9–22 ms；库增长 1.7 B/事件（刚迁移）/~5 B/事件（重建物化后）。性能关键点：会话链查询必须走 `event_dimensions ⋈ event_data`（按 harness 全表扫会把重建放大到数十分钟）。

## 6. 查询（`internal/query`）

```go
type CacheCause struct {
	Cause    string
	Requests int64
	Miss     int64
	USD      float64
}

type CacheTotals struct {
	Requests, Cacheable int64   // all requests / requests in the ledger
	Expected, Read, Miss int64  // Read = CacheRead of cacheable requests
	USD                  float64 // total MissUSD
	ByCause              []CacheCause
}

type CacheBucket struct {
	Key string // dim value (provider, session, model, harness, project)
	CacheTotals
}

type CachePoint struct {
	Day time.Time
	CacheTotals
}

// CacheStep is one request of a session's cache timeline.
type CacheStep struct {
	At                                 time.Time
	Model                              string
	Prompt, Read, Write, Expected, Miss int64
	Cause                              string
	USD                                float64
	Boundary                           string
}

// CacheDim picks the grouping of CacheBy.
type CacheDim string

const (
	CacheByProvider CacheDim = "provider" // resolved provider (relay)
	CacheByModel    CacheDim = "model"
	CacheByHarness  CacheDim = "harness"
	CacheByProject  CacheDim = "project"
	CacheBySession  CacheDim = "session" // Key = harness + "/" + session_id
)

// Added to query.Service:
CacheTotals(ctx context.Context, f Filter) (CacheTotals, error)
CacheDaily(ctx context.Context, f Filter) ([]CachePoint, error)
CacheBy(ctx context.Context, f Filter, dim CacheDim) ([]CacheBucket, error)
SessionCache(ctx context.Context, harness model.Harness, session string) ([]CacheStep, error)
```

「中转站缓存健康度」= 某 resolved_provider 的 `unexplained` 未命中 / `Expected`，GUI 和 CLI 自己从 `CacheBy(..., provider)` 算，不另设接口。

## 7. 展示

- CLI（flash-cache）：`mytoken cache [--by cause|provider|model|harness|project|session|day]`，共享 `--since/--until/--last/--json/--csv/--offline`；`mytoken cache <session-id>` 输出时间轴。
- GUI（lead）：概览加一张「缓存多花」卡片；新页「缓存」：按原因的拆解、按中转站的健康度、按天走势、会话缓存时间轴（会话详情也显示）。

## 8. 验收（每个任务都要）

- `gofmt -l .` 为空，`go vet ./...`、`go test ./... -count=1` 通过。
- 在真实库副本上跑出数字并人工核对至少 5 条会话的逐条判定（报告里贴脱敏后的样例：时间、模型、P、CacheRead、Expected、Miss、Cause），说明 `unexplained` 占比和主要来源。
- 不打印任何 Key、提示内容或项目绝对路径。
