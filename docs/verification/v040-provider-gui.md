# 0.4.0 稳定 provider 身份 GUI 集成验证

日期：2026-10-13（第一轮）、2026-10-14（第二轮，见"第二轮"一节）。环境：Go 1.27.1、darwin/arm64。第一轮写入范围：`internal/gui/**`、本文件；第二轮追加 `main.go`、`main_test.go`。始终未改 `internal/query/**`、`internal/store/**`、`internal/cli/**`、`internal/app/**`，未改任何共享规划文件（`task_plan.md`、`progress.md`、`findings.md`、`docs/ROADMAP.md`）。未提交、未发布、未打标签。

## 结论与范围

GUI 的 provider 视图改为按**端点身份**（`query.ProviderGroup`）呈现，而不是旧的归因名 bucket：

- 概览页 `byProvider` 卡片改画端点组（`providerList`），不再 `topN(ov.Providers,5,"…")` 画 bucket；
- ranking 页 Providers 标签页（`rankTab==1`）整体改为端点组视图（`providersTab`）：前三名台阶 + 表格 + 选中详情；
- 选择只按稳定 `Key`（`endpoint:v1:<sha256>` / `unresolved:v1:<sha256>`），不按 `Label`，因此两个端点起同名显示名后仍是两行；
- 每个组显示其成员来源（`harness/provider`），合并组不丢原始出处；
- 显示名（alias）编辑/清除只对**已识别**端点开放，经既有 `store.SetProviderAlias` 写库，仅改显示；
- provider 查询失败时把错误**显示出来**（概览卡片与 Providers 标签页），不画成空列表冒充"零用量"；
- 未识别端点保持各自独立、如实标注为未识别（`Unidentified`），不合并、不伪装成已识别端点。

只读使用既有后端契约（`query.Service.ProviderGroups`、`query.Filter.ProviderIDs`、`store.SetProviderAlias`），**未修改任何后端签名或业务逻辑**。pricing / relay / billing 的 Origin+KeyID 路径、价格页面与多倍率编辑器均未触碰；端点身份键与显示名从不进入价格 API。

**这不是 0.4.0 的完成声明，也不是真实窗口/IME/无障碍的验证。** 未关闭项见文末。

## 先红后绿证据（精确命令与结果）

### 红：实现前（真实首跑，5 个测试全部失败）

先写测试再实现。实现前运行：

```sh
go test ./internal/gui/ -run 'TestProvider|TestOverviewProviderCard' -count=1
```

记录到的失败原因（原始输出为逐条 `t.Errorf`，未落盘保存；行号对应当时即现在的 `internal/gui/provider_test.go`）：

```
--- FAIL: TestProviderTabShowsEndpointIdentities
    provider_test.go:102: group "https://api.example.com/tenant/A" (d4b34a8cce…) is not shown
    provider_test.go:105: group "https://api.example.com/tenant/A" does not show its stable identity d4b34a8cce…
    provider_test.go:105: group "local" does not show its stable identity 386ed67783…
    provider_test.go:105: group "https://api.example.com/tenant/B" does not show its stable identity …
--- FAIL: TestProviderTabShowsMemberProvenance
    provider_test.go:127: member provenance "Codex" is not shown
    provider_test.go:127: member provenance "Claude Code" is not shown
--- FAIL: TestProviderTabLabelsUnknownsHonestly
    provider_test.go:156: an unresolved endpoint is not labelled as unidentified
    provider_test.go:160: unresolved group "local" is not shown   (两个未识别组各一次)
--- FAIL: TestProviderTabKeepsDuplicateAliasesApart
    provider_test.go:195: row for endpoint:v1:2cb99365… is not distinguishable from its twin  (两行各一次)
--- FAIL: TestOverviewProviderCardUsesEndpointGroups
    provider_test.go:219: the overview provider card does not show the endpoint group's display name
    provider_test.go:222: the overview provider card does not show the group's stable identity
```

红态是**纯 GUI 侧**的：`TestProviderTabShowsEndpointIdentities` 里 `len(gs) != 3` 的断言**通过**，即查询侧的合并（同端点两种拼法合一）、租户路径分离（`/tenant/A` 与 `/tenant/B`）、未识别端点不合并（每会话一个身份）在实现前就已经正确；失败全部来自"界面没有按身份画"。

### 绿：实现后

```sh
go test ./internal/gui/ -run 'TestProvider|TestOverviewProviderCard|TestSpanSwitchCancelsProviderLoad|TestAliasNever' -count=1 -v
```

```
--- PASS: TestProviderErrorIsCarriedNotSwallowed (0.01s)
--- PASS: TestProviderErrorIsShownInGUI (1.41s)
--- PASS: TestProviderGroupsCanceledLoadIsNotAFailure (0.02s)
--- PASS: TestSpanSwitchCancelsProviderLoad (0.02s)
--- PASS: TestProviderAliasSaveAndClearKeepsIdentityFilterAndPrices (0.96s)
--- PASS: TestProviderAliasOnlyForIdentifiedEndpoints (0.80s)
--- PASS: TestProviderAliasUnavailableWithoutStore (0.82s)
--- PASS: TestProviderAliasErrorIsShownAndRetryable (0.98s)
--- PASS: TestAliasNeverReachesPricingPickers (1.45s)
--- PASS: TestProviderTabShowsEndpointIdentities (0.85s)
--- PASS: TestProviderTabShowsMemberProvenance (0.71s)
--- PASS: TestProviderTabLabelsUnknownsHonestly (0.70s)
--- PASS: TestProviderTabKeepsDuplicateAliasesApart (0.74s)
--- PASS: TestOverviewProviderCardUsesEndpointGroups (1.27s)
ok  	github.com/zzstar101/mytoken/internal/gui	11.133s
```

（同一次 `-run` 还匹配到两个既有测试 `TestProviderMultiplier`、`TestProviderMultipliers`，均通过。）

整包（按约束排除性能循环与截图，见下）：

```sh
go test ./internal/gui/ -count=1 -skip 'TestTrayFirstFrame|TestSnapshots'
# ok  	github.com/zzstar101/mytoken/internal/gui	16.422s

go build ./...        # 通过
go vet ./internal/gui/  # 无输出
gofmt -l internal/gui/  # 无输出
```

### 变异检查（证明新测试有咬合力）

把 `internal/gui/providers.go:104` 的 `if !g.Identified || s.Hooks.SetProviderAlias == nil || s.pv.busy` 中的 `!g.Identified ||` 删掉（即允许给未识别端点起名），同一命令立刻变红：

```
--- FAIL: TestProviderAliasOnlyForIdentifiedEndpoints (0.03s)
    provider_alias_test.go:257: an unidentified endpoint was renamed (1 writes)
FAIL
```

随后逐字恢复该行（`grep` 确认 `if !g.Identified || s.Hooks.SetProviderAlias == nil || s.pv.busy {` 回来）并复跑整包转绿。

## 第二轮（workflow22）：真实入口接线 + 两个 GUI 缺陷修复

第一轮留下的门 1（`main.go` 未接线）与两处 review 发现的问题在本轮修复。写入范围新增 `main.go`、`main_test.go`；`internal/gui/**` 内只改 `providers.go` 与 `provider_alias_test.go`。

### 修复 1：`main.go` 真实接线（门 1 关闭）

`main.go` 的 `gui.Hooks{...}` 字面量抽成可测函数：

```go
func guiHooks(a *app.App, ctx context.Context, dataDir string, openMain func(), visible func() bool) gui.Hooks
```

- 它接收 `main()` **已经 `app.Open()` 过的那个** `*app.App`，因此不可能再开第二个 `Open()`；`Settings: a.Settings` 原样透传，`guiHooks` 里没有任何价格逻辑。
- `main()` 内改为 `hooks := guiHooks(a, ctx, dataDir, func() { mygo.RunOnMain(openMain) }, func() bool { return main != nil && main.IsVisible() || panel != nil && panel.IsVisible() })`，闭包捕获与拆分前逐字相同。
- 接线实现中发现一个真实边界：**nil 接收者的方法值不是 nil**（`var s *store.Store; f := s.SetProviderAlias` 得到非 nil 的 `f`），若直接写 `SetProviderAlias: a.Store.SetProviderAlias`，无 store 的 app 会让详情卡画出编辑区并在保存时 panic。因此改为：

  ```go
  var setProviderAlias func(ctx context.Context, id, label string) error
  if a.Store != nil {
      setProviderAlias = a.Store.SetProviderAlias
  }
  ```

  无 store 时该字段为 nil，编辑区按既有契约显示"这个版本没有接入显示名存储"。

红（把上面的 `if a.Store != nil` 变异为 `if false`，编译通过、测试立刻红）：

```sh
go test . -run TestGUIHooksWireTheProviderAliasStore -count=1
```

```
--- FAIL: TestGUIHooksWireTheProviderAliasStore (0.02s)
    main_test.go:50: main.go's hooks do not wire a.Store.SetProviderAlias: the providers tab has no display-name store
FAIL	github.com/zzstar101/mytoken	0.518s
```

绿（逐字恢复该行后）：

```
--- PASS: TestGUIHooksWireTheProviderAliasStore (0.02s)
ok  	github.com/zzstar101/mytoken	0.516s
```

`main_test.go` 用真实内存库（`store.Open(":memory:")` + `store.Commit` + `query.NewService`）建一个已识别端点，调 `guiHooks(&app.App{Store: st}, …)`，再经 `hooks.SetProviderAlias` 写入，最后断言 `ProviderGroups` 里 **Key 不变、`Label` 变成写入值**——即"接线可用"是行为证据，不是源码文本匹配。同时断言零值 `&app.App{}` 下该字段为 nil 且 `Settings` 仍为 nil（透传而非替换）。

### 修复 2：详情显示完整稳定 Key，不再声称前缀唯一

`providers.go:33` 原注释称键尾"两个端点绝不共享"，这是**假的**：`providerKeyShort` 是"最后一段取前 10 字符 + `…`"的截断，两个不同 Key 完全可以共享这 10 个字符（真实 sha256 撞上前缀的概率极低，但"绝不"不是契约）。

- 注释改为如实说明：这是截断、不是身份，只用于紧凑行，详情页给出完整 Key。
- 紧凑标签保留（台阶/表格/身份行仍用 `providerKeyShort`）。
- 详情卡在身份行下新增一行完整稳定 Key，并用 MyGo 既有标准可复制控件 `Element.Selectable()`（`mygo@v0.4.8/ui/api_gen.go:4445`，拖选/右键 Copy/Cmd+C）使其可选中复制：

  ```go
  ui.Text(c, tr("providerStableKey")+"  "+g.Key).FontSize(10.5).TextColor(pal.faint).FontFeatures("tnum").Wrap().Selectable()
  ```

  新增文案 `providerStableKey`：`{"完整稳定 ID（可选中复制）", "Full stable ID (selectable)"}`。

红（实现前，`provider_alias_test.go`）：

```
--- FAIL: TestProviderDetailShowsFullStableKeyWhenShortPrefixesCollide (1.11s)
    provider_alias_test.go:479: the detail does not show the full stable key "endpoint:v1:0123456789aaaaaaaa"
    provider_alias_test.go:488: the detail does not show the full stable key "endpoint:v1:0123456789bbbbbbbb"
```

回归用**合成** Key `endpoint:v1:0123456789aaaaaaaa` / `…bbbbbbbb`（先断言 `providerKeyShort` 两者相等，确保这对真的撞前缀），选中 A 时界面必须出现完整 A 且**不出现** B，切到 B 后反之。绿后渲染帧中的实际标签（截取自 `-v` 调试输出）：

```
"Full stable ID (selectable)  endpoint:v1:dc98018c19846365846507944b2e53bfec7b52a747dee20b12bf4753a94de21b"
```

同一帧里紧凑行仍是 `"Stable ID dc98018c19…"`，即"紧凑标签 + 完整 Key"同时成立。

### 修复 3：异步写入完成后不再污染另一个选中项的编辑框/状态

原 `setProviderAlias` 的 apply 闭包无条件写 `s.pv.alias=""`、`s.pv.msg`、`s.pv.err`。用户在保存 A 期间切到 B，A 的回调会清空 B 的编辑框并把 A 的确认/错误显示在 B 的位置。修复后：

```go
s.pv.busy = false                                  // busy 是全局的：这次写结束了
if err != nil {
    if s.pv.key == key { s.pv.err = err.Error() }  // 失败只报给它自己的组
    return
}
s.loadSpan(Span(s.span))                           // 写成功后照常刷新，无论现在选中谁
if s.pv.key != key { return }                      // 结果不写给别的选中项
s.pv.alias = ""
… s.pv.msg = …
```

保留了 review 要求的两件事：**全局 busy 释放**（不因切换选择而卡住）与**成功后刷新 span**（新名字在表格/台阶上更新）；只把"结果写回"限定在仍选中的那一组。全程仍在既有 `s.run("provider-alias", …)` 的 UI 线程 apply 里，未新增跨线程写。

红（实现前，两个确定性测试用 `newLoop()` 的异步 `post` + 一个 gate channel 把写入卡在飞行中，切换选择后再放行）：

```
--- FAIL: TestProviderAliasCompletionAfterSelectionChangeLeavesTheOtherEditor (0.88s)
    provider_alias_test.go:547: A's completion cleared B's editor: pv.alias = "", want Bee
    provider_alias_test.go:550: A's confirmation was written into B's status: pv.msg = "Display name set to Friendly"
    provider_alias_test.go:561: A's confirmation is shown in B's editor
--- FAIL: TestProviderAliasFailureAfterSelectionChangeIsNotTheOtherGroupsError (0.84s)
    provider_alias_test.go:615: A's failure is shown as B's error: pv.err = "alias store is down"
    provider_alias_test.go:623: A's failure is shown in B's editor
FAIL	github.com/zzstar101/mytoken/internal/gui	3.262s
```

绿（三个回归一起）：

```sh
go test ./internal/gui/ -run 'TestProviderDetailShowsFullStableKeyWhenShortPrefixesCollide|TestProviderAliasCompletionAfterSelectionChangeLeavesTheOtherEditor|TestProviderAliasFailureAfterSelectionChangeIsNotTheOtherGroupsError' -count=1
# ok  	github.com/zzstar101/mytoken/internal/gui	3.474s
```

成功用例还断言 A 的写入**确实落地**（重查 `ProviderGroups` 后 A 的 `Label == "Friendly"`，证明刷新路径没被跳过），并且渲染帧里 B 自己的状态行 `"B was here"` 仍在、A 的确认文案不在。

一处**诚实说明**：红态还包含一条 `B's editor lost what the user typed`（对输入框显示值的断言），绿阶段已删除该断言。原因是实测发现 MyGo 的 headless tester 只为 `kindText`/带 label 的元素登记 label，`TextInput`（`kindInput`）的输入值**不进** `Texts()`/`HasText`（调试打印的整帧 label 列表里没有 `Bee`）。因此"编辑框内容未被清空"改由绑定字段 `s.pv.alias` 断言（输入框直接绑 `&s.pv.alias`），状态行仍是真实绘制标签的断言。这比原来的假断言更准确，不是把测试改绿。上引红态行号（479/488/547/550/561/615/623）是**修复前那一版测试文件**的行号；删掉该条断言后，最终文件里这三个测试分别从 `provider_alias_test.go:458`、`:500`、`:574` 开始。

### 第二轮整包与构建

```sh
go test ./internal/gui/ -count=1 -skip 'TestSnapshots'   # ok  	github.com/zzstar101/mytoken/internal/gui	25.098s
go test . -count=1                                        # ok  	github.com/zzstar101/mytoken	0.871s
go build ./...                                            # 通过
go build -o /tmp/mytoken-gui-check .                      # 通过（只构建，不启动真实窗口）
go vet ./internal/gui/ .                                  # 无输出
gofmt -l main.go main_test.go internal/gui/providers.go internal/gui/provider_alias_test.go  # 无输出
```

本次 `-skip 'TestSnapshots'` **没有**跳过性能守卫 `TestTrayFirstFrame`（`internal/gui/perf_test.go:44`），它包含在 25.098s 的那次整包里并通过；`TestSnapshots` 本身在未设置 `MYTOKEN_SNAPSHOTS` 时会 `t.Skip`，且它渲染 ranking 页用的是默认 `rankTab=0`（Models），不覆盖 Providers 详情，因此本轮新增的详情行不影响它。

## 新增/修改文件

第一轮：

| 文件 | 状态 | 内容 |
| --- | --- | --- |
| `internal/gui/providers.go` | 新增（399 行） | `providerView`、`providerKeyShort`、`providerLabel`、`providerIdentity`、`providerMembersText`、`selectedProvider`、`selectProvider`、`setProviderAlias`、`providersTab`、`providerPodium`、`providerTable`、`providerDetail`、`providerAliasEditor`、`providerList`、`providerStrs`（经 `init()` 合并进 `strs`，同 `cache.go:399-405` 的模式） |
| `internal/gui/provider_test.go` | 新增（224 行） | 5 个先红后绿测试 + `providerFixture`/`newProviderFixture`/`commit`/`fixtureGroups`/`shortTail`/`providerGUI`/`renderProviderTab` 夹具（真实 `store.Open(":memory:")` + `query.NewService`） |
| `internal/gui/provider_alias_test.go` | 新增（421 行） | 10 个测试：错误保留与呈现、取消路径、span 切换取消 provider 加载、alias 保存/清除、未识别保护、无存储提示、失败可重试、pricing 隔离、全页面冒烟渲染 |
| `internal/gui/data.go` | 修改（+21/−6 净） | `Overview.Providers` 由 `[]query.Bucket` 改为 `[]query.ProviderGroup`；新增 `Overview.ProviderErr error`；`loadSpan` 的 bucket 循环去掉 `ByProvider`，改为 `q.ProviderGroups(ctx, f)` 并**保留**其错误 |
| `internal/gui/state.go` | 修改 | `Hooks.SetProviderAlias func(ctx context.Context, id, label string) error`（nil 时隐藏编辑区）；`State.pv providerView` |
| `internal/gui/pages.go` | 修改 | `rankingPage`：`rankTab==1` 时走 `s.providersTab(c,pal)`，不再把 `ov.Providers` 当 bucket |
| `internal/gui/mainwin.go` | 修改 | 概览 `byProvider` 卡片改为 `s.providerList(c,pal,ov.Providers,ov.ProviderErr,5)` |
| `internal/gui/demo.go` | 修改 | `ProviderGroups` 由"包一层 ByProvider"改为真正按 `ResolvedProvider` 聚合：累计 tokens/cost/requests、去重会话数与成员（`harness/provider`，按 harness 再 provider 排序），仍然 `Identified:false`、键仍为 `demo:provider:<name>` |
| `docs/verification/v040-provider-gui.md` | 新增 | 本文件 |

第二轮：

| 文件 | 状态 | 内容 |
| --- | --- | --- |
| `main.go` | 修改 | `gui.Hooks` 字面量抽成 `guiHooks(a, ctx, dataDir, openMain, visible)`；新增 `SetProviderAlias`（带 `a.Store != nil` 守卫），`main()` 改为调用该函数；其余字段逐字不变 |
| `main_test.go` | 新增（72 行） | `TestGUIHooksWireTheProviderAliasStore`：真实内存库 + `query.NewService`，经 `guiHooks` 的 `SetProviderAlias` 写入后按 Key 重查断言改名生效；零值 app 下为 nil |
| `internal/gui/providers.go` | 修改（399 → 417 行） | `providerKeyShort`/`providerIdentity` 注释改为如实说明"截断非身份"；详情卡新增完整稳定 Key 行（`Selectable()`）；`setProviderAlias` 的 apply 闭包加 `s.pv.key == key` 守卫（busy 释放与 `loadSpan` 刷新保持全局）；新增 `providerStableKey` 文案 |
| `internal/gui/provider_alias_test.go` | 修改（421 → 628 行） | 追加 `twoRenameFixture`/`groupLabel` 与 3 个回归：完整 Key 可辨认、成功后不污染他项、失败不报为他项错误 |

这些路径都是**实际生效路径**，不是未被调用的 helper：`pages.go` 的 `rankTab==1` 分支、`mainwin.go` 的概览卡片、`providers.go` 的行点击与按钮回调，均由上面的 headless tester 测试经真实 `State`/`MainView` 渲染后点击覆盖。

## GUI 契约

### 概览页 provider 卡片

- 数据：`Overview.Providers []query.ProviderGroup`（`loadSpan` 经 `ProviderGroups`）。
- 每行：显示名（`Label`，空则回退为键尾）、未识别时的 `Unidentified` 小标、tokens、第二行"稳定 ID 尾 + 成员来源"、相对最长条的 `rankBar`。
- 查询失败：卡片显示 `Failed: <错误>`，不画空列表。
- 选择与详情不在此卡片（概览只读）；需要别名编辑时进 ranking → Providers。

### ranking 页 Providers 标签页

- 顶部三段台阶：前三名，含显示名、稳定身份行、tokens 大字、cost/requests；未识别加 `Unidentified` 小标。
- 表格：`#`/share/requests/cost/tokens 列（沿用既有 `rankTable` 的列宽 22/54/70/74/80），每行两行说明——第一行显示名 + 身份小标，第二行**稳定身份尾** + **成员来源**，下面 `classBar`。
- 行整行可点（`Role(ui.RoleButton)` + `OnClick`），选中态 `pal.raised`、悬停 `pal.hover`，**按 `Key` 选中**。
- 选中后下方出现详情卡：显示名标题 + `Identified endpoint`/`Unidentified` 小标、身份尾、会话/请求/成本/tokens、**完整稳定 Key 行**（`Full stable ID (selectable)  endpoint:v1:<sha256>`，用 `Element.Selectable()` 可选中复制）、成员逐行（`harnessMark` + harness 显示名 + `·` + provider 名）、边界说明（"端点身份不是计费账户……"）、分隔线、显示名编辑区。

### 显示名（alias）

- 写：`Hooks.SetProviderAlias(ctx, key, label)`，由 `setProviderAlias` 经既有 `s.run("provider-alias", …)` 执行（同步模式内联，异步模式走 job 队列）；成功后 `s.pv.alias=""`、提示 `Display name set to X` 并重载当前 span；失败则 `s.pv.err` 显示在编辑区，`busy` 复位。
- 清：`providerAliasClear` 按钮 → `SetProviderAlias(ctx, key, "")`（store 侧空别名即恢复规范化端点名）。
- 保护：`!g.Identified`、`Hooks.SetProviderAlias == nil`、`s.pv.busy` 三种情况**不写**。未识别端点显示"这个端点没有稳定身份，不能起显示名"；未接线时显示"这个版本没有接入显示名存储"。
- 边界：写入的 ID 是**稳定 Key**；`Label` 只影响显示。测试断言：写前后按 `Filter.ProviderIDs` 查到的 `Totals` 完全相等（价格与聚合不变），Key 不变、组数不变。

### 错误与取消

- `loadSpan` 不再吞掉 provider 错误：`o.ProviderErr` 保留，`loadSpan` 仅在 `ctx.Err() != nil`（即取消）时才返回错误，因此普通查询失败不会让整页变错，也不会让 provider 区块变成零。
- `ProviderErr != nil` → Providers 标签页画 `emptyState(artPicks, "Failed", err.Error())`，概览卡片画 `Failed: <err>`；测试同时断言**不出现** `No data`。
- 取消：`loadSpan` 在每次循环前检查 `ctx.Err()`，并在 `ProviderGroups` 后再次检查；`State.loadSpan` 对 `errors.Is(ctx.Err(), context.Canceled)` 的加载**直接丢弃**（不写缓存、不报错）。测试覆盖：(a) 在 `ProviderGroups` 内被取消的 `loadSpan` 返回 `context.Canceled` 且不应用任何组；(b) span 切换时被放弃的 provider 加载不落地，且 `ov.ProviderErr` 仍为 `nil`（取消不等于失败）。

## 测试覆盖（19 个新测试）

夹具：真实 `store.Open(":memory:")` + `query.NewService(st)` + `store.Commit`（`providerFixture`），时间固定 `2026-05-20 15:00 local`；端点用例覆盖 `https://API.example.com:443/tenant/A/` 与 `https://api.example.com/tenant/A` 合并、`/tenant/B` 分离、`http://127.0.0.1:8080/v1` 未识别。

| 测试 | 覆盖点 |
| --- | --- |
| `TestProviderTabShowsEndpointIdentities` | **同端点两种拼法合并为一组、不同租户路径分离、未识别端点独立**；三组都在界面上，且各自显示稳定身份尾 |
| `TestProviderTabShowsMemberProvenance` | 合并组保留原始出处（Codex/alpha 与 Claude Code/beta 都出现在界面） |
| `TestProviderTabLabelsUnknownsHonestly` | 未识别端点**不合并**（两个 session 两组、Key 不同）、界面标注 `Unidentified`、各自显示身份尾 |
| `TestProviderTabKeepsDuplicateAliasesApart` | 两个端点设同一显示名 `Shared` 后仍是两行，靠稳定身份尾区分（**选择不能靠 Label**） |
| `TestOverviewProviderCardUsesEndpointGroups` | 概览卡片画端点组：显示名与稳定身份尾都在 |
| `TestProviderErrorIsCarriedNotSwallowed` | provider 查询失败不使整页失败，错误保留在 `Overview.ProviderErr`，组列表为空（不是零） |
| `TestProviderErrorIsShownInGUI` | 失败在 Providers 标签页与概览卡片都可见，且**不**显示 `No data` |
| `TestProviderGroupsCanceledLoadIsNotAFailure` | 在 `ProviderGroups` 中被取消 → 返回 `context.Canceled`、不应用任何组 |
| `TestSpanSwitchCancelsProviderLoad` | span 切换取消进行中的 provider 加载：被放弃的 span 不落地、`ov.ProviderErr == nil`、当前 span 正确 |
| `TestProviderAliasSaveAndClearKeepsIdentityFilterAndPrices` | 经**界面点击**保存/清除：写入 ID 是稳定 Key、显示名变化、Key 与组数不变、按 `ProviderIDs` 的 `Totals` 逐字段相等、清除后回到规范化端点名 |
| `TestProviderAliasOnlyForIdentifiedEndpoints` | 未识别端点**不写库**（写入计数 0）、界面解释原因、不提供保存/清除控件 |
| `TestProviderAliasUnavailableWithoutStore` | `Hooks.SetProviderAlias == nil` 时给出"未接线"提示且不给控件（第一轮时 app 的真实状态；第二轮已接线，见门 1 关闭） |
| `TestProviderAliasErrorIsShownAndRetryable` | 失败可见、显示名未被改动、`busy` 复位、第二次重试成功并给出确认 |
| `TestAliasNeverReachesPricingPickers` | 别名写入后价格页仍只拿到归因名 `[alpha]`；不含 `endpoint:v1:` 或别名；settings 页不出现端点身份尾或别名 |
| `TestPagesRenderWithDemoEndpointGroups` | 七个页面用 demo 数据各渲染一帧不 panic 且非空；demo 的 Providers 标签页如实标注未识别（**demo 数据不伪造端点声明**） |
| `TestProviderDetailShowsFullStableKeyWhenShortPrefixesCollide` | 合成两个共享前 10 字符的 Key：详情必须给出完整 Key，且只给当前选中那个（紧凑标签仍保留） |
| `TestProviderAliasCompletionAfterSelectionChangeLeavesTheOtherEditor` | 异步保存 A 期间切到 B：A 完成后全局 busy 释放、A 的写入落地并刷新，但不清空 B 的编辑框、不把 A 的确认写进 B 的状态 |
| `TestProviderAliasFailureAfterSelectionChangeIsNotTheOtherGroupsError` | 同上但写入失败：busy 释放，A 的错误不显示为 B 的错误，B 的编辑框/状态不变 |
| `TestGUIHooksWireTheProviderAliasStore`（`main_test.go`，package main） | `main.go` 的 hooks 真接线：经 `guiHooks` 的 `SetProviderAlias` 写入后按 Key 重查可见改名；零值 app 下该字段为 nil（无 store 不画编辑区） |

演示数据：`demo.ProviderGroups` 现在带真实成员与去重会话数，但 `Identified:false`（demo 事件没有 BaseURL），界面因此显示未识别小标与成员来源——既能演练 UI，又不声称这些合成 provider 有端点身份。

## 兼容性与未变契约

- `query.Service` 全部签名未改；只读调用 `ProviderGroups`/`Totals`/`Filter.ProviderIDs`。
- `store.SetProviderAlias` 语义未改（仅显示、空值恢复、控制字符拒绝、未知身份报错）。
- 未触碰 pricing / relay / billing：`internal/gui/pricing.go`、`relays.go`、`sources.go`、`charts.go`、`theme.go`、`widgets.go` 未改；多倍率编辑器仍只吃 `Settings.Providers()`（`SELECT DISTINCT resolved_provider`，归因名）。
- 主题与原生布局约定保留：复用既有 `pane`/`card`/`kicker`/`chipOn`/`dot`/`classBar`/`rankBar`/`emptyState`/`bandAt`/`harnessMark`/`hoverFade`/`entrance`，未新增颜色、未改 `palette`、未改侧边栏（`navItems` 未动，因此侧边栏快照不变）。
- 余额从不相加：端点组只合并用量（tokens/cost/requests/会话），不含任何账户余额字段；`CostUSD` 仍是逐事件历史成本之和。
- `internal/gui` 未新增依赖；仅用 MyGo v0.4.8 已有控件（`ui.Text`/`Row`/`Column`/`Button`/`TextInput`/`Divider`/`Spacer`/`Icon`/`Box`/`Scroll`，以及第二轮详情页的 `Element.Selectable()`，见 `mygo@v0.4.8/ui/api_gen.go:4445`），全部按 `$(go env GOMODCACHE)/github.com/egoist/mygo@v0.4.8` 源码核对，未臆造方法。未新增自定义复制按钮——`Selectable()` 是 MyGo 既有的标准可复制控件（拖选 + 右键 Copy/Select All + Cmd+C）。

## 诚实说明的开放行为

- 界面上的"稳定身份"在**紧凑行**里是**键尾**（前缀去掉后的哈希前 10 位 + `…`），它是**截断、不是身份**：两个不同 Key 可以共享这 10 个字符（第二轮已把详情页改为给出完整 Key，并把原先"两个端点绝不共享"的错误注释删除）。一旦起了别名，`ProviderGroup` 契约里 `Label` 就是别名、不再暴露原始端点，因此原始 URL 无法在界面上还原；紧凑行只能给键尾，**详情页给完整稳定 Key**（可选中复制），这才是此时唯一完整的身份标识。测试用同一个 `shortTail` 逻辑断言键尾可见，并用合成撞前缀的一对 Key 断言完整 Key 可辨认。
- 未识别端点的键是**每会话**身份（`unresolved:v1:<sha256(harness,session,provider,baseURL)>`），所以同一 URL 的两个会话在界面上是两行；这是后端既有语义，界面如实呈现而非猜测合并。
- 只做了 headless（`ui.Tester`）渲染与点击验证：**未**验证真实窗口、IME、无障碍朗读、缩放/多显示器、GPU 渲染。`Role(ui.RoleButton)`/`Cursor` 已设置，但未做无障碍实测。
- 未运行截图（`TestSnapshots`）与性能循环（`TestTrayFirstFrame`），因 workflow16 正在 `internal/query`+`internal/store` 上做基准与优化。**概览页快照内容会变化**（provider 卡片改为端点组），需要下次截图运行时重新生成并人工审阅；ranking 快照用的是默认 `rankTab=0`（Models），不受本次改动影响。
- 未在真实用户库上运行（约束：无真实 DB/日志/凭据、无网络）；全部证据来自内存库合成夹具。

## 未关闭的门

1. ~~**`main.go` 接线**~~：**第二轮已关闭**。`main.go` 现有 `guiHooks(...)`，其中 `SetProviderAlias: setProviderAlias`（`a.Store != nil` 时取自 `a.Store.SetProviderAlias`），并由 `main_test.go` 的 `TestGUIHooksWireTheProviderAliasStore` 以真实内存库行为验证。真实 app 的 Providers 详情卡不再显示"未接线"。仍**未**在真实窗口里手点过保存按钮（见门 5）。
2. 未跑全量测试套件与 `go test ./...`（约束：workflow16 在 `internal/query`/`internal/store` 上跑基准）；只跑了 `go build ./...`、`go vet ./internal/gui/ .`、`go test .`、聚焦 `./internal/gui -skip TestSnapshots`。未跑 `./internal/store`、`./internal/query`、`./internal/cli`、`./internal/scan` 的测试（其他写入方在改）。
3. 截图未重跑：`TestSnapshots` 需要 `MYTOKEN_SNAPSHOTS` 才渲染（否则 `t.Skip`），本轮未设置，因此未做像素级对比与人工审阅。第一轮已记录概览页 provider 卡片改动会影响其快照；ranking 页快照走默认 `rankTab=0`（Models），且不覆盖 Providers 详情，本轮新增的完整 Key 行不影响快照。
4. 未实现"按端点身份过滤会话列表/详情"（本次只做概览与 ranking 的 provider 视图）；`Filter.ProviderIDs` 的会话级钻取属后续范围。**Providers 详情卡目前只有成员来源（harness/provider 列表），没有会话级钻取或过滤控件**——不要把它当作已交付。
5. 真实窗口/IME/无障碍未验证（见上）；接线后"点保存真的写进真实库"也只在 headless tester + 内存库里验证过。
6. 未提交、未发布、未打标签；`docs/ROADMAP.md`、`task_plan.md`、`progress.md`、`findings.md` 未改。
7. 原始端点 URL 在设置别名后无法在界面显示（契约限制），如需保留需后端在 `ProviderGroup` 上暴露原始端点——超出本范围。详情页现在给的是**完整稳定 Key**（`endpoint:v1:<sha256>`），不是原始 URL。
