# GitHub 开源生态调研 Progress（2026-10-17）

- 已启动持久目标 `goal-02cfcd94-ca8a-41bf-a84e-a58dd2f0346e`，范围为用量统计、日志解析、反代/网关、成本配额、Agent 会话存储、去重/增量/隐私等 GitHub 开源实现。
- 本轮仅研究并写入 `docs/GITHUB_ECOSYSTEM_RESEARCH.md`，不改产品代码；优先官方仓库、源码、SQL/migration 和开放标准证据。
- 首个 9 路 DeepSeek Flash 搜索工作流运行约 40 分钟无返回后已停止；GitHub REST API 触发匿名限流、直接 `git clone` 连接超时，因此不重试相同路径。主代理改用可访问的一手 raw 源码、发布包源码和标准文档完成大范围核验，并启动两个显式 `nerv-base/deepseek-flash` 只读 reviewer 做最终事实/架构复核。
- 已核验 ccusage、CodeBurn、agent-cockpit、Splunk token-meter、Agentic-Cost-Control、LiteLLM、New API/One API/Sub2API、Portkey、Helicone、Mozilla AI Otari、OpenMeter、Langfuse/Phoenix/OpenLIT、OpenTelemetry GenAI 与 OpenAI Agents SDK 等生态来源。
- DeepSeek Flash 复核实际复现并纠正了来源问题：Cursor 本地 token 只是 estimated；CodeBurn 正确仓库为 `getagentseal/codeburn`；Otari 正确仓库为 `mozilla-ai/otari` 且当前是 Python；不可访问或 owner/path 不一致的 Kilo-Usage、wb-token-meter 链接已从最终报告删除，不再作为证据。
- 当前报告结论：P0 优先 Copilot CLI OTel JSONL、在现有 `internal/relay` 上增加版本化网关 NDJSON 导入；P1 先补 usage/cost provenance，再考虑 Cursor 本地估算 + 官方 CSV 双来源与 session/trace/agent/tool 图；不建议把 MyToken 改成完整反代。
- 最终复核要求已落地：Cursor DedupKey 明确使用 `composerId + bubbleId/requestId` 而非位置索引；`UsageEvent.CostUSD` 继续只存 reported，estimated 留在 report/budget 层；网关导入复用 relay；报告不再含不可访问的 Kilo-Usage/wb-token-meter 证据。

---

# 0.4.x Progress

## 启动
- 当前 HEAD 为 v0.4.1（a9684c7），0.4.0 主体已发布；0.4.x 重点是 harness 补齐和真实日志验证。
- 用户授权 Agent Teams，指定 nerv-base/deepseek-flash；实现须遵循 TDD 和两阶段审查。
- 已建立持久目标 goal-3a20761d-107f-4dbc-99c6-ce71df8f2872（最多 30 轮）。
- 已建立独立分支 `feat/0.4x-harnesses`；无提交、推送或发布。
- 初次调用 `spawn_teammate` 时接口无法指定模型，实际创建成 sol；发现后立即停止 4 个成员，未产生文件改动。此后所有研究、实现、审查与修复 workflow 均显式固定 `provider=nerv-base, model=deepseek-flash`。
- Kimi Code、OMP/Pi、OMO/senpi、ZCode、MiniMax Code、MiMo Code、Step Code 均已实现；共享 model/app/registry/provenance/UI 显示名与颜色已接线为 32 个 harness。
- 一手源码确认：MiMo Code 使用独立 SQLite `mimocode*.db`；Step Code 是 Pi fork，默认 `~/.stepcode/agent/sessions`；OMO/senpi roots 为 `~/.omo/agent/sessions`、`~/.senpi/agent/sessions` 与旧 `~/.omo/sessions`。Trae/Qoder 无稳定公开离线 usage ledger，已明确延后；若只能在线获取则转入 0.5。
- 多轮规格/质量审查的可复现问题均已按 TDD 修复：ZCode WITHOUT ROWID/错误游标推进/无限 pending 淘汰与新行饥饿/静态 backlog drain/rowid reuse，MiniMax open/tx 游标与零成本，MiMo XDG，共享 SQLite 缺表/Scan 失败游标，OpenCode v1 `part.data`/字符串 payload、NULL/空 session fallback、NULL message id 阻塞，以及越界 Num 饱和污染账本。ZCode cursor 最终为 v4，按 500 rowid 分块复查全部 pending，并持续排空 20000 行以上静态 backlog。
- 最后一轮修复后独立验收全部通过：`go test -count=1 ./...`、`go vet ./...`、`go build ./...`、`git diff --check`、`go test -race -count=1 ./internal/harness/... ./internal/app ./internal/model`，以及 Kimi/Pi/Step/ZCode/MiniMax/MiMo/OpenCode conformance 和 ZCode pending benchmarks 均 exit 0。
- ROADMAP/README/CHANGELOG/HARNESS 与 `docs/verification/v04x-harness-matrix.md` 已更新；所有新增/扩展 adapter 仍只有官方源码/schema/fixture 形状 + 本仓库合成 fixture，明确未声称脱敏真实日志验证。

---

# 历史记录：0.4.0 Progress

## Goal round 15：独立全量验收与应用接线
- workflow-29/30已收集。scanner.NewWithInstances按harness+instance+path隔离游标/去重，共享物理watch；空列表不回退。store预检增加PK/unique/trigger存在检查与回滚测试。Lead审查发现它仅检查key包含instance_id、trigger包含字符串，不是完整规范DDL等价验证；更强的部分schema检测仍可补强，不宣称任意schema损坏均可检测。
- bash-8895独立全量go test -count=1 ./...、vet、diff-check exit0（GUI36.071s/store48.124s）。bash-8921严格22子例全部PASS：ProviderGroups3.834792ms、ID totals3.719833ms、ID cache0.647125ms；scanner race23.978s/quota race1.349s均通过。代理曾称23/23，不采用其计数。
- 原生Pie/SegmentedBase首批已入库；Lead查看zh-overview合成图，布局主题保持。环形间隔改为恒宽、Bezier光栅存在有界差异，不声称逐像素等价；真机可访问性仍待验证。
- Lead读quota/codex.go发现carry会混合不同记录limitID/credits和旧窗口，tier优先会盖过较新无效记录。暂不接受额度语义；workflow-32 quota worker负责真实红测及保守溯源修复，另一个GUI worker负责instance-safe session/cache钻取、provider ID会话入口和分段控件无障碍名称。
- workflow-31两独占任务正在执行：internal/app/**接sources.json->Resolve->NewRooted->NewWithInstances并暴露诊断；internal/store/**+internal/paths/**只读快照基础（不能使用App.OpenLocal或Store.Open，不能用immutable忽略WAL，不能创建sidecar）。这些不是已完成的端到端功能。
- 无提交/发布/真实数据访问；完整目标继续active。尚待31/32收集及独立验证。

## Goal round 14：根路径前置验收与新批次
- workflow-28已收集：28个harness具有RootProvenance，instance.DefaultFallbacks/Resolve按user/env/default来源处理；显式env无效fatal，内建默认缺失非fatal。Lead已读Resolve接口并独立bash-8728：所有harness+instance测试、registry/instance race（1.683s/1.619s）和vet全部exit0。代理红证据为后验mutation，不记录成实施前TDD。
- workflow-29仍运行：scanner实例接线及store部分schema安全检查，两个独占范围；结束后才允许app组装和store只读入口修改。
- workflow-30已启动两互斥任务：internal/gui/**首次原生Pie/SegmentedBase控件迁移（不改业务接口和主题）；internal/quota/**离线Codex rate_limits纯读取/预测基础（无联网/凭据/真实日志）。后者不改harness，均有专用验证文档，不冒充端到端完成。
- get_goal核实revision4 active/armed，14/30轮；完整目标尚未达成，不标complete。当前无发布/提交操作。

## Goal round 13：路径来源与只读设计审查
- workflow-28已启动，独占internal/harness/**、internal/instance/**及专用验证文档，给根路径提供来源信息，显式环境路径无效必须fatal，sources配置优先，默认缺失保持诊断非fatal；不改scanner/app/store/query。前序配置修复已结束，不存在重叠写入。
- 已阅读快照规划1–154行。不能将immutable=1当运行中数据库的容错读法（可能忽略WAL/产生不一致）；实现时不可安全读取应返回明确不可用，不猜测降级。只读验证需检查文件集合/sidecars而非只DB摘要，不能悄悄创建-shm。不把无损坏但缺旧标记与损坏的部分schema混为安全升级提示。
- workflow-27已收集：EstimatedSpend必含reporttime.Period，按scope/period/currency查找；持久周期水位拒绝倒退及版本下降，状态schema2。Lead独立bash-8526测试0.679s/race1.274s/vet exit0；仍无真实通知/预算配置/估算数据接线。
- workflow-24已收集：Write.InstanceID、CursorIn/CursorsIn；Filter.InstanceIDs、SessionRow/AttributedEvent.InstanceID、独立InstanceService.SessionIn/SessionCacheIn；旧空ID→default，不改model。Lead独立bash-8553 store38.821s/query12.927s/CLI14.704s及vet exit0。查询原始红测未记录，后验mutation红不可冒充TDD。严格性能待所有写入结束后独立复验。
- Lead读取instance_migration.go发现preflight只验证instance列未验证主键且允许view，可能放过部分重建。workflow-29两互斥worker：internal/store加强schema拒绝/失败回滚证据；internal/scan接实例ID/游标与watch隔离（不依赖28新API）。无app/GUI/CLI接线，尚非完整多实例。

## Goal round 12：配置审查与后续并行
- workflow-23已收集：internal/instance新增稳定ID配置/原子保存/显式路径Resolve。代理报告45PASS/1SKIP（本卷大小写不敏感）及race/vet通过，Lead尚未独立验收。
- Lead读取resolve.go发现跨entry overlap仅词法比较，大小写别名same-inode及父子路径可能漏检；workflow-25独占internal/instance和专用文档先红测再修复，保留大小写敏感卷上不同路径。Fallback当前不能区分显式环境路径与默认路径，后续app接线必须补来源以确保显式无效路径报错。
- workflow-25已收集，实际复现6个红例：跨entry大小写别名同目录/父子、同entry大小写别名父子、默认根重叠等。修复为os.SameFile与按路径深度向上比较祖先，默认根保留最宽覆盖并诊断；显式重叠仍fatal，不做字符串小写。Lead读取修复并独立bash-8338：普通测试0.370s、race1.420s、vet均exit0。大小写敏感卷分支仍待对应环境。
- workflow-24实例数据维度仍运行，尚无结果。当前不启动依赖它的scanner改动。
- workflow-26已收集：预算纯逻辑及docs/superpowers/plans/2026-10-13-v040-readonly-snapshot.md规划。Lead发现budget/evaluate.go:48-58,107仅按Scope查spend，同scope日/周/月会误用同一金额；:88裁剪旧周期假设无法回到旧周期，时钟回拨可重报。workflow-27独占internal/budget及验证文档补period身份查找、持久回拨保护、版本降级拒绝，先红后绿；未接受预算实现为完成。
- 快照规划报告指出paths.DataDir也会MkdirAll，Store.Open/OpenLocal均非只读；readonly句柄必须等workflow-24 schema稳定后再实施，避免误迁移/计价。规划尚需Lead详细审阅，不把其列出的产品问题或方案当已确认决定。
- 配置Save文件sync+rename但无目录sync，跨进程Load-modify-Save也尚无CAS/锁；后续持久配置入口接线需评估，不能声称崩溃持久/并发编辑已经验收。

## Goal round 11：独立验收与多实例前置
- workflow-22已收集并审阅别名异步选择隔离实现；main.go Hook真实接线及完整ID详情均有测试。
- bash-8091已收集exit0：严格性能22/22通过（TotalsIDFilter3.520291ms、ProviderGroups3.848709ms、ProviderGroupsIDFilter4.065541ms、CacheTotalsIDFilter0.78225ms），随后全量测试、vet、diff-check通过。均为Lead独立执行，不是转述代理结果。
- 审阅rollup迁移：摘要先于历史更新、已迁移数据库独立备份、失败回滚；查询完整日rollup/边界日facts和时区降级保留。
- workflow-23进行中：DeepSeek独占internal/instance/**与配置验证文档；稳定ID、显式根、备份副本排除，尚未接线。
- workflow-24进行中：DeepSeek独占internal/model/**、internal/store/**、internal/query/**与数据验证文档；实例维度与安全迁移。与23无文件重叠。待结果收集/审查后再启动scanner/app接线。
- 尚未完成0.4.0；供应商会话drilldown/截图、多实例端到端、快照/statusline/预算/报表/额度/原生控件及真实平台证据仍待推进。无提交、推送或发布。

## Session start
- 完成逐题选择框grilling，用户明确确认实施。
- 创建持久目标，最多30 continuation rounds。
- 决策存入 task_plan.md，初始代码事实存入 findings.md。
- 本轮未启动任何子代理、teammate、服务器或后台job。
- 仅新增规划文档，尚未改动产品代码、依赖或用户已有文档。

## 验证
- 升级前 `go test ./...`：全部通过，job bash-7024 已收集，exit code 0。
- 1x headless中英文/明暗/空状态截图生成通过：job bash-7025 已收集exit 0；输出 build/verification/v040/before，日志 snapshots.log。已查看zh-overview和en-tray-dark，布局完整。未启动真实窗口，不代表GPU或真机验证。
- 性能/vet/build基线job bash-7026已收集exit 0。首视图构建3次best分别63.25µs、53.875µs、70.416µs，50ms预算。包含CPU栅格化的headless benchmark为959358083/954851930/1012510958 ns/op，1420885/1365936/1349648 B/op，11026/9528/9057 allocs/op；不是GPU首帧时延。
- MyGo已升级至v0.4.8；job bash-7027完成exit 0，diff仅版本与两项校验和。
- 升级后test/vet/本机构建job bash-7028通过；截图和性能job bash-7030通过；Windows/Linux amd64/arm64交叉编译job bash-7031通过；所有job输出已收集，无剩余任务。
- 46张截图同名同尺寸，36张逐字节相同；PNG逐像素比较job bash-7032通过，其余10张每张7–13像素不同（会话时间线、搜索图标、托盘微型标记），抽查未见布局变化。
- 性能、命名兼容审计、TS非依赖结论及真实平台发布门槛记录于docs/verification/v040-mygo-baseline.md。git diff --check通过；保护用户ROADMAP/research改动，未commit/push。
- 升级批次完成。下一阶段供应商身份：已读internal/attrib/attrib.go:1-210、internal/store/store.go:1-190，确认旧迁移会清pricing_fingerprint，Store.Open在建表后调用migrate，单连接和writer锁。须规划备份先于任何历史修改，新增endpoint ID与历史价格隔离。

## Goal round 1：身份与备份基础
- 新增 docs/superpowers/plans/2026-10-13-v040-provider-identity.md，分纯身份、备份事务迁移、查询接入三步。
- 新增internal/provider/identity.go及identity_test.go：NormalizeEndpoint与EndpointID，严格http(s)、host case/default port/single trailing slash；隔离路径、scheme、域名、port；拒绝敏感及不确定地址、本地回环和模糊数字IP。
- TDD：首轮失败为undefined NormalizeEndpoint/EndpointID；实现后job bash-7035通过。新增模糊数字IP回归后job bash-7036观察到4个实际失败（127.1/2130706433/0177.0.0.1/0x7f000001）；修复后job bash-7038通过。
- 新增internal/store/backup.go及backup_test.go：Store.Backup一致性VACUUM INTO、包含未checkpoint WAL、独立快照、0600文件、现有目标不覆盖、无效父目录失败、预取消不创建文件。首测失败为st.Backup undefined；实现后job bash-7038通过。
- 全量go test ./...、go vet ./...、git diff --check通过（job bash-7039 exit0，输出已收集）。新增代码未接入应用启动/查询/定价，因此尚未触碰用户数据库，供应商功能仍未完成。没有剩余后台任务。
- 当轮下一步是迁移接入，已在round2推进如下。

## Goal round 2：安全迁移接入
- 新增internal/store/provider_migration.go：prepareProviderMigration在migrate的历史事务前检查provider.identity.v1。非内存且events非空时创建数据库旁的`.pre-provider-v1.db`；已有备份一律不覆写/不复用，返回错误停止（失败重试需显式恢复决策）。空新库和内存库不做磁盘备份。
- migrate事务开始记录historyDigest，按harness/dedup_key排序对请求标识、timestamp、五类token、cost/log_cost逐行JSON后SHA256；迁移末尾写provider_endpoints(raw_endpoint,provider_id,endpoint)和完成标记，再比较digest，一旦不同返回错误，defer tx.Rollback撤销DDL/标记/事实修改。旧resolved_provider及pricing_fingerprint均不由此改写。
- 磁盘迁移测试先失败：job bash-7041出现missing pre-migration backup和migration proceeded without a new consistent backup；真实SQL触发器在版本标记插入时篡改cost的回滚测试先失败（job bash-7042 invariant violation was committed）。实现后job bash-7043全部通过；全量test/vet/diff-check job bash-7044通过。
- 新增internal/attrib/history_test.go：证实NewWithSources旧迁移导致startup invalidated historical prices: "" <nil>（job bash-7045失败）。已移除构造函数中migrateGenericProviders/migrateBareGeneric的自动调用，保持新请求Resolve逻辑不变；旧私有函数暂保留但未调用。
- 最终job bash-7046针对历史保护测试+go test ./...+go vet ./...+git diff --check全部exit0，所有job已收集。
- 没有启动真实应用/使用真实DB；测试均临时或内存fixture。不commit/push/release。
- 下一步：为新采集事件填充endpoint元数据并统一共享query的供应商聚合/筛选/明细，保留原名称/价格匹配和账单Origin+KeyID；显式别名与未知身份保留分隔。当前provider_endpoints只在一次性迁移填充，不能声称展示归并已完成。还需补目录fsync耐断电检查及更广泛磁盘故障/性能证据。
- Go go1.27.1 darwin/arm64；日期2026-10-13。
- 用户确认直接在当前main工作目录实施，保留已有改动，不创建worktree。

## Goal round 5：共享身份查询与授权审查
- 用户已明确授权workflow；启动workflow-9，两个只读审查分别指定nerv-base/deepseek-flash（迁移安全）与nerv-base/step-5-preview（查询/GUI/CLI接入），不使用默认模型、不写共享代码。审查结果待收集。
- 添加Service.ProviderGroups和ProviderGroup/Members，端点跨名称/harness聚合但保留原归因成员；未知身份按harness/session/name/raw endpoint哈希隔离，不输出未知原始URL。事实聚合读取BaseURL，不能使用丢弃端点的旧daily_usage。旧ByProvider/价格/账单行为未替换。
- 测试先报ProviderGroups未定义（bash-7049，同时修正测试中错误model.Claude枚举为ClaudeCode）；实现后定向测试通过，全量发现demo缺少新接口（bash-7082）。补合成demo实现后全量test/vet/diff-check bash-7088全部exit0。
- 新增成本来源、unpriced、模型别名、未知会话隔离及序列化不泄露秘密端点测试，bash-7099通过。上述bash输出均已收集。
- GUI/CLI尚未切换到新查询，ProviderIDs全链路过滤、显示别名、性能测量与端点rollup方案仍未完成。详细后续步骤补入provider-identity计划。
- 将端点SQL构建改为显式eventAggregateSQLMode维度列表，避免文本替换耦合；最终全量test/vet/diff-check bash-7118全部exit0且已收集。workflow-9多次阻塞等待仍为running、无结果，保持任务运行，下一轮必须收集并验证报告，不能宣称审查通过。

## Goal round 6–7：别名完成与 DeepSeek 实施分工
- 显示别名存储及查询读取完成；先观察缺方法、缺通知和alias not shown三类失败，再修正。bash-7198全量test/vet/diff-check exit0，已收集。
- workflow-9两份审查已收集；包含需核实的结论和错误机制描述，不直接采纳。分流记录见docs/verification/v040-review-triage.md。
- 用户禁止step-5-preview，且要求主代理只编排审查，所有产品实现与修复交给nerv-base/deepseek-flash。task_plan及goal已更新，goal revision4 active/armed。
- workflow-10仍运行：query独占实现ProviderIDs全链路筛选/性能测量；store独占补迁移安全/恢复说明。两块互不重叠，尚未验收。
- workflow-11已完成并收集：新增internal/reporttime与internal/ingestrate纯基础包，代理报告定向/race通过，但主代理尚未独立验收，不代表报表/托盘已接入。
- 主代理实读发现ingestrate整秒桶会提前丢弃不足60秒的亚秒样本，且sum溢出不满足文档sticky承诺；reporttime使用time.Date午夜+AddDate，需覆盖午夜DST及跳过日期。
- 已启动workflow-12（仅DeepSeek）：分别独占internal/ingestrate与internal/reporttime补测试并修复上述问题，仍运行。等待冻结写入后主代理独立回归；不盲信代理覆盖率或通过声明。
- 没有commit/push/release，也未碰真实数据库或凭据。

## Goal round 8：独立验收与性能门槛
- workflow-10已收集。审查发现query的3元tuple丢掉resolved_provider，未知同session/base不同price名称会漏入cache；启动workflow-13修复并收集，红例Requests5而应2。现按event_dimensions解析，已知端点按raw spellings、未知按4元身份匹配，10K会话不会膨胀到每session参数。
- store初版把digest基线移到加列UPDATE之后，会掩盖旧触发器修改cost/token；workflow-14补红例并修复，baseline恢复所有mutation之前，缺priced用等价CASE表达式。还拒绝同名非table对象，并澄清Open bootstrap DDL导致失败时不能承诺文件字节不变。结果已收集。
- workflow-12已收集：ingestrate改为精确时间戳4096容量队列，溢出/容量不足显式不可用，移除间隔大于60秒就猜未来的规则；reporttime补午夜DST/跳日期/IANA oracle测试。reporttime任意自造timezone边界仍有有限搜索假设，不夸大为无限通用。
- 独立bash-7467：两个基础包test/race/vet全部exit0（reporttime race27.319s）。独立bash-7568：go test -count=1 ./...、go vet ./...、git diff --check全部exit0；已收集。
- 独立严格性能bash-7585 exit1：TotalsIDFilter651.801ms、ProviderGroups685.760ms、ProviderGroupsIDFilter637.750ms、CacheTotalsIDFilter152.796ms，对比100ms门槛。其他case<=15ms。不得用普通测试通过掩盖此门槛。
- 已启动workflow-16，仅DeepSeek独占internal/query、internal/store及专用性能文档，性能优化必要时安全端点派生rollup；不降阈值/删case/缩fixture，不改历史账务。仍运行。
- workflow-15已收集：多实例集成计划docs/superpowers/plans/2026-10-13-v040-multi-instance.md已读审。推荐实例维度进入事实/游标/派生聚合，必须等待性能store/query任务完成；不能把CLI优先/read-only GUI阶段误当完整交付。Lead修正文档两处把技术提案/阶段当已获授权的表述。第5节9项“未定产品问题”需与既有共识逐项去重，不全都重新询问；备份副本如何采集、账单跨实例归属等真正新重大决策需确认。尚未授权该计划中的默认值或数据层改写。
- GUI/CLI新身份入口、固定时区持久化、托盘采集集成均未完成；基础包通过不能冒充功能交付。

## Goal round 4：新增采集身份元数据
- internal/store/store.go的采集事务现对每个不同BaseURL保守规范化并维护provider_endpoints；批内缓存含无法识别的地址，不改历史/当前resolved_provider、不改定价和账单字段。失败会随事实、维度、session及cursor事务整体回滚。
- internal/store/provider_ingestion_test.go先失败（bash-7047）：endpoint identities: raw=0 stable=0; want 3/2，以及ingestion succeeded without endpoint identity。实现后bash-7048定向测试、go test ./...、go vet ./...、git diff --check全部exit0，输出已收集。
- 测试覆盖不同账户同端点合并ID、独立租户路径保持不同、localhost不猜测身份、重复导入幂等和端点写入SQL故障回滚。
- 用户询问子代理；已说明普通工具不能选择所限模型，尚无workflow授权，因此未调用任何代理。
- 查询探索：internal/query/aggregate_sql.go的daily_usage与usageColumns只有resolved_provider、不含base_url，不能直接把名称换成ID而声称正确合并。下一步应同时处理事实与rollup身份维度/过滤兼容，保留账单与价格名称；GUI/CLI尚未接入。完整0.4.0继续进行。
