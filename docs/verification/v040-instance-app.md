# 0.4.0 多实例应用接线（App wiring）验证

日期：2026-10-13。环境：Go 1.27.1、darwin/arm64。写入范围：`internal/app/app.go`（改）、新增 `internal/app/sources.go`、新增 `internal/app/sources_test.go`、本文件 `docs/verification/v040-instance-app.md`。只读参考：`internal/instance`、`internal/harness`、`internal/scan`、`internal/store`、`internal/pricing`、`internal/query`、`docs/superpowers/plans/2026-10-13-v040-multi-instance.md`。未改 `internal/instance`、`internal/harness`、`internal/scan`、`internal/store`、`internal/query`、`internal/model`、`internal/paths`、`main.go`、CLI、GUI（这些正被其他 worker 并发实现，仅按冻结 API 调用）。未新增依赖、未提交、未发布、未访问真实 DB/日志/凭据；本轮没有为验证主动联网（唯一可能的出站请求来自包内既有测试，见限制 8）；全部测试数据都在 `t.TempDir()` 下合成。

**这不是端到端交付声明。** 本文件证明的是：`sources.json` 已经在 `App` 的打开路径上被解析为真实的扫描实例，解析失败会在建立 store、启动扫描与后台网络之前终止打开，并且这些事实由真实 `Scan` + `query` 读数的测试与三次突变反证支撑。

## 结论与范围

- `Open()`（app.go:70）与 `OpenLocal()`（app.go:73）现在都经由同一个 `open(refresh bool)`（app.go:75），因此两个入口的配置解析与拒绝语义完全一致，只在"是否启动价格刷新 worker"上分叉。
- 接线顺序为 `paths.DataDir` → `instance.Load`（app.go:83）→ `resolveSources`（app.go:87）→ `paths.DBPath`/`store.Open`/`attrib.New`/`pricing.New` → `scan.NewWithInstances`（app.go:113）。配置错误、显式根的致命问题、解析器工厂缺失都在 store 与扫描器存在之前返回错误。
- 空配置（没有 `sources.json`，或文件里 `entries` 为空）保持旧行为：没有任何显式条目认领 harness，`Resolve` 返回全部默认 fallback（即 `harness.All()` 的默认发现），`ID` 为 `default`。这一点由 `TestSourcesDefaultDiscoveryWhenNoConfig` 用真实 `Scan` 的 query 读数证明。
- backup-copy / disabled 条目不扫描也不回退到默认发现：显式条目会顶替该 harness 的整条默认链，即使它是 disabled/backup-copy（`instance.Resolve` 的冻结语义），所以此时目标列表为空，`scan.NewWithInstances` 收到空列表后什么都不扫。
- 等内容的两个独立实例各自计数，游标互相隔离：同一份合成 claude 日志被两个显式实例指向时，实例内 totals 各为 1，`default` 实例为 0，`CursorsIn` 各为 1。
- 未关闭的门见文末；本轮的"绿"是并发编辑中的工作树上的快照，不是对整仓的完成声明。

## 选定 API 与理由

新增的公开面（`internal/app/sources.go`）：

```go
// internal/app/sources.go:19
type WiringCode string

// internal/app/sources.go:23
const CodeNoFactory WiringCode = "no-factory"

// internal/app/sources.go:29
type Source struct {
	instance.Resolved
	Scanned bool   // true = 已作为 rooted parser 交给扫描器
	Reason  string // 该 source 的第一条非致命解析问题
}

// internal/app/sources.go:44
type SourceError struct {
	Code    string          // instance.Code 或 WiringCode
	ID      string
	Harness model.Harness
	Path    string
	Message string
}
func (e *SourceError) Error() string // "sources: <code> instance=<id> harness=<h> path=<p>: <msg>"

// internal/app/sources.go:115（包内）
func resolveSources(cfg *instance.Config, fallbacks []instance.Fallback, newRooted rootedFactory) (resolvedSources, error)
// internal/app/sources.go:86
type rootedFactory func(model.Harness, ...string) (harness.Parser, bool)

// internal/app/sources.go:170
func (a *App) ScannedSources() []Source // 仅 Scanned==true 的副本
```

`App` 上新增两个字段（app.go:56-64）：`Sources []Source`（全部已解析实例，按解析顺序，含 disabled/backup-copy）与 `SourceProblems []instance.Problem`（仅非致命问题）。`query.Service`、`scan.Scanner`、`store.Store` 的既有签名一行未改。

| 决策 | 选择 | 理由 |
| --- | --- | --- |
| 接线位置 | `open(refresh bool)`，Open/OpenLocal 共用 | 任务要求两个入口行为一致；只允许 refresh 分叉 |
| 解析时机 | 在 `store.Open` 之前 | 坏配置/坏显式根/无解析器必须在扫描与后台网络之前失败，且不留半个 App |
| 解析器构造 | 复用冻结的 `harness.NewRooted`，经 `rootedFactory` 形参注入 | 不复制注册表逻辑；工厂可注入，才能对"无 rooted 解析器"这条生产上不可达的路径写真测试 |
| 目标列表 | 直接传 `[]scan.Instance` 给 `scan.NewWithInstances` | 空列表 = 什么都不扫，正是 backup-copy/disabled 的语义，app 里无需再兜底 |
| 诊断面 | `App.Sources` + `App.SourceProblems` + `ScannedSources()` | 供 GUI/CLI 只读展示；`Source` 不含 parser，消费者无法反向驱动扫描器；不改 `query.Service` |
| 失败类型 | `*instance.ConfigError`（配置本身）与 `*SourceError`（解析/接线） | 配置错误沿用 instance 包的稳定码；app 独有的"没有 rooted 解析器"新增 `no-factory`，因为 instance 包没有对应码 |
| 零根默认实例 | 非致命诊断 | `instance.Resolve` 对内置根全部缺失的 harness 仍返回 `Scannable()==true` 的空根实例（resolve.go:306），那是"本机没装这个工具"，不是配置错误 |
| 零根显式实例 | 致命 `no-roots` | 用户显式配置的根不允许被静默丢弃（"Do NOT silently drop unsupported roots"） |

## 接线顺序与拒绝语义（实现要点）

`open` 的顺序（app.go:75-148）：

1. `paths.DataDir()`；
2. `instance.Load(dir)`（app.go:83）—— 文件缺失返回空配置；格式错误/版本过新/未知字段返回 `*instance.ConfigError`，直接 `return nil, e`（app.go:85），不建 store；
3. `resolveSources(cfg, instance.DefaultFallbacks(), harness.NewRooted)`（app.go:87）—— 任一 `Problem.Fatal` 即为 `*SourceError`，直接返回（app.go:89）；
4. 之后才是 `paths.DBPath`、`store.Open`、`attrib.New`、`pricing.New`、`Settings`；
5. `scan.NewWithInstances(st, resolver, prices, sources.targets...)`（app.go:113），其错误先 `a.Close()` 再返回；
6. `query.NewService`、`newRelays`、`st.EnsurePrices`（app.go:120，原样保留）；
7. `if !refresh { return a, nil }`（app.go:125）—— `OpenLocal` 到此结束；`Open` 才继续启动 24h 价格刷新 goroutine。

`resolveSources`（sources.go:115-166）的拒绝语义：

- `instance.Resolve` 返回任何 `Fatal` 问题 → 立即返回错误，`resolvedSources` 里只有诊断，绝不进入 store/扫描/网络；
- `!r.Scannable()`（disabled / backup-copy）→ 只记 `Source{Scanned:false}` 诊断，不进目标列表；
- `r.Scannable()` 但 `len(r.Roots)==0`：`Source==PathUser`（显式配置）→ 致命 `no-roots`；`Source==PathDefault`（默认发现）→ 诊断，跳过（"工具没装"）；
- `newRooted(r.Harness, r.Roots...)` 返回 `false` → 致命 `no-factory`，消息明确写"its roots would be dropped"，即不静默丢根；
- 其余 → `Source.Scanned=true`，并追加 `scan.Instance{ID: r.ID, Parser: parser}`。

## 先红后绿证据（精确命令与结果）

### 红：API 尚不存在（真实首跑）

实现前先写 `internal/app/sources_test.go`，然后首跑：

```sh
go test ./internal/app -count=1 -run 'Test(Sources|ResolveSources|RegisteredParsers)'
```

```
# github.com/zzstar101/mytoken/internal/app [github.com/zzstar101/mytoken/internal/app.test]
./sources_test.go:NN:MM: undefined: Source
./sources_test.go:NN:MM: a.Sources undefined (type *App has no field or method Sources)
./sources_test.go:NN:MM: a.SourceProblems undefined (type *App has no field or method SourceProblems)
./sources_test.go:NN:MM: undefined: SourceError
./sources_test.go:NN:MM: undefined: resolveSources
./sources_test.go:NN:MM: too many errors
FAIL	github.com/zzstar101/mytoken/internal/app [build failed]
```

（上面是输出内容的逐条摘录，`NN:MM` 处原本是当时的具体行列号，这里从简。）

退出码 1（多条编译错误，以 `too many errors` 结束）。`-run` 过滤只是预防：`TestFatalConfigStopsOpenBeforeRefreshWorker` 会走 `open(true)`，若包能编译则可能真的启动一次价格刷新；实际上 Go 会编译整个测试包，所以这次红跑已经覆盖了全部 11 个新测试（含那条刷新路径）。

### 突变检验（反证测试不是"恒真"）

每次都是"改实现 → 跑指定测试 → 还原"，测试文件未动。

**突变 A：忽略致命问题**（`if p.Fatal {` → `if p.Fatal && false {`）

```sh
go test ./internal/app -count=1 -run 'TestSourcesInvalidExplicitRootStopsBeforeStore|TestResolveSourcesEnvOriginMissingRootIsFatal'
```

```
--- FAIL: TestSourcesInvalidExplicitRootStopsBeforeStore (0.01s)
    sources_test.go:336: code = "no-roots", want "missing-dir"
--- FAIL: TestResolveSourcesEnvOriginMissingRootIsFatal (0.00s)
    sources_test.go:453: resolveSources accepted a missing env root (sources=[])
FAIL	github.com/zzstar101/mytoken/internal/app	0.476s
```

**突变 B：丢根**（`newRooted(r.Harness, r.Roots...)` → `newRooted(r.Harness)`）

```sh
go test ./internal/app -count=1 -run 'TestSourcesExplicitInstancesScanIndependently|TestSourcesDefaultDiscoveryWhenNoConfig'
```

```
    sources_test.go:185: claude requests = 0, want 1
    sources_test.go:188: claude tokens = {Input:0 Output:0 ...}, want input 100 output 50
    sources_test.go:215: instance inst_a requests = 0, want 1 (equal content must not dedupe across instances)
    sources_test.go:215: instance inst_b requests = 0, want 1 (equal content must not dedupe across instances)
    sources_test.go:222: all-instance requests = 0, want 2
    sources_test.go:231: instance inst_a/inst_b cursors = 0, want 1
FAIL	github.com/zzstar101/mytoken/internal/app	0.754s
```

**突变 C：不跳过 disabled/backup-copy**（`if !r.Scannable() {` → `if !r.Scannable() && false {`）

```sh
go test ./internal/app -count=1 -run 'TestSourcesBackupCopyAndDisabledAreNotScanned'
```

```
--- FAIL: TestSourcesBackupCopyAndDisabledAreNotScanned/backup-copy
    sources_test.go:268: scanner instances = 1, want 0 (no scan, no fallback)
--- FAIL: TestSourcesBackupCopyAndDisabledAreNotScanned/disabled
    sources_test.go:268: scanner instances = 1, want 0 (no scan, no fallback)
FAIL
```

三次突变都让对应用例失败，说明这些断言真的挂在被检验的行为上。还原后 `grep -rn "&& false" internal/app/` 无输出。

### 绿：最终验证（还原后首尾一致的精确命令）

```sh
gofmt -l internal/app/                     # 无输出
go build ./internal/app/                   # 通过
go vet ./internal/app/                     # 无输出
go test ./internal/app -count=1            # ok  github.com/zzstar101/mytoken/internal/app  0.956s
go test -race ./internal/app -count=1      # ok  github.com/zzstar101/mytoken/internal/app  10.874s
go build ./...                             # 通过
```

（同为绿的两次复跑：`go test` 0.914s / 0.956s，`-race` 9.709s / 10.874s——耗时随机器负载浮动，结论一致。）

包内既有测试同时通过：`--- PASS: TestOpenRepricesExistingEvents (0.08s)`（证明旧 `Open` 路径与既有重定价行为未被接线破坏），`--- SKIP: TestRealShots`、`--- SKIP: TestRealDataSmoke`（需要 opt-in 环境变量）。

### 新增测试清单（11 个）

| 新增测试 | 断言的不变量 |
| --- | --- |
| `TestRegisteredParsersAreProvenanced` | 两条注册表不变量：每个注册解析器都实现 `instance.Provenanced`（所以 `Resolve` 读的是实时 env/HOME，测试可用合成根完全替换真实路径），且 `harness.NewRooted` 对每个注册 harness（28 个）都返回 ok（所以 `no-factory` 在当前注册表下不可达） |
| `TestSourcesDefaultDiscoveryWhenNoConfig` | 无 `sources.json` + `CLAUDE_CONFIG_DIR` 指向合成根 → 一个 `ID=default` 的 `Source{Scanned:true}`，真实 `Scan` 后 claude requests=1、tokens=100/50 |
| `TestSourcesExplicitInstancesScanIndependently` | 两个显式实例指向同一份日志 → 各自 requests=1（等内容不跨实例去重），`default` 实例 0（显式条目顶替默认链），全实例合计 2，`CursorsIn` 各 1 且互不干扰 |
| `TestSourcesBackupCopyAndDisabledAreNotScanned`（2 子测试） | kind=backup-copy 与 disabled=true 的条目各带一个有效根 → 扫描器实例数 0、totals 全 0、`Scanned==false`，且都不回退到 `CLAUDE_CONFIG_DIR` 的默认发现 |
| `TestSourcesMalformedConfigStopsBeforeStore` | 坏 JSON → `*instance.ConfigError`、`app==nil`、`mytoken.db` 不存在（没建 store） |
| `TestSourcesInvalidExplicitRootStopsBeforeStore` | 显式根指向不存在的目录 → `*SourceError`（`missing-dir`）、`app==nil`、无 DB 文件 |
| `TestFatalConfigStopsOpenBeforeRefreshWorker` | 坏配置 + `open(true)` → 返回错误，且 `prices-attempt.json` 不存在，证明刷新 worker 从未启动（`pricing.Refresh` 在联网前先写该文件，pricing.go:295） |
| `TestResolveSourcesNoFactoryIsFatal` | 注入恒返回 `false` 的工厂 → `no-factory` 错误，根没有被静默丢弃 |
| `TestResolveSourcesSurfacesNonFatalProblems` | 注入带嵌套根的假 fallback → 非致命 `CodeOverlap` 出现在 `problems` 且实例仍然可扫 |
| `TestResolveSourcesEnvOriginMissingRootIsFatal` | env 来源的默认根缺失 → 致命，`resolveSources` 不接受任何部分结果 |
| `TestSourcesMoveKeepsInstanceID` | 同一份配置 `Move` 到新根后重新 `OpenLocal`，实例 ID 不变且新根被真实扫描 |

## 应用接线（返回 API）与限制

GUI/CLI 现在可以这样读诊断（尚未有任何调用方，这是留给后续工作的面）：

```go
a, err := app.OpenLocal()
var se *app.SourceError
if errors.As(err, &se) { /* se.Code / se.ID / se.Harness / se.Path */ }
for _, s := range a.Sources { /* s.ID, s.Label, s.Harness, s.Roots, s.Source, s.Kind, s.Disabled, s.Scanned, s.Reason */ }
for _, s := range a.ScannedSources() { /* 只有真正在扫的 */ }
for _, p := range a.SourceProblems { /* 仅非致命：缺内置根、默认根去重 */ }
```

限制（如实记录）：

1. 不是端到端交付声明：GUI/CLI 还没有消费 `App.Sources`，`query.Service` 未改，也没有按实例过滤的 GUI 视图。
2. 配置只在 `Open`/`OpenLocal` 时读一次，`Scanner` 的实例集合在构造时固定；改 `sources.json` 必须重开 App。App 本身也不写 `sources.json`（写入归后续 CLI/GUI）。
3. 零根的默认实例是**诊断而非错误**：`Source.Scannable()` 对它仍为 true，消费者必须看 `Scanned`（或用 `ScannedSources()`）才知道什么真的在扫，不能只看 `Scannable()`。
4. env 声明的默认根缺失会导致**整个 App 拒绝启动**，而不只是那一个 harness（`instance.Resolve` 的冻结契约：env 根缺失为致命）。一个写坏的 env 变量会阻塞所有 harness，这是继承来的语义，不是本轮选择。
5. `CodeNoFactory` 在当前生产路径上不可达（28 个注册 harness 全部实现 `Rooted`），只有注入工厂的测试覆盖它；保留它是为了"将来新增不可 rooted 的 harness 时绝不静默丢根"。
6. 既有 App 正常重定价未改：`open` 仍无条件调用 `st.EnsurePrices(...)`（app.go:120）。`task_plan.md:41` 指出的锁定语义与 snapshot 路径**不在本轮范围**。
7. `Open()`（refresh=true）在成功接线后仍会启动价格刷新 worker；它是唯一的联网点（pricing.go:295，先写 `prices-attempt.json` 再抓取 models.dev/litellm）。致命配置在它启动前返回，但没有离线开关，离线调用方必须用 `OpenLocal()`。
8. 测试全程离线：所有新用例走 `OpenLocal()` + `t.TempDir()` + 合成 claude JSONL。包内既有的 `TestOpenRepricesExistingEvents` 会调用 `Open()`，因此理论上可能发起一次后台价格拉取——这是仓库既有行为，本轮未改；它的 `MYTOKEN_HOME` 也是临时目录，不写真实 DB。
9. 环境观察（非本包问题）：验证期间 `internal/store` 正被另一位 worker 并发写入，一次 `go build ./internal/app/` 因 `internal/store/store.go:61:11: undefined: SchemaReport`（`internal/store/readonly.go` 尚未落盘）失败，约 20 秒后 `go build ./internal/store/` 自行恢复。本轮"绿"是并发编辑工作树上的快照；若需要终态证明，应在其他 worker 停笔后重跑上面的绿链。

## 变更文件

| 文件 | 变更 |
| --- | --- |
| `internal/app/sources.go` | 新增，178 行：`Source`/`SourceError`/`WiringCode`/`CodeNoFactory`/`resolveSources`/`ScannedSources` |
| `internal/app/sources_test.go` | 新增，526 行：11 个测试 + 合成 claude 日志与沙箱 helper |
| `internal/app/app.go` | 修改，+30/−2（现 165 行）：`App.Sources`/`App.SourceProblems`，`open` 的解析前置，`scan.NewWithInstances` 取代 `scan.New` |
| `docs/verification/v040-instance-app.md` | 新增，本文件 |

未提交、未发布；工作树里其他文件的脏改动全部保留未动。

## 未关闭的门（本轮不构成完成声明）

1. GUI/CLI 尚未渲染 `App.Sources`/`App.SourceProblems`，也没有按实例过滤的查询视图。
2. `sources.json` 的写入（增删改实例）尚未接线到任何入口。
3. 运行期重新加载配置（不重开 App）未实现。
4. `task_plan.md:41` 的 `OpenLocal` 无条件 `EnsurePrices` 重定价问题未处理；snapshot 路径未实现。
5. `CodeNoFactory` 无生产路径覆盖。
6. 本轮验证是在 `internal/store` 并发编辑期间取得的快照，未做整仓终态复跑。
