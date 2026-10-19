# 0.4.0 稳定 provider 身份 CLI 集成验证

日期：2026-10-13；修订：2026-10-13（启动重定价修复，见"红 1"）；修订 2（`reconcile` 静默丢弃 provider 过滤标志，见"红 3"）。环境：Go 1.27.1、darwin/arm64。写入范围：`internal/cli/**`、本文件。未改 `internal/query/**`、`internal/store/**`、`internal/gui/**`，未改任何共享规划文件（`task_plan.md` 等）。

## 结论与范围

在 CLI 层接入既有后端契约：`query.Filter.ProviderIDs`（稳定端点身份键过滤）、`query.Service.ProviderGroups`（按端点身份分组）、`store.SetProviderAlias`（仅显示别名）。新增 `provider list` 与 `provider alias set/clear` 命令，新增可重复的 `--provider`（原始 provider 名，语义不变）与 `--provider-id`（稳定身份键），并新增 `stats --by endpoint` 端点身份分组模式。`stats --by provider`、`sessions`、`cache` 的既有 JSON/CSV 形状未变。后端 API（`query.Filter` 字段、`ProviderGroups`、`SetProviderAlias`）只读使用，未做任何修改；pricing/relay/billing 的 Origin+KeyID 逻辑路径未触碰（修订 2 的唯一例外是 `internal/cli/relay.go` 的参数解析层，见"红 3"）。

**修订（本轮）**：审查发现三条新命令经 `app.OpenLocal()` 打开，会因 `internal/app/app.go:92` 的 `EnsurePrices` 在陈旧指纹库上重定价历史事实并覆写 `pricing_fingerprint`（与帮助文本"never changes cost/fingerprint"矛盾）。已改为 `openProviderStore()`（最小本地打开），并补上真实首跑红→绿回归 `TestProviderCommandsDoNotRepriceHistory`。既有 `stats`/`sessions`/`cache` 的启动路径未改（见"未关闭的门"7）。

**这不是 0.4.0 的完成声明，也不是整库快照/statusline 的完成声明。** 本轮未实现全量只读版本化快照（后续独立范围）；未关闭项见文末"未关闭的门"。

## 先红后绿证据（精确命令与结果）

### 红 1：CLI 启动重定价回归（本轮修复；真实首跑证据）

本轮修复的缺陷：`provider list` 与 `provider alias set/clear` 原先调用 `app.OpenLocal()`，而 `internal/app/app.go:92` 无条件执行 `st.EnsurePrices(prices.Fingerprint(), prices.Evaluate)`。当库里 `settings.pricing_fingerprint` 落后于当前价格目录（任何被旧目录定价过的库），`EnsurePrices` 不是只做比较：它进入 `store.EnsurePrices` 的 `previous != fingerprint` 分支，调用 `replacePricing`，遍历所有非日志定价事件（`log_cost IS NULL`）按**当前**目录重算 `cost`，并覆写 `pricing_fingerprint`。于是"列出端点"或"给端点起个别名"会静默重定价历史事实。原有 `TestProviderIDDoesNotChangeHistoryFingerprint` 看不到这一点：它的夹具经 `app.OpenLocal()` 播种，指纹本就是当前值，`EnsurePrices` 走 no-op 分支（这正是"只测指纹匹配"掩盖启动效应的原因）。

新增回归测试 `TestProviderCommandsDoNotRepriceHistory`（`internal/cli/provider_startup_test.go`）：先用既有夹具播种（7 个事件、每个成本 0.002、非零），再把 `pricing_fingerprint` 显式改写为陈旧值 `stale-historical-pricing-fingerprint-v0`，快照 `dedup_key|resolved_provider|cost|base_url`，然后分别运行三条命令并断言指纹与全部事实逐字节不变。

修复前（首次运行、未做任何移文件/重构）的输出：

```sh
go test ./internal/cli/ -run 'TestProviderCommandsDoNotRepriceHistory' -count=1 -v
```

```
=== RUN   TestProviderCommandsDoNotRepriceHistory
=== RUN   TestProviderCommandsDoNotRepriceHistory/list
    provider_startup_test.go:127: list replaced the historical pricing_fingerprint: "stale-historical-pricing-fingerprint-v0" -> "98fb0f0525aa5637489dc79f9c9b9b1d00ca3bd5603ba08c6ef07a2f40303270"
    provider_startup_test.go:130: list repriced historical facts:
        before: [d-00|name-a|0.002000|https://api.example.com/v1 d-01|name-b|0.002000|https://api.example.com/v1 d-02|name-c|0.002000|https://api.example.com/v1/alt d-03|name-c|0.002000|https://tenant-b.example.com/v1 d-04|name-d|0.002000|https://api.example.com/v1?key=SECRETVALUE d-05|name-d|0.002000|https://api.example.com/v1?key=SECRETVALUE d-06|weird,"label|0.002000|https://api.example.com/v1?token=SECRETVALUE]
        after:  [d-00|name-a|0.000135|https://api.example.com/v1 d-01|name-b|0.000260|https://api.example.com/v1 d-02|name-c|0.000510|https://api.example.com/v1/alt d-03|name-c|0.001010|https://tenant-b.example.com/v1 d-04|name-d|0.000073|https://api.example.com/v1?key=SECRETVALUE d-05|name-d|0.000085|https://api.example.com/v1?key=SECRETVALUE d-06|weird,"label|0.000023|https://api.example.com/v1?token=SECRETVALUE]
=== RUN   TestProviderCommandsDoNotRepriceHistory/alias_set
    provider_startup_test.go:127: alias set replaced the historical pricing_fingerprint: "stale-historical-pricing-fingerprint-v0" -> "98fb0f0525aa5637489dc79f9c9b9b1d00ca3bd5603ba08c6ef07a2f40303270"
    provider_startup_test.go:130: alias set repriced historical facts: (同上，7 行 cost 由 0.002000 变为目录重算值)
=== RUN   TestProviderCommandsDoNotRepriceHistory/alias_clear
    provider_startup_test.go:127: alias clear replaced the historical pricing_fingerprint: "stale-historical-pricing-fingerprint-v0" -> "98fb0f0525aa5637489dc79f9c9b9b1d00ca3bd5603ba08c6ef07a2f40303270"
    provider_startup_test.go:130: alias clear repriced historical facts: (同上)
--- FAIL: TestProviderCommandsDoNotRepriceHistory (0.40s)
    --- FAIL: TestProviderCommandsDoNotRepriceHistory/list (0.15s)
    --- FAIL: TestProviderCommandsDoNotRepriceHistory/alias_set (0.13s)
    --- FAIL: TestProviderCommandsDoNotRepriceHistory/alias_clear (0.13s)
FAIL
FAIL	github.com/zzstar101/mytoken/internal/cli	1.207s
```

`alias set` / `alias clear` 的 `after` 行与 `list` 完全相同（同一次 `replacePricing` 的确定性结果），此处为篇幅省略，原始输出逐字保留了三条子用例的完整 before/after。

修复后（同一命令，同一夹具）：

```sh
go test ./internal/cli/ -run 'TestProviderCommandsDoNotRepriceHistory' -count=1
# ok  	github.com/zzstar101/mytoken/internal/cli	1.096s

go test ./internal/cli/ -run 'TestProviderCommandsDoNotRepriceHistory' -count=1 -v
# --- PASS: TestProviderCommandsDoNotRepriceHistory (0.26s)
#     --- PASS: .../list (0.09s)  --- PASS: .../alias_set (0.09s)  --- PASS: .../alias_clear (0.08s)
```

### 红 2：功能实现前编译失败（历史记录；下方为**重构演示**，非首跑证据）

> 说明（本轮补记）：本节的红态**不是**首次运行的原始证据。当时的做法是临时把已完成的 `provider_output.go` 移开再跑（`mv` aside / replay），属于为记录失败原因而做的**重构演示（reconstructed demonstration）**。它只证明"测试引用的新符号缺席时编译失败"，不能作为 TDD 首跑证据。相比之下，"红 1"是修复前对真实实现执行新回归测试的**真实首跑输出**，二者不可混同。

```sh
go test ./internal/cli/ -run 'TestProvider|TestStableID|TestStatsByEndpoint'
```

实现前的结果：编译失败，测试引用的新符号尚未存在（真实输出为 `# github.com/zzstar101/mytoken/internal/cli [build failed]`，首个错误为 `internal/cli/provider_identity_test.go:288:38: undefined: providerCSVHeader`，随后在 `:289`、`:320`、`:507`、`:508` 处重复失败）。

该红态可在当前工作区复现（临时移走新渲染文件，run 后立即恢复；下面是实际捕获的输出）：

```sh
mv internal/cli/provider_output.go internal/cli/provider_output.go.stub
go test ./internal/cli/ -run 'TestProvider|TestStableID|TestStatsByEndpoint'
mv internal/cli/provider_output.go.stub internal/cli/provider_output.go
```

```
# github.com/zzstar101/mytoken/internal/cli [github.com/zzstar101/mytoken/internal/cli.test]
internal/cli/output.go:152:10: undefined: writeProviderGroupsTable
internal/cli/output.go:222:10: undefined: writeProviderGroupsCSV
internal/cli/provider.go:98:2: undefined: sortProviderGroups
internal/cli/provider.go:101:21: undefined: providerListReport
internal/cli/provider.go:110:7: undefined: writeProviderGroupsCSV
internal/cli/provider.go:116:7: undefined: writeProviderGroupsTable
internal/cli/provider_identity_test.go:294:38: undefined: providerCSVHeader
internal/cli/provider_identity_test.go:295:61: undefined: providerCSVHeader
internal/cli/provider_identity_test.go:326:24: undefined: providerCSVHeader
internal/cli/provider_identity_test.go:513:26: undefined: providerCSVHeader
internal/cli/provider_identity_test.go:513:26: too many errors
FAIL	github.com/zzstar101/mytoken/internal/cli [build failed]
```

（行号相对首轮记录整体 +6，因测试文件随后增加了 `strconv` 导入与空标签用例；符号与失败原因一致。）

### 绿：实现后

```sh
go test ./internal/cli/ -run 'TestProvider|TestStableID|TestStatsByEndpoint'
# ok  	github.com/zzstar101/mytoken/internal/cli	2.844s

go test ./internal/cli/ -count=1
# ok  	github.com/zzstar101/mytoken/internal/cli	5.387s

gofmt -l internal/cli/     # 无输出
go vet ./internal/cli/     # 无输出
```

本轮（启动重定价修复）后的整包结果：

```sh
go test ./internal/cli/ -run 'TestProviderCommandsDoNotRepriceHistory|TestProviderCommandsNeedNoCredentialSource|TestProviderAndProviderIDIntersectionAcrossCommands' -count=1
# ok  	github.com/zzstar101/mytoken/internal/cli	1.680s

go test ./internal/cli/ -count=1
# ok  	github.com/zzstar101/mytoken/internal/cli	6.230s

gofmt -l internal/cli/     # 无输出
go vet ./internal/cli/     # 无输出
```

仅运行聚焦范围（`./internal/cli`）：按约束未跑全量测试套件、GUI 与 strictperf（workflow16 正在 `internal/query`+`internal/store` 上工作）。

### 过程中出现并已修复的既有测试回归（同为红→绿）

改动 `usage` 段标题后，既有 `TestCacheDispatch`（`internal/cli/cache_test.go:597`）失败：

```
--- FAIL: TestCacheDispatch (0.00s)
    cache_test.go:599: help missing "options for stats, sessions and cache"
```

处理方式：保留原文标题 `options for stats, sessions and cache:`（不重命名既有帮助文本），另起一行 `(provider list accepts the same options)` 说明共享选项，随后整包测试转绿。

## 新增/修改文件

| 文件 | 状态 | 内容 |
| --- | --- | --- |
| `internal/cli/flags.go` | 新增 | `stringList []string`（`String`/`Set`/`values`，`Set` 去空白并拒绝空值）；`providerFilterFlags` 生成 `--provider and --provider-id` 提示串 |
| `internal/cli/provider.go` | 新增 | `providerUsage`、`runProvider`、`providerList`、`providerListCommand`、`providerAlias`、`providerAliasSet`、`providerAliasClear`；本轮新增 `openProviderStore()`（`paths.DBPath()`+`store.Open()`，不经 `app.OpenLocal()`），三条命令均改用它（见"红 1"） |
| `internal/cli/provider_output.go` | 新增 | `providerListReport`、`providerCSVHeader`、`providerGroupCSVRow`、`providerMembersText`、`writeProviderGroupsTable`、`writeProviderGroupsCSV`、`sortProviderGroups` |
| `internal/cli/provider_identity_test.go` | 新增 | 11 个聚焦测试（见下） |
| `internal/cli/provider_startup_test.go` | 本轮新增 | `TestProviderCommandsDoNotRepriceHistory`（启动重定价回归，真实首跑红→绿）、`TestProviderCommandsNeedNoCredentialSource`、`TestProviderAndProviderIDIntersectionAcrossCommands` |
| `internal/cli/options.go` | 修改 | `commonFlags`/`options` 增加 `providers`、`providerIDs`；注册 `--provider`、`--provider-id`；新增 `options.filter()` |
| `internal/cli/cli.go` | 修改 | 注册 `provider` 子命令；`stats --by` 接受 `endpoint`；`statsCommand` 使用 `opts.filter()`；`statsRows` 增加 `endpoint` 分支 |
| `internal/cli/output.go` | 修改 | `writeStatsTable`/`writeStatsCSV` 分派 `[]query.ProviderGroup` 到 provider 渲染器 |
| `internal/cli/commands.go` | 修改 | `usage`/`cacheUsage` 增加 `provider list`、`provider alias`、`--provider`、`--provider-id`、`--by endpoint` 文档；`sessionsCommand` 使用 `opts.filter()` |
| `internal/cli/cache.go` | 修改 | `cacheEnv` 增加 `providers`/`providerIDs`；session timeline 拒绝两个 provider 过滤标志（exit 2）；`cacheRun` 构造过滤器 |
| `internal/cli/relay.go` | 修订 2 修改 | 仅参数解析层：`parseReconcile` 用 `providerFilterFlags` 显式拒绝 `--provider`/`--provider-id`（exit 2），并设 `fs.Usage = reconcileUsage`；`reconcileReq`/`selectSites`/`Report` 等 Origin+KeyID 与对账逻辑未改（见"红 3"） |
| `internal/cli/relay_test.go` | 修订 2 修改 | 新增 `TestParseReconcileRejectsProviderFilters`（7 条仅解析子用例 + `--key-id` 正例）；`TestRelayDispatchAndParsing` 增 2 条 CLI 边界用例 |
| `internal/cli/cache_test.go` | 修订 2 修改 | `TestCacheParseErrors` 增 3 条锁定用例，断言 session 时间线拒绝两个 provider 过滤标志（既有行为，此前无断言） |

命令注册是实际生效路径（不是未被调用的 helper）：`cli.go` 的 `run` 分支、`addCommonFlags` 的 flag 注册、`statsRows` 的 `endpoint` 分支、`writeStatsTable`/`writeStatsCSV` 的类型分派，均由真实 CLI 测试经 `runCLI(...)` 覆盖。

## 新命令契约

### `provider list [options]`

```
mytoken provider list [--since DATE|DUR] [--until DATE|DUR] [--last WINDOW]
                      [--provider NAME] [--provider-id ID] [--timezone TZ]
                      [--format table|json|csv] [--no-cost] [--offline]
```

- 默认范围为全时段（`--since` 缺省为空），与 `sessions` 一致。
- 打开方式：`openProviderStore()` = `paths.DBPath()` + `store.Open()`，**不经过 `app.OpenLocal()`**，因此**不扫描日志、不发起网络请求、不刷新价格、不重定价历史事实**（有无 `--offline` 都一样）。注意：`store.Open()` 仍会执行 schema 迁移与 provider 身份回填（可能增列/建表），所以这是"无扫描/无网络/无重定价"，**不是**逐字节只读快照。三条命令都不需要任何凭据来源（`TestProviderCommandsNeedNoCredentialSource`）。
- 输出：稳定键（`endpoint:v1:<sha256>` 或 `unresolved:v1:<sha256>`）、显示标签、`identified` 布尔、token/cost/requests/unpriced 聚合、`members`（`harness/provider`）。
- 表格 / JSON / CSV 三种格式；`--no-cost` 时去掉 `COST USD` 列与 `costUsd` 等字段。
- 行序：按稳定键升序（`sortProviderGroups`，稳定排序）。`stats --by endpoint` **不**排序，保留查询的流量序（tokens 降序）。
- 该命令不会打印原始端点 URL：未识别端点（如带查询串）的标签是被解析出的 provider 名，`--provider-id` 值只出现在其自身身份键中。

真实输出（合成库，7 事件，6 个身份）：

```
$ mytoken provider list --since 2026-03-01 --until 2026-04-01 --timezone UTC --offline
Identities: 6
ID                                                                              LABEL                            IDENTIFIED  REQUESTS  TOKENS  COST USD  UNPRICED  MEMBERS
endpoint:v1:134a8870818d01b99a4d649e06207d6a0d016c6772bfccea4a4bc5017fb64c51    https://api.example.com/v1/alt   true        1         401     0.0020    0         codex/name-c
endpoint:v1:3aed1d7e3d8e8b7127cd4fbd5a2e1e58e86ccd6bcde5712578ca5f7272629ea3    https://tenant-b.example.com/v1  true        1         801     0.0020    0         codex/name-c
endpoint:v1:b6332d8e6a2066fb8c4dc0194aaf16d0c1ed6baa389814dcce67bac25a00c67b    https://api.example.com/v1       true        2         302     0.0040    0         claude-code/name-b, codex/name-a
unresolved:v1:0367e2d0480b36921ba3b49825fd35568a24673909dd7803641b3da7df0c08e3  weird,"label                     false       1         11      0.0020    0         codex/weird,"label
unresolved:v1:119b35b933b23ef2168252bea7d57254aaa0b7c4788a976558f4efd775986b68  name-d                           false       1         51      0.0020    0         codex/name-d
unresolved:v1:b74d92eba4751151e35e3eb2cb786979fd4ba0bd4403a65cf812908f35c1e872  name-d                           false       1         61      0.0020    0         codex/name-d
```

JSON 形状（`provider list --json`，节选）：

```json
{"by":"endpoint","groups":[{"key":"endpoint:v1:b633…c67b","label":"https://api.example.com/v1",
 "tokens":{"input":300,"output":2,"cacheRead":0,"cacheWrite":0,"reasoning":0},
 "costUsd":0.004,"costLogUsd":0,"costEstimateUsd":0.004,"requests":2,"sessions":2,"unpriced":0,
 "identified":true,"members":[{"harness":"claude-code","provider":"name-b"},{"harness":"codex","provider":"name-a"}]}]}
```

CSV 表头逐字为 `providerCSVHeader`：

```
key,label,identified,requests,sessions,tokens,input,output,cache_read,cache_write,reasoning,cost_usd,unpriced,members
```

转义（同一夹具里的敌意 provider 名 `weird,"label`）：CSV 字段被引号包裹并双写引号 `"weird,""label"`、`"codex/weird,""label"`；JSON 转义为 `weird,\"label`。

### `provider alias set/clear`

```
mytoken provider alias set <provider-id> <label>
mytoken provider alias clear <provider-id>
```

- 调用 `Store.SetProviderAlias(ctx, id, label)`；仅改显示标签，不改身份键、归因、成本或 pricing fingerprint。证明来自 `TestProviderCommandsDoNotRepriceHistory`（在**陈旧** `pricing_fingerprint` + 非零成本夹具上断言逐字节不变）；`TestProviderIDDoesNotChangeHistoryFingerprint` 只覆盖"指纹已是当前值"的 no-op 路径，不足以证明启动不重定价。
- 空/纯空白标签按参数错误 exit 2（若放行会静默清除别名）：`provider alias set: empty label (use "provider alias clear" to remove an alias)`；测试同时断言标签未被改变。
- 未识别身份（`provider_endpoints` 中无该键）无法别名，store 错误原样输出并 exit 1：`unknown provider identity`。
- 标签可重复：两个身份设成同一标签后，`--provider-id` 仍按 ID 精确选择（`TestProviderDuplicateAliasLabels`）。
- 缺少子命令或参数个数不对 → exit 2 + `providerUsage`。
- 本轮**未**实现 `provider alias list`（显式视为未知子命令 → exit 2）。

真实输出：

```
$ mytoken provider alias set endpoint:v1:b633…c67b "Primary Relay"
alias set: endpoint:v1:b6332d8e…c67b -> Primary Relay
（随后 provider list 该行 LABEL 变为 Primary Relay，键与聚合不变）
$ mytoken provider alias clear endpoint:v1:b633…c67b
alias cleared: endpoint:v1:b6332d8e…c67b
（LABEL 回到 https://api.example.com/v1）
```

### 过滤：`--provider` 与 `--provider-id`

- `--provider NAME`：可重复，写入 `query.Filter.Providers`，语义仍是"原始 provider 名（解析后的定价名）"，与既有行为一致。
- `--provider-id ID`：可重复，写入 `query.Filter.ProviderIDs`，接受 `provider list` 打印的稳定键。
- 两者正交 AND（交集）；未知/格式错误的 ID 匹配 **0** 条，绝不回退为"全部"。
- 三个命令均支持：`stats`、`sessions`、`cache`（仅 ledger 视图）。

真实输出（同一夹具）：

```
$ mytoken stats --by model --provider name-a --provider-id endpoint:v1:b633…c67b --json …
{"totals":{…,"requests":1,…},"by":"model","rows":[{"key":"gpt-5",…,"requests":1,…}],…}   # 交集 = 1

$ mytoken stats --by model --provider-id endpoint:v1:deadbeef --json …
{"totals":{…,"requests":0,…},"by":"model","rows":[],"unpricedModels":[]}                 # 未知 ID = 空，不是全量

$ mytoken sessions --json --provider-id endpoint:v1:b633…c67b …
{"total":2,"sessions":[s-alt(claude-code/name-b), s-main(codex/name-a)]}

$ mytoken cache --by cause --json --provider-id endpoint:v1:b633…c67b …
{"by":"cause","totals":{"requests":2,"cacheable":0,…}}
```

`cache <session-id>` 时间线直接读单会话（`SessionCache`，无 Filter），因此显式拒绝两个过滤标志：

```
$ mytoken cache --provider-id endpoint:v1:b633…c67b s-main
cache: --provider-id cannot be combined with a session timeline   (exit 2)
```

### `stats --by endpoint`

新增分组模式，走 `ProviderGroups`，表格前打印既有汇总行；`--by provider` 行为与形状完全不变：

```
$ mytoken stats --by endpoint --since 2026-03-01 --until 2026-04-01 --timezone UTC --offline
Requests: 7  Sessions: 7  Tokens: 1627  Cost: $0.0140  Cache hit: 0.0%  Unpriced: 0
ID                                                                              LABEL                            IDENTIFIED  REQUESTS  TOKENS  COST USD  UNPRICED  MEMBERS
endpoint:v1:3aed1d7e…e5e3    https://tenant-b.example.com/v1  true  1  801  0.0020  0  codex/name-c
…（流量序，非键序；末三行为 unresolved 身份）

$ mytoken stats --by provider --format json --since … --offline
{"totals":{…},"by":"provider","rows":[{"key":"name-c",…},{"key":"name-b",…},{"key":"name-d",…},{"key":"name-a",…},{"key":"weird,\"label",…}],"unpricedModels":[]}
```

`--by provider` 的 key 仍是 pricing provider 名，不含端点身份键——脚本依赖的旧形状未被替换。

## 测试覆盖（`internal/cli/provider_identity_test.go` + 本轮 `internal/cli/provider_startup_test.go`）

夹具：`https://api.example.com/v1` 上两个不同原始名（`name-a`/codex、`name-b`/claude-code）、`https://api.example.com/v1/alt`、`https://tenant-b.example.com/v1`、带秘密查询串的 `https://api.example.com/v1?key=SECRETVALUE`（两条不同 session）、以及 `https://api.example.com/v1?token=SECRETVALUE` 上的敌意 provider 名 `weird,"label`；每个事件用 `store.Resolution{Provider, Attrib: model.AttribLog, Cost: 0.002, Priced: true}` 提交，以保留 `settings.pricing_fingerprint`。

| 测试 | 覆盖点 |
| --- | --- |
| `TestProviderListEndpointIdentity` | **同一端点两个原始名合并为一组**（requests=2、两个 members）；**租户分离**（不同 host 不同键/组）；未识别端点 `identified=false` |
| `TestProviderListFormats` | 表格/CSV/JSON 三种输出、表头、members 顺序（harness 再 provider）与分隔符（表格 `, ` / CSV `;`）、`--no-cost` |
| `TestProviderListJSONEscaping` | 敌意标签的 JSON/CSV 转义 |
| `TestStableIDFilteringAcrossCommands` | **ID 对 label**：别名只改 label，按 ID 过滤结果不变；未知 ID 匹配 0 条（不回退全部） |
| `TestStatsByEndpointGrouping` | `stats --by endpoint` 分组与 `ProviderGroups` 一致；`--by provider` 旧形状 |
| `TestProviderAliasSetClear` | set/clear 走 Store API，标签变化、键与聚合不变 |
| `TestProviderAliasErrors` | 未知身份 exit 1；控制字符 exit 1；空/空白标签 exit 2 且标签未被清除 |
| `TestProviderDuplicateAliasLabels` | **重复显示别名不改变选择**（按 ID 仍精确命中） |
| `TestProviderHelpAndFlagErrors` | `help`/`providerUsage` 含新命令与 `--provider-id`；未知子命令、位置参数、非法 `--by` 均 exit 2 |
| `TestProviderListIsOffline` | 全新 `MYTOKEN_HOME` 上 `provider list` 不写 `prices.json`（无隐藏网络/刷新） |
| `TestProviderIDDoesNotChangeHistoryFingerprint` | 过滤与别名前后，`events` 的 `dedup_key/resolved_provider/cost/base_url` 与 `settings.pricing_fingerprint` 指纹不变（**注意**：夹具指纹已是当前值，只覆盖 no-op 路径） |
| `TestProviderCommandsDoNotRepriceHistory`（本轮） | **启动不重定价**：指纹显式置为陈旧值 + 非零成本，三条命令后 `pricing_fingerprint` 与全部事实逐字节不变（真实首跑红→绿） |
| `TestProviderCommandsNeedNoCredentialSource`（本轮） | 三条命令在"毒化"凭据来源（cc-switch 路径为目录、harness 配置含密钥）下仍成功，且输出不泄漏该密钥 |
| `TestProviderAndProviderIDIntersectionAcrossCommands`（本轮） | `--provider` ∩ `--provider-id` 在 stats/sessions/cache 上均为交集，不相交组合为 0（不放大为全量） |

秘密泄漏检查：所有新身份输出中均不出现 `SECRETVALUE` 或 `key=`（表、CSV、JSON 与 `provider-id` 参数路径一致断言）。

## 兼容性与未变契约

- `stats`/`sessions`/`cache` 的既有 JSON/CSV 形状与字段名未改；仅新增可选过滤与新的 `--by endpoint`。
- `--provider` 保留"原始 provider 名"语义，没有把旧语义静默改成端点身份；需要身份语义时必须显式使用 `--provider-id` 或 `--by endpoint`。
- 后端只读调用，未修改 `query.Filter`、`ProviderGroups`、`SetProviderAlias`；未触碰 pricing/relay/billing 的 Origin+KeyID 路径的**任何逻辑**。修订 2 改动了 `internal/cli/relay.go` 的**参数解析层**：`parseReconcile` 现在拒绝两个 ledger 过滤标志（并修正帮助输出），`reconcileReq`、`selectSites`、`svc.Report`、对账/billing 计算均未改；`internal/cli/prices.go` 未改。
- 未新增任何隐式网络行为：`provider list` 恒为本地读取；`cache` 原本即为本地；`stats`/`sessions` 的 `--offline` 语义未变。
- 本轮只把**新增的三条 provider 命令**改为最小打开（`openProviderStore()`）。既有 `stats`/`sessions`/`cache`/`cache <session>` 的启动路径（`openApp()` / `app.OpenLocal()`，含其 `EnsurePrices` 行为）**未改**，本轮不为它们的启动副作用背书；如需一并收敛应单独立项（见"未关闭的门"7）。

## 诚实说明的开放行为

- `provider list` 只覆盖**当前库中已有的**本地聚合，不是全量版本化只读快照；快照是后续独立范围。
- 未识别端点（无法规范化，例如带查询串）**不能**被别名；它们按"每会话一个身份"列出，`identified=false`。
- 标签是显示文本，可重复、可任意字符串（除控制字符）；因此选择必须用 `--provider-id`，不能用标签。
- `provider alias list` 未实现；`provider alias` 只有 `set`/`clear`。
- 本轮未改动数据迁移逻辑；真实 legacy 库上的 `provider_endpoints` 覆盖情况以 `docs/verification/v040-migration-safety.md` 为准。
- 三条 provider 命令使用 `store.Open()`，因此**仍会执行 schema 迁移与 provider 身份回填**（可能增列/建表/写 `provider_endpoints`）。它保证"不扫描日志、不联网、不重定价、不改 `pricing_fingerprint`"，但**不保证**字节级只读或文件系统只读，也不是版本化快照。

## 红 3 / 绿 3：`reconcile` 静默丢弃 `--provider`/`--provider-id`（本轮修复）

缺陷：`parseReconcile`（`internal/cli/relay.go`）调用共享的 `addCommonFlags`，后者现已注册 `--provider` 与 `--provider-id`；但 `reconcileReq` 只保留 `target`/`keyID`/`by`/`format`/`rng`，两个新选择器被接受后直接丢弃、exit 0。`reconcile` 不查询 ledger：它比较**一个** relay 站点，站点由位置参数 `<origin|provider>` 加 `--key-id`（即 Origin+KeyID）选定，没有 `query.Filter` 可收紧。静默接受一个不会生效的过滤器会让人误以为比较被过滤过，也会把位置参数的 Origin+KeyID 身份与端点身份键（`--provider-id`）混为一谈。

先红：写入仅解析的回归测试（直接调用 `parseReconcile`，**不打开 App、不建库、不联网**），7 条子用例覆盖两种标志的分离值与 `=` 形式、以及位置参数在标志前/后/中间的顺序。修复前真实首跑：

```sh
go test ./internal/cli/ -run 'TestParseReconcileRejectsProviderFilters' -count=1
```

```
--- FAIL: TestParseReconcileRejectsProviderFilters (0.00s)
    --- FAIL: .../provider_separate_value (0.00s)
        relay_test.go:280: code=0 want 2 (stderr="", req={target: keyID: by:category format:table rng:{...}})
    --- FAIL: .../provider_equals_form (0.00s)
        relay_test.go:280: code=0 want 2 (stderr="", req={target: keyID: by:category format:table rng:{...}})
    --- FAIL: .../provider-id_separate_value (0.00s)   # 同上，code=0、stderr=""
    --- FAIL: .../provider-id_equals_form (0.00s)
    --- FAIL: .../target_before_provider (0.00s)
        relay_test.go:280: code=0 want 2 (stderr="", req={target:https://api.example.com ...})
    --- FAIL: .../target_after_provider-id (0.00s)
    --- FAIL: .../target_between_both_filters (0.00s)
FAIL
FAIL	github.com/zzstar101/mytoken/internal/cli	0.778s
```

7/7 子用例都是 `code=0` 且 `stderr` 为空：选择器被接受、目标照常解析、没有产生任何过滤——即"接受并丢弃"。

修复：在 `parseReconcile` 中于 `cf.parseFlags` 之后用既有 helper `providerFilterFlags`（`internal/cli/flags.go:46`）显式拒绝（与同函数里 `--no-cost` 的拒绝同型），并在 `addCommonFlags`（`internal/cli/options.go:49`）上补文档约束：调用方必须要么消费这两个标志（`options.filter()`），要么显式拒绝。另设 `fs.Usage = reconcileUsage`，使帮助输出不会再列出被拒绝的标志。

修复后（同一命令）：

```sh
go test ./internal/cli/ -run 'TestParseReconcileRejectsProviderFilters' -count=1 -v
# --- PASS: TestParseReconcileRejectsProviderFilters (0.00s)
#     --- PASS: .../provider_separate_value ... 7/7 子用例 PASS
# ok  	github.com/zzstar101/mytoken/internal/cli
```

真实 CLI 输出（`go build -o <tmp>/mytoken-check .`，全新临时 `MYTOKEN_HOME`）：

```
$ mytoken reconcile --provider name-a
reconcile: unsupported selector --provider; select the site with the positional <origin|provider> argument and --key-id (ledger filters never narrow a relay comparison)   # exit 2
$ mytoken reconcile https://api.example.com --provider-id=endpoint:v1:abc
reconcile: unsupported selector --provider-id; ...   # exit 2
$ mytoken reconcile --provider name-a --provider-id endpoint:v1:abc
reconcile: unsupported selector --provider and --provider-id; ...   # exit 2
$ mytoken reconcile https://api.example.com --key-id 0123456789ab
no enabled relay site matches "https://api.example.com"; run `mytoken relay list`   # exit 1：解析通过，Origin+KeyID 路径未改
$ mytoken reconcile --help --by day
Usage: mytoken reconcile [<origin|provider>] [--since DATE|DUR] ... [--by category|model|day] ... [--key-id ID]
（不再出现 --provider/--provider-id）
```

拒绝发生在打开任何环境之前：上述前三条（三条拒绝路径）在同一个全新 `MYTOKEN_HOME` 下依次运行后，该目录文件数 = 0（未打开 store/App，未联网）。第 4 条（解析通过、目标合法但库中无站点）按既有行为打开本地库并 exit 1，`reconcile <target> --provider ...` 中的位置目标不会被吞掉。

### 审计：所有 `addCommonFlags` 调用点（唯一注册点在 `internal/cli/options.go:56-64`）

| 调用点 | 标志去向 | 结论 |
| --- | --- | --- |
| `internal/cli/cli.go:66`（stats） | `opts.filter()`（`internal/cli/cli.go:96`）→ `query.Filter` | 已消费 |
| `internal/cli/commands.go:108`（sessions） | `opts.filter()`（`internal/cli/commands.go:131`） | 已消费 |
| `internal/cli/cache.go:65`（cache ledger 视图） | `internal/cli/cache.go:178` 构造 `query.Filter` | 已消费 |
| `internal/cli/cache.go:65`（cache session 时间线） | `internal/cli/cache.go:99` 用 `providerFilterFlags` 显式拒绝 | 已拒绝（本轮补测试锁定） |
| `internal/cli/provider.go:83`（provider list） | `opts.filter()`（`internal/cli/provider.go:110`） | 已消费 |
| `internal/cli/relay.go:606`（reconcile） | 修复前无任何消费者 | **本轮显式拒绝（`internal/cli/relay.go:624`）** |

即：除 reconcile 外无第二处"接受并丢弃"。审计同时发现 `cache <session>` 的时间线拒绝（`internal/cli/cache.go:99-102`）此前只有实现、无自动化断言（本文档第 249-254 行的手工示例即为其唯一证据）；本轮在 `TestCacheParseErrors` 补 3 条锁定用例（`--provider`、`--provider-id`、两者同时 + session id）——这是对既有行为的回归锁定（绿→绿），不是红→绿。

本轮聚焦验证（仅 `./internal/cli`，按约束未跑全量套件、GUI 与性能循环）：

```sh
gofmt -l internal/cli/                        # 无输出
go vet ./internal/cli/                        # 无输出
go test ./internal/cli/ -count=1              # ok  github.com/zzstar101/mytoken/internal/cli  6.154s
# 补 cache 锁定用例后：
go test ./internal/cli/ -count=1              # ok  github.com/zzstar101/mytoken/internal/cli  18.248s
```

过程中出现过一次**非本范围**的瞬时编译失败：`internal/store/provider_rollup.go:36/42: too many return values`（另一写入者 `internal/store/**` 正在写入，文件 mtime 与失败同一秒）。未修改该文件，稍后重跑 `go build ./internal/store/` 与 `go vet ./internal/cli/` 均通过；上表结果为该次瞬时状态恢复后的运行。

## 未关闭的门

1. 本轮只运行 `./internal/cli` 聚焦测试（未跑全量测试套件、GUI 与性能循环，因 workflow16 正在 `internal/query`+`internal/store` 上做基准与优化）。
2. 未在真实用户库上运行（遵照约束：无真实 DB/日志/凭据、无网络）；行为证据全部来自临时 `MYTOKEN_HOME` 合成数据。
3. 全量只读版本化快照与 statusline 未实现——属后续独立范围，本文件不作完成声明。
4. 未提交、未发布、未打标签；`docs/ROADMAP.md` 等共享规划文件未改。
5. `--by endpoint` 的 JSON 是本轮新增形状，尚无外部消费者校验；`provider list` 的行序（按稳定键排序）与 `stats --by endpoint`（流量序）的差异已在文档中说明，若后续统一需单独立项。
6. CLI 之外的范围（`internal/query`、`internal/store`、`internal/gui`）由其他写入者负责，本文件不为其背书。
7. 本轮只修了**新增三条 provider 命令**的启动重定价；`stats`/`sessions`/`cache`/`cache <session>` 仍走 `openApp()`/`app.OpenLocal()`，在陈旧指纹库上打开时同样会重定价历史事实（`internal/app/app.go:92`）。修复它们属于既有 legacy 行为，按本批约束未改，需单独立项。
8. 回归测试用合成临时 `MYTOKEN_HOME` 证明陈旧指纹 + 非零成本下的不变性；未在真实用户库上验证，也未跑全量套件（约束：workflow16 运行中）。
9. 修订 2 未改 `splitArgs`（`internal/cli/options.go`）对 `-h`/`--help` 的处理：`reconcile -h`（单独出现、后面没有别的 token）仍以 `flag needs an argument: -h` exit 2，因为 `splitArgs` 把所有非布尔开关都当作"需要取值"。`cache` 及使用同一 helper 的命令有同样形态。本次只保证 help 请求真正到达 flag set 时（如 `reconcile --help --by day`）打印真实 usage 且不列出被拒绝的标志；统一修 `splitArgs` 应单独立项（它属独立 helper，不在 reconcile 解析层）。
