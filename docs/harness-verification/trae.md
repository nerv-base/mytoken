# Trae（CN / Global）本地用量调研（未新增 internal/harness/trae*）

日期：2026-10-19 23:52 CST；环境：go1.27.1 darwin/arm64；工作目录 `/Users/zzstar/Developer/Programs/MyToken!!!!!`；分支 `main`；HEAD `167751c`。**未提交**。本轮写入范围只有本文件。

## 结论与范围

**不新增 harness，不新增 `model.Harness` 常量。** Trae CN 的 AI Agent 数据库里确实有逐轮 `token_usage`，但整个库是 **SQLCipher 4 加密**，本机无法在只读、无密钥提取的前提下读出；Trae Global 未安装，也没有可核对的本地数据。按任务要求，不伪造解析器，也不把远程配额接口的数据混入本地日志。

**做**：核对 Trae CN 的 `state.vscdb`、workspace DB、`ModularData/ai-agent/database.db`、日志与 `~/.trae-cn`；核对 Trae Global 的默认目录是否存在；用公开的 `trae-db-decrypt` 项目确认加密方式与 token 字段 schema。

**不做**：不从进程内存提取 SQLCipher key（本机无该工具，且属于绕过应用保护的敏感操作）、不实现 SQLCipher 解密、不调用 `api.trae.cn` 的配额/用量接口、不把 `deepseek-v4.1-flash` 等模型名或任何 token 写成推测值。

## 本机安装与目录

| 项目 | 实测 |
| --- | --- |
| App | `/Applications/Trae CN.app`（只有 CN，无 `/Applications/Trae.app`） |
| 用户数据 | `~/Library/Application Support/Trae CN`（约 74 MB） |
| 用户 home | `~/.trae-cn`（约 304 MB，主要是 builtin/plugins） |
| Global 数据 | `~/Library/Application Support/Trae` **不存在**；`~/.trae` **不存在** |

## 关键证据：AI Agent 库是加密的

`~/Library/Application Support/Trae CN/ModularData/ai-agent/database.db`：

- 大小 **5,378,048 B**；同目录 live `database.db-wal` 约 **5,001,712 B**（复制到临时目录后 WAL 被 checkpoint 为 0，说明它确实是活跃 WAL）。
- 文件头 16 字节：`fa 81 83 e0 3e 3f 24 42 64 3c 87 a4 e8 d0 64 19`，不是 `SQLite format 3\0`。
- `file` 识别为 `data`；`sqlite3` 打开报 **error 26 "file is not a database"**；字符串扫描为随机字节，没有明文表名/JSON。

公开项目 **Oh-My-Trae/trae-db-decrypt**（MIT）明确说明这是 **Trae CN SQLCipher 4** 数据库：

- README 标题："Trae CN SQLCipher 4 数据库解密 + Token 使用量分析工具"。
- 解密方式：从进程内存（Windows API，`ai_agent.dll`）提取 key，再按 SQLCipher 4 页格式（PBKDF2-HMAC-SHA512 + AES-256-CBC + HMAC-SHA512 校验）逐页解密。
- schema 共 39 张表；`chat_turn.context` 的 JSON 中有：
  `token_usage { prompt_tokens, completion_tokens, total_tokens, reasoning_tokens, cache_read_input_tokens, cache_creation_input_tokens }`，
  以及 `persist_user_message_context.model_info.config_name`（模型名）。
- 该项目只有 Windows 的 key 提取实现；**没有 macOS key extractor**。key 不落盘，无法从数据库文件本身恢复。

<https://github.com/Oh-My-Trae/trae-db-decrypt>

因此：**Trae CN 本地确实保存了真实逐轮 token 用量，但可读性被 SQLCipher 阻断；没有密钥就无法解析，不能据此实现一个"读本地明文日志"的 harness。**

## 明文位置的核对结果（均无 token）

`~/Library/Application Support/Trae CN/User/globalStorage/state.vscdb`：

- 只有 `ItemTable`，50 个键；与 AI 相关的只有 `AI.agent.model.model_list_map`、`session_selected_model`、`icubeAiChat` docset 列表与 telemetry；**没有** `bubbleId`、`tokenCount`、`usageData` 或用量账本表。

`~/Library/Application Support/Trae CN/User/workspaceStorage/{9ab391358924ec0dc78991bd2cdfa1ee,e04cdd}/state.vscdb`：

- 只有编辑器/SCM/终端等 VS Code 键；其中一个库有 `chatHistoryNeedToBeMigrated-*` 键，说明历史聊天准备迁移到加密库，但明文库中没有 token。

`~/Library/Application Support/Trae CN/logs/{aha_log,20261015T012424,20261016T092456}`：

- `[MetadataHandler] received metadata` 行有 `sessionId`、`message_id`、`turn_id`、`message_index`、`status`、`agent_type: solo_agent` 与 `model_info{config_name:"deepseek-v4.1-flash", display_model_name:"DeepSeek-V4.1-Flash", prompt_max_tokens:936000}`，**没有 token usage**。
- `[trae-chat-core] calculate step durations` 只有耗时。
- 配额来自远程 `https://api.trae.cn/trae/api/v2/pay/ide_user_ent_usage`（200 OK），不是本地账本；`TokenManager` 只写 `~/.trae-cn/trae-jwt-token`。
- 全目录按 `usage|token|credit` 搜索，没有 per-request token 计数。

`~/.trae-cn`：

- 304 MB 主要是 builtin/plugins；`assistant/` 为空；整个树只有 1 个 `.jsonl`；没有 `.db`/`.sqlite`/`.vscdb`。

## Trae Global

本机没有安装 Trae Global，`~/Library/Application Support/Trae` 与 `~/.trae` 都不存在，因此无法对 Global 做真实数据核对。公开资料只覆盖 Trae CN 的 SQLCipher 库；**不对 Global 的存储方式做未经证实的断言**。如果 Global 与 CN 同源，预计同样走 `ModularData/ai-agent/database.db`，但需要实际安装后验证。

## 决策

| 决策 | 依据 |
| --- | --- |
| 不新增 `model.Harness` / parser | 唯一含 token 的库是 SQLCipher 4 加密，本机无法读取 |
| 不实现 key 提取 / 解密 | key 只在进程内存；公开实现仅 Windows；属于绕过应用保护，超出只读调研范围 |
| 不解析 `state.vscdb` / workspace DB | 明文库没有 token 字段，只有模型选择与迁移标记 |
| 不解析日志 | MetadataHandler 只有模型/会话/消息 id，无 usage |
| 不调用远程配额接口 | 任务限定本地日志；且该接口不是 per-request token 账本 |
| 不对 Global 下结论 | 本机未安装，无证据 |

如果未来 macOS 上出现可复用的 SQLCipher key 提取方式，或 Trae 提供明文导出，再按 `chat_turn.context.token_usage` 实现；默认根应是 `ModularData/ai-agent`，模型取 `persist_user_message_context.model_info.config_name`，时间/会话 id 取 `chat_turn` 对应列。

## 复现

```console
$ ls -l "$HOME/Library/Application Support/Trae CN/ModularData/ai-agent/database.db"*
$ xxd -l 16 "$HOME/Library/Application Support/Trae CN/ModularData/ai-agent/database.db"
00000000: fa81 83e0 3e3f 2442 643c 87a4 e8d0 6419
$ sqlite3 "/tmp/mytoken-research/trae-agent.db" ".tables"
Error: file is not a database
$ sqlite3 "$HOME/Library/Application Support/Trae CN/User/globalStorage/state.vscdb" \
    "select count(*) from ItemTable where key like '%usage%' or key like '%token%';"
0
```

## Lead 集成更新

无：没有新增 `model.Harness` 常量、没有注册 parser、没有新增数据源。
