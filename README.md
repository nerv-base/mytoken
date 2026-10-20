<p align="center">
  <img src="docs/hero.png" alt="MyToken!!!!! — where did all your tokens go?" width="100%">
</p>

<h1 align="center">MyToken!!!!!</h1>

<p align="center">
  <a href="https://github.com/zzstar101/mytoken/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/zzstar101/mytoken?style=flat-square&color=5AA9D6&label=release"></a>
  <a href="https://github.com/zzstar101/mytoken/actions/workflows/ci.yml"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/zzstar101/mytoken/ci.yml?branch=main&style=flat-square&label=CI"></a>
  <a href="https://github.com/zzstar101/mytoken/releases"><img alt="Downloads" src="https://img.shields.io/github/downloads/zzstar101/mytoken/total?style=flat-square&color=FF7896"></a>
  <img alt="Platforms" src="https://img.shields.io/badge/platform-macOS%20%7C%20Windows%20%7C%20Linux-50C878?style=flat-square">
  <a href="go.mod"><img alt="Go" src="https://img.shields.io/github/go-mod/go-version/zzstar101/mytoken?style=flat-square&color=F0B43C"></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/github/license/zzstar101/mytoken?style=flat-square&color=7C6CD4"></a>
  <a href="https://github.com/egoist/mygo"><img alt="Built with MyGo" src="https://img.shields.io/badge/built%20with-MyGo-5AA9D6?style=flat-square&labelColor=2B2640"></a>
</p>

<p align="center"><b>⚡ Powered by <a href="https://github.com/egoist/mygo">MyGo</a>!</b></p>

<p align="center"><b>English</b> · <a href="README.zh-CN.md">简体中文</a></p>
<p align="center"><a href="#install">Install</a> · <a href="#screenshots">Screenshots</a> · <a href="#supported-tools">Supported tools</a> · <a href="#plan-quotas">Plan quotas</a> · <a href="#pricing">Pricing</a> · <a href="#privacy">Privacy</a> · <a href="docs/CLI.md">CLI</a> · <a href="CONTRIBUTING.md">Contributing</a></p>

**What did your AI coding cost today, and how much plan quota is left?** MyToken!!!!! reads the
session logs your AI coding tools already write to disk, prices every request at the official API
list price, and shows today's and this month's spend, your plan quotas and what is running right
now — in a native desktop app, a menu-bar panel, or the command line. Everything stays on your
machine.

- **Thirty-two harnesses in one window** — Claude Code, Codex CLI, DSH, pi, Gemini CLI, opencode,
  Crush, Cline, Roo Code, Kilo Code, Claude Desktop, WorkBuddy, CodeBuddy, Hermes, Qwen Code, Kimi,
  OpenClaude, Grok, OpenClaw, Droid, Goose, Forge, Amp, Kiro, Zed, Antigravity, Codebuff, Mux,
  ZCode, MiniMax Code, MiMo Code and Step Code.
- **Session × provider × model** — the five token classes kept apart (input, output, cache read,
  cache write, reasoning), with cache-hit rate, per-project and per-day breakdowns, and sub-agent
  trees.
- **API-equivalent cost** — models.dev prices with a LiteLLM fallback, labelled as what the same
  tokens would cost at list price. Add your own per-model prices when a plan or gateway differs;
  there are no provider multipliers or inferred discounts.
- **Spend first** — today's cost in big type, the month so far with a forecast, daily spend stacked
  by tool, and rankings by cost.
- **Plan quotas** — Codex, Claude, Cursor, Copilot, GLM, Trae, WorkBuddy, Qoder, ZCode and your
  DeepSeek API balance, fetched from each provider and detected automatically. Usage windows show the share
  used and when they reset; credit balances show what is left and how many days that buys.
- **Live sessions** — what is running right now, with its cost so far, spend per hour, cache hit
  rate and an estimated output speed.
- **Menu bar** — today's spend (or spend per hour, cache hit rate, quota) beside the icon; the panel
  shows today, the sessions running now and every quota.
- **CLI for scripts** — `mytoken` prints today's and this month's cost; `mytoken daily` prints a
  ccusage-style table with each cache class in its own column.
- **Native, small, fast** — built with [MyGo](https://github.com/egoist/mygo)'s native UI package:
  no Electron, no WebView, no cgo. A full first scan of this machine's logs takes seconds.

## Screenshots

| Overview | Sessions |
| --- | --- |
| ![Overview: today's spend, sessions running now, this month vs last month with a forecast, plan quotas and credit balances](docs/screenshots/overview.png) | ![Sessions: virtual list, cumulative cost and every request of one session](docs/screenshots/sessions.png) |

| Ranking | Menu bar panel |
| --- | --- |
| ![Ranking: a year of daily spend and cost by tool, model or provider](docs/screenshots/ranking.png) | ![Menu bar panel: today's spend, quotas and the sessions running now](docs/screenshots/tray.png) |

| CLI | Settings |
| --- | --- |
| ![mytoken summary in a terminal](docs/screenshots/cli.png) | ![Settings: detected tools, each with an on/off switch and an optional folder](docs/screenshots/settings.png) |

## Install

### macOS (disk image)

Download `mytoken-<version>-darwin-universal.dmg` from [Releases](https://github.com/zzstar101/mytoken/releases), open it and drag **MyToken!!!!!**
into *Applications*. The bundle is ad-hoc signed rather than notarized, so the first launch needs
right-click → *Open* (or `xattr -dr com.apple.quarantine /Applications/mytoken.app`). The app's file
name is `mytoken.app`; Finder and the menu bar show `MyToken!!!!!`.

### Windows (installer)

Download `mytoken-<version>-windows-amd64-setup.exe` (or `-arm64-`) and run it. The installer is unsigned, so SmartScreen asks
for *More info* → *Run anyway* the first time. It installs `mytoken.exe`, a Start Menu entry and the
uninstaller.

### Linux (install script)

```sh
v=0.5.2 arch=amd64   # or arm64
curl -fLO https://github.com/zzstar101/mytoken/releases/download/v$v/mytoken-$v-linux-$arch.tar.gz
curl -fLO https://github.com/zzstar101/mytoken/releases/download/v$v/install.sh
sh install.sh mytoken-$v-linux-$arch.tar.gz
```

Installs per user, without root: the app in `~/.local/mytoken.app`, the `mytoken` command in
`~/.local/bin` and an entry in the applications menu. `sh install.sh --uninstall` removes it and
keeps your data.

### Linux (Debian package)

```sh
sudo apt install ./mytoken_<version>_amd64.deb
```

Installs `/opt/mytoken`, the `/usr/bin/mytoken` command and a desktop entry. It depends on GTK 3,
WebKitGTK 4.1 and libayatana-appindicator3 (the tray icon).

### With the Go toolchain

```sh
go install github.com/zzstar101/mytoken@latest
```

Builds the same binary the bundles carry, including the CLI, into `$(go env GOPATH)/bin/mytoken`.

## Supported tools

MyToken!!!!! reads the logs each tool already keeps. Nothing is written back: the readers are
read-only and store only numbers plus a session title (the first 60 characters of the first user
message). A dash in the quota column means the tool has no plan quota MyToken can read — its usage
still counts toward spend.

| Tool | ID | Plan quota |
| --- | --- | --- |
| Claude Code | `claude-code` | Claude |
| Codex CLI | `codex` | ChatGPT |
| DSH — DeepSeek Harness | `dsh` | DeepSeek balance |
| pi / OMP / OMO-senpi | `pi` | — |
| Gemini CLI | `gemini` | — |
| opencode | `opencode` | — |
| Crush | `crush` | — |
| Cline | `cline` | — |
| Roo Code | `roo` | — |
| Kilo Code | `kilo` | — |
| Claude Desktop | `claude-desktop` | — |
| WorkBuddy | `workbuddy` | WorkBuddy |
| CodeBuddy | `codebuddy` | — |
| Hermes | `hermes` | — |
| Qwen Code | `qwen` | — |
| Kimi / Kimi Code | `kimi` | — |
| OpenClaude | `openclaude` | — |
| Grok | `grok` | — |
| OpenClaw | `openclaw` | — |
| Droid | `droid` | — |
| Goose | `goose` | — |
| Forge | `forge` | — |
| Amp | `amp` | — |
| Kiro | `kiro` | — |
| Zed | `zed` | — |
| Antigravity | `antigravity` | — |
| Codebuff | `codebuff` | — |
| Mux | `mux` | — |
| ZCode | `zcode` | — |
| MiniMax Code | `minimax-code` | — |
| MiMo Code | `mimo-code` | — |
| Step Code | `step-code` | — |

Cursor, Trae, GitHub Copilot, GLM and Qoder are quota sources without a session-log reader: their
local formats either report no usable tokens (Cursor's per-message `tokenCount` is always zero,
Qoder carries credits rather than tokens) or are encrypted (Trae CN). See
[Plan quotas](#plan-quotas).

<details>
<summary>Where the logs live</summary>

| Tool | ID | Where the logs live |
| --- | --- | --- |
| Claude Code | `claude-code` | `~/.claude/projects/**/*.jsonl` (`$CLAUDE_CONFIG_DIR/projects`), plus `~/.config/claude/projects` when it exists |
| Codex CLI | `codex` | `$CODEX_HOME/sessions` and `$CODEX_HOME/archived_sessions` (`~/.codex` by default) |
| DSH — DeepSeek Harness | `dsh` | `$DSH_HOME/sessions` (`~/.dsh/sessions` by default) |
| pi / OMP / OMO-senpi | `pi` | `$PI_HOME/sessions` when set; otherwise `~/.pi/agent/sessions` plus detected OMP and OMO/senpi agent roots |
| Gemini CLI | `gemini` | `$GEMINI_CLI_HOME/tmp` (`~/.gemini/tmp` by default) |
| opencode | `opencode` | `$OPENCODE_DATA_HOME`, else `$XDG_DATA_HOME/opencode`, else `~/.local/share/opencode` |
| Crush | `crush` | `$CRUSH_GLOBAL_DATA`, else `$XDG_DATA_HOME/crush`, else `~/.local/share/crush` |
| Cline | `cline` | `…/globalStorage/saoudrizwan.claude-dev/tasks`, plus `~/.cline/data/tasks` and `~/.cline/data/sessions` |
| Roo Code | `roo` | `…/globalStorage/rooveterinaryinc.roo-cline/tasks` |
| Kilo Code | `kilo` | `…/globalStorage/kilocode.kilo-code/tasks` |
| Claude Desktop | `claude-desktop` | `…/Claude/local-agent-mode-sessions` and its `Claude-3p` sibling (macOS: `~/Library/Application Support`, Windows: `%APPDATA%`), plus the Cowork usage ledger |
| WorkBuddy | `workbuddy` | `$WORKBUDDY_HOME/projects` (`~/.workbuddy/projects` by default) |
| CodeBuddy | `codebuddy` | `$CODEBUDDY_HOME/projects` (`~/.codebuddy/projects` by default) |
| Hermes | `hermes` | `$HERMES_HOME` (`~/.hermes` by default), SQLite |
| Qwen Code | `qwen` | `$QWEN_DATA_DIR/projects` (`~/.qwen/projects` by default) |
| Kimi / Kimi Code | `kimi` | Legacy `$KIMI_SHARE_DIR/sessions` (`~/.kimi/sessions`) plus `$KIMI_CODE_HOME/sessions` (`~/.kimi-code/sessions`) |
| OpenClaude | `openclaude` | `$CODEBURN_OPENCLAUDE_DIR/projects`, else `~/.openclaude/projects` |
| Grok | `grok` | `$GROK_HOME` (`~/.grok` by default) |
| OpenClaw | `openclaw` | `~/.openclaw/agents`, plus the `~/.moltbot`, `~/.moldbot` and `~/.clawdbot` spellings |
| Droid | `droid` | `$FACTORY_DIR` (`~/.factory` by default) |
| Goose | `goose` | `$GOOSE_PATH_ROOT/…`, else `~/.local/share/goose/sessions` (Windows: `%APPDATA%\Block\goose\sessions`), SQLite |
| Forge | `forge` | `~/.forge` by default, SQLite |
| Amp | `amp` | `$AMP_DATA_DIR` (comma-separated), else `~/.local/share/amp` |
| Kiro | `kiro` | `$KIRO_HOME` (`~/.kiro` by default), plus the platform Kiro `globalStorage` agent directory |
| Zed | `zed` | `…/Zed/threads/threads.db` (macOS: `~/Library/Application Support`, Windows: `%LOCALAPPDATA%`, Linux: `~/.local/share/zed`), SQLite |
| Antigravity | `antigravity` | `$GEMINI_CLI_HOME/antigravity{,-cli,-ide}/conversations` and the matching `implicit` dirs (`~/.gemini` by default), only `*.db` |
| Codebuff | `codebuff` | `$CODEBUFF_DATA_DIR`, else `~/.config/manicode`, `~/.config/manicode-dev` and `~/.config/manicode-staging` |
| Mux | `mux` | `$CODEBURN_MUX_DIR` or `$MUX_ROOT`, else `~/.mux`, `~/.mux-dev` and `~/.cmux` |
| ZCode | `zcode` | `$MYTOKEN_ZCODE_DIRS`, else `~/.zcode/cli/db/db.sqlite`, query-only SQLite |
| MiniMax Code | `minimax-code` | `$MINIMAX_DATA_DIR` / `$MAVIS_DATA_DIR`, else `~/.minimax/v2/sqlite/runtime-state.sqlite` (legacy `~/.mavis`) |
| MiMo Code | `mimo-code` | `$MIMOCODE_HOME/data`; otherwise `$XDG_DATA_HOME/mimocode` or `~/.local/share/mimocode` on every OS, query-only SQLite |
| Step Code | `step-code` | `$STEP_CODING_AGENT_SESSION_DIR`, `$STEP_CODING_AGENT_DIR/sessions`, else `~/.stepcode/agent/sessions` |

For the VS Code family extensions, `…/globalStorage` is resolved per platform and per editor —
editors: VS Code, VS Code Insiders, Cursor, Windsurf, VSCodium, Trae; platforms:
`~/Library/Application Support/<editor>/User/globalStorage` on macOS,
`~/.config/<editor>/User/globalStorage` (or `$XDG_CONFIG_HOME/…`) on Linux, and
`%APPDATA%\<editor>\User\globalStorage` on Windows.

Every reader can be pointed somewhere else for non-default installs, containers and archived logs:

| Variable | Effect |
| --- | --- |
| `MYTOKEN_HOME` | Where the index lives (all platforms) |
| `CLAUDE_CONFIG_DIR` | Claude Code config dir instead of `~/.claude` |
| `CODEX_HOME` | Codex data dir instead of `~/.codex` |
| `DSH_HOME` | DSH data dir instead of `~/.dsh` |
| `PI_HOME`, `MYTOKEN_PI_DIRS` | pi data dir, or explicit pi-compatible session roots (path list) |
| `PI_CODING_AGENT_DIR`, `OMP_PROFILE`, `PI_PROFILE`, `PI_CONFIG_DIR` | OMP agent/profile roots reused by the `pi` reader |
| `OMO_CODING_AGENT_DIR`, `SENPI_CODING_AGENT_DIR` | OMO/senpi agent roots reused by the `pi` reader |
| `GEMINI_CLI_HOME`, `MYTOKEN_GEMINI_DIRS` | Gemini CLI data dir, or explicit dirs (path list) |
| `OPENCODE_DATA_HOME`, `MYTOKEN_OPENCODE_DIRS` | opencode data dir, or explicit dirs (path list) |
| `CRUSH_GLOBAL_DATA`, `MYTOKEN_CRUSH_DIRS` | Crush data dir, or explicit dirs (path list) |
| `CLINE_DIR`, `CLINE_DATA_DIR`, `CLINE_SESSION_DATA_DIR` | Cline data dirs |
| `MYTOKEN_CLINE_DIRS`, `MYTOKEN_ROO_DIRS`, `MYTOKEN_KILO_DIRS` | Explicit Cline / Roo / Kilo dirs (path list) |
| `MYTOKEN_CLAUDEDESKTOP_DIRS` | Explicit Claude Desktop session dirs (path list) |
| `WORKBUDDY_HOME`, `CODEBUDDY_HOME` | WorkBuddy / CodeBuddy home dir |
| `HERMES_HOME` | Hermes home dir instead of `~/.hermes` |
| `QWEN_DATA_DIR`, `MYTOKEN_QWEN_DIRS` | Qwen Code data dir, or explicit dirs (path list) |
| `KIMI_SHARE_DIR`, `KIMI_CODE_HOME`, `MYTOKEN_KIMI_DIRS` | Kimi / Kimi Code data dirs, or explicit dirs (path list) |
| `CODEBURN_OPENCLAUDE_DIR`, `MYTOKEN_OPENCLAUDE_DIRS` | OpenClaude data dir, or explicit dirs (path list) |
| `GROK_HOME`, `MYTOKEN_GROK_DIRS` | Grok home dir, or explicit dirs (path list) |
| `MYTOKEN_OPENCLAW_DIRS` | Explicit OpenClaw agent dirs (path list) |
| `FACTORY_DIR`, `MYTOKEN_DROID_DIRS` | Droid home dir, or explicit dirs (path list) |
| `GOOSE_PATH_ROOT`, `MYTOKEN_GOOSE_DIRS` | Goose data root, or explicit dirs (path list) |
| `MYTOKEN_FORGE_DIRS` | Explicit Forge dirs (path list) |
| `AMP_DATA_DIR`, `MYTOKEN_AMP_DIRS` | Amp data dirs (comma-separated), or explicit dirs (path list) |
| `KIRO_HOME`, `MYTOKEN_KIRO_DIRS` | Kiro home dir, or explicit dirs (path list) |
| `MYTOKEN_ZED_DIRS` | Explicit Zed threads dirs (path list) |
| `MYTOKEN_ANTIGRAVITY_DIRS` | Explicit Antigravity conversation dirs (path list) |
| `CODEBUFF_DATA_DIR`, `MYTOKEN_CODEBUFF_DIRS` | Codebuff data dir, or explicit dirs (path list) |
| `CODEBURN_MUX_DIR`, `MUX_ROOT`, `MYTOKEN_MUX_DIRS` | Mux data dir, or explicit dirs (path list) |
| `MYTOKEN_ZCODE_DIRS` | Explicit ZCode database files or containing dirs (path list) |
| `MINIMAX_DATA_DIR`, `MAVIS_DATA_DIR`, `MYTOKEN_MINIMAX_DIRS` | MiniMax Code data root, legacy root, or explicit roots (path list) |
| `MIMOCODE_HOME`, `MIMOCODE_DB`, `MYTOKEN_MIMO_CODE_DIRS` | MiMo Code data root, exact database, or explicit roots (path list) |
| `STEP_CODING_AGENT_SESSION_DIR`, `STEP_CODING_AGENT_DIR`, `MYTOKEN_STEP_CODE_DIRS` | Step Code session root, agent root, or explicit roots (path list) |

</details>

Which readers have been checked against real logs, and which are conformance-only, is tracked in
[Harness coverage status](docs/ARCHITECTURE.md#harness-coverage-status).

## Plan quotas

Plan quota is the one thing MyToken fetches from the network, and it always asks the provider's own
server with the login that tool already stored on disk. It never refreshes, rewrites or copies that
login, and it never derives a quota from local usage.

| Source | What you get |
| --- | --- |
| Codex | ChatGPT plan usage windows, from the Codex sign-in |
| Claude | Claude plan usage windows, from the Claude Code sign-in |
| Cursor | Cursor plan usage, from the Cursor app login |
| GitHub Copilot | Copilot usage, from the Copilot sign-in |
| GLM | Z.ai / BigModel Coding Plan limits, from `Z_AI_API_KEY` or `BIGMODEL_API_KEY` |
| Trae (CN / Global) | Trae plan usage, from the app login |
| WorkBuddy | WorkBuddy plan usage, from the app login |
| Qoder | Qoder plan usage, from the app login |
| ZCode | ZCode Start Plan token grants, from the ZCode app login |
| DeepSeek | API account balance, from the DeepSeek Harness credential or `DEEPSEEK_API_KEY` |

A quota card only appears for a provider whose credential MyToken finds. Percentage windows show
used share and reset time; credit balances show what remains, the daily burn and the runway in days.
When a fetch fails the last good snapshot stays visible and the next attempt backs off; no local
number is substituted.

## Pricing

- Prices come from [models.dev](https://models.dev), with LiteLLM's
  [`model_prices_and_context_window.json`](https://github.com/BerriAI/litellm) as the fallback and a
  bundled offline snapshot for the first run. The table is cached on disk for 24 hours and the app
  works entirely offline with the cached copy.
- Costs are **API-equivalent**: what the same tokens would cost at official list price. Each of the
  five token classes is priced separately; reasoning tokens fall back to the output rate.
- **Custom prices and mappings.** When a plan, gateway or negotiated rate differs, set USD prices
  per million tokens (input, output, cache read, cache write) for a model in *Settings → Prices*.
  Model names your own config invented can be mapped to the real model and an optional provider, so
  they are priced and merge into the rankings. There are no provider multipliers or inferred
  discounts.
- The 1-hour part of a cache write costs twice the input rate; the rest costs the catalog's
  cache-write price, or 1.25× input when the catalog has none.
- A cost recorded in the log itself always wins over the computed one, and requests that no price
  can be found for are reported as *unpriced* instead of silently costing nothing.
- When a log does not name the account or gateway, the provider is resolved from the log itself,
  then cc-switch's read-only request log, then the base URL recorded in the tool's config at the
  time of the request, then the model name — and your own rules win.

## Privacy

- **Local only.** The index, the price cache and the settings never leave your machine. There is no
  telemetry, no analytics, no crash reporting, no account and no sync.
- **The complete network list:** the price table (models.dev, falling back to LiteLLM), a daily
  update check against github.com on builds that ship the desktop updater (never the CLI), and
  plan-quota fetches against the provider's own service when that tool's credential is already on
  disk. Nothing else is contacted.
- **Read-only readers.** Tool logs are opened for reading; the index is a separate SQLite file
  that you can delete at any time to start over.
- **No conversation content.** Only numbers and metadata are stored. The single exception is the
  session title: the first 60 characters of the first user message, so the session list is readable.

| Platform | Index directory |
| --- | --- |
| macOS | `~/Library/Application Support/MyToken` |
| Windows | `%APPDATA%\MyToken` |
| Linux | `$XDG_DATA_HOME/mytoken` (usually `~/.local/share/mytoken`) |

The database is `mytoken.db` inside it. Set `MYTOKEN_HOME` to keep it elsewhere (handy for trying
the app against a scratch index); delete the directory to reset.

## Command line

The app binary is also the CLI. A bare `mytoken` in a terminal prints the read-only cost summary;
from Finder, Explorer or a launcher it is the app, and `mytoken gui` always opens the window (the
login item uses `--hidden`). The full reference is in [docs/CLI.md](docs/CLI.md).

```sh
mytoken                                        # today and this month, API-equivalent cost
mytoken daily                                  # one row per day, last 30 days
mytoken daily --days 7 --json                  # scripting
mytoken quota                                  # plan quota fetched from the providers' servers
mytoken scan                                   # re-index now
mytoken statusline                             # one-line status for a shell prompt
mytoken doctor                                 # check index, sources and schema, read-only
mytoken version
```

`summary`, `daily`, `quota`, `statusline` and `doctor` open the index read-only: they never migrate,
reprice or write to it. Run `mytoken scan` once after an upgrade to migrate and reprice the index.

## Contributing

Bug fixes, new harnesses, new quota sources and translations are welcome as a pull request. For a
new feature or a UI change, open an issue first. [CONTRIBUTING.md](CONTRIBUTING.md) covers the
development setup, tests, the conformance suite and the checklists for adding a harness or a quota
provider.

## Credits

- [tokscale](https://github.com/junhoyeo/tokscale) documented many of the parsing rules MyToken
  follows; no code was copied.
- [codeburn](https://github.com/nerv-builds/codeburn) is the source of several readers (Qwen, Kimi,
  OpenClaude, Claude Desktop, Grok, OpenClaw, Droid, Goose, Forge) — see
  [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES).
- [MyGo](https://github.com/egoist/mygo) is the native UI package.
- [models.dev](https://models.dev) and [LiteLLM](https://github.com/BerriAI/litellm) provide the
  price tables.
- [cc-switch](https://github.com/farion1231/cc-switch) defines the pricing import format and the
  provider request log used by attribution.

## Disclaimer

MyToken!!!!! is an unofficial fan tribute to the band MyGO!!!!!. The name, palette and decorations
are affectionate references; the project is not affiliated with or endorsed by the band or its
rights holders, and contains no official artwork. All trademarks belong to their owners.

## License

MIT © 2026 zzstar101. See [LICENSE](LICENSE).
