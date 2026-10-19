# Contributing

Thanks for helping. Small changes are welcome as a pull request without any ceremony: bug fixes, a
new harness, a new quota source, a translation, a documentation correction. For a new feature or a
change to the interface, open an issue first so the design can be agreed before you spend time on it.

Useful references: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how the data flows, and
[docs/CLI.md](docs/CLI.md) for the command-line surface.

## Development setup

- Go **1.27** or newer (the exact version is in [go.mod](go.mod)).
- **No cgo.** The SQLite driver is pure Go and every command builds with `CGO_ENABLED=0`; do not add
  a dependency that needs a C compiler.
- In China, a module proxy makes the first build much faster:

  ```sh
  export GOPROXY=https://goproxy.cn,direct
  ```

Running the app needs macOS 12+, Windows 10+, or a Linux desktop with GTK 3 and WebKitGTK 4.1. The
tests and every CLI command need none of that.

## Build and run

```sh
go run .            # opens the GUI when stdout is not a terminal
go run . gui        # always the GUI, even from a terminal
go run . summary    # the CLI: summary, daily, quota, scan, statusline, doctor, version
go run . scan       # re-index now; the only CLI command that writes
```

`go run .` from a terminal prints the read-only summary instead of opening a window, because the
binary is both the app and the CLI. Use `go run . gui` to force the window. `make build`, `make run`
and `make check` wrap the same commands; packaging targets are `make app`, `make dmg`, `make linux`
and `make windows` (they call `go tool mygo build`, pinned in `go.mod`). `make help` lists them.

Environment variables that help while developing:

| Variable | Effect |
| --- | --- |
| `MYTOKEN_HOME` | Use a separate index directory instead of the real one. Also disables the single-instance lock, so a development build can run beside the installed app. |
| `MYTOKEN_PPROF` | Address for `net/http/pprof`, for example `MYTOKEN_PPROF=127.0.0.1:6060`. Never enable it in a release build. |
| `MYGO_FRAME_STATS=1` | Have MyGo print frame timing to stderr. Use it when checking the 8 ms frame budget. |
| `MYTOKEN_LANG` | Force `en` or `zh` instead of the system language. |

Point `MYTOKEN_HOME` at a scratch directory when experimenting. Tests must never open the real index
(`~/Library/Application Support/MyToken`, `%APPDATA%\MyToken`, `~/.local/share/mytoken`); use
`t.TempDir()` or `MYTOKEN_HOME`.

## Tests

```sh
go test ./...                     # everything
GOOS=windows go vet ./...         # catch non-portable path and syscall assumptions
gofmt -l .                        # must print nothing
go vet ./...
make check                        # gofmt check, vet and tests
```

CI runs vet and tests on macOS, Linux and Windows, checks `gofmt -l .`, and runs the 200K-event
query benchmark. Run at least `go test ./internal/...` before opening a pull request; run the full
`go test ./...` when touching the store, scanner or a parser. `-race` is useful around the scanner
and store:

```sh
go test -race ./internal/scan ./internal/store ./internal/query
```

## GUI screenshot tests

The screenshot test renders the real interface against demo data and writes light and dark variants.
It only runs when asked to:

```sh
GUI_SHOTS=/tmp/mytoken-shots GUI_SHOTS_LANG=en go test -run TestScreenshots ./internal/gui
GUI_SHOTS=/tmp/mytoken-shots GUI_SHOTS_LANG=zh go test -run TestScreenshots ./internal/gui
```

`GUI_SHOTS` is the output directory; without it the test skips. `GUI_SHOTS_LANG` is `en` or `zh`
and defaults to `zh`. The test uses a fixed clock and a demo data set, so it never touches your
index.

### Demo mode and README images

`ui.Tester` cannot run shaders, so README images come from a real window instead. Build with the
`demo` tag and the app runs on the same synthetic data as the tests — a fresh temporary index, no
scanning, no network, a clock that starts at 15:30:

```sh
go build -tags demo -o /tmp/mytoken-demo .
MYTOKEN_DEMO_LANG=en MYTOKEN_DEMO_THEME=dark /tmp/mytoken-demo gui
MYTOKEN_DEMO_LANG=zh MYTOKEN_DEMO_THEME=dark MYTOKEN_DEMO_PANEL=1 /tmp/mytoken-demo gui
```

| Variable | Effect |
| --- | --- |
| `MYTOKEN_DEMO_LANG` | `en` or `zh` |
| `MYTOKEN_DEMO_THEME` | `light`, `dark`, or empty for the system |
| `MYTOKEN_DEMO_PANEL` | `1` opens the menu-bar panel after two seconds and keeps it open |

The demo data lives in `internal/gui/demo.go`; release builds do not link it. Capture windows at 2x
with `screencapture -o -l <window-id>` (macOS) so the corners stay transparent, then:

- `docs/screenshots/*.png` — English UI, dark; `docs/screenshots/zh/*.png` — Chinese UI, dark.
  Window shots are scaled to 1920 px wide; the tray panel is kept at 2x.
- `python3 resources/hero-src/compose.py <dir>` builds `docs/hero.png` and `docs/hero-zh.png` from
  `<dir>/{en,zh}-overview.png` and `<dir>/{en,zh}-tray.png`. It needs Pillow and the SF Pro and
  PingFang fonts that ship with macOS.

## Adding a harness

A harness is one package under `internal/harness/<name>` plus a fixture. Read
[docs/ARCHITECTURE.md#parser-contract](docs/ARCHITECTURE.md#parser-contract) first; it defines the
contract in full.

1. **Package and constructor.** Create `internal/harness/<name>/<name>.go` with `New()` and
   `NewWithRoots(roots ...string)`. Use `harness.EnvOr("<ENV>", "relative", ...)` for the usual
   environment-variable override (`$CODEX_HOME` → `sessions`, and so on). The parser must implement:

   ```go
   type Parser interface {
       Harness() model.Harness
       Roots() []string
       Discover(ctx context.Context) ([]Source, error)
       Parse(ctx context.Context, src Source, cur Cursor) (Batch, error)
   }
   ```

2. **Roots.** Embed `harness.Rooted` and implement `WithRoots(roots ...string) harness.Parser`,
   copying the slice. Report only the narrowest directory that holds logs, and expand `~` and
   environment variables. A missing root is not an error: `Discover` returns zero sources and
   `nil`. The GUI's folder picker and per-tool setting go through `Detect` and `ValidateRoot`, so do
   not walk the tree in either: a shallow `ReadDir` is enough to say whether the directory looks
   usable.

3. **Discovery and parsing.** Match the real file names. Keep the cursor rules: consume only
   complete records, return `Batch{Next: cur}` when nothing changed, and make `Next` describe the
   state after the last complete record. A format that is rewritten in place (whole JSON, zstd,
   SQLite) must rely on a stable `DedupKey` plus an accurate cursor; set `Fingerprint` and the
   parser-private `Extra` field as needed. SQLite is opened read-only (`mode=ro`, `query_only(1)`)
   and must not create a WAL, journal or migration.

4. **Identity and dedup.** Add the constant and display name to
   [internal/model/model.go](internal/model/model.go), then register the parser from `init()`:

   ```go
   func init() { harness.Register(New()) }
   ```

   Add the package to the blank imports in [internal/app/app.go](internal/app/app.go). Without it
   the parser is invisible to scanning, `doctor` and the GUI while its own tests still pass.

5. **Conformance.** Add `conformance_test.go` using `harnesstest.Run` with
   `harnesstest.Case{Name, New, Fixture}`, and put the fixture in
   `internal/harness/<name>/testdata/conformance/<name>/` (the fixture root must be exactly the directory
   `Discover` reads). Cover two sessions (parent and child when the format supports it), two models,
   cache read and write, reasoning, a record without usage and a fixed UTC timestamp. Put
   `harnesstest.SentinelText` in assistant, tool or reasoning text — never in the first user message,
   because that legitimately becomes the title. Generate the golden file with:

   ```sh
   go test ./internal/harness/<name> -run Conformance -update -count=1
   ```

   Then run it without `-update` and `-count=1`, and the whole suite:
   `go test ./internal/harness/... -count=1`.

6. **Parser versions.** When a change alters what the parser outputs, bump its version by
   implementing `ParserVersion() int` (the `harness.Versioned` interface) and implement
   `harness.SourceSessions` so the reset can be scoped to the sources the parser currently reads.
   The scanner stores the version as `harness.<name>.parser_version`; a change triggers a one-time
   re-read of the affected sources and keeps history from logs the tool has already rotated away.

7. **Brand and install hint.** Add the tool's mark under
   [internal/gui/brand](internal/gui/brand) following the notes in its README, add the harness row
   to README.md and README.zh-CN.md, and add its install command and home page to `installHints` in
   [internal/gui/install.go](internal/gui/install.go). The README row is part of the change: the
   root cause of "not detected" is usually a wrong default path.

Read a few existing readers for the shape: `internal/harness/claude/claude.go` (append-only JSONL),
`internal/harness/crush/crush.go` (SQLite with cumulative counters) and
`internal/harness/cline/cline.go` (in-place rewritten task JSON).

## Adding a quota provider

Quota is the one place where MyToken contacts a provider's server. The contract lives in
`internal/quota/plan/provider.go`:

```go
type Provider interface {
    ID() string
    DisplayName() string
    Detect(ctx context.Context) bool
    Fetch(ctx context.Context) (Snapshot, error)
}
```

- **`Snapshot`** has `Provider`, `Plan`, `Windows`, `Daily`, `FetchedAt` and `Source`. `Source`
  must be `"api"`; `Validate` rejects anything else.
- **`Window`** is either a percentage window (`Name`, `UsedPercent`, `Used`, `Limit`, `Unit`,
  `ResetsAt`) or a credit balance (`Balance: true`, `Remaining`, `ExpiresAt`). Balance windows are
  never rendered as percentages and `Remaining` is authoritative. Leave `Daily` to at most 14 days
  of `DayUse{Day, Used, Known}`.
- **`Detect`** is cheap and local: it answers "is a credential for this tool present?" without any
  network call. **`Fetch`** is the only network call, with a short timeout.

Rules that are not negotiable:

- Always fetch the number from the provider's server. Never derive a quota from local usage logs.
- Use the login the tool already stored, and **never refresh, rewrite or copy it**.
- Persist only the last good snapshot and the status; the service handles backoff and caching under
  `quota.plan.<id>` in settings.
- Register the provider in `internal/quota/providers/providers.go`. If the provider needs a new
  settings key, keep it in the same `quota.*` namespace and document it.
- Test `Fetch` against an `httptest.Server`, including the error paths (401, 403, 429 with
  `Retry-After`, malformed JSON, empty windows). Do not put real tokens in tests or fixtures.
- A provider that cannot report a stable number should be left out rather than guessed. Credit
  balances (remaining amount, daily use, runway) are the preferred shape when the service only
  exposes a wallet.

## Translations

Every user-facing string is in `internal/gui/i18n.go`, keyed and stored as
`[2]string{English, Chinese}`. A new string needs both languages; a changed English string needs
the Chinese updated in the same edit. `tr(key)` is the only accessor; do not inline literals in
`internal/gui`. The CLI is English-only by design, but the tray, settings, error messages and
empty states all go through `tr`.

## Commit style

Match the history (`git log --oneline`): a short imperative subject prefixed with the area, then a
body when the *why* is not obvious.

```
scan: skip unchanged SQLite sources by WAL stamp
gui: stop the tray title flicker when the quota refresh fails
quota: DeepSeek balance from the Harness credential
```

Keep one logical change per commit. Do not commit generated binaries, test databases or
screenshots you did not mean to update, and do not commit `.vscode/` or editor state. Never include
AI process logs or transcripts in the repository.

## Releasing

Releases are cut from tags; the workflow does the rest.

```sh
go run ./scripts/setversion v0.6.0   # or: make bump TAG=v0.6.0
git commit -am "release v0.6.0"
git tag v0.6.0
git push origin main --tags
```

The version lives in [mygo.json](mygo.json) and is read by `go tool mygo build`; `setversion`
rewrites only its top-level `"version"` field. Pushing a `v*` tag runs
[.github/workflows/release.yml](.github/workflows/release.yml), which creates a draft release,
builds the macOS universal `.dmg`, the Windows installers, and the Linux `.deb`, tarballs and
`install.sh`, then publishes the release as latest. Builds are ad-hoc signed on macOS and unsigned
on Windows unless the signing secrets at the top of the workflow are present; the updater needs
`MYGO_UPDATER_PRIVATE_KEY`.

`make build`, `make app`, `make dmg`, `make linux` and `make windows` reproduce the artifacts
locally. `make version` prints what the next build would embed. Follow the structure of the existing
entries in [CHANGELOG.md](CHANGELOG.md). Write release notes for users, not a commit list.
