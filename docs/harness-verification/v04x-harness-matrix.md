# 0.4.x Harness 离线适配与证据矩阵

日期：2026-10-16；分支：`feat/0.4x-harnesses`；基线：v0.4.1 (`a9684c7`)。

## 证据等级

- **协议适配**：有官方源码或文档说明持久化路径和字段，仓库以合成 fixture 覆盖 golden、增量、游标稳定、幂等、隐私和缺失目录。
- **脱敏真实日志验证**：由真实安装产生、经检查并脱敏后提交的 fixture。只有满足这一项才称“真实验证”。
- **研究结论**：尚无足够稳定的离线 schema，不添加会静默误计的猜测性 parser；若只能在线取得用量，移到 0.5。

合成 fixture 与 conformance 通过不等于真实日志验证。本批不读取开发者机器上的用户日志、凭据或 harness 配置。

## 本批矩阵

| 产品 | 0.4.x 处理 | 身份 | 离线证据 | 真实日志状态 |
| --- | --- | --- | --- | --- |
| Kimi Code | 扩展现有 Kimi parser，读取 `~/.kimi-code/.../agents/*/wire.jsonl`；protocol 1.1 顶层记录计 `usage.record`，legacy envelope 计 `StatusUpdate` | `kimi` | 上游数据目录与协议 schema + 两种记录形状的合成 conformance fixture（`testdata/conformance/kimi/seed/protocol11` 与 legacy 目录） | 未提供；保留发布门槛 |
| Oh My Pi (OMP) | 扩展 Pi roots；只读 session JSONL，不计辅助 `model_usage` | `pi` | 官方目录与 session-entry 源码 + 合成 tests | 未提供；保留发布门槛 |
| Oh My OpenAgent (OMO)/senpi | OpenCode plugin 复用 OpenCode；senpi 复用 Pi roots；不扫描重复的编排 event logs | `opencode` / `pi` | 官方 agent-home 与 transcript provider 源码 + 合成 tests | 未提供；保留发布门槛 |
| ZCode | 独立 query-only SQLite parser；动态探测可选 cache 列，只计完成记录 | `zcode` | 官方产品资料 + 第三方公开路径/schema 交叉证据 + 合成 SQLite conformance | 未提供；保留发布门槛 |
| MiniMax Code | 独立 query-only SQLite parser，兼容 `.minimax`/`.mavis`；无法离线发现的 profile 由显式 roots 指定 | `minimax-code` | 官方 data-dir、DB client、usage repository 源码 + 合成 SQLite conformance | 未提供；保留发布门槛 |
| MiMo Code | 独立 query-only SQLite parser；读取 `mimocode*.db` 的 message JSON usage，不复用 OpenCode 文件名假设 | `mimo-code` | 官方 storage/session schema 源码 + 合成 SQLite conformance | 未提供；保留发布门槛 |
| Step Code | 复用抽取后的 Pi JSONL 解析深模块，但保留独立 root、dedup 与产品身份 | `step-code` | 官方仓库 README、Pi-compatible session manager + 合成 tests | 未提供；保留发布门槛 |
| Trae | 不新增 parser | — | 官方文档只确认产品数据目录，未公开稳定 token/cache/model usage ledger | 研究结论；离线 schema/脱敏日志到位前延后，在线用量归 0.5 |
| Qoder | 不新增 parser | — | 官方文档未公开稳定离线 usage ledger | 研究结论；离线 schema/脱敏日志到位前延后，在线用量归 0.5 |

## 防双计与隐私边界

1. Kimi Code 父 wire 的 `subagent.completed.usage` 会与 child wire 重复；只认各 wire 自己的权威计量记录（protocol 1.1 是 `usage.record`，legacy envelope 是 `StatusUpdate`，两种形状并存的协议前提见第 6 条）。上游 `usage.record` 就是一次请求的权威计量：`packages/agent-core-v2/src/session/usage/usageAgentModel.ts:27-43` 对每条 `UsageRecord` 无条件累加进 `byModel`，`record()` 仅按 `source.type` 把这一次请求标成 `turn` 或 `session`，二者不是同一请求的累计视图与逐 turn 视图；`apps/vis/server/src/lib/context-projector.ts:393-401` 对两种 scope 都把 `usage.record` 计入 cumulative `byModel`（缺省 scope 默认 `session`）；`packages/agent-core-v2/test/agent/usage/usage.test.ts:239-275` 证明没有 turn source 的正常真实请求写 `usageScope=session`，并与 turn 记录一起回放进 totals。因此本 parser 对上游定义的 scope（`turn`、`session`，以及字段缺省/空串的兼容形态）逐条计入：只计 `turn` 会静默漏掉所有非 turn 请求；只对无法落位的值（未知字符串或非字符串 scope）保守忽略，且不消耗 dedup 计数。每条 `usage.record` 各有独立 dedup key，turn 与 session 记录既不合并也不互相双计。依据为上游源码 schema（`usageOps.ts`、`wire-manifest.d.ts`）、上游官方 fixture 的记录形状与本仓库合成 conformance fixture；本仓库未导入官方 fixture，也未使用脱敏真实日志。这条只覆盖 `usage.record` 内部的 scope 语义；它与 legacy `StatusUpdate` 两条读法并存的防双计前提见第 6 条。
2. OMP/Step/senpi 只读 assistant message 的 usage；不把汇总 `model_usage`、children 结果或 event log 当成第二次请求。
3. MiMo 只从 assistant message 的 usage 字段构造事件；不把会话汇总/part 文本当成请求。
4. SQLite adapter 必须以 read-only/query-only 打开，不创建 WAL、journal、migration 或 sidecar；仅选取计量字段，不读取凭据、提示词、回复正文或工具参数。
5. 项目路径只保留安全标签（例如 basename），标题只来自既有 parser 明确允许的元数据；测试不得包含真实 HOME、API key 或完整对话。
6. Kimi 防双计的**协议前提是两种记录形状并存**，不是单一来源：上游 `MoonshotAI/kimi-code` 当前 direct-op fixture `apps/vis/server/test/fixtures/sessions/sample-main/agents/main/wire.jsonl`（顶层 `type`）把用量写在 `usage.record`；其 legacy migration fixtures `packages/migration-legacy/test/fixtures/**/wire.jsonl`（`message.type`）把用量写在 `StatusUpdate`。本仓库的 `internal/harness/kimi/testdata/conformance/kimi/seed/protocol11/**` 与既有 legacy conformance fixture 都是相应形状的**合成测试数据**，不是官方 fixture 或脱敏真实日志。parser 对同一份 wire 实现这两条读法，唯一**故意不读**的重复用量在 `context.append_loop_event` 的 `event.type=="step.end"` 里——该记录类型根本不是用量来源，所以从不进入计数；legacy `StatusUpdate` 路径保持完整兼容（`internal/harness/kimi/kimi.go` 的 `handleStatus`、`handleUsageRecord`、`usageKey`）。仓库**没有证明**同一根 wire 会为同一次请求同时写入两种权威记录：两条路径的 dedup key 命名空间不同（`StatusUpdate` 用 `message_id`，缺失时用 wire 内记录序号；`usage.record` 用 `u`+原始行 sha1，无时间戳的记录再加 `#n` 区分同形记录），跨类型重复既不会被去重合并，也没有互斥 gate。因此「同一文件不混用两种权威记录」在本批是**未验证的协议不变量/风险**（依据仅为 fixture 形状与上游 schema），列入下面的发布门槛；在拿到当前发行版的脱敏 `wire.jsonl` 对拍之前，不声称已有防双计 gate。

## 仍未关闭的发布门槛

- 新增/扩展 adapter 都需要当前发行版的匿名化真实日志对拍；本批只有官方 schema 与合成 fixture，不勾选“真实日志验证”。
- Kimi 的具体未验证项：同一 wire 不会为同一次请求同时写 `usage.record` 与 legacy `StatusUpdate`（第 6 条）。若真实日志混用，现有 parser 会双计——两条路径的 dedup key 不同且没有互斥 gate。需用当前发行版的脱敏 `wire.jsonl` 确认后才关闭此项。
- Amp、Kiro、Zed、Antigravity、Codebuff、Mux、Hermes、OpenClaw 仍只有 conformance fixture 或条件启用的本机 `MYTOKEN_REALDATA=1` 测试，仓库没有可审计的匿名化真实 fixture。
- Trae/Qoder 若未来只提供在线账户用量，应在 0.5 走明确授权的网络功能，不在 0.4.x 离线扫描器里读取凭据或猜测内部数据库。

## 上游依据

- Kimi Code：<https://github.com/MoonshotAI/kimi-code>
- OMP：<https://github.com/can1357/oh-my-pi>
- OMO：<https://github.com/code-yeongyu/oh-my-openagent>
- ZCode：<https://github.com/zai-org/ZCode>；公开 schema 交叉证据：<https://ccusage.com/guide/zcode/>
- MiniMax Code：<https://github.com/MiniMax-AI/minimax-code>
- MiMo Code：<https://github.com/XiaomiMiMo/MiMo-Code>
- Step Code：<https://github.com/stepfun-ai/Step-Code>
- Trae：<https://docs.trae.ai/>、<https://docs.trae.cn/>
- Qoder：<https://docs.qoder.com/>

## 最终验收命令

在所有 adapter、注册接线与文档修改稳定后运行并记录新鲜结果：

```console
gofmt -w <本批 Go 文件>
go test -count=1 ./...
go vet ./...
go test -race ./internal/harness/... ./internal/app ./internal/model
go build ./...
git diff --check
```
