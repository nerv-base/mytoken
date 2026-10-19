# 0.4.0 多实例配置基础（internal/instance）验证

日期：2026-10-13 19:14 CST
环境：go1.27.1 darwin/arm64；工作目录 `/Users/zzstar/Developer/Programs/MyToken!!!!!`；HEAD `2e696f2`
写入范围：仅新增 `internal/instance/**`（`instance.go` / `config.go` / `resolve.go` / `config_test.go` / `resolve_test.go`）与本文件。未改动任何其它文件：

```console
$ git status --porcelain -- internal/instance docs/verification/v040-instance-config.md
?? docs/verification/v040-instance-config.md
?? internal/instance/
```

（工作区里其余 `M` 文件是其它工作流的既有改动，本次没有触碰。）

本轮修复（2026-10-13 19:24 CST，同一 HEAD `2e696f2`）：只改 `internal/instance/resolve.go`、`internal/instance/resolve_test.go` 与本文件；未触碰 `internal/harness`、`model`/`store`/`query` 等其它工作流的文件，未跑 fullsuite，未跑真实数据，未提交。

## 结论与范围

本次交付的是**多实例配置的底座**：版本化 `sources.json` 的读写、不透明实例 ID、显式的新增/移动/备份副本语义、校验与诊断原因码，以及一次性的解析（Resolve）入口。它**不是端到端的多实例能力**：

- 采集/发现/监听链路完全没有接入 `internal/instance`，今天没有任何运行时路径会调用它；
- 没有 CLI/GUI 入口，没有游标与监听生命周期，没有迁移/回滚；
- `Resolve` 只决定"哪些根可被扫描"，不构造 parser、不碰注册表、不启动采集。

因此本次不改变现有产品行为，也不声称多实例已可用。

## 选定 API 与理由

包 `internal/instance` 只依赖标准库与 `internal/model`；**刻意不 import `internal/harness`**。下游依赖只有一个方向，接入时用结构性子类型适配即可：

```go
var fbs []instance.Fallback
for _, p := range harness.All() {
    fbs = append(fbs, p)
}
roots, problems := instance.Resolve(cfg, fbs)
// 对每个 roots[i].Scannable() 为 true 的项调用 harness.NewRooted(...)
```

已用一次性编译断言核实过该适配成立（`var _ instance.Fallback = harness.Parser(nil)` → `go vet ./internal/instance/` 通过），断言文件随后删除，仓库中不保留 `internal/harness` 依赖。

导出的完整 API（`instance.go` 的包注释里有一份同样的清单）：

```go
const FileName = "sources.json"
const DefaultID = "default"
const SchemaVersion = 1
const MaxIDLen = 128

type Kind string       // KindInstance "instance" | KindBackupCopy "backup-copy"
type PathSource string // PathUser "user" | PathDefault "default"

type Code string // invalid-config, unsupported-version, duplicate-id, reserved-id,
                 // unknown-harness, invalid-kind, no-roots, not-found, io-error,
                 // invalid-root, missing-dir, not-dir, permission, duplicate-root, overlap

type ConfigError struct { Code Code; Path, ID string; Harness model.Harness; Message string; Err error }
func (*ConfigError) Error() string
func (*ConfigError) Unwrap() error

type Entry struct { ID, Label string; Harness model.Harness; Roots []string; Kind Kind; Disabled bool }
type Config struct { Version int; Entries []Entry }
type AddRequest struct { ID, Label string; Harness model.Harness; Roots []string; Kind Kind; Disabled bool }

func NewID() (string, error)
func Load(dataDir string) (*Config, error)
func (*Config) Save(dataDir string) error
func (*Config) Validate() error
func (*Config) Add(req AddRequest) (Entry, error)
func (*Config) Move(id string, roots []string) (Entry, error)
func (*Config) Promote(id string) (Entry, error)
func (*Config) Remove(id string) error

type Fallback interface { Harness() model.Harness; Roots() []string }
type Resolved struct { ID, Label string; Harness model.Harness; Roots []string; Source PathSource; Kind Kind; Disabled bool }
func (Resolved) Scannable() bool
type Problem struct { Code Code; ID string; Harness model.Harness; Path, Message string; Fatal bool }
func Resolve(cfg *Config, fallbacks []Fallback) ([]Resolved, []Problem)
```

落地时定下来的几条关键判断及其理由：

1. **JSON 键是 `entries`，不是计划草案里的 `instances`**，并且严格解码（`DisallowUnknownFields`）。未知键必须报 `invalid-config`，否则一份写错键名的配置会安静地变成"空配置"，接着回退到默认数据源——这正是 ROADMAP 192-196 明令禁止的"显式路径无效却换了数据集"。版本先单独解一遍，才能把"缺版本"（`invalid-config`）与"版本太新"（`unsupported-version`）分开。
2. **ID 与 Label 彻底分离**：ID 是 `inst_` + 128 位随机数的 base32（`default` 保留给旧版/回退实例，用户 ID 不是路径/内容哈希）；Label 只是显示别名，两个同 Label 的条目仍是两个实例，两份逐字节相同的数据也仍是两个实例，永不去重。移动只改 roots，ID/Label/Harness/Kind/Disabled 全部保留。
3. **备份副本（`backup-copy`）只存不采**：`Resolved.Scannable()` 对 `Disabled` 或 `backup-copy` 一律为 false。升格是显式的 `Promote`，对非备份副本调用会返回 `invalid-kind`，绝不隐式升格。
4. **只读配置、只写配置**：`Add`/`Move`/`Promote`/`Remove` 完全不碰文件系统（所以配置文件里可以写离线/归档路径），`Remove` 只删条目、绝不删源文件；只有 `Save` 写文件，且只写 `sources.json`。路径校验只发生在 `Resolve`，且只针对真正要扫描的启用实例。
5. **保存是原子的且失败保留旧配置**：先 `Validate`（重复/保留 ID 直接拒绝），再 `MkdirAll(dataDir, 0700)`，`CreateTemp` → `Chmod 0600` → `Write` → `Sync` → `Close` → `os.Rename`；任何失败都删掉临时文件并返回 `*ConfigError{Code: io-error}`（原始 fs 错误通过 `Unwrap` 保留），旧文件逐字节不变。`Load` 绝不创建文件。
6. **缺配置文件不是错误，坏配置文件是错误**：文件不存在 → 返回 `Version=SchemaVersion` 的空配置（调用方据此注入默认根）；文件存在但畸形/缺版本/版本更新/未知键/校验失败 → 返回带原因码的 `*ConfigError`，绝不回退。
7. **显式条目按 harness 整体替换默认链**：只要配置文件里有某个 harness 的条目，该 harness 的 fallback 一律被抑制——**包括 disabled 和 backup-copy 条目**。理由：用户显式配置了这个 harness 又说"关掉"，语义是"不要采"，而不是"退回默认"。这是确定性的单一规则，混用也只有一个结果。
8. **默认根缺失是今天已有的静默跳过，但要带原因**：注入的 fallback 根若不存在/不可读/不是目录，产出 `Fatal=false` 的 `Problem`（附 `missing-dir`/`not-dir`/`permission`），该根被丢弃，其余照常。显式条目里的同类问题则是 `Fatal=true`，且绝不回退到默认数据集。
9. **路径不做大小写折叠，但"同一个目录"按文件身份判定**；重名/包含判定只在**同一 harness 内**算致命（同一 harness 重复计一遍数据），跨 harness 共享目录不报错（不同 parser 读同一目录是合理的）。同一 harness 内相同根 → `duplicate-root`，包含关系或跨条目相同 → `overlap`；一个条目内出现"同一根重复写两次"也能被抓到（含 symlink 指向同一目录的情形）。`EvalSymlinks` 保留调用方给的拼写（大小写不敏感卷上 `Alpha` 与 `alpha` 会各自保留原拼写），所以"相同/祖先"判定在词法前缀之外还要走目录身份：`sameDir` 用 `os.Stat` + `os.SameFile`，祖先判定 `containsDir` 先试词法前缀，未命中时从内层路径逐级 `filepath.Dir` 向上比对身份（只对给定路径做有界行走，不枚举任何目录、不做全盘扫描）。因为词法前缀是快路径，大小写敏感卷上两个真实不同的目录仍是两个根（全程不折叠大小写）。显式根之间的重叠照旧致命并如实报告，绝不静默丢弃。
10. **默认根之间重叠改为"去重 + 非致命诊断"**：同一个 `Fallback` 同时声明目录与它的子目录（例如工具配置目录与其下的 projects 目录）时，只保留**最宽**的根。两个重叠目录的并集就是较宽者，因此既不会把子树扫两遍，也不会因为"先声明了窄根"而丢掉祖先的其它兄弟子树；被丢弃的根各报一条 `Fatal=false` 的 `overlap`。完全相同的根（含大小写别名）与既有行为一致地静默跳过。去重只在同一 harness 内进行，两个 harness 共享同一默认目录仍然各自保留。
11. **输入/输出切片一律深拷贝**：`Add`/`Move` 不持有调用方的切片，`Resolve` 既不改调用方内存也不共享返回值，`Load` 两次调用互不影响。

明确**没有**决定的产品问题（计划草案第 5 节，仍是未定项）：默认报表呈现、替换还是追加、GUI 可见性、label 生成方式、relay 账单匹配、降级路径、被动可疑检测、旧版 resolver 的监听行为。本次一律没有实现。

## 先红后绿证据

先写测试后写实现，红是真实的构建失败（包只有测试文件、没有任何生产代码）：

```console
$ go test ./internal/instance/
github.com/zzstar101/mytoken/internal/instance: no non-test Go files in /Users/zzstar/Developer/Programs/MyToken!!!!!/internal/instance
FAIL	github.com/zzstar101/mytoken/internal/instance [build failed]
FAIL
$ go vet ./internal/instance/
github.com/zzstar101/mytoken/internal/instance: no non-test Go files in /Users/zzstar/Developer/Programs/MyToken!!!!!/internal/instance
```

补上实现后的绿：

```console
$ go test -count=1 ./internal/instance/
ok  	github.com/zzstar101/mytoken/internal/instance	0.456s

$ go test -race -count=1 ./internal/instance/
ok  	github.com/zzstar101/mytoken/internal/instance	1.765s

$ go vet ./internal/instance/
(无输出，退出码 0)

$ gofmt -l internal/instance/
(无输出)
```

`go test -count=1 -v ./internal/instance/`：**45 PASS / 1 SKIP / 0 FAIL**，共 46 个测试（这是首次交付时的数字；本轮修复后的数字见下一节）。唯一跳过的是 `TestResolveCaseSensitiveDistinctPathsStayDistinct`，原因是本机卷大小写不敏感（`resolve_test.go:229: case-insensitive filesystem: Alpha and alpha are one directory`），该测试在大小写敏感的卷上才会真正执行。

按要求只跑了聚焦测试与 race/vet，**没有跑 fullsuite**，也没有跑真实数据、网络或任何提交操作。

一处诚实说明：`TestResolveOrderIsDeterministic` 的期望值在实现前被我改过一次。原先写的是按 harness 排序，与已定的"显式条目保持配置顺序、注入默认按 harness id 排序"不一致；我确认配置顺序更符合"列表即用户意图"，于是把期望改成 `codex/inst_z, claude-code/inst_a, gemini/default`（测试里的条目顺序故意与字母序相反，因此确实在验证配置顺序而非巧合），实现按此落地。

## 修复：大小写别名、祖先重叠与默认根去重（2026-10-13 19:24 CST，同一 HEAD `2e696f2`）

上一版把"相同/祖先"判定完全建立在词法路径前缀上（`pathContains`），只在**同一个条目内**用 `os.SameFile` 兜底。大小写不敏感卷上 `EvalSymlinks` 保留原拼写，因此两个条目分别写 `.../Alpha` 与 `.../alpha` 时词法比较看不到它们是同一个 inode，祖先关系（`.../Alpha` 与 `.../alpha/sub`）同样看不到，同一棵子树会被扫两遍；fallback 默认根之间则**完全没有**重叠检查。

### 先红（修复前，`go test -count=1 ./internal/instance/`，行号为当时版本）

```console
--- FAIL: TestResolveCrossEntryCaseAliasEqualRootsRejected
    resolve_test.go:295: want fatal overlap for a cross-entry case alias, got []
--- FAIL: TestResolveCrossEntryCaseAliasAncestorRootsRejected
    resolve_test.go:332: want fatal overlap for a cross-entry case-alias subtree, got []
--- FAIL: TestResolveSameEntryCaseAliasAncestorRootsRejected
    resolve_test.go:359: want fatal overlap for a case-aliased subtree, got []
--- FAIL: TestResolveFallbackOverlappingRootsDeduplicated
    resolve_test.go:386: fallback roots = [.../outer .../outer/inner], want only [.../outer] so the subtree is not scanned twice
--- FAIL: TestResolveFallbackCaseAliasRootsDeduplicated
    resolve_test.go:419: fallback roots = [.../Alpha .../alpha/sub], want only [.../Alpha]
--- FAIL: TestResolveFallbackOverlapDedupedPerHarnessOnly
    resolve_test.go:439: harness claude-code roots = [outer outer/inner], want [outer]
FAIL
FAIL	github.com/zzstar101/mytoken/internal/instance	0.330s
```

`TestResolveFatalContractForConfiguredVersusDefaultRoots` 在修复前就是 PASS 的（契约保持测试），修好后仍然 PASS，用来证明本次没有把"默认根非致命"改成致命。

另外做了一次**只把 `containsDir` 退回纯词法 `pathContains`** 的临时实验（不保留在仓库里），用来证明"文件身份行走"本身是承重的，而不只是新增了重叠检查：4 个大小写别名测试重新变红（`TestResolveCrossEntryCaseAliasEqualRootsRejected`、`TestResolveCrossEntryCaseAliasAncestorRootsRejected`、`TestResolveSameEntryCaseAliasAncestorRootsRejected`、`TestResolveFallbackCaseAliasRootsDeduplicated`），其中最后一个只有 `aliased_subtree` 子测试红、`equal_alias` 子测试仍绿——相等别名由 `os.SameFile` 覆盖，祖先别名只能靠身份行走。

一次诚实的自我修正：`TestResolveFallbackCaseAliasRootsDeduplicated` 最初把别名子树写成 `.../alpha/sub` 且同时声明了 `.../alpha`，于是纯词法也能去掉它（上述临时实验第一次跑时该测试仍是绿的）。已改成两个子测试：`equal_alias`（`Alpha` + `alpha`）与 `aliased_subtree`（`Alpha` + `alpha/sub`，不声明 `alpha`），后者只在身份行走下才变绿。

### 后绿（修复后）

```console
$ gofmt -l internal/instance/
(无输出)
$ go vet ./internal/instance/
(无输出，退出码 0)
$ go test -count=1 ./internal/instance/
ok  	github.com/zzstar101/mytoken/internal/instance	0.390s
$ go test -count=1 -race ./internal/instance/
ok  	github.com/zzstar101/mytoken/internal/instance	1.441s
```

`go test -count=1 -v ./internal/instance/`：**53 PASS / 1 SKIP / 0 FAIL**，共 54 个测试（`config_test.go` 28 + `resolve_test.go` 26）。唯一跳过的仍是 `TestResolveCaseSensitiveDistinctPathsStayDistinct`（本机卷大小写不敏感）；大小写敏感卷上"两个真实不同目录仍是两个根"由它守，本机由 `TestResolvePreservesRootCase` 与 `containsDir` 的词法快路径守。

本轮同样只跑聚焦测试与 race/vet，**没有跑 fullsuite**，没有真实数据、网络或提交。

## 测试清单（54）

`config_test.go`（28）：缺配置不建文件、畸形 JSON 原因码+`Unwrap` 保留原始错误、更新版本拒绝、缺版本拒绝、未知键拒绝、重复 ID 拒绝、保留 ID 拒绝、未知 harness 拒绝、非法 kind 拒绝、启用实例无根拒绝、disabled 无根接受、往返+0600+无临时残留（目录名含中文与空格）、保存幂等（字节稳定）、非法配置拒绝且旧文件不变、目录只读时保存失败且旧文件不变、落盘无秘密字段（顶层恰好 `version`/`entries`，条目字段仅 id/label/harness/roots/kind/disabled）、ID 不透明且唯一、ID 与路径/内容无关、Add 拒绝保留与重复 ID、Add 拒绝未知 harness、Add 拷贝入参切片、两次 Load 互不影响、Move 保留 ID/Label/Harness、Move 拒绝未知 ID 与空 roots、Promote 显式升格、Remove 只动配置不动源文件、Validate 拒绝重复与保留 ID、Validate 检查 disabled 条目。

`resolve_test.go`（26）：显式根缺失致命且绝不回退、文件根拒绝、不可读根拒绝、symlink 规范化并判重、单条目内包含关系拒绝、同 harness 跨条目重叠拒绝、跨 harness 共享根不致命、根大小写保留、大小写敏感卷上 `Alpha`/`alpha` 仍为两个根、disabled 条目无需根存在且抑制默认链、backup-copy 只存不可采、配置缺失时注入默认根（缺失默认根非致命）、显式条目抑制回退链、顺序确定（三次调用一致）、Label 不是身份、Resolve 输出切片不共享内存、非法配置返回致命 `Problem`、`Fallback` 可被 parser 形状类型满足；本轮新增 8 个：跨条目大小写别名同根报致命 `overlap`、跨条目大小写别名祖先报致命 `overlap`、单条目内大小写别名祖先报致命 `overlap`、默认根包含关系去重（保留宽根 + 非致命 `overlap`）、默认根宽根不因声明顺序丢失、默认根大小写别名去重（`equal_alias` + `aliased_subtree` 两个子测试）、默认根去重只作用于同一 harness、致命契约（显式无效根致命 / 默认根缺失非致命 / 致命时仍返回部分结果供诊断）。

## 未关闭项

1. **未接入运行时**：`internal/instance` 尚未被 CLI/GUI/采集器调用；`Resolve` 的结果还没有喂给 `harness.NewRooted`。本次交付的只是 API 与语义。
2. **未接入 `paths.DataDir()`**：包本身不解析数据目录、不读环境变量、不列家目录；数据目录由调用方传入。
3. **默认根的"来源"信息缺失——接入 app/scanner 前必须补的契约缺口（本轮明确记录，未在本包内解决）**：`Fallback.Roots()` 返回的已经不只是内置默认根，各 harness parser 的 `defaultRoots()` 会把**显式环境变量覆盖**（如 `MYTOKEN_*_DIRS`、`CLAUDE_CONFIG_DIR`）一起烤进同一个 `[]string`。`Resolve` 对这些根一律标成 `PathDefault` 且 `Fatal=false`，所以"用户显式设了一个无效的 env 路径"目前**不可能**触发致命/拒绝扫描的契约：`internal/instance` 单独无法满足"显式路径无效就拒绝扫描"的要求。本包不认识、也不应去猜各工具的环境变量映射，`PathSource` 也没有"env 覆盖"这个取值，因此本次没有改动任何 parser、没有扩大配置 schema。
   建议由下一个 app/scanner worker 落地（属于 harness API 决策）：
   - 给默认根带上来源：把 `Fallback` 扩成 `Roots() []Root`（例如 `type Root struct { Path string; Source PathSource }`）并新增 `PathEnv` 取值，或改为 `Fallback.DefaultRoots()` 与 `Fallback.EnvRoots()` 的拆分；
   - `Resolve` 据此把 `PathEnv` 来源的无效根报成 `Fatal=true`（语义等同显式条目），内置默认根保持 `Fatal=false`；
   - 同一 harness 同时存在 env 覆盖与内置默认根时，还需要一条明确的"env 覆盖即替换该 harness 的默认根"规则；否则两批根会同时进入扫描集合——本包只会按 harness 去重（同 harness 先到者胜），不会做这层语义。
4. **大小写不敏感卷：已按目录身份处理，但仍不做卷级探测（本轮修复）**：`Alpha` 与 `alpha`，以及 `Alpha` 与 `alpha/sub` 这类别名，现在会被 `os.SameFile` + 有界父目录行走识别为同一个目录/祖先关系；显式条目报致命 `overlap`，默认根去重并报非致命 `overlap`，全程不折叠大小写。仍然**没有**做"这个卷是否大小写敏感"的探测，也没有把别名归一化成同一个拼写：保留在根路径里的仍是调用方给的那一份拼写，判定只发生在比较阶段。另有两点边界：`containsDir` 的父目录行走次数与路径深度成正比（不枚举目录、不做全盘扫描），`dedupeDefaults` 的默认根两两比较是 O(n²)（n 为单个 parser 声明的根数，实际为个位数）。
5. **无目录 fsync/崩溃持久性保证**：`Save` 保证原子替换与无残留临时文件，未对数据目录本身 fsync。
6. **`Problem` 只是诊断数据**：没有接入既有的报告/日志输出，也没有"原因码 → 用户文案"的映射表。
7. **计划草案第 5 节的产品选择仍然未定**（默认报表呈现、替换/追加、GUI 可见性、label 生成、relay 账单匹配、降级路径、被动可疑检测、旧版 resolver 监听），本次没有替它们做任何决定。
8. **无迁移/回滚**：没有把旧版隐式路径迁移成 `sources.json` 条目的机制，也没有降级路径。
