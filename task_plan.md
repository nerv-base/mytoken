# GitHub 开源生态大规模调研（2026-10-17）

## 目标
- 系统搜索 GitHub 上的用量统计/本地日志解析、LLM API 网关与反向代理、成本/配额跟踪、Coding Agent 会话存储、增量去重与隐私处理实现。
- 只采用可回溯到官方仓库、源码、schema 或第一方文档的一手证据；二手项目只作为候选入口，不能独立支撑结论。
- 输出单一研究报告 `docs/GITHUB_ECOSYSTEM_RESEARCH.md`，包含仓库链接、机制、可复用设计、风险、MyToken 适配优先级和真实日志候选；本轮不修改产品代码。

## 阶段
1. [completed] 定义搜索矩阵并搜集候选仓库；覆盖本地统计、桌面应用、反代/网关、计量/计费、LLM 可观测性与开放标准。
2. [completed] 深读高价值仓库的 parser、SQL/migration、用量归一、去重/游标和隐私机制。
3. [completed] 交叉验证实现真实性与 MyToken 适配价值；README/构建产物/估算器均降级标注，不冒充官方 schema。
4. [completed] 汇总报告、P0/P1/P2 优先级和后续验证清单。
5. [completed] DeepSeek Flash 只读复核引用、事实与路线；已修正 Cursor 本地估算、CodeBurn/Otari owner、现有 relay 复用、成本分层、稳定 DedupKey，并删除不可核来源，更新 progress/findings。

---

# MyToken 0.4.x Harness 补齐实施跟踪

## 当前目标与授权（2026-10-14）
- 用户已明确要求开始 0.4.x，并授权使用 nerv-base/deepseek-flash 队友；队友不可用时使用该模型子代理。当前 Agent Teams API 不暴露成员模型参数，创建后必须核对实际模型；若不是指定模型则停止委派实现。
- 目标：完成 docs/ROADMAP.md:209-217 的调研、可离线适配、兼容性与真实日志证据；只能联网获取用量的项目形成可核验结论并转入 0.5。
- 不默认访问真实用户日志、凭据或 harness 配置；测试使用合成/脱敏 fixture。新行为严格 TDD，实施后先规格审查、再代码质量审查。
- 不推送、不发布。使用独立功能分支；保护当前未跟踪的历史验证资料。

## 0.4.x 阶段
1. [completed] 盘点现有 28 个 harness、解析器契约、真实日志证据和产品名称边界。
2. [completed] 并行调研 ZCode/MiniMax Code/Step Code/MiMo Code、Trae/Qoder、Kimi Code、omp/omo、Hermes/OpenClaw/Antigravity。
3. [completed] Kimi Code、OMP、OMO/senpi、ZCode、MiniMax、MiMo、Step 已按 TDD 实现；共享身份、registry/provenance 与 GUI 展示已接线为 32 个 harness。
4. [completed] 已完成多轮规格/质量审查及全部可复现 Critical/Important 修复；修复后独立全量 test/vet/race/build/benchmark/diff-check 全绿，残留风险已写入验证文档。
5. [completed] Trae/Qoder 因无稳定公开离线 usage schema 明确延后；仅在线用量转 0.5。现存 harness 的真实日志缺口保留为发布门槛。
6. [completed] 已更新 ROADMAP/README/CHANGELOG/HARNESS 与证据矩阵，明确区分“协议+fixture”“脱敏真实日志”“仅调研”。

## 错误与阻塞
- 初次 `spawn_teammate` 无模型选择能力，误启动 sol 后立即全部停止且未写代码；后续仅用可显式固定 `nerv-base/deepseek-flash` 的 workflow。
- `workflow-12` Step 首次返回空数组，未执行任务；已核对无 diff，并以 `workflow-13` 重启。
- web_search provider 曾返回 401，不重试；一手研究改用官方仓库/API/raw URL。

---

# 历史记录：MyToken 0.4.0 实施跟踪

## 目标与授权
用户已完成 grilling 并明确授权开始实现完整 0.4.0，至待发布候选；后续 0.4.x harness 批次另行确认。每批验证通过自动继续；重大数据、隐私、破坏性或范围决策询问用户。不自动提交、推送或发布。
用户已授权 workflow。子代理仅可使用 nerv-base/deepseek-flash，禁止 step-5-preview；无法明确指定模型则不委派。主代理负责拆解编排、依赖协调、代码审查与独立验证；实现和修复交由 DeepSeek。共享目录按互不重叠的写入范围分配，不创建 teammates。

## 确认的契约
- 独立升级 MyGo v0.4.8，先保持行为与界面建立基线，再按 ROADMAP 迁移图表和控件，保留主题，不做 0.5 视觉改版。
- 0.4.0 日常功能全部纳入；Canvas/editor/DiffView 可选且不阻塞。
- 同规范化端点合并供应商用量总览，保留 Key/账单/价格边界，不盲加余额。不同路径/域名不猜测合并。
- 历史迁移先一致性备份；token、请求数、历史费用保持不变；备份失败不迁移，校验失败回滚停止。
- 多实例有稳定身份，显式区分新增、搬迁、副本；不因内容相同自动跨实例去重。第9轮用户确认：声明为备份副本的目录保留配置和诊断但不采集、不计数；转独立实例需显式操作，搬迁沿用身份。
- Claude 额度默认禁用，GUI/CLI 共用持久授权，明确告知凭据来源及请求目标；凭据不复制入库、不写日志，禁用停止自动请求。
- 预算依据本地估算费用，未知价格明确标注，实际扣款独立；全局/项目/供应商，日/周/月，默认80%/100%阈值可调，每周期每阈值通知一次，不拦截，不混币种。
- 固定持久统计时区，默认首次系统时区，周一开周；环比用上一周期同等已过时段。
- 托盘速率是最近60秒入账输出token/s，不是生成速度；历史补扫不计，无法判断实时或过期显示不可用。
- 快照默认只读，无隐式扫描/联网，包含新鲜度、缺失原因；无数据不伪装为零，刷新显式执行。
- 真机、读屏、真实日志证据缺失列为发布门槛，不声称通过；允许交付候选。

## 阶段
1. [completed] 仓库约束、构建/测试/性能基线；制定升级批次详细计划。
2. [completed] MyGo升级与兼容回归，记录截图/性能和未验证门槛。
3. [in_progress] 稳定供应商身份、安全备份迁移与一致性校验。
4. [in_progress] 统一多实例路径、监听/游标生命周期和采集诊断。
5. [in_progress] 固定时区与版本化快照契约、GUI/CLI共用查询。
6. [pending] statusline、托盘实时刷新、费用预算与通知。
7. [pending] 日/周/月/会话/项目/分支报表及环比。
8. [pending] Codex离线额度、Claude授权额度、消耗速度与预测。
9. [in_progress] 分批原生图表/控件迁移（升级基线后可与业务批次排序）。
10. [pending] 综合回归、文档和候选发布门槛清单。

## 实施原则
每个子系统先检查现有接口，再写详细任务计划。新增行为先测试失败，再最小实现并回归。保护用户已有 docs/ROADMAP.md 修改与未跟踪 docs/MYGO_UPDATE_RESEARCH.md。不自动使用真实凭据或修改 harness 配置。

## 错误与阻塞
升级批次测试/vet/本机及四目标交叉编译均通过；46张截图尺寸匹配，36张逐字节相同，另外10张每张仅7–13像素差异。详见 docs/verification/v040-mygo-baseline.md。真机交互/读屏/GPU/安装更新仍为发布门槛。供应商身份阶段已实现保守规范化、WAL一致性备份、启动迁移前备份guard、事务内逐请求token/历史费用指纹校验、端点元数据及失败回滚；已禁止Resolver启动自动重写历史归因/清定价指纹。新增采集写入、共享端点聚合、ID过滤和显示别名后端已通过功能回归；此前严格性能4项超出100ms，已由workflow-16修复并经第11轮Lead独立22/22复测通过。供应商GUI/CLI接线及后续修复已完成全量独立回归，尚有会话drilldown和截图验收。当前workflow-23/24推进多实例配置和数据层，未端到端接入。阶段3/4仍进行中。

### 第9轮协调与审查
- workflow-16性能修复仍运行；workflow-17 GUI实现仍运行，写入范围互斥。界面/命令行基于已有API先实现，不代表绕过性能验收，合并验收在全部停止写入后进行。
- workflow-18 CLI实现已收集，增加provider list、alias set/clear、--provider-id及stats --by endpoint，保留旧provider语义；尚未由Lead接受。
- Lead审查internal/app/app.go:92发现OpenLocal也无条件EnsurePrices，导致新list/alias命令可能重算历史费用/替换pricing_fingerprint，违背显示别名契约。启动workflow-19，独占internal/cli及其验证文档，要求先用故意过期指纹复现，再避开App定价启动；不改legacy stats行为。红测回放（移走实现文件）不当作实施前TDD证据。
- 第10轮：workflow-19已收集，故意过期指纹红例证明list/alias会重计价；现改为paths.DBPath→store.Open，避开App.EnsurePrices。Lead已读helper，仍需独立复测；store.Open可迁移，不能声称只读快照。
- 又发现addCommonFlags让reconcile默默接受并丢掉provider/provider-id。workflow-21独占CLI及验证文档，补解析红例并拒绝不支持的参数，不改账单算法。
- workflow-17已收集，GUI分组/别名编辑已实现但main.go未接Hook。workflow-22独占GUI、main.go及验证文档，补真实入口，同时修复异步保存A完成后误清B编辑框/显示A状态，并让详情可辨认完整ID而非声称10hex前缀绝不冲突。
- workflow-20独占internal/harness及专用文档，仅实现多实例前置的新rooted parser工厂，不改模型/扫描/schema，不决定尚未确认产品语义；默认注册表不变、显式空roots不回退。
- workflow-20已收集：28/28注册harness支持可选Rooted.WithRoots，新NewRooted在解锁后构造，不改Parser接口；显式空roots不fallback，补Roots防别名返回。Lead独立bash-8013：go test -count=1 ./internal/harness/...、registry race、vet全部exit0（race1.459s）。此接口尚无生产调用，不冒充多实例完整交付。
- workflow-21已收集：reconcile明确拒绝两类provider参数（7个解析红例转绿），Origin+KeyID逻辑不改，全部commonFlags调用点已审计；CLI独立合并回归仍待store写入结束。
- workflow-16已收集：新增provider_daily/provider_cache_daily派生rollup与单独provider.endpoint-rollup.v1迁移标记，声称22项严格性能3次通过（四个旧失败项0.6–3.7ms，忙机最差32.6ms）；这是代理报告，Lead尚未独立严格复测或完成迁移代码复审。不得照收其“无待办用户目标”结语，完整0.4.0仍有大量未完成项。
- 后端/CLI写入冻结后Lead独立bash-8059：go test -count=1 ./internal/query ./internal/store ./internal/cli及对应vet全部exit0（query5.916s、store48.072s、cli11.038s）。包括未跟踪文件的测试，未跟踪并不排除go test执行。
- 第11轮workflow-22已收集：main.go接实际Store别名Hook并补root测试，详情完整ID、异步完成不误改其他选择。全部写入停止后Lead独立bash-8091严格性能22/22 PASS：TotalsIDFilter3.520291ms、ProviderGroups3.848709ms、ProviderGroupsIDFilter4.065541ms、CacheTotalsIDFilter0.78225ms；再全量go test -count=1 ./...、vet、diff-check均exit0（GUI35.646s，store42.077s）。不是只有代理报告。迁移写前摘要/单独rollup备份/边界日facts降级已读审；真实磁盘故障和真机门槛未关闭。
- 已启动workflow-23，独占internal/instance及专用文档，实现稳定ID配置、显式路径校验、排除backup-copy；不触碰正在做的数据层。
- 已启动workflow-24，独占internal/model、store、query及专用文档，实现安全实例维度迁移、游标/session/rollup/查询隔离；原始dedup_key不改，旧调用default兼容，保留100ms性能门槛。两任务运行中，后续先收集审查再协调scanner/app/GUI/CLI接线。
- 供应商GUI目前只有成员详情，按ID到会话列表的进一步drilldown及新截图仍需补验，阶段3不冒充全部完结。剩余多实例接线/快照/预算/报表/额度/原生控件仍未完成。
