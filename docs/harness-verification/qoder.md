# Qoder（CN）本地用量调研（未新增 internal/harness/qoder*）

日期：2026-10-19 23:55 CST；环境：go1.27.1 darwin/arm64；工作目录 `/Users/zzstar/Developer/Programs/MyToken!!!!!`；分支 `main`；HEAD `167751c`。**未提交**。本轮写入范围只有本文件。

## 结论与范围

**不新增 harness，不新增 `model.Harness` 常量。** Qoder CN 的桌面端 transcript 是明文 JSONL，确实有逐轮 `message.usage`，也有 model、时间戳、session id、conversation id；但官方网关下 `input_tokens` / `output_tokens` / `cache_read_input_tokens` / `cache_creation_input_tokens` **恒为 0**，只有 `credits` / `original_credits` / `context_usage_ratio` 是真值。credits 不能换算成 USD，ratio×contextWindow 是估算而不是实测 token；按任务要求不臆造，因此不实现只能输出 0 token 的解析器。

**做**：只读解析 `~/.qoder-cn/projects/**/*.jsonl`；核对 `~/.qoder-cn/logs` 的 segment/runs；核对 Qoder 自带的 `qoder-credits-inspector` 插件对 usage 的官方口径；核对 `~/.qoder-credits-proxy` 是否存在。

**不做**：不把 `credits` 写成 `CostUSD`（`model.Bill` 规定 credits 不换算 USD），不用 ratio×window 冒充 Input token，不读 `.auth`/token，不调用 Qoder 远程接口。

## 本机真实数据

数据根：`~/.qoder-cn`。`~/Library/Application Support/com.qodercn.app.stable` 只有 Electron 缓存与一个空的 turn projection 库（`pending_turn_projections`、`active_turn_projection_items` 均 0 行），不是用量账本。

桌面端 rich transcript：`~/.qoder-cn/projects/-Users-zzstar-Developer-Programs-mygo/17dffb46-6d59-4959-abbc-4bf46218f914.jsonl`

| 指标 | 实测 |
| --- | --- |
| rich transcript 文件数 | **1**（另有 2 个只有 `memory/` 的项目目录，无 JSONL） |
| 总行数 | 126 |
| `type=assistant` | 40 |
| 带 `message.usage` 的 assistant | **16**（即 16 次计费往返） |
| 会话数 / session id | 1 / `17dffb46-6d59-4959-abbc-4bf46218f914` |
| model | 全部 `qfmodel`（`runtime-config.model` 也是 `qfmodel`） |
| `input_tokens` | 16/16 = 0 |
| `output_tokens` | 16/16 = 0 |
| `cache_read_input_tokens` | 16/16 = 0 |
| `cache_creation_input_tokens` | 16/16 = 0 |
| `credits` 合计 | **3.5747536000000006**（单条 0.0988–0.8279） |
| `billable` | 16/16 = false |
| `context_usage_ratio` | 约 0.147–0.159（单条） |
| `request_id` | 16 条均有 UUID |

单条脱敏样例（不含正文/凭据）：

```json
{"timestamp":"2026-10-16T01:21:27.046Z","sessionId":"17dffb46-6d59-4959-abbc-4bf46218f914","model":"qfmodel","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"credits":0.827912756,"request_id":"7c9fca3b-...","context_usage_ratio":0.14689}}
```

会话标题（首条 user 文本，仅用于核对，不进入任何聚合）：`你看一下这个框架，说说你的理解`；cwd `~/Developer/Programs/mygo`。

## 官方插件的口径（决定性证据）

本机安装的 `qoder-credits-inspector` 2.7.4 就是为"token & credits 消耗"设计的，其 `scripts/lib/transcript.mjs` 头注释与 `SKILL.md` 明确写着：

- 两类客户端：桌面 rich `projects/<slug>/<sessionId>.jsonl`；IDE lite `projects/<slug>/transcript/<sessionId>.jsonl`。
- 桌面 rich：`credits` / `original_credits` / `context_usage_ratio` 是**真值**；`input_tokens` / `output_tokens` **恒为 0**（作者称在整个库中没见过非 0）。
- IDE lite：`message.usage` 完全不存在，billing/context 本地不可恢复。
- 只有用户自配 **BYOK / OpenAI 兼容端点**时，`input_tokens > 0` 才可能出现；官方网关走的是"字段齐全但全 0 的壳 usage"。
- 插件自己的 token 真值优先级：`proxy.promptTokens > usage.input_tokens > ratio×contextWindow`；`ratio×window` 被标为 `transcript-ratio`，是推导值，不是实测。
- 插件只有在用户配置了本地透明代理时才写 `~/.qoder-credits-proxy/usage.jsonl`（provider 实测 prompt/completion tokens）；本机**没有**该目录/文件。

<https://docs.qoder.com/cli/sessions>（CLI session 文档，未给出本地 token 字段）

`~/.qoder-cn/logs/runs/*/qodercli.log` 与 `logs/sessions/**/segments/*.jsonl` 交叉核对：

- `model.response.completed ... input_tokens=0 output_tokens=0 ...`
- `turn.finished ... input_tokens=0 output_tokens=0 cache_read_input_tokens=0 cache_creation...`
- segment 类型只有 `model.request.started`、`model.response.completed`、`turn.*`、`tool.*`、`hook.*` 等，没有 per-block token payload；`get_context_usage` 只返回 ratio/context。

## 决策

| 决策 | 依据 |
| --- | --- |
| 不新增 `model.Harness` / parser | 16/16 条 token 全 0；不能输出真实 token |
| 不把 `credits` 写成 `CostUSD` | `internal/model/model.go:187-192` 规定 Bill 是原生单位、绝不换算 USD；credits 不是美元 |
| 不把 `credits` 塞进 `model.Bill` 了事 | `Bill` 在 `internal/query`/`internal/gui` 没有展示/聚合消费者（只在 store 持久化、query 重建 `UsageEvent`）；credit-only 事件在 dashboard 仍是 0 token / 0 USD，只会增加 request 数 |
| 不用 `context_usage_ratio × contextWindow` 冒充 Input | ratio 是上下文占用比，不是计费输入；这是估算，违反"不臆造" |
| 不读 IDE lite transcript | `message.usage` 完全缺失，连 credits 都没有 |
| 不解析 `~/.qoder-credits-proxy` | 本机不存在；只有用户自配代理才有，且属于另一个工具的私有格式，未经真实数据验证 |

如果未来 Qoder 官方网关开始写真实 token，或 MyToken 决定支持 BYOK/proxy 的 `usage.jsonl`，再实现：默认根 `~/.qoder-cn/projects`（`MYTOKEN_QODER_DIRS` 覆盖），只读桌面 rich `**/*.jsonl`（排除 `transcript/`），按 `message.usage.request_id` 或 `message.id` 去重，`Input=input_tokens`、`CacheRead=cache_read_input_tokens`、`CacheWrite=cache_creation_input_tokens`、`Output=output_tokens`，model 取 `message.model`（回退 `runtime-config.model`），时间取 `timestamp`，session id 取 `sessionId`，项目路径取 `cwd`。

## 复现

```console
$ find ~/.qoder-cn/projects -name '*.jsonl' -not -path '*/transcript/*' | wc -l
1
$ python3 - <<'PY'
import json,glob
n=0; tot=0
for f in glob.glob('/Users/zzstar/.qoder-cn/projects/**/*.jsonl',recursive=True):
    if '/transcript/' in f: continue
    for line in open(f):
        r=json.loads(line); u=(r.get('message') or {}).get('usage')
        if r.get('type')=='assistant' and u:
            n+=1; tot+=u.get('credits',0)
print(n, tot)
PY
16 3.5747536000000006
$ ls ~/.qoder-credits-proxy 2>&1
No such file or directory
```

## Lead 集成更新

无：没有新增 `model.Harness` 常量、没有注册 parser、没有新增数据源。若后续要显示 Qoder credits，需要先在 `internal/query`/`internal/gui` 为 `model.Bill` 增加展示（本轮按分工未改 GUI）；否则 Qoder 只能停留在 quota 页，不会进入用量统计。
