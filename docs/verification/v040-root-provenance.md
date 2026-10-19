# 0.4.0 源根 provenance 前置（source root provenance）验证

日期：2026-10-13。环境：Go 1.27.1、darwin/arm64。写入范围：`internal/harness/**`、`internal/instance/**`、本文件。未改 `internal/model/**`、`internal/store/**`、`internal/query/**`、`internal/budget/**`、`internal/app/**`、scanner、GUI、CLI，未改共享规划文件（`task_plan.md` 等只读）。无 agent、无 commit、无网络、无真实用户数据读取。

## 结论与范围

多实例扫描需要按 **根来源（provenance）** 而不是仅按"目录是否存在"来决定失败语义：用户显式指定的数据集缺失是硬错误，内置平台默认缺失只是"本机未安装"。本轮交付该前置能力。

- `Parser.Roots()` 的语义逐字未变（"不存在的 root 允许并被忽略"），`NewRooted`/`WithRoots` 的显式 roots 语义未变，`harness` 全局注册表结构未变（未新增注册 API、未 mutate 单例）。
- 新增**可选能力** `harness.Provenance`（`RootProvenance() ([]Root, error)`）+ 注册表函数 `harness.RegisteredProvenance(h)`，28 个已注册 harness 全部实现（`codebuddy` 复用 `workbuddy.Parser` 的方法）。
- 每个 root 带 `Origin` ∈ {`explicit`, `env`, `builtin`}；`env` 额外带产生它的环境变量名 `Env`。
- `instance.Resolve` 通过可选接口 `instance.Provenanced` 消费 provenance（不实现者按平台默认处理，保持 `Fallback` 结构兼容），并新增 `PathSource` 值 `PathEnv`。
- 顺带修复：**非空白畸形显式列表不再静默回退到内置默认**（`*harness.EnvListError` = 硬错误）；**空白（仅空格）环境值一律视为未设置**；**provenance 每次调用实时读环境**，不依赖 init 期捕获的陈旧值。

**这不是多实例交付声明。** 本轮只交付前置能力与 `Resolve` 集成，未接线 scanner/app（未改 `internal/app/**`、scan、GUI、CLI），未落地每实例 roots 的持久化/UI。未关闭项见文末。

### 优先级（本轮落地语义）

| 层级 | 来源 | 失效语义 | `Resolved.Source` |
| --- | --- | --- | --- |
| 1 | 配置条目（`sources.json`，`PathUser`） | 缺失/非目录/不可读/重复/重叠 ⇒ **fatal**，绝不回退 | `user` |
| 2 | 显式环境/工具目录（`MYTOKEN_*_DIRS`、`CODEX_HOME` 等） | 缺失/非目录/不可读 ⇒ **fatal**，且**不materialise** 该 harness 的默认实例（不做部分内置回退） | `env` |
| 3 | 内置/通用平台默认（`HOME`、`XDG_*`、`APPDATA`、固定目录名） | 缺失 ⇒ **non-fatal**（保持既有 skip 行为，但带原因上报） | `default` |

- 同 harness 内层 2 **替换**层 3，不追加（`MYTOKEN_*_DIRS` 即使只给一个目录也替换整套内置 root）。
- 某 harness 已有配置条目（启用、禁用或备份拷贝）⇒ 完全抑制其环境层读取（`RootProvenance()` 不会被调用）。
- 通用平台派生（`HOME`/`XDG_*`/`APPDATA`/`LOCALAPPDATA`/`os.UserConfigDir()`）**不**因为"由环境变量派生"就算显式：它们仍是 `builtin`，目录缺失不致命。

## 新增 API

```go
// internal/harness/harness.go
type RootOrigin string // OriginExplicit | OriginEnv | OriginBuiltin        :141-155
type Root struct { Path string; Origin RootOrigin; Env string }            :158

func BuiltinRoots(paths ...string) []Root                                   :167
func EnvRoots(env string, paths ...string) []Root                           :176
func ExplicitRoots(paths ...string) []Root                                  :185
func RootPaths(roots []Root) []string                                       :194

func Getenv(key string) (string, bool)                                      :206  // TrimSpace；空白==未设置
func SplitEnvList(env, value string) ([]string, error)                      :216  // SplitList+trim+Clean+丢空
type EnvListError struct { Env, Value string }                              :231  // 非空白但无可用目录

type Provenance interface { RootProvenance() ([]Root, error) }              :248
func RegisteredProvenance(h model.Harness) (roots []Root, ok bool, err error) :257
```

- `RootProvenance` 契约：**每次调用实时读环境**（单例在 init 期构造，捕获值会过期），不得读凭据。
- `RegisteredProvenance`：`ok=false` 表示"未注册或未实现 `Provenance`"；`err != nil` 只表示"非空白显式覆盖但无可用目录"，调用方必须按硬错误处理、不得替换为内置默认。
- 每个已注册单例持有 `prov func() ([]harness.Root, error)`；`New()` 设置它，`NewWithRoots`/`NewForHarness`/`WithRoots` **不设置**，因此显式 roots 解析器报告 `OriginExplicit` 且**绝不**回退到环境/内置。

```go
// internal/instance/resolve.go + instance.go
type PathSource string // PathUser | PathEnv | PathDefault   instance.go:134-149
type Provenanced interface { Fallback; RootProvenance() ([]harness.Root, error) }  resolve.go:32
func DefaultFallbacks() []Fallback                            resolve.go:40
func Resolve(cfg *Config, fallbacks []Fallback) ([]Resolved, []Problem)  resolve.go:124
func declaredProvenance(fb Fallback, h model.Harness) ([]harness.Root, *Problem)  resolve.go:211
func sortedFallbacks(...) []fallbackRoots                     resolve.go:237
```

`instance` 现在 import `internal/harness`。无环：`internal/harness` 只 import `internal/model`，不 import 任何 parser 子包，也不 import `internal/instance`。`Fallback` 仍是结构兼容接口，第三方/旧式 parser 不实现 `Provenanced` 时按 `builtin` 处理。

## 每 harness 环境变量表（28，逐个读源码得出，无猜测）

表中"显式覆盖"按优先级从左到右；命中的 root 标记 `OriginEnv` + 该变量名。"内置默认"派生自 `HOME`/`XDG_*`/`APPDATA`/`LOCALAPPDATA`/`os.UserConfigDir()`/GOOS 分支，标 `OriginBuiltin`。

| harness | 显式覆盖（`OriginEnv`） | 内置默认（`OriginBuiltin`） |
| --- | --- | --- |
| amp | `MYTOKEN_AMP_DIRS`（列表）、`AMP_DATA_DIR`（逗号分隔） | `~/.local/share/amp` |
| antigravity | `MYTOKEN_ANTIGRAVITY_DIRS`、`GEMINI_CLI_HOME`（5 个派生 root） | 同上 5 个 root，基址 `~/.gemini` |
| claude-code | `CLAUDE_CONFIG_DIR`（+`projects`） | `~/.claude/projects`；存在时追加 `~/.config/claude/projects` |
| claude-desktop | `MYTOKEN_CLAUDEDESKTOP_DIRS` | GOOS 分支（windows `%APPDATA%/Claude/...`、darwin 2×`~/Library/Application Support/...`、其他 `os.UserConfigDir()`） |
| cline | `MYTOKEN_CLINE_DIRS`；未设时 `CLINE_DATA_DIR`/`CLINE_DIR`（`+tasks`）、`CLINE_SESSION_DATA_DIR`（会话；未设时继承 data 的 env 标记） | `clinetask.VSCodeTaskDirs(cline)` 及其余 data/session 目录 |
| codebuddy | `CODEBUDDY_HOME`（+`projects`） | `~/.codebuddy/projects` |
| codebuff | `MYTOKEN_CODEBUFF_DIRS`、`CODEBUFF_DATA_DIR` | 3×`~/.config/<channel>` |
| codex | `CODEX_HOME`（`sessions` + `archived_sessions`） | 同上，基址 `~/.codex` |
| crush | `MYTOKEN_CRUSH_DIRS`、`CRUSH_GLOBAL_DATA` | `$XDG_DATA_HOME/crush`（windows `%LOCALAPPDATA%`，最终 `~/.local/share/crush`） |
| droid | `MYTOKEN_DROID_DIRS`、`FACTORY_DIR` | `~/.factory` |
| dsh | `DSH_HOME`（+`sessions`） | `~/.dsh/sessions` |
| forge | `MYTOKEN_FORGE_DIRS` | `~/.forge` + `~/forge`（legacy） |
| gemini | `MYTOKEN_GEMINI_DIRS`、`GEMINI_CLI_HOME`（`<h>/.gemini/tmp`、`<h>/tmp`） | `~/.gemini/tmp` |
| goose | `MYTOKEN_GOOSE_DIRS`、`GOOSE_PATH_ROOT`（`data/sessions`） | windows `%APPDATA%/Block/goose/sessions`，其他 `$XDG_DATA_HOME/goose/sessions` |
| grok | `MYTOKEN_GROK_DIRS`、`GROK_HOME` | `~/.grok` |
| hermes | `HERMES_HOME` | `~/.hermes`（`os.UserHomeDir()` 失败时无 root） |
| kilo | `MYTOKEN_KILO_DIRS` | `clinetask.VSCodeTaskDirs(kilo)` |
| kimi | `MYTOKEN_KIMI_DIRS`、`KIMI_SHARE_DIR`（+`sessions`） | `~/.kimi/sessions` |
| kiro | `MYTOKEN_KIRO_DIRS`、`KIRO_HOME` | `~/.kiro` + `defaultAgentDirs()`（linux 分支对 server 根做 `os.Stat` 存在性门控） |
| mux | `MYTOKEN_MUX_DIRS`、`CODEBURN_MUX_DIR`、`MUX_ROOT`（后两者经 `expandHome`） | `~/.mux`、`~/.mux-dev`、`~/.cmux` |
| openclaude | `MYTOKEN_OPENCLAUDE_DIRS`、`CODEBURN_OPENCLAUDE_DIR`（+`projects`） | `~/.openclaude/projects` |
| openclaw | `MYTOKEN_OPENCLAW_DIRS` | 4×`Join(home, storeRoots[i][0], storeRoots[i][1])` |
| opencode | `MYTOKEN_OPENCODE_DIRS`、`OPENCODE_DATA_HOME` | `$XDG_DATA_HOME/opencode`，否则 `~/.local/share/opencode` |
| pi | `PI_HOME`（+`sessions`） | `~/.pi/agent/sessions` |
| qwen | `MYTOKEN_QWEN_DIRS`、`QWEN_DATA_DIR` | `~/.qwen/projects` |
| roo | `MYTOKEN_ROO_DIRS` | `clinetask.VSCodeTaskDirs(roo)` |
| workbuddy | `WORKBUDDY_HOME`（+`projects`） | `~/.workbuddy/projects` |
| zed | `MYTOKEN_ZED_DIRS` | GOOS 分支（darwin `~/Library/Application Support/Zed/threads`、windows `%LOCALAPPDATA%/Zed/threads`、其他 `~/.local/share/zed/threads`） |

该表在测试里以 `overrideEnvSurface`（`internal/harness/provenance_test.go`）逐项断言：单设每个变量必须产出 `OriginEnv`+该变量名的 root；全部清空必须只剩 `builtin`；变量数必须与注册表条目数相等（新增 harness 而未登记会直接失败）。

## 空白 / 畸形 / 列表边界语义

| 输入 | 语义 | 证据 |
| --- | --- | --- |
| 未设置，或仅空白（`"   "`, `"\t\n"`） | 视为**未设置**，落到下一层 | `TestProvenanceBlankOverrideIsTreatedAsAbsent`、`TestGetenvAndSplitEnvListEdges/getenv blank is absent` |
| 非空白但无可用目录（`":"`、`"::"`、`"   :  "`；amp 为 `","`） | `*harness.EnvListError` ⇒ `RootProvenance` 返回错误 ⇒ `Resolve` 报 **fatal** `invalid-root`，**不**回退内置默认 | `TestProvenanceMalformedOverrideIsErrorNotDefault`、`TestResolveMalformedExplicitEnvListIsFatalAndNotDefaulted` |
| 列表含多余分隔符/空白/`..` | trim + `filepath.Clean` + 去空，保序 | `TestGetenvAndSplitEnvListEdges/split list cleans, trims and drops blanks` |
| 显式 roots（含空列表） | `OriginExplicit`，零回退 | `TestProvenanceExplicitRootsNeverFallBackToDefaults`、既有 `TestNewRootedExplicitEmptyRootsStayEmpty` |

## 运行时装配 API（给 scanner/app 集成）

```go
cfg, err := instance.Load(dataDir)              // 缺失文件 ⇒ 默认配置（零配置行为不变）
rs, ps := instance.Resolve(cfg, instance.DefaultFallbacks())
for _, p := range ps {
    if p.Fatal { /* 该 ID/harness 的实例不可用：报错，禁止替换为别的数据集 */ }
}
for _, r := range rs {
    if !r.Scannable() { continue }              // !Disabled && Kind==KindInstance
    // r.Source == instance.PathUser  ⇒ 配置条目
    // r.Source == instance.PathEnv   ⇒ 显式环境覆盖（root 已验证存在且可读）
    // r.Source == instance.PathDefault ⇒ 内置默认（可能为空 roots：本机未安装）
    // 用 r.Roots 交给 harness.NewRooted(r.Harness, r.Roots...) 构造有根解析器
}
```

- `Resolve` 不因某个 fallback 的 fatal 问题中止整体解析（与既有"逐条 fatal"行为一致）：调用方必须检查 `Problem.Fatal`。
- 环境层 fatal ⇒ 该 harness **没有**默认 `Resolved`（不会产生"空 roots 的默认实例"来掩盖错误）。
- `PathEnv` 实例的 roots 已由 `canonicalRoot` 验证（`Abs`/`Stat`/`IsDir`/`EvalSymlinks`/可读性），可直接扫描。
- 同 harness 重叠默认根的 `samefile`/祖先去重（workflow-25 引入的 `sameDir`/`containsDir`/`dedupeDefaults`）逐字保留，provenance 路径复用同一批 helper。

## 测试：先观测红，再验证绿

红证据（临时把三类历史缺陷注回源码后运行，随后按备份 sha1 逐字节还原）：

1. 把 `claude` 的 `CLAUDE_CONFIG_DIR` root 错误标成 `builtin`（env 与内置混淆）：
   `--- FAIL: TestProvenanceHonoursEveryDocumentedOverride/claude-code/CLAUDE_CONFIG_DIR` —
   `produced no env-origin root; roots=[{Path:.../projects Origin:builtin Env:}]`
2. 把 `qwen` 的 `SplitEnvList` 错误吞掉并落到内置默认（畸形列表被转成默认）：
   `--- FAIL: TestProvenanceMalformedOverrideIsErrorNotDefault/qwen/MYTOKEN_QWEN_DIRS` —
   `MYTOKEN_QWEN_DIRS=":" must be a hard error, got roots=[{Path:/Users/zzstar/.qwen/projects Origin:builtin Env:}]`
3. 把 `Resolve` 的 fallback 失败一律改成 `Fatal: false`（"所有 fallback 失败都非致命/默认"）：
   `--- FAIL: TestResolveMissingExplicitEnvRootIsFatalAndNotReplaced`（`Fatal:false`）、
   `--- FAIL: TestResolveUnusableExplicitEnvRootVariantsIsFatal/file`（not-dir `Fatal:false`）、
   `--- FAIL: TestResolveUnusableExplicitEnvRootVariantsIsFatal/unreadable`（permission `Fatal:false`）

还原校验：三文件 sha1 与备份一致（`claude.go bd3c2c08…`、`qwen.go 7bb04179…`、`resolve.go f7184a92…`），`gofmt -l` 无输出。

绿证据（还原后）：

```
go build ./...                                        OK
go vet ./internal/harness/ ./internal/instance/       OK
go test ./internal/harness/... ./internal/instance/... 31 packages ok
go test -race ./internal/harness/ ./internal/instance/ ok
```

新增测试文件：`internal/harness/provenance_test.go`（10 个顶层测试，含 28×N 子测试）、`internal/instance/provenance_test.go`（11 个测试）。均用 `t.TempDir()` 合成路径与 `t.Setenv`，**不**探测真实 home、不读凭据。

覆盖到的红转绿清单：显式 env root 缺失 fatal / 内置缺失 non-fatal / 配置条目（含 invalid env）胜出且不读环境 / 两个合法 env root 均保留且标 `PathEnv` / 空白与列表边界 / 禁用与备份拷贝抑制 fallback / 构造期显式 roots 零回退 / 28 个 harness 一致（无"意外不支持"）/ 实时非 init 捕获。

## 平台限制（未验证项）

- 本轮运行在 darwin/arm64：`claudedesktop`、`zed`、`kiro`、`goose`、`crush` 的 **windows** 内置分支与 `LOCALAPPDATA` 路径**未在本次运行中被执行**；它们的 darwin/默认分支已覆盖。windows 行为需要 windows 运行环境或跨平台 CI 才能验证。
- `SplitEnvList` 的分隔符取 `filepath.ListSeparator`（darwin/linux `:`，windows `;`）：测试用该常量，故在 windows 上同样成立，但本次未在 windows 上跑。
- 权限子测试在 `euid == 0`（root）时自动 skip——root 不受权限位限制。
- `harness.Home()`/`os.UserHomeDir()` 不可用时，`hermes` 等会返回零内置 root；测试用 `homeKnown` 守卫，不把"无 home"误判为失败。
- `claude` 的第二个内置 root 与 `kiro` linux 分支的一个 root 是**存在性门控**（`os.Stat`）：provenance 计算会触碰文件系统元数据（只读、只看目录存在性），这是既有行为的保留；不读文件内容、不读凭据。
- macOS `t.TempDir()` 位于 `/var/folders/...`（`/var` 是符号链接）：`canonicalRoot` 的 `EvalSymlinks` 会规范到 `/private/var/...`，测试断言统一走 `canonicalRoot`，不比较字面路径。

## 未关闭项

- scanner/app 未接线：`internal/app/**`、scan、GUI、CLI 本轮按任务边界未改；装配方式见上文"运行时装配 API"。
- 每实例 roots 的持久化写入（`instance.Save` 的 UI/CLI 路径）不在本轮。
- `Resolve` 目前对"环境层 fatal"不生成默认实例，但**会**在 `Problem` 里同时上报其它非致命问题；调用方需要按 ID/harness 关联 `Problem` 与 `Resolved`（既有契约未变）。
- windows 分支未验证（见平台限制）。
