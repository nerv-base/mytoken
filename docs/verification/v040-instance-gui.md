# 0.4.0 实例安全的 GUI 会话下钻与端点下钻验证

日期：2026-10-13。环境：Go 1.27.1、darwin/arm64。本批次写入范围：`internal/gui/**`（`data.go`、`state.go`、`sessions.go`、`cache.go`、`providers.go`、`widgets.go`、`i18n.go`、`mainwin.go`、`tray.go`、`pages.go`、`snapshot_test.go`、`demo.go`、`demo_cache.go`，以及新增的 `internal/gui/instance_gui_test.go`）、本文件，以及未跟踪构建产物 `build/verification/v040/instance-gui/**`。本批次未改 `main.go`、`internal/app/**`、`internal/cli/**`、`internal/store/**`、`internal/query/**`、`internal/harness/**`、`internal/attrib/**`、`internal/model/**`、`go.mod`、`go.sum`，也未改共享规划文件（`task_plan.md`、`docs/ROADMAP.md`、`progress.md`）。`git status` 中 `go.mod`/`go.sum`、`internal/{app,cli,harness,attrib}/**`、`internal/gui/charts.go` 以及 `internal/gui/{providers.go,provider_test.go,provider_alias_test.go,native_controls_test.go}` 的改动来自其他并行工作（见 `docs/verification/v040-native-controls.md`、`docs/verification/v040-provider-gui.md`），不属于本批次。未提交、未发布、未打标签。

## 结论与范围

同一 `harness + session_id` 可能同时存在于多个实例（例如 `default` 与 `alpha`）。旧的 GUI 把 `Session(h, id)` 当作"这个会话"的唯一入口，当两个实例各有一个 `s-dup` 时，列表键（`string(r.Harness)+"/"+r.SessionID`）会撞车、`Select` 只能带上 harness 与 id，下钻时打开的可能是**另一个实例**的同名会话。本批次只动 GUI：把实例身份从列表键、选中状态、回调、缓存页一直传到查询入口。

1. **会话身份贯穿**（`internal/gui/data.go:180-275`、`internal/gui/state.go:92-95,336-402`）：新增 `sessionRef{Harness, InstanceID, SessionID}`；`refOf` 用 `store.NormalizeInstanceID` 把空实例归一成 `store.DefaultInstanceID`（`internal/store/store.go:28` 的 `"default"`）；列表键 `key()` 为 `harness + "/" + instance + "/" + id`，两个实例的同名会话因此是两行、两个键。`State.sel` 由 `string` 改成 `sessionRef`，`loadDetail` 只在 `s.sel == ref` 时应用结果，`loadSessions` 只在排序键与端点筛选都没变时应用结果（避免慢查询覆盖新状态）。
2. **选中已知实例时不再走旧入口**（`internal/gui/data.go:241-275`）：`sessionIn`/`sessionCacheIn` 先做 `q.(query.InstanceService)` 断言走 `SessionIn`/`SessionCacheIn`，只有服务**根本没有**实例感知接口时才退回旧的 `Session`/`SessionCache`。测试用 `legacyBanService` 把两个旧方法改成记录并报错，断言下钻一次都没调到它们。`State.Select(h, id)` 保留，语义收窄为"缺省实例"的便捷入口（无行可用的调用点），不再是有行时的选择路径。
3. **实例只在会歧义的地方出现**（`internal/gui/data.go:211-234`、`internal/gui/sessions.go:90,131`）：`ambiguousSessions` 只把"同一 `harness/key` 被一个以上实例持有"的会话标记为需要标注，行内加 `chip` 显示**原始实例 id**；详情页头在实例非缺省时加 `Instance: <id>`。不歧义的行不加标记（不制造噪声），也从不把实例标签伪造成别的名字。
4. **端点下钻**（`internal/gui/providers.go:82-126,323-360`）：provider 详情新增 `View sessions` 动作 → `showProviderSessions(g)` 把 `State.provFilter` 设成 `providerDrill{Key: g.Key, Label: providerLabel(g)}` 并跳到会话页；`sessionFilter()`（`internal/gui/state.go:336-342`）只设 `query.Filter.ProviderIDs = []string{stableKey}`，**从不设** `Providers`（解析后的显示名），因此聚合走事实分支、定价与账单口径不变。会话页顶部出现筛选横幅（`providerFilterBanner`，含稳定 id 短码与 `Clear`）。重复 alias（两个端点都叫 `Shared`）也因此分得开。
5. **缓存页**（`internal/gui/cache.go:50-102`）：`cacheSession` 带 `ref sessionRef` 与解析出的 `row`；`parseCacheSessionKey` 反解 `query.CacheSessionKeyIn` 的 `instance:<id>:<harness>/<session>` 形式（缺省实例仍是旧的 `harness/session`）；`cacheSessionsFrom` 用解析出的实例去取行、取不到时用合成行兜底，保证点击落到正确实例。`cacheSessionLimit`/`USD <= 0` 的 Top-N 策略被拆到 `loadCache` 一侧，映射函数本身可单测。
6. **下钻会收起被筛掉的开着的会话**（`internal/gui/state.go:393-402`）：`showProviderSessions`/`clearProviderSessions` 调 `clearSelection`。否则详情区会继续显示一个已被筛掉的会话——看起来像筛选没生效（合成截图里确实看到过这个假象）。
7. **SegmentedBase 的可访问组名**（`internal/gui/widgets.go:251-287`）：新增 `segmentedGroup(c, pal, sel, group, labels...)`，与原 `segmented` 同体，只在 `group != ""` 时对 `parts.Track` 调 `Element.Label(group)`；`segmented` 变成 `segmentedGroup(..., "")` 的一行转发，签名与绘制不变。三个调用点改为传组名：时间跨度 `tr("span")`（`internal/gui/mainwin.go:193`）、排行依据 `tr("rankingBy")`（`internal/gui/pages.go:20`）、会话排序 `tr("sortBy")`（`internal/gui/sessions.go:15`）。mygo v0.4.8 的 `Element.Label` 只写无障碍标签（`ui/element.go:1072`），不参与布局与绘制，所以像素与宽度不变（有测试与像素对比兜底）。
8. **演示数据保持可用**（`internal/gui/demo.go:198-225,403-490`、`internal/gui/demo_cache.go`）：`add` 给行补上 `store.DefaultInstanceID`；`demoProviderID` 成为端点稳定 id 的唯一来源（`ProviderGroups` 与 `Sessions(ProviderIDs)` 共用）；`each`/`eachStep` 同时尊重 `InstanceIDs` 与 `ProviderIDs`；新增 `SessionIn`/`SessionCacheIn`，**只**回答缺省实例，其它实例返回 `demo: no instance %q` 错误——宁可答不上来，也不假装演示数据有第二个实例。

明确**不做**的事：不改 `internal/query/**` 与 `internal/store/**`（`SessionIn`/`SessionCacheIn`/`SessionRow.InstanceID`/`Filter.InstanceIDs`/`Filter.ProviderIDs`/`CacheSessionKeyIn` 全部按现状消费）；不改 CLI、main、app；不把 `SessionCacheKeyIn` 的字符串形式改成无损编码（只在其上做解析，并在代码注释里写明它对含 `:`/`/` 的 id 有损）；不做 sources/配置界面的端到端验证。

## 先红后绿证据

先写测试（`internal/gui/instance_gui_test.go`），在实现之前运行：

```sh
go test ./internal/gui -run 'TestSessionList|TestSelectedInstance|TestCachePage|TestCacheSessionKeys|TestSessionRowShows|TestProviderDrilldown|TestDemoServiceAnswers|TestSegmentedGroups|TestSegmentedGroupName' -count=1
```

结果是编译失败（不是断言失败），原文包含：

```
undefined: refOf
s.sel.InstanceID undefined (type string has no field or method InstanceID)
undefined: cacheSessionsFrom
undefined: parseCacheSessionKey
FAIL	github.com/zzstar101/mytoken/internal/gui [build failed]
```

红在哪里：那时 `State.sel` 还是 `string`（所以测试读 `s.sel.InstanceID` 编不过），也还没有实例键、缓存键反解与端点筛选。实现落地后同一条命令输出：

```
--- PASS: TestSessionListKeepsEqualIDsInDifferentInstancesApart (1.92s)
--- PASS: TestSelectedInstanceNeverUsesTheLegacyLookup (1.15s)
--- PASS: TestCachePageResolvesTheRightInstance (0.06s)
--- PASS: TestCacheSessionKeysParseBackToTheirInstance (0.00s)
--- PASS: TestSessionRowShowsInstanceOnlyWhenAmbiguous (0.34s)
--- PASS: TestProviderDrilldownFiltersByStableIDNotAlias (1.91s)
--- PASS: TestDemoServiceAnswersInstanceAwareLookups (0.05s)
--- PASS: TestSegmentedGroupsAreNamedAtEveryCallSite (2.54s)
--- PASS: TestSegmentedGroupNameDoesNotChangePainting (0.01s)
ok  	github.com/zzstar101/mytoken/internal/gui	8.346s
```

## 测试清单

新增 `internal/gui/instance_gui_test.go`，11 个测试。夹具（`instanceFixture`）用 `store.Open(":memory:")` + `query.NewService`，两个实例各一条同名会话 `s-dup`（`alpha` 2 条事件 + 1 个子会话，`beta` 1 条），两个端点 `endpointA`/`endpointB` 都起别名 `Shared`。

| 测试（行号） | 钉住的契约 |
| --- | --- |
| `TestSessionListKeepsEqualIDsInDifferentInstancesApart`（:154） | 两个实例的同名会话是两行两个键；点 alpha 那行，详情来自 alpha |
| `TestSelectedInstanceNeverUsesTheLegacyLookup`（:216） | `legacyBanService` 禁掉 `Session`/`SessionCache`，选中已知实例后断言调用次数为 0 |
| `TestCachePageResolvesTheRightInstance`（:241） | 真实 `CacheBy(CacheBySession)` 的键能反解并原样重建；两个实例的同名会话在缓存页是两行、各自取到自己的标题 |
| `TestCacheSessionKeysParseBackToTheirInstance`（:304） | 带实例键 / 旧式无实例键 / 缺省实例键 / 非法键的解析 |
| `TestSessionRowShowsInstanceOnlyWhenAmbiguous`（:334） | 只有真歧义的行才出现实例 chip，不歧义的行不出现 |
| `TestProviderDrilldownFiltersByStableIDNotAlias`（:369） | 端点详情动作把 `ProviderIDs` 设成稳定 id、`Providers` 为空、`Breakdown` 里解析出的 provider 仍是原名；下钻列表只剩该端点的会话；被筛掉的开着的会话被收起；`Clear` 能清掉筛选 |
| `TestDemoServiceAnswersInstanceAwareLookups`（:459） | 演示服务回答缺省实例、拒绝未知实例；演示行按实例路径打开 |
| `TestInstanceSnapshots`（:521） | `MYTOKEN_INSTANCE_SNAPSHOTS` 渲染合成截图（见下） |
| `TestSegmentedGroupsAreNamedAtEveryCallSite`（:592） | 三个调用点构建出的分段控件都有组名（`span`/`rankingBy`/`sortBy`） |
| `TestSegmentedGroupNameDoesNotChangePainting`（:618） | 带组名与不带组名渲染出的像素完全一致 |

既有契约：`go test ./internal/gui/ -count=1` → `ok github.com/zzstar101/mytoken/internal/gui 36.155s`（含 provider alias、provider 稳定身份、原生控件、快照与性能守卫等全部既有测试）。`go vet ./internal/gui/` 无输出。`go build ./...` 通过。冻结包只读消费方自检：`go test ./internal/query/ ./internal/store/ -count=1` → 两者均 `ok`。

## 合成截图

```sh
MYTOKEN_INSTANCE_SNAPSHOTS="$PWD/build/verification/v040/instance-gui" go test ./internal/gui -run TestInstanceSnapshots -count=1
```

写到新目录 `build/verification/v040/instance-gui/`（升级基线 `build/verification/v040/before|after|native-controls` 一字未动），共 10 张：`{zh,en}-sessions-twins.png`、`-provider-detail.png`、`-provider-drilldown.png`、`-provider-drilldown-dark.png`、`-cache-twins.png`。人工看过 en-sessions-twins.png：列表两行 "Alpha work / Codex [alpha] · alpha-app" 与 "Beta work / Codex [beta] · beta-app"，详情页头是 `Codex` + `Instance: alpha`，会话 id `s-dup`；zh-provider-drilldown.png：顶部 "按端点筛选 Shared 1a012f8d33… 清除"，列表只剩该端点的一条会话，详情区是空态（不残留旧会话）。

## 像素级回归对比（同数据、同参数）

`docs/verification/v040-mygo-baseline.md:23` 记录的渲染参数是 `MYTOKEN_SNAP_SCALE=1`。用同一参数把当前工作树的整页快照渲染到 `build/verification/v040/instance-gui/pages-after/`（46 张），再与基线 `build/verification/v040/after/` 逐像素比对（复用 `build/verification/v040/compare.go` 的算法，程序放在仓库外的临时目录）：

```
en-empty-overview.png: 672 pixels (0.06105%), bounds (1044,388)-(1194,539)
en-overview-dark.png: 1479 pixels (0.13436%), bounds (1044,388)-(1194,539)
en-overview.png: 1478 pixels (0.13427%), bounds (1044,388)-(1194,539)
en-tray-dark.png: 14 pixels (0.00792%), bounds (19,56)-(29,60)
en-tray.png: 15 pixels (0.00848%), bounds (19,56)-(32,60)
zh-empty-overview.png: 672 pixels (0.06105%), bounds (1044,388)-(1194,539)
zh-overview-dark.png: 1479 pixels (0.13436%), bounds (1044,388)-(1194,539)
zh-overview.png: 1478 pixels (0.13427%), bounds (1044,388)-(1194,539)
zh-tray.png: 12 pixels (0.00679%), bounds (19,57)-(29,60)
```

其余 37 张（会话、排行、缓存、项目、Relay、各空态、各暗色）与基线**逐像素相同**——即"只在歧义时加实例标记"与分段控件组名没有改动任何绘制。

差异归属（都做过头两次渲染的确定性检查）：

- overview 的 150×150 差异框裁出来看是 `TOKEN MIX` 圆环里那几片细扇形的边缘，来自 `internal/gui/charts.go` 的圆环迁移（`v040-native-controls.md` 记录过它会影响概览页快照），不是本批次；本批次没有编辑该文件或概览页代码。同一工作树渲染两次，overview 的像素差为 0（确定性）。
- tray 的 10×4 差异框在同一工作树渲染两次之间**同样出现**（`en-tray.png` 两次运行差 10 像素，边界同为 `(19,56)-(29,59)`），是托盘上那个会动的指示点，属于渲染侧不确定性，不是本批次引入的偏移。

## 限制与未关闭项

1. **没有真实窗口验证**。全部证据是 `ui.Tester` 合成帧 + 包内测试；没有真实窗口、IME、屏幕阅读器、DPI/缩放、长列表滚动的验证。分段控件的组名是 setter 级别的，**没有**用无障碍树验证过（mygo v0.4.8 未暴露读取 `label` 的 API，测试只能证明"调用点都设置过"）。
2. **演示数据只有单实例**。`TWINS` 类界面（同名会话分实例、端点下钻、缓存页双行）只能用内存 store 夹具证明；`internal/gui/demo.go` 的 `SessionIn`/`SessionCacheIn` 对非缺省实例直接报错，所以演示模式下**看不到**这些新界面。这是刻意的诚实做法，不是已完成的多实例演示。
3. **缓存键形式有损**。`query.CacheSessionKeyIn` 的 `instance:<id>:<harness>/<session>` 形状在 id 含 `:` 或 `/` 时无法可靠反解；`parseCacheSessionKey`（`internal/gui/cache.go:66`）在其上解析并注明了这一点，本批次未改查询层的键编码（超出写入范围）。当前实例 id 来自扫描器目录名/配置，未发现含这些分隔符，但这是**假设**而非保证。
4. **只测了 GUI 包**。没有跑整仓 `go test ./...`（其他并行工作的工作树可能处于中间态）；本批次的证据限于 `internal/gui` 与冻结包的只读自检。
5. **sources/配置界面未验证**。本批次没有碰 `internal/gui/sources.go` 或配置界面，也没有在真实数据目录上做过端到端核对；"实例身份处处可见"只覆盖会话列表、会话详情、缓存页与端点下钻四处。
6. **`State.Select(h, id)` 仍是缺省实例入口**。它保留给没有行可用的调用点，语义是"缺省实例"。若将来有调用点在没有行的情况下指向非缺省实例，必须改用 `SelectRef`；测试只钉住了"有行时绝不走旧查询入口"，没有（也无法）钉住未来新调用点的选择。
