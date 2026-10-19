# Kimi Code 离线适配验证

## 结论与范围

MyToken 继续使用既有 `kimi` 身份，同时兼容两代本地格式：

- legacy Kimi CLI：`$KIMI_SHARE_DIR/sessions`，默认 `~/.kimi/sessions`；
- Kimi Code：`$KIMI_CODE_HOME/sessions`，默认 `~/.kimi-code/sessions`；
- `MYTOKEN_KIMI_DIRS` 显式替换整个根集合，重复根按规范路径去重并保留显式环境变量来源。

Kimi Code 会话从 `sessions/<workDirKey>/<sessionId>/agents/main/wire.jsonl` 与 `agents/<subagentId>/wire.jsonl` 发现。主 agent 使用会话 id；子 agent id 以父会话限定，避免不同会话中同名 agent 冲突，并保留 `ParentID`。

本批只有上游源码/schema 与合成 fixture 证据；没有读取真实 HOME、凭据或用户日志，也没有提交脱敏真实日志。因而这是**协议适配**，不是“真实日志验证”。

## 上游依据

- 数据位置：<https://github.com/MoonshotAI/kimi-code/blob/main/docs/en/configuration/data-locations.md>
- transcript usage schema：<https://github.com/MoonshotAI/kimi-code/blob/main/packages/transcript/src/contract/schema.ts>
- usage 聚合语义：<https://github.com/MoonshotAI/kimi-code/blob/main/packages/agent-core-v2/src/session/usage/usageAgentModel.ts>
- context projector：<https://github.com/MoonshotAI/kimi-code/blob/main/apps/vis/server/src/lib/context-projector.ts>
- usage scope 上游测试：<https://github.com/MoonshotAI/kimi-code/blob/main/packages/agent-core-v2/test/agent/usage/usage.test.ts>

上游实现表明每条 `usage.record` 表示一次请求；`usageScope` 的 `turn` 与 `session` 是请求来源标签，而不是同一次请求的明细/汇总两份副本。因此 parser 计入缺省、空串、`turn` 与 `session`，保守忽略未知字符串、显式 `null` 与其他非字符串值。

## 映射与防双计

Kimi Code `usage.record.usage` 映射如下：

| 上游字段 | MyToken |
| --- | --- |
| `inputOther` | `Tokens.Input` |
| `output` | `Tokens.Output` |
| `inputCacheRead` | `Tokens.CacheRead` |
| `inputCacheCreation` | `Tokens.CacheWrite` |
| `model` | `UsageEvent.Model` |
| epoch-ms `time` | `UsageEvent.Timestamp` |

只把各 wire 自己的 `usage.record` 当作 protocol 1.1 权威记录；父 wire 中重复子 agent 用量的 `subagent.completed.usage` 与 context loop 的 `step.end` 不计。带有效毫秒时间戳的字节级重复记录折叠为同一稳定 key；无时间戳的同形记录保留出现序号，避免把两次合法请求错误合并。legacy `StatusUpdate` 读法保持兼容。

仍有一项未验证协议不变量：仓库证据没有证明当前发行版的同一条 wire 永远不会为同一次请求同时写 legacy `StatusUpdate` 与 protocol 1.1 `usage.record`。两条路径使用不同 dedup 命名空间；若真实日志混写，可能双计。必须以当前发行版脱敏 `wire.jsonl` 对拍后才能关闭该发布门槛。

## 隐私与增量

- protocol 1.1 仅读取 usage、model、时间、会话/agent 关系及 `config.update.cwd` 的 basename；不输出提示词、回复正文、工具参数或完整 cwd。
- legacy 行为保持原有隐私契约；标题仍受既有长度与文本规则限制。
- byte cursor 只推进到完整 JSONL 行；尾部半行在补全后重读。
- truncate/replace、append、重复扫描与相同记录的稳定 dedup 均有合成测试覆盖。
- 不读取凭据文件或 Kimi Code 配置中的密钥。

## 合成验证覆盖

`internal/harness/kimi/protocol11_test.go` 与 `internal/harness/kimi/testdata/conformance/kimi/seed/protocol11/` 覆盖：

- golden、incremental、cursor-stable、idempotent、privacy、discover-missing；
- legacy 与 Kimi Code roots/provenance；
- 主 agent、子 agent、父子关系与目录 basename；
- `turn`/`session`/缺省/空串/未知/null scope；
- epoch-ms 时间、尾部半行、truncate/replace；
- 时间戳重复折叠与无时间戳同形请求区分；
- 不计 `subagent.completed.usage` 与非权威 step usage。

最终验收命令由 `docs/harness-verification/v04x-harness-matrix.md` 统一记录；该矩阵同时保留匿名化真实日志门槛。
