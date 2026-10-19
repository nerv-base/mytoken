# 0.4.0 MyGo 独立升级验证

日期：2026-10-13。环境：Go 1.27.1、darwin/arm64、Apple M5。基线提交：2e696f2（当前工作区，保留用户已有文档修改）。

## 结论与范围

已将 MyGo v0.3.6 升至 v0.4.8。依赖差异仅为 go.mod 中版本及 go.sum 中对应两个校验和；未改动业务逻辑、历史数据或原生控件实现。现有自动化检查通过，本批可继续后续实现；这不是完整 0.4.0 候选完成声明，也不是正式发布许可。

## 自动化结果

| 验证 | 升级前 | 升级后 |
|---|---|---|
| `go test ./...` | 通过 | 通过 |
| `go vet ./...` | 通过 | 通过 |
| macOS arm64 `go build` | 通过 | 通过 |
| Windows amd64 / arm64，CGO_ENABLED=0 | 本批未测 | 均通过交叉编译 |
| Linux amd64 / arm64，CGO_ENABLED=0 | 本批未测 | 均通过交叉编译 |
| 固定 demo、1x headless 截图 | 46张 | 46张，名称与尺寸全部一致 |

产物与原始日志位于被git忽略的 `build/verification/v040/before` 和 `after`。截图命令：

```sh
MYTOKEN_SNAPSHOTS="$PWD/build/verification/v040/before" MYTOKEN_SNAP_SCALE=1 go test ./internal/gui -run '^TestSnapshots$' -count=1 -timeout=15m
# 升级后将 before 改成 after
```

46张中36张PNG逐字节相同。其余10张用Go标准库PNG解码逐像素比较：
- 4张会话页（中英文/明暗）：各8像素不同，范围(664,672)–(1236,813)，占0.00073%，位于请求时间线。
- 2张搜索空状态：各7像素不同，范围(594,110)–(602,119)，占0.00064%，位于输入控件图标附近。
- 4张托盘页：各12–13像素不同，范围局限于(19,56)–(32,60)，占0.00679%–0.00735%，位于小型动态标记。

已人工查看升级前后的中文概览、英文暗色托盘、英文会话截图，未见布局或主题变化；不声称全部逐像素等价。差异根因未全部隔离，不能仅凭像素少就推断渲染器实现完全相同。

## 性能

依次运行截图和性能测试，避免两者并行争用；未运行真实窗口或GPU采样。

```sh
go test ./internal/gui -run '^TestTrayFirstFrame$' -count=3 -v
go test ./internal/gui -run '^$' -bench '^BenchmarkTrayFirstFrame$' -benchtime=3x -benchmem -count=3
```

| 指标（3次样本） | v0.3.6 | v0.4.8 |
|---|---|---|
| 预热后首视图构建，各次best-of-5 | 63.25 / 53.875 / 70.416 µs | 68.542 / 63.292 / 74.166 µs |
| CPU headless完整栅格化，ns/op | 959358083 / 954851930 / 1012510958 | 978636514 / 998019611 / 979066556 |
| B/op | 1420885 / 1365936 / 1349648 | 1525202 / 1479360 / 1492373 |
| allocs/op | 11026 / 9528 / 9057 | 10426 / 9162 / 9537 |

首视图构建均低于现有50ms预算（此测试默认未启用严格失败环境开关，但已读出数值核对）。CPU栅格化中位数约959.36ms→979.07ms（+2.1%）；B/op中位数约1.366MB→1.492MB（+9.3%）。样本少，仅提供基线，不作显著性结论。CPU tester benchmark不是应用GPU首帧时延，更不是“50ms预算失败”；实际内存/CPU/GPU仍须真机取证。

## 兼容性审计

- **Alert Escape**：v0.4.8 `ui/feedback.go` 定义 `AlertDismissed = -2`。应用Go代码搜索Alert/Confirm/ShowMessage/MessageDialog无调用点，不需要改现有逻辑；后续引入对话框须显式处理关闭与确认。
- **安装包命名**：v0.4.8 `cmd/mygo/nsis.go:265` 生成 `<Name> Setup <version> <arch>.exe`；`cmd/mygo/dmg.go:22` 对单架构增加arch，对universal保留无arch形式。现有release workflow的`*Setup*.exe`与`*.dmg`上传模式仍匹配，发布步骤按父平台目录统一命名，所以不会因新增arch后缀丢包。tar.gz、delta、update-*.json走原名分支，不改签名引用的资源名。这里只完成源码对照，尚未运行NSIS、DMG、签名更新/回滚链路。
- **TypeScript客户端**：这是Go原生UI应用；Makefile与GitHub workflow的构建为Go路径，无npm/pnpm/tsc步骤。原生与跨平台Go编译通过，未引入缺失的TS客户端依赖。
- 本批未运行发布、安装或真实用户扫描，未触碰凭据；未自动提交或推送。

## 仍属发布门槛

- 真实窗口焦点、Escape/Enter、中文IME、emoji编辑、键盘导航与读屏可访问性。
- 明暗切换、ReduceMotion交互、不同DPI、滚动与文本裁切。
- 托盘真实开窗延迟、持续运行RSS、空闲/活跃CPU与GPU。
- Windows多屏与缩放，Linux窗口边界/托盘实际行为，macOS实际安装。
- DMG/NSIS/linux包安装、签名manifest下载及升级回滚。
- 后续功能所需的真实harness日志与明确授权的额度访问证据。

这些项目未标为通过；headless截图和交叉编译不能替代它们。
