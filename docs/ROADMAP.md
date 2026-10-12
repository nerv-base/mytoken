# MyToken!!!!! 路线图

> 状态：已定稿（2026-10-09）。当前版本 0.1.0。
> 本文记录方向、版本划分和做/不做的边界，以及每个决定的理由。实现细节以代码和 [SPEC.md](SPEC.md) 为准。

## 定位

**中转站用户的对账工具。** 核心问题：「我的钱花哪了，和中转站扣的对不对得上」。

开源社区里已有大量 token 统计工具（ccusage、codeburn、tokscale、CodexBar、Claude-Code-Usage-Monitor、token-monitor……），
但截至 2026-10 没有一个会抓中转站倍率并与站点账单对账：CodexBar 只读官方 API 花费，cc-switch 只显示余额、倍率靠手填。
这是 MyToken 的空位。

两根支柱：

1. **对账** —— 自动拉倍率 → 本地按倍率算应扣 → 对照余额 / 扣费日志 → 解释差额。归因可追溯，**每个数字标注来源**。
2. **缓存账本** —— 只用 token 数推导，解释「为什么比预期贵」。

与 cc-switch 的关系：互补。cc-switch 是可选的只读数据源（抽象为 `ProviderSource`）；
MyToken 不做供应商切换、本地路由、MCP、Skills、提示词管理。

## 全局约定

### 隐私

- 默认只发两类网络请求：价格表（models.dev → LiteLLM），和 App 每天一次到 github.com 检查更新（0.2.1 起，设置里可关，命令行不检查）。
- 中转站、官方 API 余额、Claude 订阅额度一律**默认关闭、逐站开启**。
- 只用用户已经交给该站点的凭据（harness 配置或 cc-switch 里现成的 `sk-` Key、本机登录态），只访问该站点本身，不经过任何第三方。
- 中转站能力分层、各自可关：L1 倍率 / 价格快照，L2 余额，L3 逐条或按天扣费记录。
- 0.2 发布时，README 的隐私承诺改为：
  「默认只发一个请求（价格表）；中转站对账需逐站开启，只用你已交给该站点的 Key 访问该站点本身」。

### 历史

只增不删。harness 日志被清理（例如 Claude Code 默认 30 天）后，已入库的用量必须保留；全量重扫不得清空历史。

### 数字来源标签

每个金额 / 用量都带来源，界面、CLI、JSON 一致展示，例如：

| 标签 | 含义 |
| --- | --- |
| `relay-bill` | 中转站扣费记录（L3） |
| `log` | harness 日志自带的费用 |
| `local-estimate` | 本地按价格表 × 倍率估算 |
| `inferred` | 供应商由模型名推断 |
| `unpriced` | 找不到价格，不计为 0 |

与已有的归因链标签（`log` / `cc-switch` / `config-timeline` / `inferred` / `user-rule`）并存：前者回答「钱怎么算的」，后者回答「算到谁头上」。

### 前端

全部用 mygo 原生 `ui`（不用 WebView）。优先复用 mygo 的图表、控件与动画能力，不重复维护比例尺、路径生成、命中判断和通用键盘交互；
应用保留热力图、桑基图（harness → 中转站 → 模型）、对账瀑布图、缓存时间轴的业务表达与品牌视觉。
设计吸收 codeburn 与 GitHub 动效库的长处；`ui` 缺的通用能力优先补进 mygo。

### 性能预算（基准数据 20 万条请求）

CI 在三个平台上跑 200K 事件的查询基准（`internal/query/bench_test.go`）；墙钟硬门槛只在设置 `MYTOKEN_STRICT_PERF=1` 时启用，因为 CI 机器的速度波动太大。复现方法见 [SPEC §10](SPEC.md)。

| 指标 | 预算 | 0.1.0 实测（206,615 条 / 627 会话） | 0.1.x 实测（206,728 条） |
| --- | --- | --- | --- |
| 托盘弹窗首帧 | < 50 ms | — | 构建视图 0.07 ms（`internal/gui/perf_test.go`，数据常驻内存，打开即画缓存） |
| `stats` / `statusline` | < 100 ms，峰值内存 < 50 MB | `stats` 1.08 s / 173 MB | 单查询 2–15 ms，基准进程 RSS 47.5 MB |
| 概览切换周期 | — | 2.7–8.2 s | 全套 66–95 ms；切换只重查周期相关部分，约 20 ms |
| 无新数据的增量扫描 | < 300 ms | 482 ms（墙钟 2.1 s）/ 115 MB | 204–209 ms |
| 全量重扫 | 只跟踪 | 8.8 s / RSS 225 MB | 9.8 s / RSS 154 MB（保留历史 + 维护汇总表） |
| 托盘常驻空闲 | < 60 MB，CPU ≈ 0（待修订，见下） | agent 写日志时 CPU ≈ 93%、footprint 342 MB | 没有数据时 footprint 71 MB；真实数据 + agent 持续写日志时 CPU 8–14%、footprint 118–126 MB。0.2.3：主窗口关闭即销毁（打开时 ~210 MB，关掉后回到 ~100 MB），只重读有变化的文件，agent 持续写日志时 CPU ~7%、footprint 85–95 MB（214K 条） |
| 数据库体积 | < 300 B / 条，另建按天预聚合表 | ≈ 590 B / 条（121 MB + 12 MB WAL） | 284 B / 条（含汇总表与请求 ID 等信号；旧库首次升级约 3 s） |
| 二进制体积 | 只跟踪不卡 | 27 MB | 29.5 MB |

托盘空闲 60 MB 的预算定得不现实：没有任何数据时，窗口系统、GPU 和 Go 运行时就占了约 71 MB footprint。建议改成「没有数据时 < 80 MB；20 万条数据时 < 130 MB，Go 堆存活 < 30 MB」。

### Harness

覆盖 codeburn 支持的全部工具，外加 WorkBuddy、CodeBuddy（腾讯，Claude Code 分支），分三批随版本推进。
每个新 harness 必须带 fixture 测试并通过性能基准。可参照 codeburn（MIT）的解析逻辑用 Go 移植，并在 `THIRD_PARTY_NOTICES` 中注明。

| 批次 | 跟随版本 | 范围 |
| --- | --- | --- |
| 第 1 批 | 0.2 | 能接中转站的 CLI：Grok Build、Hermes、OpenClaw、Claude Desktop、Qwen、Kimi / Kimi Code、Droid、Goose、OpenClaude、Forge 等；WorkBuddy、CodeBuddy |
| 第 2 批 | 0.3 / 0.4 | 其余可离线读取的：Amp、Kiro、Zed、Antigravity、Codebuff、Mux 等 |
| 第 3 批 | 0.5 | 订阅制或需联网的：Cursor、Copilot、Warp、Devin、Vercel AI Gateway（联网的一律逐个开启） |

订阅制工具（Cursor、Copilot、Warp、Devin）没有中转站，只做用量统计，不参与对账。

## 版本

### 0.1.x —— 打地基

- ✅ **修倍率 bug**：[`internal/pricing/rules.go`](../internal/pricing/rules.go) 的 `effective()` 在 `{provider,model}`、`{"",model}`、`{provider,""}` 中取第一条命中并用它的 `Multiplier`，导致模型价格规则（倍率 1）覆盖供应商倍率（例：网关 0.3 × sonnet $3/M，1M 输入算成 $3 而非 $0.9）。改为**价格按模型优先、倍率按供应商优先，分开查**；用户规则同样受影响。
- ✅ **价格按时间存快照**：倍率会随高峰时段、站长调整而变，`PriceRule` 改为带生效时间的快照，历史请求按当时的价格计算。
- ✅ **`ProviderSource` 抽象**（`internal/source`）：cc-switch 归因 / 价格导入改为可选数据源之一，为中转站数据源铺路。
- ✅ **历史保全**：全量重扫不再执行 [`internal/store/store.go`](../internal/store/store.go) 中的 `DELETE FROM events; DELETE FROM sessions; DELETE FROM cursors;`；源日志消失后数据保留。
- ✅ **数字来源标签**写进数据模型（费用拆成 `log` / `estimate` / `unpriced`，`relay-bill` 预留）。
- ✅ **性能**：按天预聚合表、维度字典化、基准测试进 CI；GUI 只在窗口可见时刷新、两次刷新至少间隔 2 s；扫描器在 agent 持续写日志时也能触发、两次扫描至少间隔 2 s；Go 堆软上限 64 MiB。
- ✅ **CLI 对齐**：补 `--until`、`--last`、`--timezone`、`--offline`、`--no-cost`，以及 CSV 导出（目前只有 `--since`）。
- ✅ **新增 harness 流程标准化**：事件补 `RequestID`、compact 边界、harness 自带账单（`Bill`）三类信号；`internal/harness/harnesstest` 一致性套件（golden / 增量 / 游标稳定 / 幂等 / 隐私 / 缺目录）覆盖现有 10 个 harness，接入规范见 [HARNESS.md](HARNESS.md)。顺带修了 Claude 游标空转重写、opencode 新旧两张表共用水位线漏行两个 parser bug。

### 0.2 —— 对账 v1

> 状态：0.2.0 发布，接口契约见 [RELAY.md](RELAY.md)。已在真实 new-api（kamiapi、rinnebeat）、sub2api（kksk、xlabapi）和 DeepSeek 官方余额上端到端验证；「手填倍率」沿用定价设置里的供应商倍率。
>
> 0.2.x 遗留：
> - 倍率变化点按最新账单 / 同步时间定日期，不是真实变化时刻（应遍历账单找变化点）。
> - sub2api 的高峰时段倍率（`peak_start` / `peak_end` / `peak_rate_multiplier`）已解析，但没用到本地计价。
> - new-api 按时段变化的计费表达式算不出公式价，显示为「无法计算」。
> - Windows 上的标题栏让位、托盘、自动更新还没在真机上看过。

- ✅ 自动识别站点类型：先试 `/v1/sub2api/billing`，再试 `/api/status`，都不是则手填倍率。
- ✅ **new-api**（逐条对账）：
  - `GET /api/pricing` —— `model_ratio`、`completion_ratio`、`cache_ratio`、`create_cache_ratio`、`model_price`、`group_ratio`
  - `GET /api/usage/token/` —— Key 余额 / 额度
  - `GET /api/log/token` —— 该 Key 最近的请求日志（最多 1000 条，需轮询并本地持久化），按 `request_id` / 时间 + token 与本地事件配对
  - 额度换算：`quota / 500000 = USD`
- ✅ **sub2api**（按天对账）：
  - `GET /v1/sub2api/billing` —— 分组 / 用户 / 实际倍率，高峰时段与高峰倍率
  - `GET /v1/usage` —— 余额、今日 token（含缓存）、1–90 天日用量
- ✅ **官方 API 余额与花费**：OpenRouter、DeepSeek、SiliconFlow、Moonshot、z.ai、MiniMax，与中转站走同一套对账框架，同样逐个开启。
- ✅ **对账视图**：应扣 vs 实扣、差额拆解、来源标签。
- ✅ harness 第 1 批（12 个）。WorkBuddy 的日志实际是 CodeBuddy 格式，不是 Claude 格式，所以单独写了读取器（CodeBuddy 复用它）；每请求积分直接取日志里的 `rawUsage.credit` 作为 harness 自带账单（比 `workbuddy.db` 的 `credit_json` 更完整）。

### 0.3 —— 缓存账本

> 状态：实现完成，契约见 [CACHE.md](CACHE.md)，判定规则的真实数据校准见 [CALIBRATION.md](../internal/ledger/CALIBRATION.md)。真实库（21.6 万请求）：全量重建约 3 s（旧库首次打开含迁移约 9 s），增量 ≤ 42 ms/会话，查询 9–22 ms，库增长约 5 B/请求。

- ✅ 只用 token 数，所有 harness 通用：按会话链判定，应命中前缀取历史上同一分支的提示长度（不是机械地取上一轮——旁路短请求、分支切换不算未命中）；未命中 = 应命中 − `cache_read`；多花 = 实际费用 − 「全部命中」的反事实费用。
- ✅ 未命中归因四类：
  1. **过期** —— 两轮间隔超过各家缓存时长（Anthropic 5 min / 1 h，OpenAI 约 10 min，DeepSeek 1 h）
  2. **压缩后重付** —— 日志里的 compact 标记（Codex、Claude Code、DSH、Pi、OpenCode、WorkBuddy）
  3. **换模型**
  4. **说不清** —— 前缀没变、也没过期却没命中，多为中转站缓存透传或粘性路由问题 → 计入**供应商缓存健康度**（说不清的未命中 / 应命中）
- ✅ 整条链都没有缓存字段的会话记为「不可缓存」，不进分母。
- ✅ GUI 缓存页（命中率、重发 token、多花的钱、按天、按原因、按供应商健康度、最贵的会话）、会话详情的缓存时间轴；CLI `mytoken cache [--by cause|provider|model|harness|project|session]` 与 `mytoken cache <会话>`。
- ✅ harness 第 2 批：Amp、Kiro、Zed、Antigravity、Codebuff、Mux（本机都没有真实数据，尚未在真实日志上验证）。
- 已知：真实库里「说不清」占未命中 token 的 84%，但只占应命中的 1.8%；它是排查线索，不能直接当成中转站故障。

### 0.4.0 —— 日常使用与 MyGo 原生能力收敛

#### MyGo 升级与自研实现替换

> 状态：计划，尚未实施或完成升级后的编译、运行验证。源码对照基线：项目锁定 MyGo `v0.3.6`，本次核对的最新发布版为 `v0.4.5`。
> `ui/chart` 与增强动画是这一版本跨度的新增能力；`SegmentedBase`、`Sidebar`、`Table` 等此前已有，本轮一并收敛重复实现。
> 原则：先替换通用行为和绘图基础，再改善交互；保留五色、玻璃、乐队主题，不做整套 UI 重写。

**第一批：升级基线与低风险替换**

- [ ] 升级 MyGo 至 `v0.4.5`，先保持界面与行为不变，建立编译、测试、截图及性能对比基线。
- [ ] 环形图改用 `chart.Pie` / `Sector` / `SliceAt`，替换手工圆弧离散和角度、半径命中判断；保留配色、中心数字和入场效果。见 [`arc` / `donut`](../internal/gui/charts.go)。
- [ ] 分段选择器改用 `ui.SegmentedBase` 接管选择、方向键与可访问性，保留胶囊外观；单独验证用 `Transition.Match` 替换 `glider` 的位置、宽高插值，不假定基础控件自带当前滑块效果。见 [widgets.go](../internal/gui/widgets.go)。

**第二批：统一图表基础**

- [ ] 堆叠柱图、缓存时间线接入 `chart.NewLinear` / `NewBand` / `Plot` / `Stack` / `Select`，移除重复的坐标、刻度、堆叠与选中位置计算，并补齐键盘选择和读屏描述。见 [charts.go](../internal/gui/charts.go)、[cache.go](../internal/gui/cache.go)。
- [ ] 折线和面积图采用 `chart.Line` / `Area`，保留渐变、末端光点等视觉；核对 `CurveMonotone` 与现有 Bézier 曲线的差异，以及 `Nice()` 与现有含 `2.5` 的刻度规则差异，不要求机械等价。
- [ ] 普通提示采用 `TooltipBase`；随选中点即时更新的数据提示采用 `chart.Select` + `Anchor`，替换手工测字和定位。默认 Tooltip 的悬停延迟不适用于所有图表提示。
- [ ] 对账瀑布图仅复用比例尺、`Rect`、`Anchor` 等基础能力；累计差额、截断轴标记、连接线、缓存压缩边界及图例仍由应用负责。MyGo 不提供可直接替换的完整瀑布图。见 [relays.go](../internal/gui/relays.go)。

**第三批：桌面交互，按收益逐项迁移**

- [ ] 优先将数据量较大的排行榜改为 `ui.Table`，利用虚拟化、列宽调整、列重排和键盘交互；小型摘要表不强制迁移。排序数据、查询及列配置持久化仍由应用负责。见 [pages.go](../internal/gui/pages.go)。
- [ ] 先验证样式，再用 `Sidebar` / `SidebarItem` 接管侧栏选择、键盘导航与滚动；保留每页独立色、拨片装饰和滑动高亮。会话页已使用 `List` / `SearchField`，仅补原生选择与键盘行为，不重写列表。见 [mainwin.go](../internal/gui/mainwin.go)、[sessions.go](../internal/gui/sessions.go)。
- [ ] 模型映射候选交互评估 `ComboboxBase`，保留异步目录查询和别名保存；倍率输入评估 `NumberInput`，先明确范围、精度及保存时机，禁止因步长舍入改变计费倍率。见 [pricing.go](../internal/gui/pricing.go)。
- [ ] 纯位移、缩放、透明度动画优先采用 `AnimatePose` / `Transition`，利用只重绘、不重新构建视图的能力；数字递增和图表数值增长不直接套用。性能收益以实测为准。

**边界与验收**

- 不替换 token 分类、缓存归因、模型映射规则、计费与对账算法；品牌插画、Logo、特殊数字排版继续保留。窗口、托盘、更新和玻璃材质已使用 MyGo，不重复计为迁移成果。
- 每批独立验证：现有测试、明暗主题截图、空数据 / 单点 / 零值、窗口缩放、鼠标及键盘交互；升级后复查控件绑定与元素生命周期。
- 回归托盘首帧和大数据图表性能，对照本文件的性能预算；不能因为将绘图拆成大量元素而增加构建成本。跨平台标题栏、托盘和更新仍需真机验证。
- 本轮目标是减少通用实现的维护负担，不承诺未经测量的删行数或性能提升；不提前展开 0.5 的全局视觉改版。

#### 日常使用功能

- **带版本号的快照输出**（`--json --once`）：statusline、托盘、MCP 共用同一份，口径一致。
- statusline：会话花费 + 中转站余额 + 缓存命中率。
- 托盘实时刷新，含 tok/s。
- 预算与告警：只通知，不拦截。
- 报表：日 / 周 / 月 / 会话、环比，按项目与 git 分支统计（worktree 折叠到所属仓库）。
- 额度窗口：Codex 直接读日志里的 `rate_limits`（离线）；Claude 需开启后读 Keychain `Claude Code-credentials` 或 `~/.claude/.credentials.json` 并调用 OAuth 用量接口。不经 cc-switch。
- 额度消耗速度、预计耗尽时间、均匀用完参考线。
- harness 第 2 批。

### 0.5 —— 视觉

- 可视化组件库：热力图、桑基图、对账瀑布图、缓存时间轴。
- 全局动效与视觉改版。
- 只读 MCP server（查询用量、花费、对账结果）。
- harness 第 3 批。

### 以后

上下文内容分析（DSH 式折叠经济学）、多币种与充值汇率、模型对比、任务分类、TUI。

## 明确不做

| 功能 | 理由 |
| --- | --- |
| `optimize --apply` / 自动改配置 | MyToken 只读，不改用户的 harness |
| guard 拦截请求 | 同上；预算只做通知 |
| PR / yield（git 产出关联） | 偏离对账定位 |
| 多设备同步 | 本地优先，不引入服务端 |
| VS Code 扩展、桌面小组件 | 维护成本高，托盘 + statusline 已覆盖 |
| 遥测 | 隐私承诺 |
| 排行榜 / 社交 | 需要上传数据 |
| 用 LLM 给会话做摘要 | 需联网且产生费用 |
| 服务状态页 | 与对账无关的额外网络请求 |

## 参考

| 工具 | 借鉴点 |
| --- | --- |
| [ccusage](https://github.com/ccusage/ccusage) | 报表维度、5 小时块、CLI 参数 |
| [codeburn](https://github.com/getagentseal/codeburn) | harness 覆盖、桌面端与菜单栏、项目 / 分支花费 |
| [Claude-Code-Usage-Monitor](https://github.com/Maciek-roboblog/Claude-Code-Usage-Monitor) | 来源标签、历史保全、额度预测、版本化快照 |
| [CodexBar](https://github.com/steipete/CodexBar) | 官方 API 花费、额度燃尽曲线 |
| [tokscale](https://github.com/junhoyeo/tokscale) | worktree 折叠、贡献热力图 |
| [token-monitor](https://github.com/Javis603/token-monitor) | tok/s、CSV 导出、保留已删会话 |
| [ccstatusline](https://github.com/sirmalloc/ccstatusline) | statusline 组件 |
| [cc-switch](https://github.com/farion1231/cc-switch) | 只读数据源、余额模板 |
