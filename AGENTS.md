# AGENTS.md

MyToken!!!!! is a local, read-only spend and plan-quota dashboard for AI coding tools. Go 1.27,
native UI via MyGo v0.7.1, no cgo. Read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) before
changing the data layer.

## Commands

```sh
go run .                 # GUI when stdout is not a terminal
go run . gui             # force the GUI
go run . summary         # CLI; read-only
go test ./...            # full suite
GOOS=windows go vet ./...  # portability gate
gofmt -l .               # must be empty
GUI_SHOTS=/tmp/shots GUI_SHOTS_LANG=en go test -run TestScreenshots ./internal/gui
```

`make check` runs gofmt, vet and tests. Never let tests touch the real index: use `t.TempDir()` or
`MYTOKEN_HOME`.

## Repo map

- `internal/app` — wires store, scanner, pricing, query, quota; `Open` / `OpenLocal`.
- `internal/attrib` — provider resolution: rules, log, cc-switch, config timeline, inference.
- `internal/claudequota` — Claude Code OAuth usage client.
- `internal/cli` — summary, daily, quota, scan, statusline, doctor; all read-only except scan.
- `internal/gui` — MyGo app, pages, tray, i18n, brand icons, install hints.
- `internal/harness` — parser contract and registry, 32 readers, `harnesstest` conformance suite.
- `internal/ingestrate` — live ingestion rate tracker for the UI.
- `internal/model` — shared types (`Tokens`, `UsageEvent`, `SessionMeta`, harness ids).
- `internal/paths` — data directory and DB path resolution.
- `internal/pricing` — models.dev + LiteLLM catalog, snapshot, custom rules.
- `internal/provider` — provider display identity independent of pricing labels.
- `internal/query` — read API: totals, daily, usage, sessions, price rules, aliases.
- `internal/quota` — plan service and provider clients (Codex, Claude, Cursor, …).
- `internal/reporttime` — reporting-zone day/hour arithmetic (DST-safe).
- `internal/scan` — full/quick/watched/poll scanner, fsnotify plan, parser-version resets.
- `internal/snapshot` — read-only views for CLI diagnostics and statusline.
- `internal/source` — cc-switch and harness-config readers for attribution.
- `internal/sqlitedsn` — cross-platform SQLite file URIs.
- `internal/store` — schema, migrations, rollups, dedup, read-only pool.
- `internal/sysproxy` — detects the OS proxy for outbound HTTP.

## Hard constraints

- **No cgo.** Keep `CGO_ENABLED=0` builds working on macOS, Windows and Linux.
- **Readers are read-only.** Harness readers open a tool's logs without writing; SQLite sources are
  `query_only`. Store only numbers plus metadata and a 60-rune session title. Never store message
  text, tool output, reasoning or credentials.
- **Quotas come from servers only.** Fetch with the login the tool already stored; never derive a
  quota from local logs and never refresh or rewrite a stored token. `Source` must be `"api"`.
- **Cost is official API list price.** models.dev first, LiteLLM fallback, user custom per-model
  prices and aliases. Label it API-equivalent. No multipliers, discounts or relay bills. An unknown
  price is *unpriced*, never zero.
- **Bump the parser version when its output changes.** Implement `ParserVersion()` and
  `SourceSessions`; the setting is `harness.<id>.parser_version`. Rotated-away logs keep history.
- **Every UI string needs English and Chinese.** Add both to `internal/gui/i18n.go`; no literals.
- **Animate only while live.** Animations stop when the window is hidden; check `p.After` usage.
- **Tests use `t.TempDir()` or `MYTOKEN_HOME`.** They must never open the real user index.

## Performance budgets

Real-data copy, ~255K events. Do not regress these; measure before claiming a fix.

| Budget | Target | Measure |
| --- | --- | --- |
| Full scan | ≤ 15 s | `MYTOKEN_REAL_PERF=1 go test ./internal/scan -run TestCopiedDatabaseScanPerformance -v` on a copy |
| Incremental scan | ≤ 25 ms | time a watched scan after appending one event |
| Idle CPU | < 0.5 % | `ps -o %cpu` while idle |
| File descriptors | < 150 | `lsof -p <pid> \| wc -l` |
| UI refresh | < 25 ms | time a reload; it must run off the UI thread |
| Frame build+layout+paint | ≤ 8 ms | `MYGO_FRAME_STATS=1` while switching, hovering, scrolling |
| CLI read-only | < 150 ms | `time ./mytoken summary` |

## MyGo pitfalls

- `.Ellipsis()` only works with `.SingleLine()` or `.MaxLines()`.
- `Painter.Line` is axis-aligned only; use a `Path` + `StrokePath` for slanted lines.
- Absolute children paint above in-flow siblings; layer deliberately.
- A `Transition` Enter only fires if the parent existed last frame.
- Views are immediate-mode and rebuilt every frame: keep build cheap, no allocation storms, and
  load data off the UI thread via the job/`Post` pattern in `internal/gui/app.go`.
- `AnimateTo`/`Animate` cause rebuilds; `AnimatePose`, `ContentTransition` and `Draw` with
  `p.Now()` + `p.AnimationFrame()` are paint-only. Prefer paint-only for continuous motion.
- `ui.Tester` cannot render shaders; screenshot tests cover what it cannot assert.

## Pre-commit checklist

```sh
gofmt -l .                       # empty
go vet ./...
GOOS=windows go vet ./...
go test ./...
```

- New harness: package, model constant, `Register`, blank import in `internal/app/app.go`,
  conformance fixture + golden, README row, brand icon, install hint.
- New quota provider: registered in `internal/quota/providers`, server-only, `httptest` tests.
- New string: both languages. New price behavior: log cost still wins.
- No binaries, test databases, editor state or AI transcripts in the commit.
- Commit subject: `area: imperative summary`.
