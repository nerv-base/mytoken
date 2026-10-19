# Cursor 本地用量调研（未新增 internal/harness/cursor）

日期：2026-10-19 23:50 CST；环境：go1.27.1 darwin/arm64；工作目录 `/Users/zzstar/Developer/Programs/MyToken!!!!!`；分支 `main`；HEAD `167751c`。**未提交**。本轮写入范围只有本文件（`internal/gui/**` 与 `main*.go` 的改动来自 lead，未触碰）。

## 结论与范围

**不新增 harness，不新增 `model.Harness` 常量。** Cursor 的本地数据库确实有 `bubbleId:*` 与 `composerData:*` 的 schema 位置，但本机没有任何一条带 token 的消息；上游独立实现也确认 Cursor v3 的 per-message `tokenCount` 恒为 `{0,0}`、`composerData.usageData` 恒为空对象，只能按字符数估算。按任务要求"证据不足就不臆造"，不实现一个只能输出 0 或估算值的解析器。

**做**：只读复制 `state.vscdb` 到 `/tmp/mytoken-research/` 后查询；核对 `cursorDiskKV`、`ItemTable` 与 `~/.cursor/ai-tracking/ai-code-tracking.db`；与 skiagram、codeburn 两个公开实现交叉验证。

**不做**：不实现字符数 ÷ 4 的估算、不把 context 占用当输入 token、不把 Cursor 控制台 CSV 同步（网络/账号范围）混入本地日志 harness、不读取或输出 `cursorAuth/accessToken`、`refreshToken` 等凭据。

## 本机真实数据

数据根：`~/Library/Application Support/Cursor`（约 7.6 MB）；`~/.cursor`（约 316 KB）。为避免打开 live SQLite，先复制主库与 sidecar 到 `/tmp/mytoken-research/cursor-state.vscdb` 再以 `mode=ro` 查询。

`~/Library/Application Support/Cursor/User/globalStorage/state.vscdb`：

| 表 / 键 | 实测 |
| --- | --- |
| `ItemTable` | 98 个键；UI/workbench、`cursorAuth/*`、`serverConfig` 等；无 usage/token 账本键 |
| `cursorDiskKV` | 4 行，全部是 `composerData:<uuid>`，无任何 `bubbleId:%` |
| `bubbleId:%` 行数 | **0** |
| 值中包含 `tokenCount` 的行数 | **0** |
| `composerData` 的 `usageData` | 4/4 为 `{}` |
| `composerData.modelConfig.modelName` | 4/4 为字面量 `"default"` |
| `fullConversationHeadersOnly` | 4/4 长度为 0 |
| `createdAt` | `1783617397664` / `1783617472877`（ms），本机 Cursor 尚未产生聊天消息 |

`~/.cursor/ai-tracking/ai-code-tracking.db` 是代码归因库，不是 token 账本：

| 表 | 行数 | 用途 |
| --- | --- | --- |
| `ai_code_hashes` | 0 | 代码片段哈希归因 |
| `scored_commits` | 0 | commit 归因 |
| `tracking_state` | 1 | 跟踪状态 |
| `conversation_summaries` | 0 | 对话摘要，无 token 列 |
| `tracked_file_content` | 0 | 文件内容 |
| `ai_deleted_files` | 0 | 删除记录 |

`~/.cursor/projects` 只有 `canvases/`、`terminals/`、`mcps/`，没有 chats；`~/.cursor/extensions` 只有 `extensions.json`。

## 上游交叉验证

- skiagram-core 0.1.0 `adapters/cursor.rs`（MIT）：schema 与本机一致，并明确记录"on real data the per-message `tokenCount` is **~99% zeroed** (3 of 275 bubbles non-zero in a real globalStorage db) and `modelConfig.modelName` is frequently the literal `default`"；因此作者故意把 Cursor adapter 留为 stub。 <https://docs.rs/skiagram-core/0.1.0/src/skiagram_core/adapters/cursor.rs.html>
- codeburn `docs/providers/cursor.md`：读同一 `state.vscdb`，Bubbles + agentKv 两路；quirks 原文 "**Cursor v3 reports zero token counts.** The parser falls back to char-counting (`CHARS_PER_TOKEN = 4`) for those rows"；"no bubble carries a real `tokenCount` (it stays `{0,0}`), and `composerData.usageData` is an empty object"。codeburn 明确把本地结果标为 estimated，并说明精确值只能来自 Cursor 控制台导出的 CSV（`https://cursor.com/api/dashboard/export-usage-events-csv`）。 <https://github.com/getagentseal/codeburn/blob/main/docs/providers/cursor.md>

这两份独立实现与本地 0 行 `bubbleId`、0 个 `tokenCount`、4/4 空 `usageData` 相互印证：Cursor 没有可用于 MyToken 的本地逐请求 token 账本。

## 决策

| 决策 | 依据 |
| --- | --- |
| 不新增 `model.Harness` / parser | 本地 0 条 token 记录；上游确认 v3 恒为 0，只能估算 |
| 不读 `cursorDiskKV` 做"会话/请求数"统计 | 没有 token 的事件只会制造 0-token 行，无法计入用户要的 token/spend |
| 不解析 `~/.cursor/ai-tracking` | 表结构无 token 列，且本机数据表全为 0 行 |
| 不实现字符估算 | 任务与 `docs/HARNESS.md` 禁止把估算写成实测；`CostUSD` 也不能据此定价 |
| 不读取 `cursorAuth/*` | 凭据与用量无关，且禁止打印/落盘 |

如果未来 Cursor 恢复真实 `tokenCount`（或 MyToken 决定支持导入 Cursor 控制台 CSV），再按 `bubbleId` + `composerData` 实现；届时默认根应为 `User/globalStorage`（`state.vscdb`），并同时扫描 `workspaceStorage/*/state.vscdb`。

## 复现

```console
$ cp "$HOME/Library/Application Support/Cursor/User/globalStorage/state.vscdb"* /tmp/mytoken-research/
$ sqlite3 /tmp/mytoken-research/cursor-state.vscdb \
    "select count(*) from cursorDiskKV where key like 'bubbleId:%';"
0
$ sqlite3 /tmp/mytoken-research/cursor-state.vscdb \
    "select count(*) from cursorDiskKV where value like '%tokenCount%';"
0
$ sqlite3 /tmp/mytoken-research/cursor-state.vscdb \
    "select key from ItemTable where key like '%usage%' or key like '%token%';"
# 仅 cursorAuth/* 与 workbench 面板键，无用量键
```

## Lead 集成更新

无：本轮没有新增 `model.Harness` 常量、没有注册 parser、没有新增数据源，因此不需要颜色/图标或 Settings 条目。
