# 0.4.0 原生控件迁移第一批验证：Pie 与 SegmentedBase

日期：2026-10-13。环境：Go 1.27.1、darwin/arm64。本批次写入范围：`internal/gui/charts.go`、`internal/gui/widgets.go`、`internal/gui/native_controls_test.go`、本文件，以及未跟踪的构建产物 `build/verification/v040/native-controls/**`。本批次未改 `main.go`、`internal/app/**`、`internal/store/**`、`internal/query/**`、`internal/harness/**`、`internal/attrib/**`、`internal/model/**`、`go.mod`、`go.sum`，也未改共享规划文件（`task_plan.md`、`docs/ROADMAP.md`、`docs/MYGO_UPDATE_RESEARCH.md`）。`git status` 中 `go.mod`/`go.sum`、`internal/{cli,harness,attrib}/**` 以及 `internal/gui/{data,demo,mainwin,pages,state}.go`、`internal/gui/{providers.go,provider_test.go,provider_alias_test.go}` 的改动来自其他并行工作，不属于本批次。未提交、未发布、未打标签。

## 结论与范围

按 `docs/ROADMAP.md:146-151`（第一批：升级基线与低风险替换）只迁移两处自研实现：

1. **圆环图**（`internal/gui/charts.go`）：`donut` 不再用手写 `arc` 逐点采样画扇形，改用 MyGo v0.4.8 `ui/chart` 的 `chart.Pie` 切片 + `chart.Sector` 路径；指针命中从自算 `Atan2`/`Hypot` 改为 `chart.SliceAt`。手写的 `func arc(cx, cy, ro, ri float32, a0, a1 float64) *ui.Path` 已删除（全仓无其他调用者）。五色（Tomori/Anon/Rana/Soyo/Taki）、中心数字与 `tokens` 文案、入场扫过动画（`entrance`）、hover 外扩 4px 全部保留。
2. **分段选择器**（`internal/gui/widgets.go`）：`segmented` 改用 `ui.SegmentedBase(c, sel, n)` + `parts.Segment(i)`，由框架接管单选语义、方向键、Home/End 与越界索引收敛；胶囊外观、`glider` 拇指动画、hover 底色、12/600 字号字重全部保留。签名 `func segmented(c *ui.Context, pal palette, sel *int, labels ...string) bool` 未变，三个调用点（`internal/gui/mainwin.go:193` 时间跨度、`internal/gui/pages.go:20` 排行标签页、`internal/gui/sessions.go:14` 排序）一行未改。

明确**不做**的事（不拿自造实现冒充"原生"）：堆叠条/缓存时间轴（`Stack`/`Select`）、折线与面积、tooltip、大表格、侧边栏、模型映射与定价多倍率输入全部留到后续批次（见"未迁移清单"）。provider 稳定身份与 alias 异步修复未触碰；本批次只消费既有 API，没有新增或修改任何后端契约。

**这不是 0.4.0 的完成声明，也不是真实窗口/IME/屏幕阅读器/DPI 的验证。** 本批次证据是合成快照 + 包内测试，未关闭项见文末。

## 先红后绿证据（精确命令与结果）

先写测试再迁移。先把测试与三个纯辅助函数（`donutGap`、`donutSlices`、`donutHover`）落地、**绘制仍是旧 `arc`** 时运行：

```sh
go test ./internal/gui/ -run 'TestSegmented|TestDonut' -count=1
```

结果 2 失败 9 通过，失败原文（行号对当时的 `internal/gui/native_controls_test.go`，也即现在的行号）：

```
--- FAIL: TestSegmentedArrowKeysMoveChoiceAndFocus
    native_controls_test.go:106: Right from the first segment chose 0, want 1
    native_controls_test.go:109: Right did not move the keyboard focus to the second segment
    native_controls_test.go:115: Right from the second segment chose 0, want 2
    native_controls_test.go:125: Left from the first segment chose 0, want it to wrap to 2
--- FAIL: TestSegmentedClampsAChoiceOutOfRange
    native_controls_test.go:139: choice 99 settled at 99, want 2
    native_controls_test.go:139: choice -4 settled at -4, want 0
```

红在哪里：迁移前每段只是 `ui.Row(c.Key(l)).Role(ui.RoleButton)`，没有 `Focusable()`，键盘到不了控件，也没有方向键语义；越界索引原样留在调用方变量里（`internal/gui/state.go:325` 的 `sortKeys[s.sortIdx]` 会因此越界）。这两点迁移后成为框架行为，测试转绿。

需要说清楚：4 个圆环测试在迁移**前后都通过**。圆环这次换的是内部几何与命中实现，不是新增行为，所以它们是**契约测试**（新旧实现都必须满足同一契约），本文不把它们算作红绿证据——红绿证据只属于分段控件。

迁移后：

```sh
go vet ./internal/gui/                                                                             # 干净
go test ./internal/gui/ -run 'TestSpanSwitchClickLoadsTheNewSpan|TestSegmented|TestDonut' -count=1  # 12 个测试全过
go test ./internal/gui/ -count=1 -skip 'Tray'                                                       # ok github.com/zzstar101/mytoken/internal/gui 21.875s
```

按任务要求跳过 `TestTrayFirstFrame`（首帧性能预算）与 `BenchmarkTrayFirstFrame`，未跑完整仓库测试。

## 关键实现决策：为什么仍在 build 期写回 `*sel`

`ui.SegmentedBase` 生成的每段绑定 `radioInput`，它在 `applyInputs()` 中写 `*selected`；而 `applyInputs()` 在整棵视图 build 完之后才执行（`ui/runtime.go:378-445`、`ui/value_input.go:40`），随后可能再 build 一遍以呈现结果。调用方 `s.spanSwitch` 依赖 `segmented` 的返回值**当场**调用 `s.setSpan()`（`internal/gui/state.go:262-274` 读 `s.span`）并触发新跨度加载；若只依赖框架绑定，那一帧读到的是旧索引，新跨度永远不会加载。

因此迁移后保留迁移前的写法：

```go
if b.Clicked() && !on { *sel = i; changed = true }
```

框架随后写入同一个值，幂等。`TestSpanSwitchClickLoadsTheNewSpan` 专门钉住这条端到端路径：渲染真实的 `s.spanSwitch`，点击 `spanKeys[Span30]` 后同时断言 `s.span == int(Span30)` 与已加载数据的 `s.ov.Span == Span30`。

另一个刻意的偏离：去掉每段的 `.Role(ui.RoleButton)`，让框架的 `RoleRadio`（每段）与 `RoleRadioGroup`（轨道）生效。视觉无影响（20/20 分段快照逐字节相同），但无障碍树从"若干无名按钮"变为"单选组 + 有名单选"。**未关闭**：单选组自身还没有可访问名，需要给调用方加一个组名参数，会改到 `mainwin.go`/`pages.go` 等并行工作者正在改的文件，本批次不动。

`glider` 320ms 拇指动画**故意保留**，没有改为 `Transition.Match`：`docs/ROADMAP.md:151` 要求单独验证，不得假定基础控件自带当前拇指效果。`inner/gui/mainwin.go:124` 的侧边栏导航也复用 `glider`，未受影响。

## 与升级前基线的诚实差异

### 聚焦快照（40 张：分段 20 + 圆环 20）

**20/20 分段快照与迁移前逐字节相同**（`segmented-{rank,span,sort,one,none}-{zh,en}[-dark].png`）；**20/20 圆环快照不同**。差异不是等价，逐条列出（完整清单 `build/verification/v040/native-controls/diff.txt`）：

| 图（en，zh 数值相同） | 差异像素 | 占比 | 最大通道差 | 差异包围盒 |
| --- | --- | --- | --- | --- |
| `donut-mixed-en.png` | 3371 | 2.1069% | 176 | (40,40)-(340,340) |
| `donut-mixed-en-dark.png` | 3384 | 2.1150% | 227 | 同上 |
| `donut-single-en.png` | 2165 | 1.3531% | 106 | 同上 |
| `donut-two-en.png` | 2285 | 1.4281% | 106 | 同上 |
| `donut-uneven-en.png` | 2247 | 1.4044% | 122 | 同上 |
| `donut-zero-en.png` | 1128 | 0.7050% | **3** | 同上 |

差异的成因，逐条说明：

- **间隙几何不同**。旧 `arc` 在每段两端各内缩固定 `0.025` 弧度；`chart.Sector` 的间隙是 `Pad * sqrt(inner²+outer²)`（d3 padRadius），换算到 `ri=0.68·ro`、`ro=75`、`Pad=0.025` 时是约 `2.27px` 的**等宽**间隙（外缘 `0.015117` 弧度、内缘 `0.022232` 弧度）。所以新图间隙在内外缘宽度一致、视觉上略宽且更均匀。这是可见但合理的变化，不是等价。
- **过薄的切片会消失**。`chart.Sector` 在 `End-Start <= 2d` 时把切片收成中点、不画；`donut-uneven`（`{Input:1, Reasoning:999999}`）迁移前会画出一根发丝状细条 + 12 点方向细缝，迁移后是干净的一段缺口。真实数据下这类份额（<约 0.5%）肉眼不可辨，但这是行为差异，如实记录。
- **描边由折线变贝塞尔**。`Sector` 用圆弧/曲线构造路径，旧实现是 `ceil(跨度/(π/48))` 段折线，边缘更平滑；`donut-mixed` 的差异像素几乎全部来自弧边与间隙重采样。
- **空环只有抗锯齿差异**：`donut-zero`（全零数据）最大通道差为 **3**，即底色细环的重采样，无可见变化。
- 中心数字、文案、配色、占比、半径比例全部未变（目视对照 `donut-mixed-en` 前后两图一致）。

### 页面级交叉核对（46 张整窗快照）

迁移后整窗快照渲染到**新目录**（未覆盖任何既有基线）：

```sh
MYTOKEN_SNAPSHOTS="$PWD/build/verification/v040/native-controls/pages-after" MYTOKEN_SNAP_SCALE=1 \
  go test ./internal/gui -run '^TestSnapshots$' -count=1 -timeout=15m
go run ./build/verification/v040/native-controls build/verification/v040/after build/verification/v040/native-controls/pages-after
```

与迁移前的整窗基线 `build/verification/v040/after`（Oct 13 16:05）比较：**46 张中 36 张逐字节相同**，10 张不同（`pages-diff.txt`）：

- `{en,zh}-overview{,-dark}.png`：1478–1479 像素（0.1343–0.1344%），最大通道差 183–204，包围盒**恰好是 `(1044,388)-(1194,539)`**，即概览卡片里 150px 圆环的位置——差异被限制在圆环内，没有任何布局位移。
- `{en,zh}-empty-overview.png`：672 像素（0.0610%），最大通道差 **3**，同一包围盒（空环重采样）。
- `{en,zh}-tray{,-dark}.png`：3/12/24/12 像素，包围盒 `(19,56)-(32,60)`。**不能归因于本批次**：`internal/gui/tray.go` 既不使用 `donut` 也不使用 `segmented`；本动这两个文件之外无改动。

**保持逐字节相同**的还包括所有渲染了被迁移控件、以及未被迁移控件的页面：`sessions`（含 `sessions.go:14` 分段排序）、`ranking`（含 `pages.go:20` 标签页）、`cache`、`projects`、`relays`、`settings` 及全部 `empty-*`。概览页头部的时间跨度分段（`mainwin.go:193`）也在圆环包围盒之外，同样逐字节相同。

**诚实的限制**：整窗对比的参照物是 Oct 13 16:05 的基线，而当前工作树里其他工作者已改过 `internal/gui/{data,demo,mainwin,pages,state}.go`，所以"36 张相同"里也可能混入他们的影响；本批次能确定归属的只有圆环包围盒内的差异与分段控件的零差异。既有基线目录 `build/verification/v040/{before,after}` 全程未写入、未覆盖。

## 测试与快照清单

迁移文件：

- `internal/gui/charts.go`（+52/−55）：新增 `donutGap`、`donutSlices`、`donutHover`，`donut` 改用 `chart.Pie`/`chart.Sector`/`chart.SliceAt`，删除 `arc`。
- `internal/gui/widgets.go`（11 行，净 +7）：`segmented` 改用 `ui.SegmentedBase`/`parts.Segment`，签名不变。
- `internal/gui/native_controls_test.go`（新增，476 行）。

测试（`go test ./internal/gui`）：

- 选择与变更上报：`TestSegmentedClickSelectsAndReportsChange`、`TestSegmentedSingleChoiceReportsNoChange`、`TestSegmentedChoiceSurvivesResize`
- 键位映射与焦点：`TestSegmentedArrowKeysMoveChoiceAndFocus`（Right 0→1→2→0 回绕，Left 0→2，`Focused("Beta")`）
- 越界/零/未知数据：`TestSegmentedClampsAChoiceOutOfRange`（99→2、−4→0）、`TestSegmentedWithoutLabelsIsInert`、`TestDonutSlicesDropZeroAndUnknownClasses`（空、负数"未知"类、单类、跳类 `{Input:3,CacheRead:1}`）
- 端到端：`TestSpanSwitchClickLoadsTheNewSpan`（真实 `spanSwitch` → `s.span` + `s.ov.Span`）
- 渲染契约：`TestDonutHoverMapsTheAngleToTheClass`（π/4→input、5π/4→output、孔内/环外/空环→无、跳类后 5π/4→reasoning）、`TestDonutRingDrawsEachClassAndAnEmptyMix`
- reduced motion：`TestSegmentedThumbSettlesAtOnceWithReducedMotion`、`TestDonutSweepSettlesAtOnceWithReducedMotion`
- 快照：`TestNativeControlSnapshots`（需 `MYTOKEN_NATIVE_SNAPSHOTS=<dir>`）

快照路径（均为未跟踪产物，`.gitignore` 忽略 `/build/`）：

- 迁移前：`build/verification/v040/native-controls/before/`（40 PNG）
- 迁移后：`build/verification/v040/native-controls/after/`（40 PNG）
- 差异：`build/verification/v040/native-controls/diff.txt`、`compare.go`
- 整窗交叉核对：`build/verification/v040/native-controls/pages-after/`（46 PNG）、`pages-diff.txt`

复现命令：

```sh
MYTOKEN_NATIVE_SNAPSHOTS="$PWD/build/verification/v040/native-controls/after" \
  go test ./internal/gui -run '^TestNativeControlSnapshots$' -count=1
go run ./build/verification/v040/native-controls                   # before/ 对 after/
go run ./build/verification/v040/native-controls <beforeDir> <afterDir>   # 任意两目录
```

## 无障碍语义与 reduced motion：已验证 / 未验证

已被测试钉住（合成环境）：

- 分段控件是单选组：方向键同时移动焦点与选择（Right/Down +1、Left/Up −1），两端回绕，`Focused(label)` 在方向键后成立；每段有 `Label(l)` 作为可访问名；焦点可见状态由框架 `focusVisible` 管理。
- 越界索引被框架收敛到 `[0, n-1]`，不再可能出现 `sortKeys[s.sortIdx]` 越界。
- reduced motion：拇指在点击帧直接落到新选择（无 320ms 滑动）；圆环入场扫过在第二帧即为完整环。开启动画时扫过仍从 0 开始（有对照断言）。

未验证（本批次无法在合成测试器里覆盖）：

- 真实窗口的 Tab 顺序、Escape、焦点环渲染、屏幕阅读器朗读（`ui.SegmentedBase` 的可访问树只能通过 `Find`/`Focused`/`Announcements` 间接观察；`ui.Tester` 不导出 `platform.AccessTree`）。
- 圆环中心数字用 `Painter.RichText` 在 `Draw` 回调里绘制，**不进入** `rt.labels`，因此无法用 `Find` 定位；命中映射改用纯函数 `donutHover` 测试、渲染改用采样像素测试。这是测试方式上的限制，不是实现缺陷。
- 单选组自身仍无可访问名（见上文"关键实现决策"）。

## 未迁移清单（有代码依据的后续批次）

第二批（`docs/ROADMAP.md:153-158`）：

- 堆叠条与缓存时间轴 → `Stack`/`Select`、`chart.Select`：`internal/gui/charts.go`（柱状/堆叠绘制）、`internal/gui/cache.go`（135 行附近的跨度索引与时间轴）。
- 折线与面积 → `Line`/`Area`：`internal/gui/charts.go`。必须先比较 `CurveMonotone` 与当前贝塞尔、`Nice()` 与当前 2.5 刻度规则，按路线图要求不得机械等价。
- tooltip：普通 tooltip → `TooltipBase`；数据提示 → `chart.Select` + `Anchor`。
- 瀑布图只复用 scales/`Rect`/`Anchor`：`internal/gui/relays.go`。

第三批（`docs/ROADMAP.md:160-165`）：

- 大表格 → `ui.Table`：`internal/gui/pages.go`（排行表）。
- 侧边栏 → `Sidebar`/`SidebarItem`：`internal/gui/mainwin.go:124` 区域与 `internal/gui/sessions.go`。
- 模型映射 → `ComboboxBase`、定价多倍率 → `NumberInput`：`internal/gui/pricing.go`；路线图明确要求步进取整不得改变计费倍率。
- 纯位移/缩放/透明度 → `AnimatePose`/`Transition`。
- 分段控件的组名参数（见上）。

边界（`docs/ROADMAP.md:174-181`）：token 分类、缓存归因、模型映射规则、计费/对账算法不替换；品牌图形/logo/特殊数字排版保留；window/tray/update/glass 已由 MyGo 承担，不计入迁移额度。

## 未关闭项（平台门槛）

以下全部**未验证**，且本批次的合成快照不能替代：真实窗口焦点/Escape/IME（中文输入法）/emoji/屏幕阅读器；明暗主题实时切换（快照只覆盖两种静态主题）；设备上的 Reduce Motion；DPI 缩放；RSS/CPU/GPU；Windows 多显示器；Linux bounds 与托盘；安装包/签名/更新回滚。另：整窗对比受其他并行工作者改动影响这一限制（见上）。

本批次不是 0.4.0 的完成声明，也不是真实窗口/IME/无障碍的验证。
