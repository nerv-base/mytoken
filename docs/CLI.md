# MyToken!!!!! CLI

The same `mytoken` binary runs the desktop app and the command line.

```
mytoken                 # today's and this month's API-equivalent cost (default)
mytoken daily           # one row per day
mytoken quota           # plan quota fetched from the providers' servers
mytoken scan            # index the harness logs once (also migrates the index)
mytoken statusline      # one-line status for a shell prompt
mytoken doctor          # read-only health check
mytoken version
mytoken gui             # start the tray app and main window (--hidden for login)
```

Every command reads the same local index (`~/Library/Application Support/MyToken` on macOS,
`%APPDATA%\MyToken` on Windows, `$XDG_DATA_HOME/mytoken` or `~/.local/share/mytoken` on Linux).
Set `MYTOKEN_HOME` to use another directory. Exit status: `0` success, `1` operational error,
`2` invalid arguments.

`summary` (also `today`/`month`), `daily`, `quota`, `statusline` and `doctor` open the index
**read-only**: they never create it, migrate it, reprice it or write to it. Run `mytoken scan`
after an upgrade to run the in-place migration and reprice history.

## summary (default)

```
mytoken [summary|today|month] [--json] [--no-cost] [--timezone TZ] [--db PATH]
```

Prints today and the current calendar month:

```
API-equivalent cost · Asia/Shanghai
Today  $19.49  73,889,931 tokens (...)  412 requests · cache hit 97.2%
Month  $2,843.10  ... 
```

Cost is what the same tokens would cost at the official API list price. Requests that no price
can be found for are marked `(+unpriced)` and never counted as $0. `--json` emits the same
periods (`timezone`, `today`, `month`: `from`, `to`, `tokens`, `costUsd`, `requests`,
`unpriced`).

## daily

```
mytoken daily [--days N] [--json] [--no-cost] [--timezone TZ] [--db PATH]
```

One row per day for the last `N` days (default 30), in the style of `ccusage daily`:

```
DATE        INPUT      OUTPUT    CACHE READ   CACHE WRITE  TOTAL       API-EQUIV  CACHE HIT
2026-10-19  818,508    451,334   71,386,822   1,233,267    73,889,931  $19.49     97.2%
```

Input, output, cache read and cache write are separate columns; `TOTAL` excludes cache read
from the headline token count and `API-EQUIV` is the API-equivalent cost. `--json` emits the
daily points, `--no-cost` omits the cost column.

## quota

```
mytoken quota [plan] [--json|--format table|json|csv] [--db PATH]
```

Shows the plan windows the background quota service fetched from each provider's own server
(Codex, Claude, Cursor, GitHub Copilot, GLM, DeepSeek, Trae, WorkBuddy, Qoder, ZCode). A server response
is cached in the index (`quota.plan.*`) so the command can show the last good snapshot offline;
its source is always `api`. Quotas are never derived from local usage logs — those only supply
tokens and cost. Credentials the tool itself stored are read to authenticate; no credential is
copied into the index, and no token is refreshed or rewritten. A provider with no detected plan
is hidden. Read-only.

## scan

```
mytoken scan [--cpuprofile FILE] [--memprofile FILE]
```

Indexes every enabled harness log once (incrementally) and prints one line:

```
Scanned 2075/2075 sources: 206020 events, 626 sessions in 9.8s
```

This is the only command that opens the index writable; it runs the in-place 0.5 migration
(drop removed tables/columns, delete obsolete settings, add `cache_write_1h`, reprice, VACUUM)
and writes one pre-migration backup next to the index. The profiling flags write Go `pprof`
profiles and are meant for development.

## statusline

```
mytoken statusline [--json] [--session ID] [--harness NAME]
                   [--since DATE|DUR] [--until DATE|DUR] [--last WINDOW]
                   [--timezone TZ] [--provider NAME] [--provider-id ID]
                   [--no-cost] [--db PATH]
```

Prints one line (or JSON) with the selected session's cost, cache hit rate and tokens, the live
token rate published by a running scan, and any plan-quota windows. A value with no reliable
source is reported as unavailable, never as zero. Read-only.

## doctor

```
mytoken doctor [--json]
```

Reports the data directory, database size, schema state, price-table source, and — for every
harness — whether it is enabled, whether its roots exist, and how many events and cursors the
index holds. Read-only: a missing index stays missing, and an index that predates 0.5 is
reported as upgrade-required instead of being migrated.

## version / help

```
mytoken version
mytoken help
```

`version` prints the release version, `help` prints this command list.

## Environment

| Variable | Meaning |
| --- | --- |
| `MYTOKEN_HOME` | data directory that holds `mytoken.db`, the price cache and settings |
| `TZ` | fallback timezone when no reporting timezone is recorded |
