# Pi parser 的 OMO/senpi（Oh My OpenAgent）离线 root 兼容验证

日期：2026-10-16；环境：go1.27.1 darwin/arm64；工作目录 `/Users/zzstar/Developer/Programs/MyToken!!!!!`；分支 `feat/0.4x-harnesses`；HEAD `a9684c7`。

写入范围：仅 `internal/harness/pi/pi.go`、`internal/harness/pi/pi_test.go`、`internal/harness/pi/root_test.go` 与本文件。未改动 `sessionlog` / `stepcode` / `model` / `app` / `README` / `ROADMAP` / 规划文件，未提交：

```console
$ git status --porcelain -- internal/harness/pi docs/verification/omo-senpi.md
 M internal/harness/pi/pi.go
 M internal/harness/pi/pi_test.go
?? internal/harness/pi/root_test.go
?? docs/verification/omo-senpi.md
```

工作区里其余 `M`/`??`（`internal/app/app.go`、`internal/harness/kimi/**`、`opencode/**`、`provenance_test.go`、`rooted_*_test.go`、`model/model.go`、`docs/superpowers/`、`findings.md`、`progress.md`、`task_plan.md`）是其它工作流的既有改动，本次没有触碰。本轮保留上一轮交付的 Step `sessionlog` 抽取与既有 OMP roots：`rootProvenance()` 的前两档（`MYTOKEN_PI_DIRS`、`PI_HOME`）与 OMP 分支的判定顺序一个字节都没改，只在 OMP 之后追加了 OMO/senpi 一档，并把两处重复的"agent 目录 → sessions root"拼接抽成一个助手。全文行号均指本轮修改后的文件；`omp-pi.md` 内的行号因 `pi.go` 前移而变旧，但该文件不在本轮允许修改的清单内，故未同步。

## 结论与范围

给现有 Pi parser 增加 OMO/senpi 的**离线 root 兼容**：Oh My OpenAgent（OMO）是 senpi 引擎的品牌发行版，senpi 又是 Pi 的 fork，三者写出字节格式相同的 session 日志（`packages/memory-core/src/search/senpi-session-provider.ts` 明确复用 `type=session` header 与 `type=message` 条目），但仓库里没有 OMO/senpi parser，所以由 `internal/harness/pi` 顺带发现它的 sessions 目录。

**做**：默认 roots 扩展为「Pi `~/.pi/agent/sessions`」+「OMP sessions」+「OMO/senpi sessions」，三者并列、互不覆盖；且 OMO/senpi 只贡献**一个**解析后的 root。`PI_HOME` 与 `MYTOKEN_PI_DIRS` 的原有行为逐一保留。

**不做**（本轮明确不变）：不新增 harness；不改 registry、model provider 推断与 UI；不读 `credentials`/`settings.json`/`agent.db`/`stats.db`；除 `<agentHome>/sessions` 外不扫描任何目录，**特别是不扫描 OMO 的 `children` 与 event logs，避免与 Pi/OMP 双计**；OMO 的 OpenCode plugin 模式仍由现有 OpenCode parser 读它自己的数据库，**不在 Pi parser 新增根**；解析 schema 完全沿用现有 Pi 解析器，没有为 OMO/senpi 增加任何解析分支。

## 官方依据（外部一手源码，仅作证据）

`/tmp/oh-my-openagent.BCB1xe/packages/omo-senpi/src/components/agent-home/resolve-agent-home.ts`（`AGENT_DIR_ENV_NAMES` / `AGENT_HOME_SENTINEL` / `resolveAgentHome` / `resolveAgentSessionsDirectory`）。实现严格遵守其语义，其中与直觉相关的几条值得记录：

1. **env 优先级是 OMO → SENPI → PI**：`for (const name of AGENT_DIR_ENV_NAMES)`，逐个 `env[name]?.trim()`，**第一个非空**即 `return resolve(configured)`。空串与纯空白都必须跳过，否则会塌到 cwd。
2. **只有 sentinel 才让 `~/.omo` 成为 engine home**：`canonical = join(homeDir, ".omo", "agent")`，`exists(join(canonical, "settings.json"))` 命中才返回 canonical；否则再看 `exists(join(homeDir, ".omo", "settings.json"))`，命中返回旧 flat 的 `~/.omo`；两处都不命中才退回 `join(homeDir, ".senpi", "agent")`。**canonical 优先 legacy**，且目录名叫 `.omo` 不等于它是 engine 状态。
3. **sessions 永远是 `<agentHome>/sessions`**：`resolveAgentSessionsDirectory` = `join(resolveAgentHome(...), "sessions")`，所以 root 是 agent 目录**下面一层**，不是 agent 目录本身。
4. **transcript schema 与 Pi 相同**：`packages/memory-core/src/search/senpi-session-provider.ts` 解析 `SessionHeader {type:"session", id, timestamp, cwd, version?, parentSession?}`、`SessionEntryBase {type,id,parentId,timestamp}`、`SessionMessageEntry {type:"message", message}`，内容块 `text`/`thinking`/`toolCall`。这正是 `piLogKind`（`pi.go:480`）已经接受的形态，因此无需新解析代码。

缺失的是当前发行版的**脱敏真实产物对拍**，见「未关闭的门」第 1 条。本文件的全部 fixture 均为按上述官方 schema 手工构造的合成日志。

## 实现要点

`rootProvenance()`（`pi.go:174`）的优先级，逐条与官方对齐：

| 顺序 | 条件 | 结果 |
| --- | --- | --- |
| 1 | `MYTOKEN_PI_DIRS` 非空 | 逐条 verbatim，`Env` 记为 `MYTOKEN_PI_DIRS`；列表不可解析时返回 `*harness.EnvListError`（硬错误，不回落默认值） |
| 2 | `PI_HOME` 非空 | `$PI_HOME/sessions`，**整组替换**（与其它 parser 的 `*_HOME` 一致，不拖入无关的 OMP/OMO 安装） |
| 3 | 默认 | `~/.pi/agent/sessions`(builtin) + OMP sessions + OMO/senpi sessions |

OMO/senpi 的 agent 目录由 `omoSenpiAgentDir(home)`（`pi.go:265`）按官方顺序给出，返回值第二项是被命中的环境变量名（检测/回落路径为 `""`）：

| 顺序 | 条件 | agent 目录 | provenance |
| --- | --- | --- | --- |
| 1 | `OMO_CODING_AGENT_DIR` 非空 | `resolve($OMO_CODING_AGENT_DIR)` | Env，`Env="OMO_CODING_AGENT_DIR"` |
| 2 | `SENPI_CODING_AGENT_DIR` 非空 | `resolve($SENPI_CODING_AGENT_DIR)` | Env，`Env="SENPI_CODING_AGENT_DIR"` |
| 3 | `PI_CODING_AGENT_DIR` 非空 | `resolve($PI_CODING_AGENT_DIR)` | Env，`Env="PI_CODING_AGENT_DIR"` |
| 4 | `~/.omo/agent/settings.json` 存在 | `~/.omo/agent`（canonical） | Builtin |
| 5 | `~/.omo/settings.json` 存在 | `~/.omo`（legacy flat） | Builtin |
| 6 | 否则 | `~/.senpi/agent`（standalone） | Builtin |

每一档都再拼 `/sessions`，并复用新助手 `appendAgentSessions`（`pi.go:203`）在 env/builtin 两种来源间切换；`dedupeRoots`（`pi.go:420`）保留首次出现的 provenance 与输入顺序。

安全与稳定性：

- **空值即未设置**：`harness.Getenv` 语义（trim 后为空 = 未设置）用于全部三个变量，所以 `OMO_CODING_AGENT_DIR="   "` 不会把 agent home 塌到 cwd。
- **trim + resolve**：`resolveDir`（`pi.go:289`）镜像 `node:path.resolve`——相对值以进程工作目录为锚点，结果 clean；`filepath.Abs` 失败时回落 `filepath.Clean` 而不是丢弃用户请求。
- **sentinel 按存在性判定**：`pathExists`（`pi.go:298`）镜像 `existsSync`，只认 `settings.json`；同目录下只有 `credentials.json` 时不算 engine 状态，走 standalone 回落（`TestRootsOMOSenpiSentinelDetection/directory without sentinel is not engine state`）。
- **不改变整组替换语义**：OMO 一档严格位于 built-in 分支内部，`PI_HOME`/`MYTOKEN_PI_DIRS` 仍在它之前 return，因此设了这两个变量的用户看不到 OMO/OMP root（`TestRootsOMOSenpiDoesNotChangeReplacementSemantics`）。
- **stable dedupe**：`PI_CODING_AGENT_DIR` 被 OMP 与 OMO 两档共用，当它（或与 OMP 同路径的 `OMO_CODING_AGENT_DIR`）命中时两层解析到同一个 agent 目录，去重后只留 OMP 的首次出现，root 数量不增加，provenance 仍标 `Env="PI_CODING_AGENT_DIR"`。
- **只发现 `*.jsonl` 且首行须是 session header / async record**：`piLogKind`（`pi.go:480`）已保证；`credentials.json`、`agent.db`、`settings.json`、`events.jsonl`（`type=event`）、`notes.txt` 都进不了 `Discover`。

## 严格 TDD 过程

1. **RED**：先写 `root_test.go` 的 OMO/senpi 用例（*未*改实现），运行
   `go test ./internal/harness/pi/ -run 'OMOSenpi|DiscoverOMOSenpi'` → **FAIL**：4 个顶层测试、12 个子测试失败，失败信息形如

   ```text
   root_test.go:572: roots lack "/var/folders/.../.senpi/agent/sessions":
     [{Path:.../.pi/agent/sessions Origin:builtin Env:} {Path:.../.omp/agent/sessions Origin:builtin Env:}]
   ```

   即现有 roots 只有 Pi+OMP 两层，完全没有 OMO/senpi。同一批里
   `TestRootsOMOSenpiEnvPriority/pi coding agent dir dedupes against the OMP root` 与
   `TestRootsOMOSenpiDoesNotChangeReplacementSemantics` 在 RED 下即通过——它们是"语义不变"的守卫，按设计不参与 RED。
2. **GREEN**：实现 `omoSenpiAgentDir` / `resolveDir` / `pathExists`、常量表，并把两处重复拼接抽成 `appendAgentSessions`；重跑全量 `./internal/harness/pi` → ok。
3. **REFACTOR**：消掉 OMP 分支里手写的 if/else provenance 拼装；补上 brief 点名要求但 RED 用例未覆盖的"`OMO_CODING_AGENT_DIR` 与 OMP 覆盖同路径"用例（`TestRootsOMOSenpiEnvPriority/same path as the OMP override dedupes`），以及 `TestRegistration` 的 root 数断言 2 → 3；覆盖率见下。

RED 用例清单（`root_test.go:566` 起，5 个顶层测试 / 20 个子测试）：

- `TestRootsOMOSenpiDefaultIsAddedOnceBeyondPiAndOMP`：默认 roots 恰好 3 个且 OMO root 在最后；`~/.senpi/agent` 与 `~/.senpi` 本身不是 root。
- `TestRootsOMOSenpiSentinelDetection`：canonical / legacy flat / canonical beats legacy / 无 sentinel 的目录不算 engine 状态（4 个子测试）。
- `TestRootsOMOSenpiEnvPriority`：OMO > SENPI > PI 的逐级压制与 provenance 标签、`PI_CODING_AGENT_DIR` 与 OMP 去重、OMO 与 OMP 同路径 stable dedupe、空/空白视作未设置、trim、相对路径按 cwd resolve（7 个子测试）。
- `TestRootsOMOSenpiDoesNotChangeReplacementSemantics`：`PI_HOME` 恰好 1 个 root 且不带 OMO/OMP；`MYTOKEN_PI_DIRS` 恰好 2 个且不带 OMO（2 个子测试）。
- `TestDefaultsDiscoverOMOSenpiRoots`：canonical home 经 `New()` + `Discover()` 只发现该 home 的那条 session 日志并成功 `Parse` 出 1 个事件，同树下的 `credentials.json`/`agent.db`/`events.jsonl`/`notes.txt` 都不是 source，且 standalone `~/.senpi` 里的日志**不被扫描**（不双计）；standalone 回落单独成例（2 个子测试）。

测试侧：`isolateHome`（`root_test.go:36`）的空白名单扩展为 `PI_HOME`、`PI_CODING_AGENT_DIR`、`PI_CONFIG_DIR`、`OMP_PROFILE`、`PI_PROFILE`、`XDG_DATA_HOME`、`MYTOKEN_PI_DIRS`、`OMO_CODING_AGENT_DIR`、`SENPI_CODING_AGENT_DIR`，并把 `HOME`/`USERPROFILE` 指向 `t.TempDir()`；全部 fixture 只用临时目录，不读真实 HOME、真实用户数据与凭据。`pi_test.go:787` 的 `TestRegistration` 断言由 2 改为 3，注释同步。

## 验证命令与结果

```console
$ gofmt -l internal/harness/pi/
(空)

$ go test ./internal/harness/pi/
ok  	github.com/zzstar101/mytoken/internal/harness/pi	0.460s

$ go test ./internal/harness/
ok  	github.com/zzstar101/mytoken/internal/harness	0.506s

$ go vet ./internal/harness/pi ./internal/harness
(无输出)

$ go test -race ./internal/harness/pi ./internal/harness
ok  	github.com/zzstar101/mytoken/internal/harness/pi	2.292s
ok  	github.com/zzstar101/mytoken/internal/harness	2.472s

$ go test ./... -count=1
(无失败包)

$ go build ./...
(成功)

$ go test ./internal/harness/pi/ -coverprofile=/tmp/pi_cover.out
ok  	github.com/zzstar101/mytoken/internal/harness/pi	0.580s	coverage: 87.1% of statements
```

新增/改动的函数覆盖率：`rootProvenance` 100%、`appendAgentSessions` 100%、`omoSenpiAgentDir` 100%、`pathExists` 100%、`dedupeRoots` 100%、`resolveDir` 66.7%（只有 `filepath.Abs` 报错时的防御性回落分支未覆盖；触发它需要删掉进程工作目录，代价与风险高于收益，故刻意不测）。

## 未关闭的门（schema / 环境风险）

1. **尚无脱敏真实 OMO/senpi 产物对拍**：官方源码 `senpi-session-provider.ts` 已证实与 Pi 同形（`type=session` header + `type=message` 条目），所以并非纯猜测；但仓库中的验证仍是按该官方 schema 生成的合成 fixture，**不能冒充真实日志验证**。若发行包与源码偏离，症状可能是静默 0 事件而不是报错。
2. **provenance conformance 的环境面未接线**：共享 `internal/harness/provenance_test.go:64` 的 `overrideEnvSurface` 目前只列了 `PI_HOME`；`PI_CODING_AGENT_DIR`/`OMO_CODING_AGENT_DIR`/`SENPI_CODING_AGENT_DIR` 尚**未**登记，按 brief 由 Lead 统一接线，本轮刻意未改共享测试。同时提醒：共享 harness 目前没有"读取未登记变量即失败"的反向断言，所以本轮读取新变量不会让既有共享测试变红；一旦引入该断言，必须把这三个变量补进白名单。
3. **`PI_CODING_AGENT_DIR` 被两层共用**：只设它时，OMO 一档解析到与 OMP 相同的 agent 目录并去重，最终 provenance 标签是 `PI_CODING_AGENT_DIR`（OMP 的首次出现）而不是 OMO 侧的变量名。这是"同一 home 只报一次"的正确取舍，但若未来需要按品牌区分归属，就要在 `harness.Root` 上增加多来源信息。
4. **`~/.omo` 的 sentinel 判定是存在性判定**：目录名叫 `.omo` 但没有 `settings.json` 时不算 engine 状态，会继续回落到 `~/.senpi/agent`——与官方 `existsSync` 语义一致，但意味着"用户手工建了 `~/.omo/agent` 却没生成 settings"时会看不到该目录。
5. **canonical 会压掉 standalone**：官方 resolver 只返回**一个** home，因此 `~/.omo/agent/settings.json` 存在时 `~/.senpi/agent/sessions` 不会被扫描。这是防双计的刻意行为；如果用户的两套安装各有独立日志，需要显式用 `MYTOKEN_PI_DIRS` 同时列出。
6. **未扫描 OMO `children` 与 event logs**：本轮的 OMO 一档只贡献 `<agentHome>/sessions`，OMO 的 `children`/`event` 产物既不在该目录下，也不被本 parser 展开——按 brief 这是为了避免与 Pi/OMP 双计；若将来确认 `children` 里有唯一的会话数据，需要单独评估，不要在 Pi parser 里顺手加目录。
7. **`PI_HOME` 语义未变**：它仍是"整组替换"，所以设了 `PI_HOME` 的 Pi 用户不会同时看到 OMP/OMO sessions。这是为保持既有行为而做的选择，不是遗漏；若希望并存，需要显式用 `MYTOKEN_PI_DIRS`。
