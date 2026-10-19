# GitHub 开源生态调研 Findings（2026-10-17）

- 完整报告：`docs/GITHUB_ECOSYSTEM_RESEARCH.md`。候选均按官方标准/上游源码、活跃第三方 parser、README/构建产物/估算器三级证据标注；没有把营销描述或外部 parser 当作 MyToken 已兼容/真实验证。
- 最优下一步是先补 usage/cost evidence provenance，再做 GitHub Copilot CLI OTel JSONL 和版本化的本地网关 usage 导入；Cursor 先验证本地 estimated 与官方 CSV 的边界再决定是否发布 identity。不要把 MyToken 改造成完整反代。Otari 的 `(source, source_event_id)` 幂等账本、OpenMeter/CloudEvents 和 OTel GenAI attributes 提供了可复用边界。
- token 账本必须区分非缓存输入、cache read、cache write、可见输出、reasoning 与 reported total；`UsageEvent.CostUSD` 继续只存来源直接报告值，估算留在 report/budget 层并带 pricing revision。parent/child、trace/span、agent/tool 只做归属，每个权威 provider request 只计一次。
- 高价值交叉实现：`nashory/agent-cockpit` 的 Copilot OTel/Kilo/Goose parser，`splunk/token-meter` 的 Cursor/OpenCode parser，`getagentseal/codeburn` 的 Cursor 本地估算与官方 CSV 分层，以及 `PrairieLearn/Agentic-Cost-Control` 的 request ledger。它们用于 differential fixture，不替代脱敏真实日志门禁。
- 最终 DeepSeek Flash 复核发现并纠正来源污染：Cursor 本地 token 只有 estimated，不能称 reported；CodeBurn 正确 owner 为 `getagentseal`；Otari 正确 owner 为 `mozilla-ai` 且当前唯一约束在 Python `UsageLog`；不可访问或 owner/path 不一致的 Kilo-Usage、wb-token-meter 不再作为最终证据。Qoder/Trae 的保守延期仍由官方未公开稳定 usage schema 与既有一手调研支撑。

---

# 0.4.x Findings

## 当前基线
- 基线 HEAD a9684c7（tag v0.4.1，origin/main 同步）声明支持 28 个 harness；当前未提交分支已接线 32 个独立身份。
- docs/ROADMAP.md:209-219 已回填 0.4.x 结果：协议适配/研究项完成，脱敏真实日志门槛保持未勾选。
- README 中 Amp、Kiro、Zed、Antigravity、Codebuff、Mux、Hermes、OpenClaw 以及本批新增/扩展 adapter 均明确标注尚无仓库内可审计的脱敏真实 fixture。
- 当前工作树已有未跟踪的 0.4.0 计划/验证资料；不得误删或覆盖。

## Kimi Code 一手证据（2026-10-16）
- 旧 `MoonshotAI/kimi-cli` 已归档并指向 TypeScript 后继 `MoonshotAI/kimi-code`；现有 `~/.kimi` 解析器不能直接作为新版兼容证据。
- 官方数据目录文档：`KIMI_CODE_HOME` 默认 `~/.kimi-code`；会话为 `sessions/<workDirKey>/<sessionId>/agents/main/wire.jsonl` 和 `agents/<subagentId>/wire.jsonl`，不同于 legacy `wire.jsonl` / `subagents/`。
- 上游协议源码/schema 显示 `usage.record` 与 `context.append_loop_event` 的 `step.end` 会表达同一笔 usage；离线适配必须只选 `usage.record` 作为权威计量，避免双计。当前验证 fixture 为仓库合成数据，不冒充官方或真实安装 fixture。
- 新 usage 字段为 `inputOther`、`output`、`inputCacheRead`、`inputCacheCreation`；`usage.record` 还带 `model`、`usageScope`、毫秒 `time`。当前上游 `UsageAgentModel` 会把每一条记录无条件累加进 `byModel`，`turn` 与 `session` 是单次请求来源的互斥标签而非累计/明细重复表示；二者都必须计入，缺省 scope 按 `session` 兼容。
- `turn.prompt.input[].text` 可生成标题；`config.update.cwd` 只应取 basename 作为项目名，不能泄露完整路径；`metadata.created_at` 可补会话起始时间。
- 基线 `go test ./...` 已通过（exit 0）。

## OMP/OMO 一手证据（2026-10-16）
- OMP 官方仓库 `can1357/oh-my-pi` 明确是 `badlogic/pi-mono` fork；官方 `packages/coding-agent/src/session/session-entries.ts` 仍定义 `type:"session"` JSONL header、`type:"message"` assistant usage，以及 `type:"model_usage"` 辅助记录。当前 MyToken Pi 解析器读取 assistant message usage，不应把 `model_usage` 作为额外请求计数。
- OMP 官方 `packages/utils/src/dirs.ts` 定义默认会话目录 `~/.omp/agent/sessions`，并支持 `PI_CODING_AGENT_DIR` 覆盖；profile 由 `OMP_PROFILE`（优先）或 `PI_PROFILE` 选择，Linux/macOS 的已迁移 XDG 数据目录为 `$XDG_DATA_HOME/omp/sessions`。凭据/agent.db/stats.db 与会话目录分离，不应读取。
- OMO（`code-yeongyu/oh-my-openagent`，homepage `https://omo.dev`）有 OpenCode plugin 与 senpi 两种宿主面，不应添加独立 harness：OpenCode plugin 复用 OpenCode identity/parser；senpi 会话复用 Pi 格式。官方源码 `packages/omo-senpi/src/components/agent-home/resolve-agent-home.ts` 明确 branded root `~/.omo/agent/sessions`、standalone fallback `~/.senpi/agent/sessions`、旧版 flat `~/.omo/sessions`，并按 `OMO_CODING_AGENT_DIR` → `SENPI_CODING_AGENT_DIR` → `PI_CODING_AGENT_DIR` 优先；`packages/memory-core/src/search/senpi-session-provider.ts` 明确 session header/message JSONL 契约。仅计这些 session transcript，不读编排 `children`/event log，避免父子结果重复入账；真实脱敏日志仍是发布门槛。
- OMP、OMO/senpi roots 与 Step Code 独立 parser 已依次完成，避免了共享 Pi package 的并发写冲突；Pi/OMP/OMO 只扫描权威 session transcript，Step 保留独立 `step-code` 身份、roots 和 dedup 前缀。对应聚焦、全量与 race 测试均通过。

## 现有真实数据证据审计（初步）
- `internal/harness/zed/realdata_test.go` 与 `internal/harness/antigravity/realdata_test.go` 只有 `MYTOKEN_REALDATA=1` 条件启用的本机扫描；默认 CI 会跳过，仓库中没有对应脱敏真实 fixture。
- Amp、Kiro、Codebuff、Mux、Hermes、OpenClaw 当前只有 conformance fixture；未找到 `realdata_test.go`。因此 README 的“未真实数据验证”对前四项仍然成立，ROADMAP 的 Hermes/OpenClaw/Antigravity 补证据项也仍未完成。
- 现有 conformance fixture 证明解析契约、增量/幂等/隐私等测试可以跑通，但不能等同于当前上游版本真实日志兼容。

## workflow-3 一手调研摘要（2026-10-16）
- **ZCode**：官方仓库 `https://github.com/zai-org/ZCode`；独立 Electron/Web/TUI harness。官方 FAQ `https://zcode.z.ai/cn/docs/qa` 记录 `~/.zcode/v2/config.json`（供应商/Base URL/模型，可能含 API key）与加密 `credentials.json`；第三方 schema 交叉证据 `https://ccusage.com/guide/zcode/` 指向 `~/.zcode/cli/db/db.sqlite`，`model_usage`（`session_id`, `started_at` epoch ms, `model_id`, `status`, `input_tokens`, `output_tokens`，cache 列可选）及 `session`。只读 query-only，过滤 completed；input 可能已包含 cache，必须按列扣除并做字段可用性探测；绝不读取 credentials。支持 `ZCODE_HOME` 多根的事实需再以官方源码核对后实现。
- **MiniMax Code**：官方文档 `https://agent.minimax.io/docs/code/welcome`、`https://agent.minimax.io/docs/cli/quick-start`，仓库 `https://github.com/MiniMax-AI/minimax-code`。官方源码 `packages/config/src/data-dir.ts`：默认 `~/.minimax`，兼容 `~/.mavis`，profile 根 `~/.minimax-<name>`，环境变量 `MINIMAX_DATA_DIR` / `MAVIS_DATA_DIR`；`packages/local-runtime-v2/src/infra/db/client.ts`：`<root>/v2/sqlite/runtime-state.sqlite`。表 `local_runtime_token_usage` 有 session/turn/model/时间/input/output/cache 等字段；需 query-only、schema 探测、增量 cursor 与隐私测试。
- **MiMo Code**：官方仓库 `https://github.com/XiaomiMiMo/MiMo-Code`，README 明确 terminal-native coding assistant，基于 OpenCode fork并新增 memory/orchestration；官方 `packages/cli/src/storage/db.ts`、`storage/schema.ts`、`session/session.sql.ts` 证明数据库为 `mimocode*.db`，表/MessageV2 usage 与 OpenCode v2 同源但文件名、roots 和产品身份独立。已实现独立 `mimo-code` parser，并抽取 OpenCode 只读 SQLite 深模块；只有官方源码 + 合成 fixture，尚无脱敏真实库。
- **Step Code**：后续一手仓库核对确认官方 `stepfun-ai/Step-Code` 是 Pi facade；`packages/coding-agent/src/config.ts` 与 `step/environment.ts` 定义默认 `~/.stepcode/agent/sessions`、`STEP_CODING_AGENT_DIR`、`STEP_CODING_AGENT_SESSION_DIR`，session-manager 保持 Pi v3 JSONL 形状。应复用 Pi 解析深模块但保留独立 `step-code` 身份/roots/dedup，不能按模型供应商配置处理。
- **Trae**：一手文档 `https://docs.trae.ai/ide/traework-to-traecode-data-migration` 与 `https://docs.trae.cn/cli_about-trae-code-cli-2` 确认桌面 IDE 的 macOS `~/Library/Application Support/Trae/ModularData`（中国版 `Trae CN/ModularData`），CLI 2.0 另有 `~/.trae`/平台 config；文档未稳定公开可离线读 token/cache/model usage ledger。旧 TraeWork 卸载可能删除本地未迁移数据。当前只做调研，不宣称支持；联网用量归 0.5。
- **Qoder**：官方文档站 `https://docs.qoder.com/` 主要描述桌面/CLI/IDE 功能与配置，但未给出可依赖的离线 token usage schema；当前不新增 parser，需真实脱敏日志或官方 schema 后再做，联网接口归 0.5。
- **Kimi Code**：新 TS CLI（`https://github.com/MoonshotAI/kimi-code`，npm `@moonshot-ai/kimi-code` latest 2.2.0）与旧 Python `~/.kimi` 不兼容；高风险双计：父 wire 的 `subagent.completed.usage` 与 `agents/<id>/wire.jsonl` 双写，迁移不删旧目录且 session id 变为 `ses_<uuid>`。现有 Kimi identity 已扩展新 layout，只计权威 `usage.record`，字段映射为 `inputOther/output/inputCacheRead/inputCacheCreation/model/usageScope/time`。
- **OMP/OMO**：OMP 是 Pi 进程内扩展，不自建 token 数据面，已复用 Pi parser/root；OMO 已改名 oh-my-openagent，senpi 子会话是 Pi 格式，同样复用 Pi；OpenCode plugin 模式继续复用 OpenCode。编排 `children/.../sessions` 与 `logs/...` 是重复表示，不能同时计；两者都不新增 harness identity。
- **现实日志证据**：Amp/Kiro/Zed/Antigravity/Codebuff/Mux 仍只有 fixture 或条件 `MYTOKEN_REALDATA=1` 本机扫描，未提交脱敏真实 fixture；Hermes/OpenClaw 也只有 fixture。应在文档中明确“fixture/conformance ≠ 真实验证”，不能勾选真实覆盖项。

## 最终复审结论（2026-10-16）
- 复审用仓库外合成 SQLite 端到端复现并修复了三项 OpenCode 问题：NULL/空 `session_id` 未回退 payload 会让 store 拒绝整批；NULL message id 会卡住 keyset 且隐藏后续行；越界/Inf 数字会饱和为 `math.MaxInt64` 并令账本 `SUM(input)` overflow。正式测试覆盖 parser、sqlitecore 与 scanner/store ingest 路径。
- DB 与迁移残留 legacy JSON 同时出现相同 message id 时仍只写一个 dedup event，但并行 source 完成顺序会影响冲突字段最终取值；在不修改全局 scanner/store 排序契约的本批中保持为已记录残留风险。真实上游是否会留下内容冲突的同 id 双表示仍需脱敏样本验证。
- 协议正确性与合成 fixture 验证完成不等于真实安装验证；`docs/ROADMAP.md` 的脱敏真实日志门槛继续保持未勾选。

---

# 历史记录：0.4.0 Findings

## 已核验仓库事实
- 工作目录 /Users/zzstar/Developer/Programs/MyToken!!!!!。
- 初始 git status：docs/ROADMAP.md 已修改；docs/MYGO_UPDATE_RESEARCH.md 未跟踪。均属于开始前已有改动。
- go.mod:3 要求 go 1.27.1；go.mod:6 已从 github.com/egoist/mygo v0.3.6 升为 v0.4.8，依赖图无其他改变。
- docs/ROADMAP.md:138-207 定义0.4.0；209-217是后续harness批次。
- 原internal/attrib/attrib.go:60-73在构造Resolver时执行两种既有归因迁移，migrateBareGeneric会清空pricing_fingerprint。round2回归证实风险后已移除自动调用；新请求Resolve不变，历史归因/价格指纹保留。
- internal/scan/scanner.go:79-89 默认最多4 workers，定期2分钟，debounce250ms，minGap2秒；watch目录上限2048。
- internal/cli/options.go:43-51 已支持 timezone、offline、json等通用flag。timezone当前默认local，修改默认必须保留显式覆盖语义。
- 未找到 AGENTS.md。

## 证据边界
MyGo升级基线已通过，详见 docs/verification/v040-mygo-baseline.md；真机/读屏/GPU/安装更新仍未验证。端点身份和Backup基础、启动迁移guard/不变量回滚测试已通过；尚未接入新采集和查询归并，因此不能声称供应商归并功能完成。

## 身份与迁移接口事实
- internal/source/credentials.go:77 的 Origin 会补默认https并删除全部路径，不适用于供应商展示身份；必须保持账单语义不变，新增provider包独立规范化。
- internal/store/compact.go:8 的 dimensionColumns 包括harness/session/project/model/provider/base_url/resolved_provider/attrib等；event_data引用event_dimensions，events是兼容view，新增身份不能修改历史定价维度。
- internal/store/store.go:44 的 Open 会先建基本schema再执行migrate；新备份guard必须插在历史迁移之前，不能在Open返回后才备份。round2已在migrate事务之前接入prepareProviderMigration；只要events有历史记录且为磁盘库，必须先成功创建旁路.pre-provider-v1.db备份。
- internal/provider/identity.go 实现严格显式http(s)端点、默认端口和单尾斜杠规范化；不猜/v1等价，不合并租户路径，不接受userinfo/query/fragment/点段/本地代理/模糊数字IP。
- internal/store/backup.go 使用VACUUM INTO包含WAL，O_EXCL+0600保留已有目标，Sync后只读quick_check；备份失败返回错误并保留失败文件，不自动删除。还需在迁移协调器实现事务不变量、版本记录、失败停止与备份路径分配。
