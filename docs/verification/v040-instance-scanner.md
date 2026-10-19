# 0.4.0 实例感知扫描器（instance-aware scanner）验证

日期：2026-10-13。环境：Go 1.27.1、darwin/arm64。写入范围：`internal/scan/**`、本文件。未改 `internal/store/**`、`internal/harness/**`、`internal/instance/**`、`internal/model/**`、`internal/query/**`、`internal/app/**`、`internal/cli/**`、`internal/gui/**`（这些仅只读参考，其中 store/harness/instance 正被其他 agent 并发实现）。未新增依赖、未提交、未发布、未使用真实数据、未联网。

## 结论与范围

为扫描器落地多实例索引（`docs/superpowers/plans/2026-10-13-v040-multi-instance.md` 工作流 V4）：同一 harness 可以有多个实例，多个实例可以共享同一条物理路径，各自独立记账。

- 新增 `scan.Instance{ID, Parser}` 与 `scan.NewWithInstances(st, resolver, pricer, instances...) (*Scanner, error)`。
- 旧 `scan.New(...)` 签名与零配置行为**逐字不变**：每个 parser 包装为 `Instance{ID: store.DefaultInstanceID, ...}`；无 parser 参数时仍取 `harness.All()`。`internal/app/app.go:89` 无需改动即可继续编译。
- cursor、watched-change 的 `seen` 指纹全部按 **harness + instance + path** 分桶；`store.Write.InstanceID` 携带实例 ID，因此 store 侧的 `cursors(harness,instance_id,path)` 与 `event_data(harness,instance_id,dedup_key)` 天然分桶：**同内容不同实例各计一次，同实例重复解析幂等**。
- watch 目录仍按**物理路径**去重（同一 root 的两个实例只 watch 一份目录，仍受全局 `maxWatchDirs = 2048` 约束）；文件事件触发的 `scan(ctx, true)` 覆盖**全部**实例，不存在"只给第一个实例派发"的路径。
- 构造期校验：`nil` parser → `ErrNilParser`；同一 harness 下重复的实例 ID → `ErrDuplicateInstance`（ID 先经 `store.NormalizeInstanceID`，空串归一为 `"default"`）。**没有**任何"从注册表回退"的分支：`NewWithInstances()` 空列表就是什么都不扫。

**这不是端到端交付声明。** 本轮只交付扫描器侧能力：未接线 app/CLI/GUI，未读 `sources.json`，未做 disabled/backup 过滤（按任务约定属于 resolvers 上游），未跑真实数据。未关闭项见文末。

## 选定 API 与理由

```go
// internal/scan/instance.go:16
type Instance struct {
	ID     string         // 空 = store.DefaultInstanceID
	Parser harness.Parser // 不得为 nil
}

// internal/scan/instance.go:42
func NewWithInstances(st *store.Store, resolver *attrib.Resolver, pricer *pricing.Pricer, instances ...Instance) (*Scanner, error)

// internal/scan/instance.go:62
func (s *Scanner) Instances() []Instance // 防御性拷贝，供调用方按同一批 ID 汇报状态
```

| 决策 | 选择 | 理由 |
| --- | --- | --- |
| 命名 | `Instance` + `NewWithInstances` | 任务指定用 `Instance`；与 `internal/instance` 的 `Resolved.ID`、store 的 `InstanceID` 词汇一致 |
| 旧入口 | `New` 原签名保留，内部换成 `[]Instance` | `app.go:89` 是唯一调用方，避免跨范围改动；零配置行为（含 `harness.All()` 回退）不变 |
| 构造失败 | 返回 `(nil, error)`，不"修复"配置 | 重复 ID 会让两个实例静默共用一份 cursor 且互为 dedup 重复，必须硬失败 |
| 空列表 | 合法，扫描 0 个目标 | 显式"什么都不扫"必须与"回退到全部注册 harness"区分开，避免误扫用户数据 |
| ID 归一 | 交给 `store.NormalizeInstanceID` | 与 store 的 `cursors`/`event_data` 归一化点完全一致，不自己造第二套规则 |
| 重复判定粒度 | 按 (harness, 归一化 ID) | `internal/instance.DefaultID` 是**每个 harness 各铸一次**的 legacy 回退 ID，跨 harness 同 ID 合法 |
| watch 目录 | 不按实例复制，仍按物理路径去重 | 同一 root 上 N 个实例不应消耗 N 份 fd；事件派发本来就是"全实例 scan" |
| 实时重配置 | 不提供 | 本轮不引入 watcher 热重建的复杂度；调用方取消 `Run` 后新建 Scanner（见文末限制） |

## 状态分桶（实现要点）

| 状态 | 旧键 | 新键 |
| --- | --- | --- |
| cursor | `(harness, path)`，`store.Cursors` | `CursorsIn(ctx, h, instance)` → `(harness, instance, path)`（`scanner.go:189`） |
| watched-change `seen` 指纹 | `harness + "\x00" + path` | `harness + "\x00" + instance + "\x00" + path`（`scanner.go:179`） |
| 事件去重 | store 侧 `event_data(harness, dedup_key)` | store 侧 `(harness, instance_id, dedup_key)`，由写携带的 ID 分桶（`scanner.go:329`） |
| watch 目录 | 路径去重 | 不变（物理共享）；`watchRoots` 只额外收集全部实例的 `Roots()` 后仍走 `watchDirs` 去重（`scanner.go:386-388`） |
| 事件派发 | `scan(ctx, true)` 全量遍历 | 不变：遍历全部 `s.instances`，每个实例各读自己的 cursor |

其余未变：worker 池上限 4、`debounce` 250ms、`minGap` 2s、`maxWatchDirs`、`newDirBudget`、`DeferEmptyIndexes`/`NormalizeProjects`/`EnsurePrices` 调用顺序、大文件优先排序。

## 先红后绿证据（精确命令与结果）

新增 `internal/scan/instance_test.go`（同包 `scan`，14 个测试），只用 `t.TempDir()` 临时库与假解析器（`testParser`、`recordingParser`）；`recordingParser` 记录每次 `Parse` 收到的 cursor 偏移，用来反证"每个实例各读自己的 checkpoint"。

### 红 1：API 尚不存在（真实首跑）

```sh
go test ./internal/scan/ -count=1
```

```
# github.com/zzstar101/mytoken/internal/scan [github.com/zzstar101/mytoken/internal/scan.test]
internal/scan/instance_test.go:131:12: undefined: NewWithInstances
internal/scan/instance_test.go:132:3: undefined: Instance
internal/scan/instance_test.go:133:3: undefined: Instance
internal/scan/instance_test.go:135:21: undefined: ErrDuplicateInstance
internal/scan/instance_test.go:148:13: undefined: NewWithInstances
internal/scan/instance_test.go:149:3: undefined: Instance
internal/scan/instance_test.go:150:3: undefined: Instance
internal/scan/instance_test.go:163:13: undefined: NewWithInstances
internal/scan/instance_test.go:163:44: undefined: Instance
internal/scan/instance_test.go:171:11: undefined: NewWithInstances
internal/scan/instance_test.go:171:11: too many errors
FAIL	github.com/zzstar101/mytoken/internal/scan [build failed]
FAIL
```

退出码 1。测试先于实现存在，断言的是本文档声明的契约。

### 突变检验（反证测试不是"恒真"）

只把实现改坏、不动测试，确认红灯确实指向被声明的行为，随后原样还原：

```sh
# 突变 A：scanOne 不再携带实例 ID（去掉 InstanceID: item.instance）
go test ./internal/scan/ -count=1 -run 'TestInstanceScanSamePathSameHarnessStaysIndependent|TestWatchDispatchScansEveryInstance|TestInstanceWithNoRootsNeverFallsBack'
```

```
--- FAIL: TestInstanceWithNoRootsNeverFallsBack (0.06s)
--- FAIL: TestInstanceScanSamePathSameHarnessStaysIndependent (0.05s)
--- FAIL: TestWatchDispatchScansEveryInstance (10.13s)
FAIL	github.com/zzstar101/mytoken/internal/scan	10.677s
```

```sh
# 突变 B：cursor 读回默认实例（CursorsIn(ctx, h, inst.ID) → Cursors(ctx, h)）
go test ./internal/scan/ -count=1 -run 'TestInstanceCursorIsNotShared|TestMovedInstanceDoesNotReuseOldPathCursor'
```

```
--- FAIL: TestInstanceCursorIsNotShared (0.03s)
FAIL	github.com/zzstar101/mytoken/internal/scan	0.566s
```

突变 A 覆盖"写侧分桶 + 派发"，突变 B 覆盖"读侧分桶"。两处均已在运行最终绿灯前还原（`gofmt -l internal/scan/` 无输出）。

### 绿：最终验证（还原后首尾一致的精确命令）

```sh
gofmt -l internal/scan/        # 无输出（干净）
go build ./internal/scan/      # OK
go vet ./internal/scan/        # OK（干净）
go test ./internal/scan/ -count=1
go test -race ./internal/scan/ -count=1
```

```
ok  	github.com/zzstar101/mytoken/internal/scan	1.050s
ok  	github.com/zzstar101/mytoken/internal/scan	18.667s   (-race)
```

包内 20 个顶层测试全部 PASS（14 个本轮新增 + 6 个既有：`TestIncrementalAppendNoDuplicates`、`TestParallelParsing`、`TestQuickScanSkipsUnchangedFiles`、`TestRunCancellationStopsDispatch`、`TestRunDoesNotWatchWholeRoot`、`TestWatchDirs`）。`real_bench_test.go` 的 `TestCopiedDatabaseScanPerformance` 按设计 SKIP（需要真实数据库，本轮不使用真实数据），未受影响。

| 新增测试 | 断言的不变量 |
| --- | --- |
| `TestNewWithInstancesRejectsDuplicateIDPerHarness` | 同 harness 同 ID → `ErrDuplicateInstance` |
| `TestNewWithInstancesAllowsSameIDAcrossHarnesses` | 同 ID 不同 harness 合法（legacy `default` 每 harness 各一次） |
| `TestNewWithInstancesNormalizesEmptyIDToDefault` | 空 ID 归一为 `store.DefaultInstanceID`；空 ID 与显式 `default` 视为重复 |
| `TestNewWithInstancesRejectsNilParser` | 无 parser → `ErrNilParser` |
| `TestNewWithInstancesEmptyDoesNotFallBackToRegistry` | 空列表 = 0 目标、`Progress()=0/0`、0 事件（不回退注册表） |
| `TestInstanceWithNoRootsNeverFallsBack` | 显式空 roots 的 parser 被原样使用（`Discover` 只被调用 1 次），事件只落在该实例 |
| `TestLegacyNewMapsParsersToDefaultInstance` | 旧 `New` 的实例 ID 是 `default`，事件落在 `default` |
| `TestInstanceScanSamePathSameHarnessStaysIndependent` | 同路径同 harness 两个 ID → 2 条事件、2 条 cursor、`Progress()=2/2`；重复 `Scan` 与 `Rebuild` 后仍是 2 条 |
| `TestInstanceCursorIsNotShared` | A 二次扫读从偏移 2 续读，新实例 B 从 0 开始（无跨实例 cursor 复用） |
| `TestMovedInstanceDoesNotReuseOldPathCursor` | 同一 ID 换路径后从 0 开始；旧路径被新实例 B 接手也从 0 开始；迁移前的历史事件保留 |
| `TestQuickScanSeenStateIsPerInstance` | 快速扫描的跳过/解析按实例独立；`seen` 中恰好出现两条含实例段的键 |
| `TestWatchDirsSharedAcrossInstances` | 单实例 1 个 watch；同 root 两实例仍 1 个；两 root 两实例 2 个 |
| `TestWatchDispatchScansEveryInstance` | 共享 watch 下一次追加 → 两个实例各前进一条 cursor（事件 2→4） |
| `TestRunCancellationAndRestartWithInstances` | `Run` 取消后立即返回 `context.Canceled`；同一 Scanner 重新 `Run` 能重新建立 watch |

## 应用接线（返回 API）与限制

调用方（后续 app 接线）面对的全部新增面：

```go
instance := scan.Instance{ID: resolved.ID, Parser: parser} // ID 空串即默认实例
sc, err := scan.NewWithInstances(st, resolver, pricer, instances...)
if err != nil { /* ErrNilParser / ErrDuplicateInstance，视为配置错误 */ }
for _, in := range sc.Instances() { /* 按同一批 ID 汇报状态 */ }
```

数据来自上游 resolver（示意，**本轮未接线、未验证**）：每个 `instance.Resolved` 满足 `Scannable()`（即非 disabled、`Kind == KindInstance`）时，用 `harness.Get`/`harness.NewRooted(r.Harness, r.Roots...)` 构造 parser，再交给 `NewWithInstances`。scanner 侧**不做**任何 disabled/backup 过滤：备份副本与禁用实例的排除必须在调用方完成，这是刻意的边界——scanner 只接收"活跃 spec"。

限制（本轮的诚实边界）：

1. **无实时重配置**：实例集合在构造期固定。增删实例需取消 `Run` 并新建 Scanner（`Run` 每次返回都会 `Close` watcher，因此可以在同一 ctx 生命周期内安全重建；`Rebuild` 会删除**所有**实例的 cursor 并全量重扫）。
2. **watch 上限仍是全局的**：`maxWatchDirs = 2048` 不按实例数放大。同一 root 的 N 个实例共享 1 个 watch，但实例各自拥有互不重叠的 root 时，watch 会更快触顶；超出部分依赖周期扫描覆盖。
3. **物理目录共享、但同目录内多实例的成本仍是 O(实例数)**：同一文件被 N 个实例扫描会读 N 次、写 N 份 cursor/维度行（这是"同内容不同实例各计一次"的必然代价），本轮未做按内容缓存的跨实例复用。
4. **`NewWithInstances` 不校验 roots**：空 roots 的 parser 保持空（不发现任何源），不存在的路径由 `os.Stat` 静默跳过——与本包既有行为一致。
5. **未做端到端验证**：`internal/app`、CLI、GUI、`sources.json` 的接线和 query 侧按实例聚合均未触及，因此**不构成多实例端到端可用声明**。

## 变更文件

- 新增 `internal/scan/instance.go`（66 行）——`Instance`、`ErrNilParser`、`ErrDuplicateInstance`、`NewWithInstances`、`Scanner.Instances`。
- 修改 `internal/scan/scanner.go`（+34/−18）——`parsers []harness.Parser` → `instances []Instance`；抽出 `newScanner`；`source` 增加 `instance`；`seen` 键、`CursorsIn`、`Write.InstanceID`、`watchRoots` 四处按实例分桶。
- 新增 `internal/scan/instance_test.go`（624 行，14 个测试）。
- 新增本文件。

未触碰 `internal/store/**`、`internal/harness/**`（含各解析器包）、`internal/instance/**`、`internal/model/**`、`internal/query/**`、`internal/app/**`、`internal/cli/**`、`internal/gui/**`、`go.mod`、共享规划文件（`task_plan.md`、`docs/superpowers/**` 仅只读参考）；未提交、未联网、未使用真实数据与凭据。

## 未关闭的门（本轮不构成完成声明）

1. **不是端到端多实例**：没有任何生产调用方使用 `NewWithInstances`；实例配置的加载/持久化/命名/冲突提示、app/CLI/GUI 接线、query 与报表的按实例聚合全部未做。
2. **未跨范围验证**：仅按任务约束跑 `./internal/scan/` 的 build/vet/test/race；未跑 `go build ./...` 或 `./internal/store/...`、`./internal/query/...` 等（这些正被其他 agent 并发修改）。
3. **resolver 契约未联调**：`internal/instance` 的 `Resolve` 输出与 scanner 的对接只是文档示意；`Scannable()` 的过滤责任在调用方，本轮未写集成测试。
4. **无实时重配置**：见上"限制 1"；也没有 `AddInstance`/`RemoveInstance`/`Reconfigure` 之类 API。
5. **未做性能度量**：没有测量多实例下同路径重复解析的额外 IO/耗时（`real_bench_test.go` 的真实库性能测试在本轮 SKIP）。
