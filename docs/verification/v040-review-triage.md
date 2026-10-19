# 0.4.0 审查分流与实施编排

## 当前授权

- 主代理负责编排、代码审查及独立验证；产品实现与修复交由 `nerv-base/deepseek-flash`。
- 禁止 `step-5-preview`。此前只读 workflow-9 已结束；报告是待核实建议，不是通过证明。
- 不提交、推送、发布；不触碰真实用户数据库、凭据或 harness 配置。

## 活跃批次（编排记录）

| Job | 工作 | 独占写入范围 | 下一步 |
| --- | --- | --- | --- |
| workflow-10 | ProviderIDs 全链路过滤、回归、端点性能测量 | internal/query/** | 收集、审查 diff、独立回归；未通过则交回修复 |
| workflow-10 | 备份/迁移安全、legacy schema、恢复说明 | internal/store/** 与 docs/verification/v040-migration-safety.md | 同上；禁止自动覆盖/删除备份或恢复真实库 |
| workflow-11 | 固定时区日/周/月和上一周期同等实际已过时长 | internal/reporttime/** | 审查 DST、月长、周一边界；再安排持久化/报表接入 |
| workflow-11 | 最近60秒 live 入账输出速率 | internal/ingestrate/** | 审查 stale/unknown/backfill、边界与并发；再安排采集/托盘接入 |

这些批次尚未完成验收。两个新包属于基础逻辑，不代表完整报表或托盘功能已实现。

## 对 workflow-9 报告的初步判定

1. **备份失败后的恢复体验**：固定备份文件已存在时拒绝覆盖是确认的保护策略，不能照报告建议自动删除/复用。可补行动明确的报错和保留证据的恢复说明；任何自动恢复方案需另审。
2. **目录 fsync / Windows**：需核实并测试平台安全实现。macOS 测试不能代替 Windows 执行证据。
3. **historyDigest 没有显式 row count**：哈希逐行编码的有序流已经能检测丢行；“无 count 所以检测不到丢行”不成立。priced 等额外字段需要考虑旧 schema 不存在及合法旧迁移，不能直接加字段破坏兼容。
4. **未知端点缺少 registry 行**：保守策略本身合理，查询必须保留 unresolved 分组并允许确切 ID 选择；不可 inner join 后静默丢数据。
5. **SQL 字符串替换**：实读 aggregate_sql.go 的 ReplaceAll(where,"timestamp","day") 只替换 timestamp，不会把 base_url 变成 day。报告对具体机制的描述不正确；但 daily_usage 不含 base_url，端点筛选不能走旧 rollup 的结论成立。
6. **性能**：报告称 200K 事件 ProviderGroups 约709ms、7日约109ms，ByProvider约2.60ms；该报告 fixture 未包含 BaseURL，不能当作端点合并真实性能基线。709/2.60约273倍，而非报告笼统295倍。应由新批次补真实端点 fixture 后测量，Lead复核；不豁免严格性能门槛。
7. **显示别名只写不读**：报告捕获了开发中间状态；现已实现同一只读事务加载别名、仅改变Label，并测试清除别名恢复端点标签。不能把旧报告当作当前缺陷。
8. **未识别ID跨重启不稳定**：同一 harness/session/provider/raw URL 的确定性哈希跨重启稳定；输入身份改变才改变，后续多实例源身份仍需加入。

## 已验证的别名批次

- 存储测试先因 SetProviderAlias 未定义失败；实现后又捕获缺少通知，修正后通过。
- 查询测试先因 alias not shown 失败；加入读取后通过。
- bash-7198：query/store 定向测试、go test ./...、go vet ./...、git diff --check 全部 exit0。
- 别名不改 endpoint key、历史 resolved_provider/cost、pricing_fingerprint；通知订阅者刷新。
- GUI/CLI 操作入口未接入。
