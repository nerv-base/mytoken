# GitHub 开源生态调研：用量统计、平台反代与可观测性

> 调研日期：2026-10-17  
> 目标：为 MyToken 0.4.x 之后的兼容性、统一账本、成本治理与网关接入提供可执行路线。  
> 方法：优先读取项目源码、SQL/迁移、协议定义与官方标准；README 只作产品定位证据，不把 Stars、宣传文案或未验证兼容性当成实现依据。

## 结论先行

这轮调研最值得落地的不是再复制一个“多模型反代”，而是把 MyToken 强化为**本地优先、来源可追溯、可接收网关账本的统一用量层**：

1. **P0：补 GitHub Copilot CLI 的 OTel JSONL 适配器。** `agent-cockpit` 已公开 `COPILOT_OTEL_FILE_EXPORTER_PATH` 的读取、trace/span 关联、旧新属性兼容和去重逻辑。相比逆向私有数据库，这条路径更稳定。
2. **P0：扩展现有 `internal/relay`，增加版本化的网关用量导入契约。** MyToken 已有 New API/Sub2API 的 ratio、balance、bills 读取，不应另起一套平行系统；LiteLLM、Portkey、Helicone、Otari 等来源可导出本地 NDJSON/OTLP/CloudEvents，并以 `(source_instance, source_event_id)` 幂等入账。
3. **P1：Cursor 先做证据分级，再决定本地估算或官方 CSV 导入。** Cursor 本地 SQLite/transcript 能提供 session、model、tool、agent 等元数据，但 token/cost 是估算值且缺 cache；reported usage 来自 Cursor 官方 usage export，必须作为显式授权的独立导入路径，不能包装成精确离线账本。
4. **P1：统一精细 token 与成本 provenance。** 最小规范应分开保存非缓存输入、cache read、cache write、可见输出、reasoning、供应商 reported total；现有 `UsageEvent.CostUSD` 继续只保存日志/网关直接报告值，估算留在 report/budget 层并补定价来源、版本和币种，不能把估算写回事件冒充账单。
5. **P1：把“会话”升级为图，而不只是字符串。** 父子会话、subagent、trace/span、agent role、工具类别应成为可选维度；聚合层按事件唯一性去重，不按父子关系猜测扣除。
6. **继续保持真实日志门禁。** 这些外部实现是很好的 schema 证据和交叉验证来源，但不能替代 MyToken 自己的脱敏真实日志 fixture。

### 调研边界

- 共纳入 20 余个项目/标准，按“本地统计—网关/计量—可观测性/协议”三层交叉搜索；优先直接读 parser、SQL/migration、协议类型和测试。
- GitHub 匿名 REST API 在调研中触发限流，批量 clone 也出现连接超时；后续改用可访问的 `raw.githubusercontent.com`、项目源码页面、npm 发布源码和官方规范。没有因网络失败把猜测补成事实。
- 本轮没有安装或运行第三方项目，没有访问用户日志、凭据或网关数据库，也没有修改 MyToken 产品代码。
- Stars、最近提交时间和 README 自述会快速变化，本报告不做数值排行榜；“活跃”只用于存在当前源码/测试的定性描述。许可证只在直接看到 LICENSE 或包元数据时提及。

## 一、项目地图

### 1. 本地用量统计 / 桌面统计

| 项目 | 类型与覆盖 | 源码层面值得借鉴 | 对 MyToken 的价值 |
|---|---|---|---|
| [ccusage](https://github.com/ryoppippi/ccusage) | CLI/库；Claude Code、Codex、Amp、OpenCode、Pi 等 | 以本地日志为事实来源；按日/月/session 聚合；提供 MCP/状态栏与价格更新 | 交叉验证聚合语义、状态栏和定价展示 |
| [CodeBurn](https://github.com/getagentseal/codeburn) | 本地 CLI/Web；覆盖多种 coding agent | [Cursor provider 文档与源码入口](https://github.com/getagentseal/codeburn/blob/main/docs/providers/cursor.md) 明确区分本地估算与官方 usage CSV，同一 SQLite 的 bubbles 用 `bubbleId`、agentKv 用 `requestId` 去重；[`package.json`](https://github.com/getagentseal/codeburn/blob/main/package.json) 声明 MIT | Cursor 的来源分层、稳定键和导入覆盖窗口值得借鉴；不把本地 chars/4 估算包装成账单 |
| [agent-cockpit](https://github.com/nashory/agent-cockpit) | Go 本地 TUI；多 agent | [Copilot parser](https://github.com/nashory/agent-cockpit/blob/main/internal/source/copilot/copilot.go) 从 OTel JSONL 建立 trace 上下文并兼容多代属性；[Kilo](https://github.com/nashory/agent-cockpit/blob/main/internal/source/kilo/kilo.go) 和 [Goose](https://github.com/nashory/agent-cockpit/blob/main/internal/source/goose/goose.go) 提供 SQLite 交叉实现 | Copilot CLI 是最成熟的新适配候选；Kilo/Goose 可用于 differential test |
| [Splunk token-meter](https://github.com/splunk/token-meter) | 本地 evidence scanner；agent session、tools、cost | [Cursor adapter](https://github.com/splunk/token-meter/blob/main/token_meter/runtimes/cursor.py) 读取 SQLite/transcript、隐藏原始 Composer ID、归一 MCP/tool 类别，但明确把本地 token 标为 estimated 且 cache/cost 不可得；[OpenCode adapter](https://github.com/splunk/token-meter/blob/main/token_meter/runtimes/opencode.py) 区分 `session_v2/session_message` 与旧 schema | Cursor 元数据/估算和 MyToken OpenCode v2 的交叉验证来源，不能作为 Cursor reported usage 证据 |
| [Agentic-Cost-Control](https://github.com/PrairieLearn/Agentic-Cost-Control) | 多来源收集 + SQLite + Web UI | [usage_collector.py](https://github.com/PrairieLearn/Agentic-Cost-Control/blob/main/usage_collector.py) 具有本地扫描、增量游标、文件身份和 normalized usage；[sqlite_store.py](https://github.com/PrairieLearn/Agentic-Cost-Control/blob/main/sqlite_store.py) 按 request 写账本 | `UsageEvent` 主干和 MyToken 很接近；其派生 request id 碰撞处理值得参考，但不能直接照搬弱哈希方案 |
| [TokenMeter](https://github.com/janreges/ai-token-meter) / [VibeMeter](https://github.com/jlucaso1/vibemeter) / [CK-Usage](https://github.com/ryoppippi/ck-usage) / [CodexBar](https://github.com/steipete/CodexBar) | 菜单栏、仪表盘、轻量 CLI | 分别强调多代理聚合、会话分析、账户/额度查询和状态栏体验 | 适合 UI、预算提醒、provider quota 展示研究；不应仅凭 README 采纳解析 schema |

### 2. 反代、路由、预算与计费

| 项目 | 解决的问题 | 值得吸收的设计 | 不建议复制的部分 |
|---|---|---|---|
| [LiteLLM](https://github.com/BerriAI/litellm) | 多供应商 OpenAI-compatible gateway、路由、预算、成本跟踪 | 统一 response usage、model/provider/route、回调式 spend tracking；网关能得到客户端日志没有的 HTTP 状态、重试、延迟和准确 request id | 不把 MyToken 变成又一个 100+ provider proxy；维护面、鉴权面和安全责任过大 |
| [New API](https://github.com/QuantumNous/new-api)、[One API](https://github.com/songquanpeng/one-api)、[Sub2API](https://github.com/Wei-Shaw/sub2api) | 渠道聚合、额度、用户/令牌/分组计费 | 账户、渠道、上游模型、倍率、配额和日志维度；可作为“网关事件来源” | 不直接耦合其内部 DB；优先稳定导出契约，避免随私有迁移破裂 |
| [Portkey Gateway](https://github.com/Portkey-AI/gateway) | AI gateway、路由、fallback、guardrails | route/upstream/retry/latency/error 维度；可将同一用户请求与多个上游 attempt 区分 | 路由执行不是 MyToken 核心职责 |
| [Helicone](https://github.com/Helicone/helicone) | proxy + LLM observability | request/response 关联、custom properties、成本/延迟/错误分析 | 云端或代理采集不应取代本地只读适配 |
| [Mozilla AI Otari](https://github.com/mozilla-ai/otari) | self-hosted OpenAI/Anthropic-compatible gateway、预算与 usage ledger | 当前 Python [UsageLog 模型](https://github.com/mozilla-ai/otari/blob/main/src/gateway/models/usage.py) 以唯一约束 `(source, source_event_id)` 保证外部事件幂等，区分 gateway 自产与 external-events/OTLP 导入；README 明确预算在 dispatch 前检查、usage 在响应后结算 | 不复刻代理执行面；复用“有 provenance 的导入账本 + 幂等结算”数据模型 |
| [OpenMeter](https://github.com/openmeterio/openmeter) | usage metering / billing | CloudEvents、subject attribution、幂等事件、meter 聚合、billing pipeline 分层 | 对个人桌面 app 过重；借事件契约，不引入整套计费平台 |

### 3. 可观测性与开放标准

| 项目/标准 | 关键内容 | MyToken 应如何使用 |
|---|---|---|
| [OpenTelemetry GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/) | 统一 `gen_ai.*` 请求、响应、模型、provider、usage、operation 名称 | 作为网关导入和 Copilot/未来 agent OTel 解析的兼容目标 |
| [GenAI metrics](https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-metrics/) | `gen_ai.client.token.usage` 明确 input/output histogram 和 cache 维度；当前仍标为 Development | 支持但不锁死：保存 raw attributes + schema version；不要把实验规范当永久数据库 schema |
| [GenAI agent spans](https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-agent-spans/) | agent id/name/version、conversation、operation、tool/agent lifecycle | 为 parent/child session、agent role、tool usage 提供跨产品公共词汇 |
| [OpenAI Agents SDK tracing](https://github.com/openai/openai-agents-python/blob/main/src/agents/tracing/span_data.py) | `GenerationSpanData` 含 input/output token details 和 `response_id`；agent/tool/handoff 等 span 类型统一 | `response_id` 是优先 request/dedup identity；trace/span 是会话图关联，不应直接当计费 id |
| [Langfuse](https://github.com/langfuse/langfuse)、[Phoenix](https://github.com/Arize-ai/phoenix)、[OpenLIT](https://github.com/openlit/openlit) | 开源 LLM observability / tracing / OTLP | 作为可选 OTLP/trace 导入来源；MyToken 保持 metadata-only，不复制 prompt/output 内容 |

## 二、建议的实现路线

### P1 候选：Cursor 本地证据 + 官方 usage export

当前 MyToken 的 32 个 harness identity 中没有 Cursor，覆盖缺口真实存在，但本地精确 token 的证据不足：

- Splunk `token-meter` 与 CodeBurn 都提供只读 SQLite/transcript 实现；本地可提取 session、model、tool、agent、`contextTokensUsed` 和输出字符数；
- 两者都明确把本地 token 视为 **estimated**：output 常按 chars/4，cache/cost 不可得，本地数值会显著低于 Cursor dashboard；
- reported usage 来自 Cursor 官方 usage export/同步，应作为显式授权的 CSV/import source，不能和本地估算混为一个账本证据等级。

**建议边界：**

- 先给现有 UsageEvent 增加 `reported|estimated` provenance，再决定是否发布 Cursor adapter；
- 本地路径只读 SQLite、query-only/WAL-safe；不返回 prompt/tool payload；官方 CSV 是独立 source；
- DedupKey 优先 `cursor:<composerId>:<bubbleId|requestId>`，不要用气泡位置索引；DB 与 transcript 的同一 bubble 使用同键覆盖；
- Composer ID、绝对项目路径做稳定哈希或 basename；
- fixture 必须覆盖 schema 漂移、NULL/动态类型、历史截断后重扫、DB+transcript 同事件、subagent fork、MCP tool、增量游标和原地更新。

### P0-1：GitHub Copilot CLI OTel JSONL

以 `COPILOT_OTEL_FILE_EXPORTER_PATH` 为显式入口，兼容目录/文件配置。参考 `agent-cockpit` 的源码，但重新按 MyToken contract 实现。实施顺序必须是：先用上游样例和合成记录写“旧属性 → 新属性 → UsageEvent”的 mapping 表与 golden，再写 parser：

- 优先 `response_id`；否则使用 `(trace_id, span_id, operation, timestamp)` 构造稳定键；
- 同一 trace 里同时存在 metric/log/span 记录时只选权威 usage 记录；
- 兼容新旧 OTel attribute 命名，但保存 schema/来源版本；
- 文件轮转、截断和 JSONL 尾部半行必须复用现有增量/幂等测试门禁。

### P0-2：扩展现有 relay 的网关用量导入契约

MyToken 已有 [`internal/relay`](../internal/relay)；它能从 New API/Sub2API 读取 ratio、balance 和 bills，并已经定义只读、少联网、不持久化 key 的隐私边界。因此应在这个深模块上增加**版本化本地导入源**，而不是新增一套平行 gateway 子系统或网络监听服务器。第一版支持一个或多个目录中的 NDJSON，后续再扩展 SQLite、OTLP file exporter 或 CloudEvents。

建议事件骨架：

```json
{
  "schema": "mytoken.usage.v1",
  "source": "litellm",
  "sourceInstance": "work-gateway",
  "sourceEventId": "req_01J.../final",
  "occurredAt": "2026-10-17T09:00:00Z",
  "status": "completed",
  "provider": "anthropic",
  "model": "claude-sonnet-4-5",
  "route": "anthropic-primary",
  "sessionId": "hashed-or-opaque",
  "requestId": "provider-response-id",
  "tokens": {
    "inputNonCache": 100,
    "cacheRead": 900,
    "cacheWrite": 50,
    "outputVisible": 120,
    "reasoning": 30,
    "reportedTotal": 1200
  },
  "cost": {
    "usd": 0.0123,
    "basis": "reported",
    "currency": "USD",
    "pricingSource": "gateway",
    "pricingRevision": null
  },
  "latencyMs": 842,
  "httpStatus": 200,
  "attempt": 1
}
```

必须满足：

- 唯一键为 `(source, sourceInstance, sourceEventId)`；不能只靠 timestamp/model/token 组合；
- `pending/streaming` 不落最终用量，或以相同 event id 原地替换；
- 一个用户请求的多次 upstream attempt 可分别记录，但要有共同 `requestId`，聚合时区分“用户账单”和“上游真实消耗”；
- 缺少 source event id 时只能标为弱证据，不能默默合并；
- 导入器不读取 API key、authorization header、prompt、response body。

### P1-1：精细 token 账本

当前 MyToken 的精确模型是 `Tokens{Input, Output, CacheRead, CacheWrite, Reasoning}`，`Tokens.Total()` 汇总五类；`UsageEvent` 另有 `Provider`、`BaseURL`、`RequestID`、日志直接报告的 `CostUSD` 与原生单位 `Bill`（见 [`internal/model/model.go`](../internal/model/model.go)）。下一步不是再造一套平行模型，而是增加 evidence/provenance，并明确 adapter 不变量：

```text
input_noncache + cache_read + cache_write
output_visible + reasoning
reported_total                  // 原样保存，允许与分项不相等
```

- 不要默认“input 包含 cache”或“不包含 cache”；为每个 adapter 写明确 mapping；
- 不要为了凑 `reported_total` 把差值随意塞进 output；差值应独立诊断；
- `reasoning` 是 output 的子集还是附加量必须由来源契约决定；
- unknown、missing、zero 三者严格区分。

### P1-2：reported cost 与 estimated cost 分离

保持当前深模块边界：`UsageEvent.CostUSD` 只存来源直接报告的 USD；本地计算仍由 report/budget 的 `EstimatedSpend`/price rule 负责，不把估算写回事件。建议只给估算与价格规则补 provenance：

- `PricingSource` / `PricingRevision` / `Currency`；
- provider + model + effective time 的价格键；
- 用户自定义价可覆盖 catalog，但必须展示 provenance；
- UI 同时有 reported 与 estimated 时按来源分行，不相加成一个不透明总数。

这样能吸收 ccusage/LiteLLM/OpenMeter 的优势，同时避免历史价格更新后把旧账重新算成新价格，也避免在 UsageEvent 中增加两个会漂移的平行成本字段。

### P1-3：会话图、agent 与工具维度

建议给 UsageEvent 添加可选关联，而不是增加多个彼此重叠的“父子总计”：

```text
TraceID / SpanID / ParentSpanID
ParentSessionID
AgentID / AgentRole
ToolName / ToolNamespace / MCPServer
OperationName
```

原则：

- 每个权威 provider request 只计一次；parent/child 只负责归属和展示；
- orchestrator 的 summary/aggregate 记录不重复计底层请求；
- 若 parent 自己也发请求，则作为独立 event 计数；
- tool payload、prompt、response 默认不进入 MyToken 数据库。

## 三、暂不建议做的事

1. **不内置完整 LLM reverse proxy。** LiteLLM/New API/Portkey 已有成熟生态，MyToken 应通过标准账本接入它们。
2. **不根据 Qoder/Trae 消息文本估算 token 后宣称“用量支持”。** 可以提供明确标注的 estimate 模式，但默认统计和预算必须只用权威 usage。
3. **不直接读取反代产品的内部业务数据库。** 内部 migrations 变化快，还可能包含用户、密钥、请求正文；应推动/提供脱敏 export adapter。
4. **不把 OTel Development 规范固化成唯一表结构。** 支持版本化映射与 raw metadata allowlist。
5. **不以“外部项目有 parser”替代真实日志验证。** 外部源码只能降低逆向成本，不能证明 MyToken 在真实环境不漏计。

## 四、建议的里程碑

### 0.4.x 后续补丁 / 0.4.2 候选

- 先补 evidence provenance UI/诊断：reported / estimated / synthetic fixture / real-log verified；
- Copilot CLI OTel JSONL adapter；
- Cursor 先做官方 usage CSV fixture 与本地 estimated fixture 的边界验证，满足 provenance 后再决定是否发布 identity；
- 用 agent-cockpit、Splunk token-meter、CodeBurn 做 differential fixtures，但每一条仍以 MyToken 自有 golden 固化。

### 0.5

- `mytoken.usage.v1` NDJSON 本地导入；
- LiteLLM/New API/Otari/OpenMeter 示例 exporter；
- reported/estimated cost 分层与 pricing revision；
- trace/session/agent/tool 可选维度；
- provider quota API 与本地 token ledger 明确分层，不混成一套“余额”。

### 0.6 以后

- OTLP/CloudEvents ingest；
- gateway request 与 local agent session 的跨来源关联；
- 团队 chargeback、多租户 subject、项目预算；
- 基于实际消耗的异常检测，而非扫描 prompt 内容。

## 五、证据等级与未解决问题

- **A级：** 官方标准或上游实现源码（OTel、OpenAI Agents SDK、Mozilla AI Otari 的当前 UsageLog 模型）。
- **B级：** 可访问的第三方 parser 源码且有测试/明确只读路径（Splunk token-meter、agent-cockpit、CodeBurn）。
- **C级：** README、启发式扫描或 token 估算；只能用于候选发现，不能支撑精确兼容声明。

仍需真实样本回答的问题：

- Cursor 不同版本/平台的本地 schema、历史截断与官方 CSV 覆盖窗口如何确定性合并，本地 estimated 事件和 reported import 如何避免重叠；
- Copilot CLI 的 OTel file exporter 在轮转、崩溃尾行和多个 metric/span 表达下的权威记录选择；
- LiteLLM/New API/Portkey 是否能稳定导出 provider response id、cache write 和 reasoning，而不需要读取请求正文；
- OpenCode DB 与 legacy JSON 并存时，冲突字段的确定性来源优先级；
- Qoder/Trae 是否存在未公开但稳定的权威 usage ledger。

因此，本报告给出的优先级是“下一步研究/实现顺序”，不是新的兼容性声明；所有新增 adapter 仍必须通过 fixture、conformance、增量、幂等、隐私、性能和脱敏真实日志门禁。
