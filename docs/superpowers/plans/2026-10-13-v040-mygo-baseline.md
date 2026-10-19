# MyToken 0.4.0 MyGo Baseline Implementation Plan

> **For agentic workers:** Execute inline using executing-plans; subagents cannot be used unless their model is explicitly one of the two user-approved models. Steps use checkbox syntax. User approved current workspace, no worktree and no automatic commits.

**Goal:** Upgrade MyGo from v0.3.6 to v0.4.8 without simultaneous UI rewrites, recording repeatable compatibility, screenshot and performance evidence.

**Architecture:** Keep all current GUI views and business algorithms unchanged. Separate dependency-only upgrade from later native component migration. Existing headless screenshot and tray construction tests provide comparable app-level evidence, not real-window GPU or accessibility certification.

**Tech Stack:** Go 1.27.1, MyGo v0.4.8, modernc SQLite, native Go UI.

---

## Files and boundaries
- Modify `go.mod` and `go.sum`: only required MyGo upgrade graph.
- Read `.github/workflows/release.yml`, `mygo.json`, `Makefile`, `internal/desktop` if present: packaging and updater compatibility. Change only demonstrated incompatibility, with a regression test first for application behavior.
- Use `internal/gui/snapshot_test.go` and `internal/gui/perf_test.go`: existing deterministic demo rendering and first-build benchmark.
- Create `docs/verification/v040-mygo-baseline.md`: evidence, limitations and outstanding release gates.
- Use ignored `build/verification/v040/before` and `after` for outputs. Do not capture real user logs or credentials.

## Task 1: Pre-upgrade baseline
- [x] Run `go test ./...`; expected exit 0. Observed all packages passed.
- [x] Run screenshots:
```sh
MYTOKEN_SNAPSHOTS="$PWD/build/verification/v040/before" MYTOKEN_SNAP_SCALE=1 go test ./internal/gui -run '^TestSnapshots$' -count=1 -timeout=15m
```
Expected: 46 PNGs (28 populated main pages, 4 tray views, 14 empty views), no test failures. Inspect overview and tray in light/dark; all images use synthetic data.
- [x] Run baseline after screenshot job finishes to avoid performance interference:
```sh
go test ./internal/gui -run '^TestTrayFirstFrame$' -count=3 -v
go test ./internal/gui -run '^$' -bench '^BenchmarkTrayFirstFrame$' -benchtime=3x -benchmem -count=3
go vet ./...
go build -o build/verification/v040/before/mytoken .
```
Expected exit 0. Record best first-view build duration separately from tester rasterization benchmark. Budget is 50ms; no GPU inference.

## Task 2: Upgrade the dependency
- [x] After baseline is captured, replace in `go.mod`:
```diff
-	github.com/egoist/mygo v0.3.6
+	github.com/egoist/mygo v0.4.8
```
- [x] Resolve graph with `go mod tidy`; inspect exact go.mod/go.sum diff for unrelated dependency changes. Read go.sum before tool changes.
- [x] Run `go test ./...` and `go vet ./...`. If a failure occurs, diagnose before changing app code, add a failing focused regression test for the required behavior, then make the smallest compatibility fix. No speculative control rewrites.
- [x] Build `go build -o build/verification/v040/after/mytoken .`.

## Task 3: Compatibility audit
- [x] Confirm application has no Alert callsites depending on Escape sentinel. Repository Go search for Alert/Confirm/ShowMessage/MessageDialog returned no matches before upgrade; repeat after upgrade.
- [x] Inspect pinned MyGo installer generation for architecture naming; compare with release upload patterns `build/*/*.dmg`, `build/*/*Setup*.exe` and existing final filename normalization. Verify updater archives/manifests remain unrenamed.
- [x] Confirm native build does not require generated TypeScript client. Search build scripts and config, then compile.
- [x] Compile Windows/Linux targets into separate ignored verification directories where feasible; label as cross-compilation, not runtime tests. Do not publish or invoke signed update distribution.

## Task 4: Compare and checkpoint
- [x] Repeat Task 1 screenshot and performance commands using `after` output directory.
- [x] Inspect rendered PNGs with read_image and compare dimensions/counts. Explain actual changes rather than declaring pixel equivalence without checking.
- [x] Record tests, exact metrics, dependency diff and compatibility findings in `docs/verification/v040-mygo-baseline.md`.
- [x] List unverified real-window input/IME, emoji editing, focus, reduced-motion interactions, accessibility, DPI, idle/active RSS/CPU/GPU, Windows multi-monitor, Linux bounds, installation and signed-update gates. Do not equate headless frames with these checks.
- [x] Review final diff; update progress and task_plan. Continue to next subsystem plan without automatic commit, push or release.

## Later subsystem plans
The parent scope and accepted contracts are in `task_plan.md`. Identity/migration, paths/diagnostics, snapshot/calendar, budgets/statusline, reports, quotas and native widgets each receive a separate test-first implementation plan after their existing interfaces are inspected. This file only authorizes the dependency baseline batch, not invented API contracts for unexplored modules.
Self-review: covers ROADMAP upgrade-first and breaking-change audit; broad 0.4.0 requirements remain tracked as later phases, not claimed complete.
