# 0.4.0 有根解析器工厂（rooted parser factory）验证

日期：2026-10-13。环境：Go 1.27.1、darwin/arm64。写入范围：`internal/harness/**`、本文件。未改 `internal/query/**`、`internal/store/**`、`internal/gui/**`、`internal/cli/**`、`internal/model/**`、任何 scanner 或共享规划文件（`task_plan.md`、`docs/superpowers/**` 仅只读参考）。

## 结论与范围

为多实例源（`docs/superpowers/plans/2026-10-13-v040-multi-instance.md` 工作流 C）落地**前置能力**：注册表级"按显式 roots 构造全新解析器"的入口，不改变任何已注册单例，不改变零配置行为。

- 新增可选能力接口 `harness.Rooted`（`WithRoots(roots ...string) Parser`）与注册表函数 `harness.NewRooted(h model.Harness, roots ...string) (Parser, bool)`。
- 27 个已注册 harness 解析器包各自实现 `WithRoots`；`codebuddy` 复用 `workbuddy.Parser`，因此由 `workbuddy.WithRoots` 覆盖（共 **28** 个已注册 harness，全部支持）。
- 显式空 roots 保持为空：`WithRoots` 只有看到至少一个非空白 root 时才走既有的 `NewWithRoots`，否则返回零 roots 的全新 `&Parser{}`，**绝不**触达 `defaultRoots()`／环境变量／配置。
- 未知 harness、未实现 `Rooted` 的解析器一律返回 `(nil, false)`，由调用方按硬错误处理，**没有** env/config/默认值回退。
- `Parser` 接口逐字未变；`New()`／`NewWithRoots()`／单例注册路径未变（零配置行为不变）。
- 顺带修复 4 处 `Roots()` 泄漏内部切片、1 处 `hermes.NewWithRoots` 直接别名调用方切片——都是本前置"防御性拷贝"要求的必要条件。

**这不是多实例交付声明。** 本轮只交付工厂前置；未接线任何调用方，未落地每实例 roots 的持久化/默认值/UI，未改 model/schema/scanner。未关闭项见文末。

## 选定 API 与理由

```go
// internal/harness/harness.go
type Rooted interface {
	WithRoots(roots ...string) Parser
}

func NewRooted(h model.Harness, roots ...string) (Parser, bool)
```

`Rooted` 定义在 `internal/harness/harness.go:114-116`，`NewRooted` 实现于 `internal/harness/harness.go:125-137`：`mu.RLock()`（:126）只包住 map 查询，`mu.RUnlock()`（:128）之后才做类型断言（:132）并调用 `r.WithRoots(roots...)`（:136）。

| 决策 | 选择 | 理由 |
| --- | --- | --- |
| 能力暴露方式 | 已注册解析器上的**可选接口** `Rooted` | 任务明确优先此方案；无需新增注册 API、无第二份易漂移的全局工厂表；漏实现者天然落入 `(nil,false)`，与计划风险 R5 一致 |
| 未注册 / 未实现 | 返回 `(nil, false)` | 显式失败，禁止默认 roots 回退 |
| 锁边界 | 查表在锁内、构造在锁外 | 构造函数是任意第三方代码，不得在持锁时调用；`TestNewRootedDoesNotCallWithRootsUnderLock` 反证（见红 2） |
| `codebuddy` | `WithRoots` 用 `NewForHarness(p.harnessID, kept...)` 保留 harness id | `codebuddy` 注册的是 `*workbuddy.Parser`，丢弃 `harnessID` 会让新解析器报错成 workbuddy |
| 空 entries | 过滤空白项后若为空则返回零 roots | 与其余 26 个包经 `NewWithRoots` 归一化后的行为一致（否则 `NewRooted(codebuddy, "", "  ")` 会留下 `["", "  "]`，红 3） |

各包的 `WithRoots` 统一为（`codebuddy` 除外，见上）：

```go
// WithRoots implements harness.Rooted. It returns a fresh parser over exactly
// the given roots; an explicit empty list stays empty instead of falling back
// to this harness's default roots.
func (p *Parser) WithRoots(roots ...string) harness.Parser {
	for _, r := range roots {
		if strings.TrimSpace(r) != "" {
			return NewWithRoots(roots...)
		}
	}
	return &Parser{}
}
```

关键点：13 个包（`cline`、`kilo`、`roo`、`crush`、`opencode`、`gemini`、`mux`、`codebuff`、`grok`、`openclaw`、`dsh`、`pi`、`hermes`）的 `NewWithRoots` 在归一化后为空时会回退 `defaultRoots()`。守卫在"全是空白项"时根本不会调用 `NewWithRoots`，因此 `defaultRoots()`／env 不会被读取。

## 先红后绿证据（精确命令与结果）

新增两个测试文件：`internal/harness/rooted_test.go`（外部包 `harness_test`，只走公开 API，空导入全部 28 个 harness 包）与 `internal/harness/rooted_internal_test.go`（内部包，才能注册"故意残缺"的解析器并在 cleanup 中摘除）。共 9 个测试函数、121 条 PASS（含子测试）。

### 红 1：注册表 API 尚不存在（真实首跑）

```sh
go test ./internal/harness/ -run 'Rooted|NewRooted' -count=1
```

```
# github.com/zzstar101/mytoken/internal/harness [github.com/zzstar101/mytoken/internal/harness.test]
internal/harness/rooted_internal_test.go:72:11: undefined: NewRooted
internal/harness/rooted_internal_test.go:89:15: undefined: NewRooted
FAIL	github.com/zzstar101/mytoken/internal/harness [build failed]
FAIL
```

退出码 1。

### 红 2：只加 `Rooted`/`NewRooted`、未实现各包 `WithRoots`（能力缺失红）

```sh
go test ./internal/harness/ -run 'NewRooted' -count=1
go test ./internal/harness/ -run 'NewRooted' -count=1 2>&1 | grep -c 'does not implement harness.Rooted'
```

前者输出（节选）：

```
    rooted_test.go:179: NewRooted(opencode, ...) = (false, false), want (true, true)
    rooted_test.go:224: NewRooted(amp) = false, want true
--- FAIL: TestNewRootedIgnoresEnvironmentAndDefaults (0.00s)
--- FAIL: TestNewRootedConcurrentAccess (0.00s)
FAIL	github.com/zzstar101/mytoken/internal/harness	0.391s
```

后者输出：`28`，即 28/28 个已注册 harness 全部报"未实现能力"，无一漏网、无一误判为可用。这段证明是"覆盖全部已注册 harness"的有效性证据——若某个包漏加 `WithRoots`，这里会精确点名。

同时 `TestNewRootedDoesNotCallWithRootsUnderLock` 在锁语义错误时不是"断言失败"而是**超时失败**：注册的 `blockingRooted.WithRoots` 会阻塞，第二个 goroutine 若拿不到注册表锁，5s 后 `t.Fatal("registry stays locked while a parser's WithRoots runs...")`。它在红 2 阶段即已通过（`NewRooted` 已存在且实现正确），说明"锁外构造"不是事后补的。

### 红 3：实现过程中自暴露的一致性缺口（`workbuddy`/`codebuddy`）

首轮 26 个包全绿，仅 `workbuddy` 与 `codebuddy` 失败：

```
    rooted_test.go:117: NewRooted(workbuddy, empty entries) produced 2 roots ["" "  "]; explicit empty roots must not fall back to defaults
--- FAIL: TestNewRootedExplicitEmptyRootsStayEmpty/codebuddy (0.00s)
--- FAIL: TestNewRootedExplicitEmptyRootsStayEmpty/workbuddy (0.00s)
```

原因：`workbuddy` 的 `NewForHarness` 逐字拷贝、不做归一化，我的自定义 `WithRoots` 直接透传，于是全空白列表留下 `["", "  "]`，与其余 26 个包不一致。修复为先在 `WithRoots` 内过滤空白项再交给 `NewForHarness`。修复属于 `internal/harness/workbuddy/workbuddy.go` 的 `WithRoots`，未改 `NewForHarness`/`NewWithRoots`/单例，零配置行为不变。

### 绿：最终验证（首尾一致的精确命令）

```sh
gofmt -l internal/harness/          # 无输出（干净）
go vet ./internal/harness/...       # 干净
go test ./internal/harness/ -run 'NewRooted' -count=1
go test -race ./internal/harness/ -run 'NewRooted' -count=1
go test ./internal/harness/... -count=1
```

```
ok  	github.com/zzstar101/mytoken/internal/harness	0.364s
ok  	github.com/zzstar101/mytoken/internal/harness	1.469s   (-race)
ok  	github.com/zzstar101/mytoken/internal/harness	0.448s
ok  	github.com/zzstar101/mytoken/internal/harness/amp	0.428s
... （31 个包全部 ok：根包 + 30 个子包，含 clinetask、harnesstest）
```

已注册 harness 数（测试内断言 `>= 28`，日志实测）：

```
    rooted_test.go:65: registered harness parsers: 28
```

顶层 9 个测试全部 PASS：

| 测试 | 断言的不变量 |
| --- | --- |
| `TestNewRootedSupportsEveryRegisteredHarness` | 28/28 实现 `Rooted`；`NewRooted` 返回全新解析器（≠ 单例）、harness id 与 roots 精确相等 |
| `TestNewRootedExplicitEmptyRootsStayEmpty` | `NewRooted(h)` 与 `NewRooted(h, "", "  ")` 均为 0 roots（不回退默认值） |
| `TestNewRootedUnknownHarness` | 未知 harness → `(nil, false)` |
| `TestNewRootedDoesNotMutateSingleton` | 构造前后单例 `Roots()` 逐字不变；两次 `NewRooted` 互不相同 |
| `TestNewRootedParsersAreIsolated` | 改写调用方切片 / 改写 `Roots()` 返回值，均不影响已构造解析器；兄弟解析器互不影响 |
| `TestNewRootedIgnoresEnvironmentAndDefaults` | `HOME` 与 `MYTOKEN_CLAUDE_DIRS`/`CLAUDE_CONFIG_DIR`/`CODEX_HOME`/`GEMINI_CLI_HOME`/`MYTOKEN_KILO_DIRS`/`MYTOKEN_ROO_DIRS` 全部指向临时目录；显式 roots 逐字生效、空保持空，且构造前后该目录条目数不变（构造期无 IO/无发现） |
| `TestNewRootedConcurrentAccess` | `-race` 下 8 worker × 25 轮遍历全部 harness 并并发读 `All`/`Get`，注册表集合与单例指针不变 |
| `TestNewRootedRejectsParserWithoutCapability`（内部） | 注册一个仅实现 `Parser` 的解析器 → `(nil, false)` |
| `TestNewRootedDoesNotCallWithRootsUnderLock`（内部） | `WithRoots` 阻塞期间另一 goroutine 能拿到注册表锁并完成 `All`/`Get` |

测试只用临时合成路径：`t.TempDir()` 与 `$TMPDIR` 拼接路径；`HOME` 被重定向到临时目录，因此不存在对真实 home 的发现。整条 `./internal/harness/...` 测试均未读取任何凭据。

## 变更文件（28 个 .go + 2 个新测试 + 本文件）

- `internal/harness/harness.go` — 新增 `Rooted` 接口与 `NewRooted`（+34 行）。
- 27 个解析器包各 +12 行 `WithRoots`：`amp`、`antigravity`、`claude`、`claudedesktop`、`cline`、`codebuff`、`codex`、`crush`、`droid`、`dsh`、`forge`、`gemini`、`goose`、`grok`、`hermes`、`kilo`、`kimi`、`kiro`、`mux`、`openclaude`、`openclaw`、`opencode`、`pi`、`qwen`、`roo`、`workbuddy`、`zed`。
- 其中 4 处防御性修正：`internal/harness/gemini/gemini.go`、`internal/harness/grok/grok.go`、`internal/harness/openclaw/openclaw.go`、`internal/harness/hermes/hermes.go` 的 `Roots()` 由 `return p.roots` 改为返回拷贝；`internal/harness/hermes/hermes.go` 的 `NewWithRoots` 由 `&Parser{roots: roots}` 改为 `append([]string(nil), roots...)`。
- 新增 `internal/harness/rooted_test.go`、`internal/harness/rooted_internal_test.go`。
- 新增本文件。

未触碰 `internal/model/**`、scanner、`internal/query/**`、`internal/store/**`、`internal/gui/**`、`internal/cli/**`、`internal/app/**`、`task_plan.md`、`docs/superpowers/**`；未新增 harness 包、未提交、未发布、未使用真实数据或凭据。

## 未关闭的门（本轮不构成完成声明）

1. **不是多实例交付**：没有任何调用方使用 `NewRooted`；每实例 roots 的来源、持久化、命名与冲突策略、query/store/cli/gui 接线全部未做。
2. **产品默认未确认**：计划 §5 的每实例默认值/行为属未确认产品决策，本轮明确未实现，也未改动任何既有默认 roots。
3. **未跑全库/性能测试**：按任务约束只跑 `./internal/harness/...` 与 `-race`；`internal/cli`、`internal/gui`、`internal/query`、`internal/store` 正被其他 agent 并发修改，`go build ./...` 通过但未跑其测试。
4. **能力只有"有/无"**：`NewRooted` 不校验 roots 是否绝对路径、是否存在，也不做安全检查；调用方需自行保证输入边界。
5. **单例仍是零配置唯一入口**：本轮只增加旁路构造，未改变 `Register`/`All`/`Get` 语义，也未提供注销/替换注册的 API。
