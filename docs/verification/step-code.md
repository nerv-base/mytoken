# Step Code 离线适配验证（internal/harness/stepcode）

日期：2026-10-16 10:35 CST；环境：go1.27.1 darwin/arm64；工作目录 `/Users/zzstar/Developer/Programs/MyToken!!!!!`；分支 `feat/0.4x-harnesses`；HEAD `a9684c7`。**未提交**（工作区有并发改动：不提交、不 reset、不覆盖他人内容）。

写入范围（只此三处）：

```console
$ git status --porcelain -- internal/harness/pi/pi.go internal/harness/sessionlog internal/harness/stepcode docs/verification/step-code.md
 M internal/harness/pi/pi.go
?? internal/harness/sessionlog/
?? internal/harness/stepcode/
```

未改动 `internal/model`、`internal/app`、`README`、`ROADMAP`、规划文件（`task_plan.md`/`findings.md`/`progress.md`）以及并发工作流正在写的 `internal/harness/kimi/**`、`internal/harness/mimocode/**`、`internal/harness/minimax/**`、`internal/harness/zcode/**`、`internal/harness/opencode/**`、`rooted_test.go`、`provenance_test.go`、其他 `docs/verification/*`。

`internal/harness/pi/pi.go` 的 `git diff` 同时包含**本轮之前就存在的 OMP 改动**（同一工作区的并行工作）与本轮的抽取重构，两者都完整保留：`internal/harness/pi/root_test.go` 的全部 OMP 用例本轮仍全绿（见 GREEN）。

## 结论与范围

新增离线 harness 包 `internal/harness/stepcode`，只读解析 Step Code 的本地会话日志 `~/.stepcode/agent/sessions/**/*.jsonl`（Pi-derived schema）。它保持**独立来源身份** `model.Harness("step-code")`（`internal/harness/stepcode/stepcode.go:108`）与**独立事件命名空间** `step-code:`（`stepcode.go:113`），因此同一段对话即使同时被 Pi 与 Step Code 记录，也是两个 source、两套 dedup key，永不碰撞（`stepcode_test.go` 的 `TestNoSharedStateWithPi`）。

**复用而非复制**（本次架构要求）：Step 是 Pi 派生 schema，但不得复制整套 Pi parser。可复用的**深层模块**抽出为 `internal/harness/sessionlog`（546 行）：字节游标（fingerprint / 完整行读取 / rewind）、发现遍历、JSON 标量解码（`Num`/`Fnum`/时间戳）、纯文本与标题判据，以及 **usage 账本**（`Usage`/`Cost`/`Events`/`Boundary` 状态）。Pi 迁到该模块后从 1256 行降到 925 行，**行为不变**——Pi 的 conformance golden `internal/harness/pi/testdata/conformance/pi/golden.json` 逐字节未变（`git status` 无该文件），`internal/harness/pi` 全部用例含 OMP 根用例全绿。`stepcode.go`（613 行）只剩 Step 特有部分：根解析、header 判据、计数规则、父子关系、隐私收紧。

**做**：默认根 `~/.stepcode/agent/sessions`；`MYTOKEN_STEP_CODE_DIRS` 整组替换、`STEP_CODING_AGENT_SESSION_DIR`、`STEP_CODING_AGENT_DIR` 的官方优先级与 `~/` 展开；坏列表硬错误不回退；只发现「首行是合法 session header」的 `*.jsonl`；assistant message 与 compaction / branch_summary 各自真正的模型调用 usage 各计一次；token/cache/reasoning/cost/model/provider/session/timestamp 精确映射；父 session 关系（fork 的 `parentSession` 绝对路径 → 父 session id）；增量对 append、partial final line、truncate/replace 稳健且 dedup 稳定；合成 fixture + conformance 六项与额外用例；严格 RED→GREEN→REFACTOR。

**不做**（没有证据就不臆造、不越权）：不解析 `config.toml`/`settings.json`/`auth.json`/`models.json`；不读 telemetry、日志与凭据（Discover 只按 `*.jsonl` 扩展名 + 首行 header 判定，非 session 的文件除首行外一个字节都不解析，且从不落库）；不实现 `--session-dir` 与 `settings.json sessionDir`（前者是 CLI flag，后者属于「不解析 config」）；不注册、不改 `internal/model` 的共享常量与 `internal/app` 的注册（见「未关闭的门」）；不推断 BaseURL/GitBranch/Boundary 之外的任何 Git 信息。

## 上游依据（官方一手，只读 `/tmp/step-code.U02A7A`）

- `packages/coding-agent/src/core/session-manager.ts:30` `export const CURRENT_SESSION_VERSION = 3;`
- 同文件 `:32-39` `SessionHeader`：`type:"session"`、`version?`、`id`、`timestamp`、`cwd`、`parentSession?`；`:41-47` `SessionEntryBase`（`type`/`id`/`parentId`/`timestamp`）；`:53-56` `SessionMessageEntry`（`type:"message"` + `message:AgentMessage`）；`:69-80` `CompactionEntry`（自带顶层 `usage?:Usage`）；`:82-91` `BranchSummaryEntry`（自带顶层 `usage?:Usage`）。其余 entry 类型（`thinking_level_change`/`model_change`/`custom`/`custom_message`/`label`/`session_info`）都不带模型调用 usage。
- `packages/coding-agent/src/config.ts:197-198` `ENV_AGENT_DIR="STEP_CODING_AGENT_DIR"`、`ENV_SESSION_DIR="STEP_CODING_AGENT_SESSION_DIR"`；`:211-217` `getAgentDir()` = 变量 trim + tilde 展开，否则 `join(homedir(), CONFIG_DIR_NAME, "agent")`（Step 入口的 `CONFIG_DIR_NAME = ".stepcode"`）；`:252-254` `getSessionsDir() = join(getAgentDir(), "sessions")` ⟹ 默认 `~/.stepcode/agent/sessions`。
- `packages/coding-agent/src/step/environment.ts:59-62` `resolveStepSessionDir = $STEP_CODING_AGENT_SESSION_DIR?.trim() || join(resolveStepAgentDir(env),"sessions")`——session 根变量**直接**覆盖 agent 根。
- `packages/coding-agent/src/main.ts:1017-1030` 官方优先级：`--session-dir` > `STEP_CODING_AGENT_SESSION_DIR` > `settings.json sessionDir` > `join(agentDir,"sessions")`。
- `packages/providers/src/types.ts:320-337` `Usage`：`input`/`output`/`cacheRead`/`cacheWrite`/`reasoning?`/`totalTokens`/`cost{...total}`；`:325-332` 对 `reasoning` 的注释是「**subset of `output`: `output` already includes these tokens**」。`:365-392` `AssistantMessage`（`provider`/`model`/`responseModel?`/`responseId?`/`usage`/`timestamp` 毫秒）；`:394+` `ToolResultMessage` 也带 `usage?`（"Nested LLM work performed by the tool"）。
- `packages/providers/src/api/openai-completions.ts:1528-1537` `outputTokens = rawUsage.completion_tokens`，注释「OpenAI completion_tokens already includes reasoning_tokens」；`packages/providers/src/api/anthropic-messages.ts:700-706` thinking 来自 `output_tokens_details`（"a subset of output_tokens"）。⟹ Step 的 `reasoning` 含在 `output` 内。
- `packages/coding-agent/src/core/usage-totals.ts`：assistant `message` 的 usage 按 `provider/model` 分桶；`toolResult` 的 usage 与 `(branch_summary|compaction).usage` 一起记到 "Tools/summaries"；`custom` + `customType==="step-auto-clm-usage"` 的 `data.usage` 也记到 "Tools/summaries"（展示/聚合口径）。`packages/coding-agent/src/core/agent-session.ts:3997-4045` `getSessionStats()` 是「对全部 entry 求和」的权威口径。
- `packages/coding-agent/docs/session-format.md`：文件位置 `~/.stepcode/agent/sessions/--<cwd 的 / 换成 ->--/<timestamp>_<uuid>.jsonl`；entry 通过 `id`/`parentId` 构成树（首条 `parentId: null`）；`compaction`/`branch_summary` 的 `usage` 是「生成摘要那次 LLM 调用的用量，计入会话 token 与 cost 总额」。

## 关键取值决策（都写进代码注释）

1. **reasoning 含在 output 内**：`model.Tokens` 要求 Output 排除 reasoning（`internal/model/model.go` 的注释「if a log's output already includes reasoning, Output must exclude it」），而 Step 的 `Usage.output` 已包含 `reasoning`。所以 Step 侧 `Tokens` 取 `Output = output - reasoning`（`stepcode.go:458`、`:489`，`sessionlog.ReasoningInOutput`）。这是**两种 schema 的差异开关**：Pi 侧仍用 `sessionlog.ReasoningSeparate`（`pi.go` 原样），`pi_test.go` 的 `TestEventFields`（`Output:20, Reasoning:30`）与 pi golden 均未变。fixture 里 assistant 记录 `output:40, reasoning:15` → 计 `Output:25, Reasoning:15`。
2. **只计三类真实模型调用**：`assistant` message（`stepcode.go:435`）、`compaction.usage`（`:403-409`）、`branch_summary.usage`（`:410-413`），各计一次。`toolResult.usage`（产品把该轮 usage 复制一份做展示）与 `custom(step-auto-clm-usage).data.usage`（产品自己的聚合/展示记录）**明确排除**——这正是「忽略聚合/展示记录」的字面要求，避免双计；两者作为**范围决策**记录于此（官方 `getSessionStats()` 会另计它们，见「未关闭的门」）。未知 `customType` 一律忽略。
3. **边界标记属于 compaction 之后的第一个请求**：`compaction` 本身是「发生在压缩点」的调用，不带边界；`MarkPending(BoundaryCompact)` 在它的 usage 入账**之后**执行（`stepcode.go:403-409`），由下一个请求 claim；无法建模的 compaction 记录仍会留下标记（`:372-374`）。
4. **隐私最小化**：`ProjectPath` 只存 `cwd` 的 basename（`stepcode.go:385-389`、`baseName()` `:530-535`，跨平台切 `/` 与 `\`）；fork 的 `parentSession` 绝对路径只用来解析出父 session id（`:390-395`）；prompt/content/tool payload/摘要/图片一律解码后丢弃。唯一例外是 `Title`（`harness.SessionMeta` 的设计如此）：优先 `session_info.name`（`:415-418`），其次首条合格 user 消息。

## 根解析与发现

优先级（`stepcode.go:178-198`，与官方一致）：`MYTOKEN_STEP_CODE_DIRS`（整组替换，`harness.SplitEnvList`，非空但无目录 → `*harness.EnvListError` 硬错误，**绝不回退**到默认）→ `STEP_CODING_AGENT_SESSION_DIR`（session 根本身）→ `STEP_CODING_AGENT_DIR/sessions` → `~/.stepcode/agent/sessions`。两个 Step 变量支持 `~/` 展开（`expandHome` `:201-212`），与官方 `expandTildePath` 对齐。

发现（`stepLogKind` `:269-286`）：只认 `*.jsonl` 且首行 JSON 满足 `type=="session" && id!="" && version<=3`（`CURRENT_SESSION_VERSION`；`version` 缺省即 v1）。非 session header 的文件（调试 sink、artifact mirror、凭据 JSONL）不发现；`auth.json`/`models.json`/`config.toml` 连扩展名都不匹配，从不打开。

## RED 证据（先写测试、后写实现）

在本轮实现之前，`stepcode` 的测试与 conformance fixture 已经写好（`conformance_test.go`、`roots_test.go`、`stepcode_test.go` 与 `testdata/conformance/step-code/**`），命令直接编译失败：

```console
$ go test ./internal/harness/stepcode
internal/harness/stepcode/conformance_test.go:30:19: undefined: HarnessID
internal/harness/stepcode/conformance_test.go:31:54: undefined: NewWithRoots
internal/harness/stepcode/roots_test.go:15:29: undefined: stepDirsEnv
internal/harness/stepcode/roots_test.go:15:42: undefined: stepSessionDirEnv
internal/harness/stepcode/roots_test.go:15:61: undefined: stepAgentDirEnv
internal/harness/stepcode/roots_test.go:27:16: undefined: New
internal/harness/stepcode/roots_test.go:38:12: undefined: New
internal/harness/stepcode/roots_test.go:53:12: undefined: stepSessionDirEnv
internal/harness/stepcode/roots_test.go:54:12: undefined: stepAgentDirEnv
internal/harness/stepcode/roots_test.go:55:12: undefined: stepDirsEnv
FAIL	github.com/zzstar101/mytoken/internal/harness/stepcode [build failed]
```

Pi 侧的 RED 由「抽取前先跑一遍 baseline」提供：`go test ./internal/harness/pi` 为 `ok (cached)`，重构后必须仍为 `ok` 且 golden 不变。实现过程中被测试抓出并修掉的真实缺陷有两处（都在 GREEN 之前）：

- compaction 事件被自己的边界标记「吃掉」（应先入账、后 `MarkPending`）→ `TestParseCountsOnlyRealModelCalls` 报 `compaction boundary = "compact"`。
- 小文件（小于 4096 字节 fingerprint 窗口）追加时 fingerprint 变化会整文件重读，测试原先断言「0 个新事件」不成立。真正的增量契约是「偏移停在上一个完整行、partial 行不产生新 dedup key」，用例已改为断言这一点（`stepcode_test.go` 注释写明了原因）。

## GREEN 证据

```console
$ gofmt -l internal/harness/pi internal/harness/sessionlog internal/harness/stepcode
（无输出）
$ go build ./...
（成功）
$ go vet ./internal/harness/pi ./internal/harness/stepcode
（成功）
$ go test -count=1 ./internal/harness/pi ./internal/harness/stepcode
ok  	github.com/zzstar101/mytoken/internal/harness/pi	0.453s
ok  	github.com/zzstar101/mytoken/internal/harness/stepcode	0.197s
```

`internal/harness/pi` 全绿（33 个顶层用例），其中包含并发工作流留下的全部 OMP 用例：`TestRootsOMPBuiltinIsDiscoveredBeyondPiDefault`、`TestRootsAgentDirOverride`、`TestRootsAgentDirOverrideIsDirectoryExistenceAgnostic`、`TestRootsXDGDataHomeGatedOnOMPDirectory`、`TestRootsXDGDoesNotReplaceExplicitAgentDir`、`TestRootsProfileAgentDir`、`TestRootsProfilePrecedence`、`TestRootsProfileRejectsPathTraversal`、`TestRootsMyTokenPiDirsOverride`、`TestRootsConfigDirOverride`、`TestRootsBlankConfigDirIsUnset`、`TestRootsDedupesOverlappingRoots`、`TestDedupeRoots`、`TestRootsMyTokenPiDirsRejectsBlankList`、`TestDefaultsDiscoverOMPAndPiRootsTogether`、`TestDefaultsSkipAbsentOMPRoots`、`TestRequestBoundarySignals`。Pi 原有 `TestParseTotals`（原始/去重 token 总量）、`TestEventFields`（reasoning 与 output 分离）、`TestAsyncRunTotals` 等全部未回归，`testdata/conformance/pi/golden.json` 未被改动。

`internal/harness/stepcode` 15 个顶层用例 + conformance 六项全绿：

```console
$ go test -count=1 -v ./internal/harness/stepcode
--- PASS: TestConformance (0.03s)
    --- PASS: TestConformance/golden
    --- PASS: TestConformance/incremental
    --- PASS: TestConformance/cursor-stable
    --- PASS: TestConformance/idempotent
    --- PASS: TestConformance/privacy
    --- PASS: TestConformance/discover-missing
--- PASS: TestDefaultRoot            # 默认 ~/.stepcode/agent/sessions
--- PASS: TestEnvRootPriority        # MYTOKEN_STEP_CODE_DIRS > SESSION_DIR > AGENT_DIR/sessions，且 ~ 展开
--- PASS: TestBadDirsListIsHardError # 坏列表 = *harness.EnvListError，无回退
--- PASS: TestExplicitRoots
--- PASS: TestHarnessIdentity        # Harness()=="step-code"、注册表可取、dedup 前缀独立
--- PASS: TestParseCountsOnlyRealModelCalls  # 三类模型调用各一次；toolResult/auto-clm 不计；compaction 边界
--- PASS: TestParseSessionTree       # fork parentSession → ParentID；分支树上每个 usage 一次
--- PASS: TestPrivacyNoPathsNoPayload # 不出现 SENTINEL 文本、不出现任何绝对路径
--- PASS: TestDiscoverOnlySessionHeaders # auth.json/models.json/debug.jsonl 不发现
--- PASS: TestIncrementalAppendAndPartialLine
--- PASS: TestTruncateAndReplaceRewindsCursor
--- PASS: TestDiscoverMissingRootAndBadFile
--- PASS: TestNoSharedStateWithPi
```

Pi 的 conformance `privacy` 检查由 `harnesstest` 统一执行：fixture 里的 `SENTINEL_PRIVATE_TEXT` 出现在 user/assistant/toolResult/compaction/branch_summary/debug.jsonl/auth.json/models.json，全部未泄漏到 Batch。

**fixture 声明**：`internal/harness/stepcode/testdata/conformance/step-code/**`（含 `golden.json`）是**手写合成日志**，按上面的官方 schema 逐字段构造，不含任何真实用户日志、真实路径或真实凭据；`cwd` 是 `/home/user/fixture-alpha`，敏感文件内容统一是 `SENTINEL_PRIVATE_TEXT`。它不声称代表真实运行日志（真实日志 gate 见下）。

## 未关闭的门

1. **真实日志 gate（未做，无法在本轮完成）**：本机没有 Step Code 的真实会话日志语料，全部验证建立在手写合成 fixture 上。真实 `~/.stepcode/agent/sessions` 语料到手后需要复核：(a) v1/v2 旧日志（`version` 缺省/2）在真实文件上的字段形态；(b) 真实 compaction 记录是否还带 `retainedTail` 等新字段而 usage 仍在顶层；(c) `session_info.name` 的真实取值与 `Title` 期望；(d) 真实 `cost` 是否为对象（当前两种形状都接受）。
2. **计数口径的第三类（有意的范围决策）**：官方 `getSessionStats()` 还把 `toolResult.usage` 与 `custom(step-auto-clm-usage).data.usage` 计入会话总额；本任务明确要求「只计 assistant message usage 与 compaction/branch_summary 各自真正的模型调用 usage，忽略聚合/展示记录」，故二者被排除。若产品口径要对齐官方总额，只需在 `consumeMessage`/`consume` 里增加一个分支，开关是显式的。
3. **注册与 `model.Known`（Lead 的接线步骤）**：`internal/model/model.go` 的 `DisplayName()`/`Known()` 不含 `"step-code"`，而本任务不允许改 `internal/model` 与 `internal/app`，因此 `model.Harness("step-code").Known()` 目前为 `false`。`stepcode` 包已通过 `init(){ harness.Register(New()) }`（`stepcode.go:257`），并在 `stepcode.go:250-254` 用编译期断言钉住 `harness.Parser`/`harness.Rooted`/`harness.Provenance` 三个接口进入注册表，但要在真实二进制里出现在扫描列表中，仍需 Lead 在 `internal/model` 增加常量/展示名、在 `internal/app` 增加空白导入。

## 风险

- **fingerprint 窗口**：会话文件小于 4096 字节时，任何追加都会改变首 4096 字节指纹而触发整文件重读（Pi 的既有语义，故意保持一致）。正确性不受影响（dedup key 稳定、store 折叠重放），只是小文件在增长期的重复解析成本；真实会话通常很快超过该窗口。
- **`Model` 继承**：compaction / branch_summary 记录不带 model，只能继承「此前最后一次请求」的 model/provider；一条只有 compaction 的会话报告 `"unknown"`（与 Pi 的 `TestCompactionOnlySessionHasUnknownModel` 同类语义）。
- **多根去重**：`MYTOKEN_STEP_CODE_DIRS` 若包含互相包含的目录，会重复发现同一文件；store 按 dedup key 折叠，故不双计，但会产生重复 Parse。Pi 侧用 `dedupeRoots` 处理重叠根，Step 侧当前只在多根间按最长根归属（`sessionlog.RootFor`）。

## Lead 集成更新（2026-10-16）

共享接线已完成：`model.StepCode`、app blank import、32-harness registry/provenance 断言、HARNESS/README 与 GUI 名称/颜色均已同步。本文原“注册与 model.Known”条目保留为实现工作流当时的审计快照，不再是当前缺口；脱敏真实日志 gate 仍保持未关闭。
