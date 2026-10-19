# MyGo 最新发布调研：v0.4.8 对比 v0.4.5，以及 MyToken v0.3.6 升级影响

## 结论

**本次查询时，官方最新 release 是 v0.4.8，发布时间为 2026-10-12 21:54:51 UTC（北京时间 2026-10-13 05:54:51）。** 官方 `/releases/latest` 返回 v0.4.8 内容，远端最大框架版本 tag 也为 v0.4.8，commit 为 `c0e1068027faf8b14777c9cb925c937ef34ac2bb`。不把仓库其他资源 tag（SQLite、NSIS、libghostty）当作框架版本。

MyToken 当前锁定 `github.com/egoist/mygo v0.3.6`，Go 为 `1.27.1`，CLI 也通过同一模块的 tool 声明引入，见 [MyToken go.mod](../go.mod#L3-L28)。**可以用，但要区分两组增量：**

- **相对 v0.4.5 真正新：** 原生代码编辑器与 diff viewer（v0.4.8）；离屏 `ui.Canvas`、绘制级变换和图片区域绘制（v0.4.7）；窗口状态自定义存储、文本布局工作集缓存、裁剪优化、入场动画布局跟随与焦点修复、macOS 新图标资源（v0.4.6）。
- **相对项目 v0.3.6 新、但相对 v0.4.5 已经旧：** `ui/chart`、弹簧/关键帧/变换、flyout、表格圆角选中行及选择器修复。不能宣称这次 v0.4.8 才有图表、表格、Combobox、托盘或自动更新。
- **最值得 MyToken 采用：** 原生用量/费用图表（旧于比较基线，但项目可因升级获得）、配置 JSON/TOML 编辑及导入前差异预览（真正新）、大列表/图表页面的渲染修复。游戏与音频能力优先级低。
- 本次仅研究，**没有升级依赖、修改业务代码、运行新版 GUI 或做性能实测**。下文“可用于 MyToken”是根据官方接口及约束作出的应用建议，不是已经接入的功能。

官方依据：[latest](https://github.com/egoist/mygo/releases/latest)、[v0.4.8 release](https://github.com/egoist/mygo/releases/tag/v0.4.8)、[v0.4.8 tagged source](https://github.com/egoist/mygo/tree/v0.4.8)、[v0.4.5…v0.4.8 完整比较](https://github.com/egoist/mygo/compare/v0.4.5...v0.4.8)。

## 1. 发布范围与核验方式

| 版本 | 官方 release 时间（UTC） | 本次相关内容 |
|---|---|---|
| v0.4.5 | 2026-10-11 18:54:39 | 比较基线，新增 Draggable；不是本轮增量 |
| v0.4.6 | 2026-10-12 14:19:01 | 渲染/焦点/布局修复，WindowStateStore、macos.icon、audio |
| v0.4.7 | 2026-10-12 17:49:17 | Canvas、Painter.Transform/ImageRegion、Pixelated、游戏组件 |
| v0.4.8 | 2026-10-12 21:54:51 | ui/editor 编辑器与 diff；浏览器软键盘修复 |

来源：[.5](https://github.com/egoist/mygo/releases/tag/v0.4.5)、[.6](https://github.com/egoist/mygo/releases/tag/v0.4.6)、[.7](https://github.com/egoist/mygo/releases/tag/v0.4.7)、[.8](https://github.com/egoist/mygo/releases/tag/v0.4.8)。时间读取官方 release 页的 `relative-time datetime`，不是拿 commit 时间充当发布时间。

核验只使用官方 GitHub releases、远端 tags、tag 固定的源码与仓库文档，以及项目自身锁定版本。对两个区间 `.3.6→.4.5`、`.4.5→.4.8` 分别检查官方 compare diff；后一区间没有 `ui/chart`、`ui/table.go` 或 updater/tray 功能源码变更，不能把这些旧能力标新。

访问限制：`https://api.github.com/repos/egoist/mygo/releases/latest` 返回 **403 API rate limit exceeded**；Python urllib 的系统证书链报 `CERTIFICATE_VERIFY_FAILED: unable to get local issuer certificate`。随后使用正常 TLS 验证的 curl、web_fetch、只读 `git ls-remote --tags` 成功读取官方站点和 raw 源码，没有关闭证书验证。限制不影响本文最新 release/tag 结论；没有使用第三方新闻、镜像或包索引补猜。

## 2. 相对 v0.4.5 真正新增：API、用途和限制

### 2.1 原生代码编辑和差异审阅：v0.4.8，优先级高

新包 `github.com/egoist/mygo/ui/editor` **是原生 UI，不要求 WebView、Monaco 或网页前端**。

已核对的入口：

```go
func NewBuffer(text string) *Buffer
func View(c *ui.Context, st *State) ui.Element
func DiffView(c *ui.Context, d *DiffState) ui.Element
func ParsePatch(text string) ([]FilePatch, error)
```

使用形态（依官方文档；是研究示意，未加入项目）：

```go
buf := editor.NewBuffer(configText)
buf.SetLanguage(editor.LanguageFor("config.toml"))
code := &editor.State{Buffer: buf}
// code 跨帧保留；在 view 中：
editor.View(c, code).Grow(1).FillWidth()

review := &editor.DiffState{
    Old: editor.NewBuffer(oldConfig), New: editor.NewBuffer(newConfig),
    Editable: true, HunkActions: true,
}
editor.DiffView(c, review).Grow(1).FillWidth()
```

**真实支持：** 28 种语言的默认行状态 tokenizer（含 JSON、TOML、YAML、SQL）、多光标、列选择、软换行、折叠、正则查找替换、撤销、输入法；`ReadOnly`；`DiffBase` gutter 变化；split/unified diff、词级高亮、折叠未变行、注释扩展区、逐 hunk Revert/Keep、补丁 `ParsePatch` / `FilePatch.Apply` / `SetPatch`。渲染只处理可见行；buffer 使用持久化 rope，并可由多个视图共享。

**MyToken 建议：** 在高级设置中编辑配置；导入提供商或迁移配置时先预览 before/after；错误配置显示诊断。先做只读预览，再考虑写入。必须由应用负责敏感 API key 脱敏、确认保存、校验和原子落盘，不能因为有 editor 就把密钥原文开放给复制/导出/读屏。

**限制与误区：**

- 并非内置 IDE/LSP 服务：`OnCompletion`、`OnHover`、`OnDefinition`、`SetDiagnostics`、`SetInlays`、`SetSuggestion` 是应用提供结果的接口；接语言服务器要自行实现。tree-sitter 是 `Language.Highlighter` 的可插拔实现示例，不是已捆绑解析器。
- buffer API 的 offset 是 **UTF-8 字节**；`Pos` 的行与行内字节均从 0 起。不可把 `ui.TextBuffer` 的 rune/UTF-16 输入法索引直接混用。CRLF 在内存归一化为 LF，`WriteTo` 按 `LineEnding` 保存。
- buffer 方法可跨 goroutine；改变应用 UI 状态和触发显示仍按文档使用 `Window.Update`。自定义 Highlighter 的调用发生在主线程，长解析应做后台快照处理。
- `DiffState.Accept` / Keep 是把新行接受进 `Old` buffer；不是自动写文件、应用配置或提交 Git。审阅事务与验证仍归 MyToken。
- 官方“百万行如十行般滚动编辑”是发布描述，本次未实测，不能扩大为任意超长单行、全文搜索、首次加载和 diff 都是常数时间。

源码：[buffer.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/editor/buffer.go)、[build.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/editor/build.go)、[state.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/editor/state.go)、[diffview.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/editor/diffview.go)、[patch.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/editor/patch.go)、[lsp.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/editor/lsp.go)。文档：[code editor](https://github.com/egoist/mygo/blob/v0.4.8/docs/ui/code-editor.md)、[diff viewer](https://github.com/egoist/mygo/blob/v0.4.8/docs/ui/diff-viewer.md)。

### 2.2 Canvas 与自绘：v0.4.7，对图表有间接价值

| API | 真正新增能力 | MyToken 用法与约束 |
|---|---|---|
| `ui.NewCanvas(w, h int) *Canvas`；`Canvas.Draw(p *Painter, fn func(cp *Painter))`；`Canvas.Bitmap()` | 离屏画布/渲染到纹理，保留上次内容 | 静态图表网格/背景只绘一次，再作为图片显示；不是新增图表类型 |
| `Painter.Transform(t Transform, ox, oy float32, fn func())` | 嵌套变换自绘内容及 clip | 自制图表缩放/平移；**不变换输入坐标**，命中测试要应用自行逆映射 |
| `Painter.ImageRegion(b *Bitmap, src, dst Rect, o ImageOptions)` | 只绘 bitmap 区域，翻转及 Tint | 图标图集/帧动画；不是 SVG/图表引擎升级 |
| `Bitmap.Pixelated` | 像素画清晰放大 | 对统计桌面 GUI 通常优先级低 |

Canvas 大小单位是像素，绘制在 `Draw` 回调当前帧内执行；每次 `Canvas.Draw` 清为透明重画，不是自动脏区缓存。主题、尺寸、DPI 或数据背景变化后必须重绘/重建。官方源码明确适用于图表网格，但没有承诺 CPU/GPU 显存成本为零；动态曲线和静态层应分开。不要缓存整张仍频繁变化的图表后又每帧重画，期待自动变快。

源码：[canvas.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/canvas.go)、[paint.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/paint.go)、[bitmap.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/bitmap.go)；文档：[drawing](https://github.com/egoist/mygo/blob/v0.4.8/docs/ui/drawing.md)。

### 2.3 动画、性能与交互：主要是 v0.4.6 修复，而非新框架

- **Enter 跟随布局修复：** 入场期间，未由 Transition 动画接管的 Position/Size 会立即跟随布局，Collapse 随尺寸成比例变化；适合异步加载/插入的用量行，避免行在入场途中偏离新布局。源码：[transition.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/transition.go)、[motion.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/motion.go)。
- **文本布局有界工作集：** 普通预算仍 8 MiB、最多 4096 项；最近 8 个全局帧内使用的布局可保留至 **64 MiB** 硬上限，后续 EndFrame 回收超额。减少工作集略大于旧预算时的循环淘汰/重排，代价是活跃文本缓存可能更高，不是“升级必然降内存”。这是布局估算预算，不是进程 RSS 上限。源码：[internal/text/layout.go](https://github.com/egoist/mygo/blob/v0.4.8/internal/text/layout.go)。
- **裁剪：** 只为变换真正需要的 clip 建 mask；按 runs 栅格化并跨帧保留 clip mask。可能改善圆角/变换密集界面，但无 MyToken 实测倍数。源码：[internal/raster/clipmask.go](https://github.com/egoist/mygo/blob/v0.4.8/internal/raster/clipmask.go)、[internal/raster/transform.go](https://github.com/egoist/mygo/blob/v0.4.8/internal/raster/transform.go)。
- **焦点：** 只有视图实际显示/读取窗口焦点时，焦点变化才触发所需帧；toast 在 popover 之后构建时恢复焦点的修复。源码：[ui/runtime.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/runtime.go)、[ui/input.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/input.go)、[ui/toast.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/toast.go)。
- **内部按钮开始拖拽父元素：** 改善带按钮/操作区的 draggable/reorder 元素；需回归“短按操作 vs 超阈值拖动”的区分。源码：[ui/dragdrop.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/dragdrop.go)。

v0.4.7 的 sprite-heavy benchmark 是游戏渲染基准增加，不能据此声称 MyToken 表格/图表有特定帧率提升。以上发布归属见 [.6 release](https://github.com/egoist/mygo/releases/tag/v0.4.6)、[.7 release](https://github.com/egoist/mygo/releases/tag/v0.4.7)。

### 2.4 桌面窗口、托盘与应用更新

**真正新：**

- `Application.SetWindowStateStore(s WindowStateStore)` 和 `WindowState`：把 `StateKey` 对应的位置、大小、最大化和全屏状态写入应用选择的存储（例如项目 SQLite），而非默认 `window-state.json`。接口是 `LoadWindowStates() (map[string]WindowState, error)` 与 `SaveWindowStates(states map[string]WindowState) error`。应在第一个带 StateKey 的窗口创建前调用；**主线程调用，必须快**；加载无数据/失败用窗口 options；保存失败下次再试；nil 恢复默认文件。浏览器不调用存储。**窗口状态恢复本身早于 .6，新增的是可替换存储。** [window.go 源码](https://github.com/egoist/mygo/blob/v0.4.8/window.go)、[window_state.go](https://github.com/egoist/mygo/blob/v0.4.8/window_state.go)。
- Windows 最大化窗口从最小化恢复时，根据 proposed rect 所在显示器决定 work area，修复多显示器内容错位/空白。[Windows window 源码](https://github.com/egoist/mygo/blob/v0.4.8/internal/windows/window.go)。
- macOS `macos.icon` 可补充 `Assets.car` 或 Icon Composer `.icon`；`.icon` 编译要求 macOS/Xcode 26+ 的 actool，失败或其他平台构建会跳过并保留 PNG 派生的 `.icns`；开发态仍显示 `.icns`。这不等于应用最低系统版本升级至 26。建议先保留现有图标链，仅作为视觉增强。[distribution 文档](https://github.com/egoist/mygo/blob/v0.4.8/docs/distribution.md#name-icon-and-version)。
- DMG 按 device detach，构建流程修复；WebView2 运行时检测不再只看 client library，纯原生 UI 不应因此引入 WebView。[dmg.go](https://github.com/egoist/mygo/blob/v0.4.8/cmd/mygo/dmg.go)、[webview2 检测](https://github.com/egoist/mygo/blob/v0.4.8/internal/webview2/webview2.go)。

**没有核验到 .5→.8 新增托盘 API 或自动更新能力。** updater 和 tray 不在该区间功能源码变化中。不能把“框架 release 更新”解释为“新增应用自动更新”。Flyout 在 .4.1 已加入，虽然对托盘用量弹窗很适合，但不是最新增量：[.4.1 release](https://github.com/egoist/mygo/releases/tag/v0.4.1)、[窗口文档](https://github.com/egoist/mygo/blob/v0.4.8/docs/windows.md)。

### 2.5 可访问性：新编辑器有语义，不是首次支持读屏

- .4.8 editor 对辅助技术呈现带 `Label` 的多行文本框、buffer 文本和最新 selection；completion 是 list，find bar 是 toolbar。diff 的两侧为 “Old text” / “New text” 文本框，折叠分隔为 group，Revert/Keep 为按钮。**应回归中文 IME、VoiceOver 和只读配置场景，而不能仅凭文档声称所有平台已验证。** [editor 文档](https://github.com/egoist/mygo/blob/v0.4.8/docs/ui/code-editor.md#accessibility)、[diff 文档](https://github.com/egoist/mygo/blob/v0.4.8/docs/ui/diff-viewer.md#accessibility)。
- 新 `Element.KeepFocus()` 适合编辑器/自定义选择 popup 中点选按钮时保持输入焦点；`Painter.Visible()` 可供自绘控件只画可见部分。
- **不要向 MyToken 推荐直接调用 `Element.Access` / `Painter.TextLine` 来自造语义或文本布局。** 虽然 release 把它们列为新增 exported 方法，签名依赖模块 `internal/platform.AccessNode` 与 `internal/text.Layout`，源码明确说明主要供 MyGo 包内（如 ui/editor）使用，外部应用不能正常导入这些类型。外部仍用公开 `Label`、`Description`、`Role`、`Value`、`c.Announce` 等。源码：[ui/api_gen.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/api_gen.go)、[ui/glyphs.go](https://github.com/egoist/mygo/blob/v0.4.8/ui/glyphs.go)。
- VoiceOver/Orca/Narrator/NVDA、控件语义、虚拟列表辅助滚动和 ReduceMotion 都不是 .8 首次加入：[v0.4.5 UI 文档](https://github.com/egoist/mygo/blob/v0.4.5/docs/ui/README.md)、[accessibility](https://github.com/egoist/mygo/blob/v0.4.8/docs/ui/accessibility.md)、[motion](https://github.com/egoist/mygo/blob/v0.4.8/docs/ui/motion.md#less-motion-and-frames)。

### 2.6 其他真实新增，MyToken 低优先级

v0.4.6 新 `audio`（WAV，自选 MP3/Vorbis 插件）；v0.4.7 新 `Player.SetLoopRange`、gamepad、`game.View` 固定时间步与 `game.Camera`。可用于可关闭的完成/额度提醒声音，但不是图表/桌面组件改善的主要理由。新增音频后模块增加 `go-mp3 v0.3.4`、`oggvorbis v1.0.5`、`vorbis v1.0.2`；不应误称它们全被打进未导入音频的 GUI 可执行文件。源码：[audio](https://github.com/egoist/mygo/tree/v0.4.8/audio)、[game](https://github.com/egoist/mygo/tree/v0.4.8/plugins/game)、[go.mod](https://github.com/egoist/mygo/blob/v0.4.8/go.mod)。.4.8 浏览器软键盘仅文本输入唤起的修复不属于原生桌面新增能力。

## 3. 重点旧能力核验：对当前 v0.3.6 有价值，但不能标成 .5 后新增

| 领域 | 可用能力/API | 版本判定与限制 | MyToken 建议 |
|---|---|---|---|
| 图表 | `ui/chart`：`NewLinear`、`NewBand`、`NewTime`、`Plot`、`Rect/Point/RuleX/RuleY/Anchor`、`Line/Area`、`Stack`、`Pie/Sector`、`Select` | **v0.4.3 新增，v0.4.5 已有，.5→.8 未改**；headless，需要应用样式；不内置对数轴、缩放平移、legend、标签避碰 | 费用/Token 时序线、堆叠柱、供应商份额 donut；最贴合主界面 |
| 表格 | `ui.Table`、`TableColumn`、`ListState`，虚拟行、可变行高、键盘单/多选、列 resize/reorder、横滚、排序头、行内重命名 | **.3.6 已有 Table**；.3.7 修复跨多列拖动；.4.1 加 `RoundRows`；排序信号不替应用排序，需稳定 `Key` | 供应商/模型/账单用量表；不要把数据排序误交给 widget |
| 选择控件 | `ui.Select(c, *string, []string)`；泛型 `SelectBase`；Combobox、Radio、Segmented | **.5 前已有，.5→.8 未新增选择器类别**；.4.0 修滚轮跳回高亮项，.4.3 修 Combobox 打开即选中文本 | 时间范围、模型/提供商筛选；长列表选 Combobox |
| 动画 | `AnimateTo`、`Spring`、`Smooth/Snappy/Bouncy`、`Keyframes`、元素变换、`AnimatePose`、内容过渡 | .4.3 motion/transform 已有；.4.5 已有 `Draggable`；.6 是布局跟随修复 | 数字/筛选反馈及小范围入场，优先轻量 paint-only pose，避免统计页持续整树 rebuild |
| 托盘弹窗 | parent-owned flyout | .4.1 已有，非 .6/.7/.8 新能力；桌面平台行为需逐一验证 | 托盘快速查看今日用量/额度 |

明确来源：

- [v0.4.3 release：charts、motion、transform](https://github.com/egoist/mygo/releases/tag/v0.4.3)、[v0.4.5 chart 文档与限制](https://github.com/egoist/mygo/blob/v0.4.5/docs/ui/chart.md)、[chart 源码](https://github.com/egoist/mygo/tree/v0.4.5/ui/chart)。`chart.Select` 要求 `xs` 升序，左右/Home/End/Escape 可操作，describe 提供读屏值；图形本身仍要 Label/Description。
- [v0.3.6 Table 源码](https://github.com/egoist/mygo/blob/v0.3.6/ui/table.go)、[v0.3.7 release](https://github.com/egoist/mygo/releases/tag/v0.3.7)、[v0.4.1 release](https://github.com/egoist/mygo/releases/tag/v0.4.1)、[Table 文档](https://github.com/egoist/mygo/blob/v0.4.8/docs/ui/table.md)。
- [v0.4.0 release：Select/Combobox 滚动修复](https://github.com/egoist/mygo/releases/tag/v0.4.0)、[Select 文档](https://github.com/egoist/mygo/blob/v0.4.8/docs/ui/select.md)、[v0.4.5 UI 控件目录](https://github.com/egoist/mygo/blob/v0.4.5/docs/ui/README.md)。
- [v0.4.5 release：Draggable](https://github.com/egoist/mygo/releases/tag/v0.4.5)、[v0.4.5 motion 文档](https://github.com/egoist/mygo/blob/v0.4.5/docs/ui/motion.md)。`AnimatePose` 为绘制级动画；读取 `AnimateTo`/`DragOffset` 每帧要 rebuild。尊重 `c.Preferences().ReduceMotion`，不要把已有 reduced-motion 支持写作新版首创。

## 4. 从项目 v0.3.6 升级到 v0.4.8 的兼容风险

### 官方明确 breaking changes（v0.4.0，不能只看 .5 后补丁）

1. **AlertDialog Escape：** 不再按英文 “Cancel” 按钮自动选择其索引，而是关闭并在该帧返回 `ui.AlertDismissed == -2`。若应用要求必须回答，可在收到 dismissed 时把 `*open` 设回 true。删除提供商/覆盖密钥/导入配置等确认逻辑必须区分 button index、dismiss 和未选择，避免负值流进数组或误当确认。
2. **发布产物文件名带架构：** DMG/Windows installer 改为类似 `My App 0.1.0 arm64.dmg`、`My App Setup 0.1.0 amd64.exe`；darwin/universal 例外保持无架构名。构建脚本、release upload globs、更新清单/下载路径都要检查，虽非 updater 新 API，也会影响升级分发。
3. **无服务/事件时不生成 TypeScript client：** 无 bindings/events 的应用不再生成 `src/mygo.ts`。纯原生一般影响低，但若构建流水线硬依赖其存在需修改。

来源：[v0.4.0 breaking changes](https://github.com/egoist/mygo/releases/tag/v0.4.0)。

### 其余应回归项

- **不是 Go 大版本跃迁：** 上游 v0.3.6 与 v0.4.8 都要求 `go 1.27.1`，三项既有直接依赖版本相同；.8 多音频 indirect 依赖。依赖更少变化不等于业务零风险。[.3.6 go.mod](https://github.com/egoist/mygo/blob/v0.3.6/go.mod)、[.4.8 go.mod](https://github.com/egoist/mygo/blob/v0.4.8/go.mod)。
- **坐标与窗口行为：** .4.1 Linux `Bounds` 返回窗口管理器 frame，`ContentBounds` 返回内容；已有托盘/弹窗定位、跨 DPI 和屏幕边缘计算要回归。[.4.1 release](https://github.com/egoist/mygo/releases/tag/v0.4.1)。
- **布局/事件修复也改变行为：** tooltip 从按指针位置改为按元素定位（.3.7）；选择器滚轮、Combobox 文字选中、popover/toast 焦点、拖动父元素、退出/入场动画都应覆盖。
- **性能基线不可挪用：** .4.3 宣称某 Exit 场景稳态分配由 297 KB→3.3 KB，属于旧版本特定场景而非 .8 全应用收益；.4.4 macOS presentation、选择框计算优化也早于 .5。源码/发布：[.4.3](https://github.com/egoist/mygo/releases/tag/v0.4.3)、[.4.4](https://github.com/egoist/mygo/releases/tag/v0.4.4)。
- **.4.8 文本存储重构：** 旧 `ui.TextBuffer` 已是持久化树，.8 改成共享 `internal/rope`；不能把“TextBuffer 首次有 rope”写成新功能。中文/emoji、组合输入、撤销、selection 与跨线程更新应回归。[旧 TextBuffer](https://github.com/egoist/mygo/blob/v0.4.5/ui/text_buffer.go)、[新 TextBuffer](https://github.com/egoist/mygo/blob/v0.4.8/ui/text_buffer.go)。
- **没有官方免测试兼容承诺：** .6/.7/.8 的 release 未列全局 breaking API，但 .4.8 新增大型 editor/GPU/rope 代码，不能据此保证所有平台无回归。本次没有用新依赖编译 MyToken，升级通过与否仍未验证。

## 5. 建议落地顺序与验收边界

1. **先做独立升级分支**，保持业务行为不变；同时验证依赖与 CLI 版本、AlertDismissed、打包产物/更新清单。研究任务本身不执行这些修改。
2. **第一批用户可见收益：** 用原生 `ui/chart` 做 7/30 天 Token/费用趋势（项目从 .3.6 获得，标注其首次出现在 .4.3）；继续利用已有 Table/Combobox，而不是重造控件。
3. **可选最新能力：** .8 `editor.View` / `editor.DiffView` 用于采集诊断详情、导入/迁移配置只读对比，不阻塞 0.4.0。默认脱敏，保持读屏与复制内容一致地不泄露秘密；暂不扩展为完整配置编辑器，不修改 harness 配置。
4. **性能按需：** 数据集规模固定后测 rebuild/paint 时间、滚动掉帧、空闲 CPU 和活跃/空闲内存；只有绘图网格成为热点时引入 Canvas，检查 DPI/主题切换刷新。不要以游戏 benchmark 或官方“百万行”措辞代替实际测量。
5. **桌面验收：** macOS 中文 IME、VoiceOver、ReduceMotion、托盘快速弹窗与焦点；Windows 双屏最大化→最小化→恢复、tray popup、Narrator/NVDA；Linux bounds/DPI/flyout 与 Orca（按项目支持平台执行）。若使用自定义 WindowStateStore，测关闭/退出时锁等待与失败重试。

**面向用户的一句话：框架确实又更新了，最新 .8 最实用的新东西是原生配置编辑器和差异预览；图表和更完整的动画也值得 MyToken 从 .3.6 升级采用，但它们在 .4.3 就有，不是这次才新增。**

## 6. 可复核的官方比较入口

- [完整升级跨度 v0.3.6…v0.4.8](https://github.com/egoist/mygo/compare/v0.3.6...v0.4.8)
- [旧能力边界 v0.3.6…v0.4.5](https://github.com/egoist/mygo/compare/v0.3.6...v0.4.5)
- [本次新内容边界 v0.4.5…v0.4.8](https://github.com/egoist/mygo/compare/v0.4.5...v0.4.8)
- [v0.4.5…v0.4.6](https://github.com/egoist/mygo/compare/v0.4.5...v0.4.6)、[v0.4.6…v0.4.7](https://github.com/egoist/mygo/compare/v0.4.6...v0.4.7)、[v0.4.7…v0.4.8](https://github.com/egoist/mygo/compare/v0.4.7...v0.4.8)

以上源码/文档链接固定到发布 tag，避免把 main 上尚未发布的能力混入本报告。
内容证据分级：release 给出首次发布归属；tag 源码/文档核验 API 和限制；MyToken 用途与优先级是研究建议。没有未注明来源的速度倍数、已实测平台声明或自动兼容保证。


## 7. 留档与未验证项

- 上游 v0.4.8 tag：`c0e1068027faf8b14777c9cb925c937ef34ac2bb`。
- 比较基线 v0.4.5 tag：`4d7114c4522498f991f7ebcba9edc9af0f820c6a`。
- 项目当前 v0.3.6 tag：`ca2668d1f4646be1c5c72e53bce0ed92c0958706`。
- 未编译/运行 MyToken + v0.4.8，未测海量账单/超长 JSON、读屏、多显示器；应用确认框和发布脚本是否依赖旧行为，留给升级分支专项审查。
- 本任务唯一项目写入为本研究文件；未运行 go get/go mod tidy，未修改其他文件或依赖。本文是发布快照，未来询问“最新”需要重新查询 releases/tags。

**最终建议：值得安排升级验证。统计图表是当前项目升级收益但不是 .5 后新增；原生 editor/diff 是本轮最实用的新能力。**
