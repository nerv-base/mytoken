# Pi parser 的 Oh My Pi（OMP）离线 root 兼容验证

日期：2026-10-14；环境：go1.27.1 darwin/arm64；工作目录 `/Users/zzstar/Developer/Programs/MyToken!!!!!`；分支 `feat/0.4x-harnesses`；HEAD `a9684c7`。

写入范围：仅 `internal/harness/pi/pi.go`、`internal/harness/pi/pi_test.go`、新增 `internal/harness/pi/root_test.go` 与本文件。未改动 `model` / `registry` / `UI` / `store`，未新建 harness，未提交：

```console
$ git status --porcelain -- internal/harness/pi docs/harness-verification/omp-pi.md
 M internal/harness/pi/pi.go
 M internal/harness/pi/pi_test.go
?? internal/harness/pi/root_test.go
?? docs/harness-verification/omp-pi.md
```


## 结论与范围

给现有 Pi parser 增加 OMP 的**离线 root 兼容**：OMP 是 Pi 的 fork，写出字节格式相同的 session 日志，但仓库里没有 OMP parser，所以由 `internal/harness/pi` 顺带发现 OMP 的 sessions 目录。

**做**：默认 roots 扩展为「Pi 自己的 `~/.pi/agent/sessions`」+「OMP 的 sessions 目录」，两者并列、互不覆盖；`PI_HOME` 与 `~/.pi/agent/sessions` 的原有行为逐一保留（`pi_test.go:787` 的注册测试仍要求 `NewWithRoots`/`NewWithRoot` 语义不变）。

**不做**（本轮明确不变）：不新增 harness、不做 OMP 的 model/provider 推断、不读 `credentials`/`agent.db`/`stats.db`、不扫描 `sessions` 之外的目录、不改 registry 与 UI。解析 schema 完全沿用现有 Pi 解析器，没有为 OMP 增加任何分支。

## 官方依据（外部资料，仅作证据）

`packages/utils/src/dirs.ts`（`can1357/oh-my-pi` main）。实现严格遵守其语义，其中两条与直觉相反、值得记录：

1. **XDG 的 app 目录是 `omp`，不是 `.omp`**：`APP_NAME: string = "omp"`，而 `CONFIG_DIR_NAME: string = ".omp"`。`$XDG_DATA_HOME/omp` 存在即重定向，sessions 直接落在其下（`agent/` 层被压平）：`~/.omp/agent/sessions` → `$XDG_DATA_HOME/omp/sessions`。
2. **`PI_CODING_AGENT_DIR` 优先于 XDG**：构造函数里 `const agentDirOverride = profile ? undefined : options.agentDirOverride; this.agentDir = agentDirOverride ? path.resolve(agentDirOverride) : defaultAgent`，XDG 只在 `isDefault`（未做 override）时才参与 `xdgData = resolveIf("XDG_DATA_HOME")`。所以显式 agent 目录不会被 XDG 迁移位置顶掉。
3. 其余：`xdgPlatform = process.platform === "linux" || "darwin"`（XDG 只在这两个平台生效，且只对 default profile）；`resolveProfileEnv(omp, pi)` 是 `omp !== undefined ? omp : pi` 的**取值判定**；`PROFILE_NAME_RE` 与 Windows 设备名黑名单决定合法 profile。

虽然 `docs/session-format.md` 不存在（HTTP 404），但官方源码 `packages/coding-agent/src/session/session-entries.ts` 已直接定义 `SessionHeader`（`type: "session"`）、assistant `message.usage` 与独立的 `model_usage` 记录。因此字段兼容有一手源码依据；尚缺的是当前发行版脱敏真实产物对拍，见「未关闭的门」第 1 条。

## 实现要点

`rootProvenance()`（`pi.go:179`）的优先级，逐条与官方对齐：

| 顺序 | 条件 | 结果 |
| --- | --- | --- |
| 1 | `MYTOKEN_PI_DIRS` 非空 | 逐条 verbatim，`Env` 记为 `MYTOKEN_PI_DIRS`；列表不可解析时返回 `*harness.EnvListError`（硬错误，不回落默认值） |
| 2 | `PI_HOME` 非空 | `$PI_HOME/sessions`，**整组替换**（与其它 parser 的 `*_HOME` 一致，不拖入无关 OMP 安装） |
| 3 | 默认 | `~/.pi/agent/sessions`(builtin) + OMP sessions |

OMP sessions 目录由 `ompAgentDir(home)`（`pi.go:243`）按官方顺序给出：命名 profile → `PI_CODING_AGENT_DIR`（仅 default profile）→ XDG（`linux`/`darwin`、仅 default profile、且 `$XDG_DATA_HOME/omp` 存在）→ `~/.omp/agent`。`PI_CONFIG_DIR`（`pi.go:213`）替换配置目录名 `.omp`。

安全与稳定性：

- **profile 名字先校验再拼路径**：`validProfileName`（`pi.go:326`）实现 `^[a-z0-9][a-z0-9._-]{0,63}$`，并额外拒绝 `.`、`..`、尾随 `.`、Windows 设备名，"%"字面量不参与拼接，因此 `profiles/<name>/agent` 无法穿越。
- **非法/空 profile 视作 default profile**，不报错：一个坏环境变量不该阻止扫描 Pi 自己的日志。
- **去重稳定**：`dedupeRoots`（`pi.go:340`）保留首次出现的 provenance 与输入顺序。
- **空值即未设置**：`harness.Getenv` 语义（空白 = 未设置）用于所有新变量，因此 `PI_CONFIG_DIR=""` 不会把配置根塌到 `$HOME` 本身（`TestRootsBlankConfigDirIsUnset`）。
- **只发现 `*.jsonl` 且首行须是 session header / async record**：`piLogKind`（`pi.go:425`）已保证；`credentials.jsonl`/`notes.txt`/`agent.db`/`stats.db`/`omp.pid` 都进不了 `Discover`。

## 严格 TDD 过程

1. **RED**：先写 `root_test.go`（16 个测试），在未改动的实现上运行，确认失败：
   `go test ./internal/harness/pi/ -run 'TestRoots|TestDefaults'` → FAIL，并从中读出了真实缺陷。
2. **GREEN**：最小实现后重跑，逐个修掉暴露出的问题（见下）。
3. **REFACTOR**：抽出 `profileName`、`xdgSupported`、`dedupeRoots`、`isDir`，去掉重复分支；覆盖率 82.4%，新函数 `ompAgentDir` / `activeProfile` / `profileName` / `profileEnv` / `validProfileName` / `xdgSupported` / `isDir` / `dedupeRoots` / `rootProvenance` 均 100%。

过程中由测试暴露并修正的 5 个缺陷（都先 FAIL 再改）：

| # | 缺陷 | 根因 | 修法 |
| --- | --- | --- | --- |
| 1 | 空 `OMP_PROFILE` 不回落到 `PI_PROFILE` | 用了 set-ness（`LookupEnv`） | 改用 truthiness（`harness.Getenv`），对齐 `omp !== undefined ? omp : pi` |
| 2 | 空 `PI_PROFILE` 被当成 profile | 同上 | 同上 |
| 3 | XDG 门禁查 `.omp` 而非 `omp` | 误用 config 目录名 | 新增 `ompDataDirName = "omp"`（官方 `APP_NAME`） |
| 4 | `PI_CODING_AGENT_DIR` 被 XDG 顶掉 | 分支顺序与官方相反 | override 提到 XDG 之前 |
| 5 | 空 `PI_CONFIG_DIR` 把配置根塌到 `$HOME` | set-ness 判定 | 空白视作未设置 |

测试侧修正（同样是"测试先坏"暴露的）：`TestRegistration` 的 root 数量断言由 1 改 2；两个默认发现用例改为直接调 `New()`（registry 单例在包 init 时已捕获真实 `$HOME`，而 `t.Setenv` 在 init 之后）；XDG 用例的目录名；traversal 用例的断言改为"不得落到 `profiles/` 前缀下"；`too_long` 用例原用 `string(make([]byte,200))`（200 个 NUL，`t.Setenv` 直接报 `setenv: invalid argument`）改为 `strings.Repeat("a",200)`；去掉一个与"缺失 root 也要报告"相矛盾的重复 fixture。

测试全程不读真实 HOME：每个用例 `isolateHome` 把 `HOME`/`USERPROFILE` 指向 `t.TempDir()`，并清空 `PI_HOME`/`PI_CODING_AGENT_DIR`/`OMP_PROFILE`/`PI_PROFILE`/`XDG_DATA_HOME`/`MYTOKEN_PI_DIRS`。

## 验证命令与结果

```console
$ gofmt -l internal/harness/pi/
(空)

$ go test ./internal/harness/pi ./internal/harness
ok  	github.com/zzstar101/mytoken/internal/harness/pi	0.432s
ok  	github.com/zzstar101/mytoken/internal/harness	0.254s

$ go vet ./internal/harness/pi ./internal/harness
(无输出)

$ go test -race ./internal/harness/pi ./internal/harness
ok  	github.com/zzstar101/mytoken/internal/harness/pi	1.534s
ok  	github.com/zzstar101/mytoken/internal/harness	1.731s

$ go build ./...
(成功)
```

新增 `root_test.go`（16 个顶层测试，含子测试共 32 个用例）并更新既有 `TestRegistration`（`pi_test.go:787`）：`TestRootsOMPBuiltinIsDiscoveredBeyondPiDefault`、`TestRootsAgentDirOverride`、`TestRootsAgentDirOverrideIsDirectoryExistenceAgnostic`、`TestRootsXDGDataHomeGatedOnOMPDirectory`、`TestRootsXDGDoesNotReplaceExplicitAgentDir`、`TestRootsProfileAgentDir`、`TestRootsProfilePrecedence`、`TestRootsProfileRejectsPathTraversal`、`TestRootsMyTokenPiDirsOverride`、`TestRootsConfigDirOverride`、`TestRootsBlankConfigDirIsUnset`、`TestRootsDedupesOverlappingRoots`、`TestDedupeRoots`、`TestRootsMyTokenPiDirsRejectsBlankList`、`TestDefaultsDiscoverOMPAndPiRootsTogether`、`TestDefaultsSkipAbsentOMPRoots`。

## 未关闭的门（schema 风险）

1. **尚无脱敏真实 OMP 产物对拍**：官方源码 `packages/coding-agent/src/session/session-entries.ts` 已证实 `type=session` header、assistant `message.usage` 与独立 `model_usage`，所以并非纯猜测；但仓库中的验证仍是按该官方 schema 生成的合成 fixture，不能冒充当前发布包的真实日志验证。若发行包与源码偏离，症状可能是静默 0 事件而不是报错。
2. **`PI_CONFIG_DIR` 超出 brief 列出的变量**：brief 只列了 `PI_CODING_AGENT_DIR`/`OMP_PROFILE`/`PI_PROFILE`/`XDG_DATA_HOME`（并明确要求"不要臆造变量"）。`PI_CONFIG_DIR` 是官方 `dirs.ts` 真实读取的变量（默认 `.omp`），为正确性而支持，但它不在 brief 白名单内；如认为超出范围，可只删 `pi.go:213` 的 `ompConfigDirEnv` 与 `TestRootsConfigDirOverride`/`TestRootsBlankConfigDirIsUnset`。
3. **XDG 只按 `$XDG_DATA_HOME/omp` 存在性判定**：与官方一致，不校验 `omp config migrate` 是否真的跑过。若用户设了 `XDG_DATA_HOME` 且恰好存在同名 `omp` 目录（但没迁移），我们会去看那个空目录而看不到 `~/.omp`（官方行为亦然）。
4. **`PI_HOME` 语义未变**：它仍然是"整组替换"，因此设了 `PI_HOME` 的 Pi 用户不会同时看到 OMP sessions。这是为保持既有行为而做的选择，不是遗漏；若希望 `PI_HOME` 与 OMP 并存，需要显式用 `MYTOKEN_PI_DIRS`。
5. **未处理 `OMP_STATE_HOME`/`OMP_CACHE_HOME`**：官方同一处还读 `XDG_STATE_HOME`/`XDG_CACHE_HOME`，但 sessions 只与 data 类别相关，本轮未实现（`sessions` 不在 state/cache 下）。
