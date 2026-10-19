# 0.4.0 多实例 CLI（internal/cli + internal/instance）验证

日期：2026-10-13 21:10 CST
环境：go1.27.1 darwin/arm64；工作目录 `/Users/zzstar/Developer/Programs/MyToken!!!!!`；HEAD `2e696f2`
写入范围：`internal/cli/**`（新增 `sources.go`、`sources_test.go`、`instance_selector_test.go`、`doctor_sources_test.go`；改 `cli.go`、`commands.go`、`options.go`、`cache.go`、`flags.go`、`relay.go`）、`internal/instance/**`（新增 `update.go`、`update_test.go`）与本文件。

```console
$ git status --porcelain -- internal/cli internal/instance docs/verification/v040-instance-cli.md
 M internal/cli/cache.go
 M internal/cli/cache_test.go
 M internal/cli/cli.go
 M internal/cli/commands.go
 M internal/cli/options.go
 M internal/cli/output.go
 M internal/cli/relay.go
 M internal/cli/relay_test.go
?? internal/cli/doctor_sources_test.go
?? internal/cli/flags.go
?? internal/cli/instance_selector_test.go
?? internal/cli/provider.go
?? internal/cli/provider_identity_test.go
?? internal/cli/provider_output.go
?? internal/cli/provider_startup_test.go
?? internal/cli/sources.go
?? internal/cli/sources_test.go
?? internal/instance/
```

说明：`internal/cli/cache_test.go`、`internal/cli/output.go`、`internal/cli/relay_test.go`、`internal/cli/provider*.go`、`internal/cli/flags.go` 属于**并发的 provider-CLI 工作流**（见文末"并发写入与协调"）。本轮我改动的 CLI 文件是 `cli.go`、`commands.go`、`options.go`、`cache.go`、`flags.go`（新增 `ledgerFilterFlags`）、`relay.go`（reconcile 改用 `ledgerFilterFlags`）以及四个新文件；没有触碰 `main.go`、`internal/app`、`internal/store`、`internal/paths`、`internal/query`、`internal/gui`、`internal/quota`。

## 结论与范围

本次交付的是**多实例的 CLI 切片**：用共享的 `sources.json` 做源管理（list/add/move/remove/promote），把实例维度接进诊断（doctor）与既有查询类命令（stats/sessions/cache/provider list），并让不支持的命令**显式拒绝**实例选择器。

它不是 0.4.0 的完成，也不声称多实例已端到端可用：

- 采集链路（`app`/`scanner`/`store` 的 per-instance 扫描与游标）由其它工作流负责；`sources promote` 只把配置条目从 `backup-copy` 变成可采集的 `instance`，**本身不扫描、不迁移数据**。
- GUI 的 sources 行、quota/预算的实例维度同样不在本轮。
- 本轮没有提交、没有发布、没有跑 fullsuite、没有读真实用户数据、没有联网。

## 交付的命令契约

### `mytoken sources list [--json]`

只读：用 `paths.ResolveDataDir()`（**不创建**数据目录）定位 `sources.json`，不打开数据库、不刷新价格、不读任何日志。文本视图首行 `Config: <path> (present|absent)`，随后是
`ID  HARNESS  ORIGIN  KIND  STATUS  SCANNABLE  ROOTS  REASON` 表与 `PROBLEMS` 段。JSON 是 `{configPath, configPresent, configError?, instances[], problems[]}`，条目含 `source`（user/env/default）、`kind`、`status`（enabled/disabled/backup-copy/error）、`scannable`、每个根的 `exists`、以及不致命的 `reason`。

退出码：配置读不出来、或存在致命 problem（如显式根缺失）→ 1；只有非致命问题（内置默认根缺失）→ 0；参数错误 → 2。

### `mytoken sources add --harness H --root P [--root P ...] [--id ID] [--label TEXT] [--kind instance|backup-copy] [--disabled] [--json]`

`--kind` 默认 `instance`；ID 默认 `instance.NewID()`（不透明，`inst_` + 128 bit base32），`--id` 可显式指定。启用的 `instance` 根必须存在且是目录，否则退出 2 并提示
`root <path> does not exist (store it with --kind backup-copy or --disabled to skip the check)`；`backup-copy` 与 `--disabled` 的根按原样存储、**从不 stat**（备份副本契约）。重复 `--root` 拒绝。

### `mytoken sources move <id> --root P [--root P ...] [--json]`

保留 ID/Label/Harness/Kind/Disabled，只换根；启用实例的新根同样要存在。未知 ID → 1（`unknown instance id`）。

### `mytoken sources promote <id> [--json]`

唯一把 `backup-copy` 变成 `instance` 的入口，**保留 ID**；对已经是 `instance` 的条目退出 1 并说明 `only a backup-copy entry can be promoted to instance`。

### `mytoken sources remove <id> [--json]`

只删 `sources.json` 里的配置条目：文本输出 `Removed instance <id> (configuration entry only; no data was deleted)`，JSON 输出 `{"id":...,"removed":true}`。不删日志、不删数据库、不删目录（有测试用哨兵文件证明根内文件仍在）。

### `mytoken doctor [--json]` 的实例诊断

doctor 现在用 `paths.ResolveDataDir()`/`paths.ResolveDBPath()` 解析路径（不再用会建目录的 `DataDir()`），并**增量**增加 JSON 字段：`configPath`、`configPresent`、`configError?`、`instances[]`；每个 `instances[]` 行内嵌 sources 条目（`id/harness/kind/origin/status/scannable/roots/reason`）并加上按 `(harness, instance_id)` 统计的 `sources`（`cursors`）与 `events`。既有字段（`dataDir`、`dbPath`、`dbBytes`、`pricingSource`、`pricingFetchedAt`、`pricingAgeSeconds`、`harnesses[]`、`providerSources[]`）与退出语义（0，除非 DB/价格读取失败）不变；文本视图新增 `SOURCES CONFIG: <path> (present|absent)` 与一张 `ID HARNESS ORIGIN KIND STATUS SCANNABLE SOURCES EVENTS ROOTS REASON` 表。

### `--instance-id`（共享 flag，可重复）

- **求交**：`stats`、`sessions`（列表）、`cache`（账本视图）、`provider list` 通过 `query.Filter.InstanceIDs` 与其它过滤条件取交集；未设置时行为与旧版逐字节一致。
- **下钻**：`sessions <session-id> [--harness H] [--instance-id I]` 与 `cache <session-id> [--harness H] [--instance-id I]` 只接受**至多一个** `--instance-id`，走 `query.SessionIn` / `SessionCacheIn`（显式实例下钻用 `SessionIn`）。同一 session 存在于多个实例时，不带实例选择器会报错（`more than one instance`）而不是任选一个；多个 `--instance-id` → 2。
- **拒绝**：`reconcile` 明确报 `unsupported selector --instance-id`（连同 `--provider`/`--provider-id`）；`scan`、`prices list`、`relay list`、`doctor` 没有注册该 flag，Go flag 解析直接失败 → 2。**没有任何命令会静默忽略它。**

`sessions <id>` 的下钻另外拒绝 `--limit`（`--limit cannot be combined with a session id`）与 `--provider`/`--provider-id`（该命令直接读一个 session，没有列表可过滤），并说明它覆盖整个 session、忽略区间 flag。

## 先红后绿证据

### 红（实现前/首跑）

新测试先写，首次聚焦跑红 3 个：

```console
$ go test ./internal/cli/ -run 'TestSources|TestInstanceID|TestSessionDrilldown|TestCacheTimeline|TestUnsupportedCommands|TestReconcileNames|TestDoctor' -count=1
--- FAIL: TestSourcesUsageAndDispatch        (sources add/move/remove/promote 裸调用 stderr 缺 "Usage: mytoken sources")
--- FAIL: TestSourcesAddRejectsInvalidArguments (sources add --root <dir> 打的是 "--harness is required"，测试要 "unknown harness")
--- FAIL: TestSourcesBackupCopyPromoteKeepsIdentity (sources promote <id> --json 返回 2：flag.Parse 在位置参数处停止，--json 变成第二个位置参数)
FAIL
```

修复：每个子命令各自的 usage 常量（都以 `Usage: mytoken sources` 开头）；缺 `--harness` 与拼错一样报 `unknown harness`（缺时附 `--harness is required`）并打印 usage；`remove`/`promote` 改用 `splitArgs`，允许 flag 出现在 id 之后。同时把我自己 RED 测试里三处小写 `usage: mytoken sources move/remove/promote` 的期望改成大写 `Usage:`（仅大小写，退出码 2 的契约未变）。

随后审计发现 `sessions <id>` 下钻仍会接受 `--provider`/`--provider-id` 却只用在"解析归属 harness"这一步（部分静默应用），于是补上显式拒绝并加了两个子用例。

### 绿（实现后）

```console
$ go test ./internal/cli/ -count=1 -v -run 'TestSources|TestInstanceID|TestSessionDrilldown|TestCacheTimeline|TestUnsupportedCommands|TestReconcileNames|TestDoctor'
--- PASS 共 25 项，0 FAIL
$ go test -count=1 -race ./internal/instance/
ok  	github.com/zzstar101/mytoken/internal/instance	1.946s
$ go test ./internal/instance/... -count=1
ok  	github.com/zzstar101/mytoken/internal/instance	0.605s
$ go test ./internal/cli/... -count=1
ok  	github.com/zzstar101/mytoken/internal/cli	11.088s
$ gofmt -l internal/cli internal/instance
(无输出)
$ go vet ./internal/cli/... ./internal/instance/...
(无输出，退出码 0)
$ grep -n 'openApp\|OpenLocal\|app\.\|store\.\|query\.\|http\.\|net\.' internal/cli/sources.go
(无匹配：sources 命令不打开 App/DB、不刷新价格、不联网)
```

## 真实二进制冒烟（临时根 + 合成数据，无家目录/凭据发现）

```console
$ MYTOKEN_HOME=/tmp/mytoken-cli-check/home /tmp/mytoken-cli-check/mytoken sources list
Config: /tmp/mytoken-cli-check/home/sources.json (absent)
default  codex  default  instance  enabled  true  /Users/...（仅内置默认根，缺失为非致命 warning）
exit=0
$ ... sources add --harness codex --root .../rootA --label "work logs"
Added instance inst_gyp56dyfvw3v75rjno6ikeqroe (harness codex, kind instance)
exit=0
$ ... sources add --harness gemini --root .../gone --kind backup-copy --label "old drive" --json
{"id":"inst_7gml2il4dpezqv3bbvddy6rv3a","label":"old drive","harness":"gemini","roots":["/tmp/.../gone"],"kind":"backup-copy"}
exit=0                       # 备份副本允许缺失根，且不 stat
$ ... sources move inst_pnjvj... --root .../absent
sources move: root .../absent does not exist (store it with --kind backup-copy or --disabled to skip the check)
exit=2                       # 启用实例的显式无效路径必须响亮失败，绝不静默存储/回退
$ ... sources add --harness nope --root .../rootA
sources add: unknown harness "nope" (want a harness id such as claude-code, codex or dsh)
Usage: mytoken sources add --harness NAME --root PATH ... [--json]
exit=2
$ ... sources promote <backup-copy-id> --json
{"id":"inst_7gml...","label":"old drive","harness":"gemini","roots":["/tmp/.../absent"],"kind":"instance"}
exit=0                       # 升格保留 ID
$ ... sources promote <同一个 id>
sources promote: instance: invalid-kind id=inst_7gml... harness=gemini: only a backup-copy entry can be promoted to instance
exit=1
$ ... sources remove <id> --json
{"id":"inst_7gml...","removed":true}
exit=0
$ ... sources remove nosuch
sources remove: instance: not-found id=nosuch: unknown instance id
exit=1
$ printf '{ not json at all\n' > $MYTOKEN_HOME/sources.json
$ ... sources list --json     # stdout 里 configError 有值，stderr 也有原因码，exit=1
$ ... sources add --harness codex --root .../rootA
sources add: instance: invalid-config path=.../sources.json: parse config: invalid character 'n' ...
exit=1
malformed config unchanged: YES; lock left: 0      # 前后 sha256 相同，无 .sources.lock 残留
```

选择器与拒绝（同一冒烟脚本）：

```console
$ ... stats --instance-id inst_a --json --by model --since 7d      exit=0
$ ... stats --instance-id '' --json                               exit=2  (invalid value "" for flag -instance-id: empty value)
$ ... sessions --instance-id inst_a --json --limit 1              exit=0
$ ... cache --instance-id inst_a --json                           exit=0
$ ... provider list --instance-id inst_a --json                   exit=0
$ ... reconcile --instance-id inst_a
reconcile: unsupported selector --instance-id; select the site with the positional <origin|provider> argument and --key-id (ledger filters never narrow a relay comparison)
exit=2
$ ... scan --instance-id inst_a            exit=2 (unknown flag)
$ ... prices list --instance-id inst_a     exit=2
$ ... relay list --instance-id inst_a      exit=2
$ ... doctor --instance-id inst_a          exit=2
$ ... sessions nosuch --offline --harness codex --json
sessions: no session "nosuch" for harness codex
exit=1
```

## 共享 flag 审计

唯一注册点是 `internal/cli/options.go:63 addCommonFlags`（`--provider`、`--provider-id`、`--instance-id` 以及区间/格式 flag）。每个调用点要么通过 `options.filter()` 消费，要么显式拒绝：

| 调用点 | `--provider` / `--provider-id` | `--instance-id` |
| --- | --- | --- |
| `cli.go:68` stats | 消费（`opts.filter()`） | 消费（交集） |
| `commands.go:152` sessions 列表 | 消费 | 消费（交集） |
| `commands.go` sessions 下钻 | **拒绝**（`cannot be combined with a session id`） | 至多 1 个，走 `SessionIn` |
| `cache.go:74` cache 账本 | 消费 | 消费（交集） |
| `cache.go:74` cache 时间线 | 拒绝（既有） | 至多 1 个，走 `SessionCacheIn` |
| `provider.go:83` provider list | 消费 | 消费（交集） |
| `relay.go:606` reconcile | 拒绝（`unsupported selector`） | 拒绝（`unsupported selector --instance-id`） |
| `scan` / `prices list` / `relay list` / `doctor` | 未注册 → 解析失败 exit 2 | 未注册 → 解析失败 exit 2 |

## 测试清单（新增 31 个）

`internal/cli/sources_test.go`（10）：usage/分发（7 种裸调用都要 usage + 2）、add/list/remove 往返（不透明 ID、origin user、0600、remove 后消失）、ID 不透明且互异（`--id` 生效）、11 种非法参数（未知 harness/无根/根不存在/非法 kind/重复根/各子命令 usage，且不落盘）、backup-copy+promote 保留身份（缺失根允许、`scannable=false`、二次 promote 失败）、未知 ID 为运行错误（1）、畸形配置绝不覆盖（字节相同、无锁残留）、list 不创建数据目录、致命根缺失 → 非 0 且 status=error/reason 含 `does not exist`、remove 只删配置条目（根内哨兵文件存活）。

`internal/cli/instance_selector_test.go`（9）：`--instance-id` 可重复求交、空值拒绝、stats/sessions/cache/provider list 的实例收窄（合成 store fixture：`store.Write{Harness, InstanceID, Path, Batch}` + `CommitMany`）、session 下钻的实例感知（同一 session 两个实例 → 报错；指定实例 → 精确行与事件；无 `--harness` 时解析归属；不存在 → 1）、下钻非法组合（多位置参数/多实例/`--limit`/坏 harness/`--provider`/`--provider-id`）、cache 时间线走 `SessionCacheIn`（假 service 记录调用）、cache 时间线多实例拒绝、5 个不支持命令拒绝 `--instance-id`、reconcile 报出 `unsupported selector --instance-id`。

`internal/cli/doctor_sources_test.go`（4）：doctor 旧 JSON 字段与 harness 行字段全在、新增 `configPath/configPresent/instances` 且行内有 `sources`/`events` 计数、文本视图含 `SOURCES CONFIG` 与条目、凭据不泄漏（`OPENAI_API_KEY`/`ANTHROPIC_API_KEY`/`MYTOKEN_RELAY_KEY` 设为哨兵值，根内放内容含哨兵的 canary 日志，`sources list`/`doctor` 的文本与 JSON 都不得出现哨兵，也不得出现日志内容）。

`internal/instance/update_test.go`（8）：并发 8 写者不丢更新、返回落盘配置、畸形配置绝不被重写、mutation 失败保留原配置、另一个写者持锁时拒绝（不改写对方锁、不建 sources.json）、回收过期锁、按需创建数据目录（0600）、同进程调用串行化。

## 兼容性与未变契约

- 没有 `sources.json` 时，`stats`/`sessions`/`cache`/`provider list`/`doctor` 的输出与旧版一致（新增字段只在 doctor 的 JSON 里，且是增量键；`internal/cli/cli_test.go` 的 `TestCommandSurface` 与 `TestStatsJSON` 保持通过）。
- 未给新 option 的命令输出契约不变；`sessions <session-id>` 是**新增**的位置参数形式（旧版位置参数一律 exit 2）。
- `sources` 命令完全不打开 App/DB、不刷新价格、不做迁移、不联网；`doctor` 仍按既有语义打开 DB（这是 doctor 的既有行为，本轮只换路径解析函数）。

## 并发写入与跨平台说明

- `internal/instance.Update(dataDir, mutate)` 是"读-改-写"的唯一入口：进程内 `sync.Map` 互斥 + 数据目录内 `O_CREATE|O_EXCL` 的 `.sources.lock`（写入 pid，0600），等待上限 `lockTimeout=5s`（每 `lockRetryEvery=5ms` 重试），超过 `lockStaleAfter=30s` 的锁视为崩溃残留并回收。`os.Rename` 只保证"替换是原子的"，**它不是 CAS**：没有这个锁，两个 CLI 进程的 add 会互相覆盖（测试用 8 个 goroutine 证明有锁时不丢更新，另有用例证明锁被占时不写、不删别人的锁）。
- 跨平台：锁只用 `os.OpenFile(O_EXCL)`（POSIX 与 Windows 均支持），不依赖 `flock`/`fcntl`（因此 NFS 上的语义取决于服务端实现，未验证）；配置写入是"临时文件 + 同目录 rename"，Windows 上同目录 rename 覆盖已存在文件由 Go 标准库处理；路径拼接全部走 `filepath`；根规范化用 `filepath.EvalSymlinks`（macOS 上 `/tmp` 会规范成 `/private/tmp`，冒烟输出里可见）。**本轮只在 macOS 上运行过**，Windows 行为未实测。
- 崩溃持久性：`Save` 保证原子替换且不留临时文件，但**不对数据目录本身 fsync**；崩溃后可能残留 `.sources.lock`，30s 后由下一个写者回收。

## 未关闭项（诚实说明）

1. **不是 0.4.0 完成**：采集/扫描/GUI/quota 的实例维度不在本轮；`promote` 之后仍需 `mytoken scan`（由其它工作流落地）才真正采集。
2. **跨进程互斥是 advisory**：绕过 CLI 直接写 `sources.json` 的进程不会被锁阻止；锁只覆盖本包的 `Update`。
3. **doctor 的 `instances[].sources/events` 对 `backup-copy`/`disabled` 条目也会读既有账本计数**——这是只读诊断，`scannable=false` 才表示不参与采集；本轮不重扫、不合并不去重备份副本。
4. **`sources list` 文本视图会列出全部内置默认根**（本机 30+ 个 harness），表格很宽；脚本应使用 `--json`。
5. **`sessions` 下钻忽略区间 flag**（`--since/--until/--last/--timezone`）——与 `cache` 时间线同一约定，已在 `sessionsUsage` 里写明；不是静默丢弃选择器。
6. **并发写入与协调**：本轮与 provider-CLI 工作流并发编辑 `internal/cli`（`cli.go`、`relay.go`、`cache.go`、`flags.go`、`cache_test.go`、`relay_test.go`、`output.go`）。合并后的树 `gofmt`/`vet`/两个包测试全绿，但该状态会随对方继续写入而变；本文件只声明我这一轮的改动与验证。
7. **未提交、未发布、未跑 fullsuite、未用真实数据/网络**；`main.go` 与 app/store/paths/query/gui/quota 未被本轮触碰。
