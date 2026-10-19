# Changelog

The section of each version is also what the app's update window shows, so
every release needs one here (`## 0.2.1`), written for the people updating.

## Unreleased

- **Four more coding agents can be counted offline.** ZCode, MiniMax Code, MiMo Code and Step Code
  now have their own source identities and readers. SQLite databases are opened read-only and
  query-only; JSONL readers keep incremental cursors and stable request identities.
- **Kimi Code is covered by the existing Kimi source.** The reader understands the current
  `~/.kimi-code` agent-wire protocol as well as legacy Kimi logs, and avoids counting a child
  agent once in its own wire and again in the parent's summary.
- **Pi and OpenCode recognize their compatible orchestrators.** Pi can discover Oh My Pi and
  Oh My OpenAgent/senpi sessions; OpenCode remains the identity for OMO plugin-mode sessions.
  Aggregate `model_usage`, child-result and orchestration event logs are not counted a second time.
- **Unsupported products stay honest.** Trae and Qoder do not expose a stable, publicly verifiable
  offline usage ledger, so this release does not guess at private databases or add network access.

## 0.4.1

- **A leftover backup no longer blocks an upgrade.** If an earlier attempt
  left a `mytoken.db.pre-provider-v1.db` or `mytoken.db.pre-instance-v1.db`
  behind, the migration writes its own backup beside it under a timestamped
  name instead of refusing to start, and the error message names the file it
  actually tried. Nothing is ever overwritten, reused or deleted.
- **A failed start says so in a window.** A database or migration error used
  to quit without showing anything; it now opens a window with the error, the
  database path, what to do about it, a button to reveal the data folder and
  a way to quit.
- **The sources card lists your sources again.** The card could come up with
  only its subtitle once the settings page had been reloaded, as if no tool
  had ever been found.
- **Codex quota shows the last reading the logs contain.** Quota was only
  published while new records were being indexed, so a source that was
  already fully scanned never reported anything at all. A bounded read of the
  tail of the newest rollout files now finds the most recent usable reading
  without rescanning events, advancing cursors or guessing from tokens, and
  the card states the age of the reading and why a pace cannot be computed.
- **Claude quota says when there is nothing to read.** The card marks a
  credentials file that is not there instead of offering it as ready, and
  still reads nothing until you enable it.
- **The settings page stops re-running its heavy panels on every commit.**
  Each store commit re-ran the pricing, source and 90-day scope queries, which
  kept the load queue busy while the page was open. They now load when the
  page is opened or when their own data changes.
- **The provider ranking merges one endpoint into one row.** Unidentified
  endpoints were keyed per session, so a single provider appeared once for
  every conversation.

## 0.4.0

- **Rebuilt on MyGo 0.4.8.** Charts, tables, selectors and search boxes are
  now the framework's own controls, so keyboard navigation, screen readers
  and animation come from the platform: data tips follow the arrow keys and
  are announced, the sidebar is a labelled tab list, and the sliding
  highlight is a real transition instead of a per-frame animation. The
  five-colour theme, the glass panels and each page's own accent are
  unchanged.
- **More than one copy of a tool.** Settings › Sources can add, move, remove
  and promote sources, including turning a backup copy of a folder into a
  live one. Every copy keeps a stable id, so sessions, cache rows, providers
  and reports never mix two installations. An explicit path that is wrong is
  now reported as an error instead of silently falling back to a default.
- **Providers by identity, not by name.** Requests are grouped by the
  endpoint they came from, so a renamed alias or a moved domain no longer
  splits the statistics and two accounts behind one relay keep their own
  billing. Renaming a provider changes its label only.
- **`mytoken snapshot --once --json`,** a versioned JSON envelope with
  freshness, source and diagnostics for scripts and status bars, plus
  `mytoken statusline`, `mytoken report day|week|month` and
  `mytoken report --by branch`. Read-only commands never write to the
  database, even while another copy of MyToken is running.
- **Know why a source is empty.** Settings and `mytoken doctor` say whether a
  source has no data because it was never scanned, has no token counts,
  cannot be read, or is not supported — instead of showing a silent zero.
- **Budget alerts** (Settings › Budget): notify when a day, week or month
  crosses 80% or 100% of a limit, at most once per period, from estimated
  cost. Alerts stay off until a fixed IANA timezone is selected, because day
  boundaries are otherwise undefined.
- **Quotas.** Codex rate limits are read from its own logs, offline, and a
  fresh unusable reading never replaces a usable older one; `quota list`,
  `quota status` and the countdown come from the percentages the provider
  reports. Claude's quota is opt-in and off by default: enabling it shows the
  exact credential source and endpoint first, never refreshes tokens, and can
  be turned off from the CLI while the app runs.
- **Live speed in the tray:** committed output tokens per second over the
  last minute, labelled as a measurement of committed output and not of model
  generation speed. `mytoken statusline` shows the same.
- **Branch reports:** usage grouped by the git branch a session's own records
  name, with worktrees of one repository folded together; contradictory
  evidence is reported as unknown rather than guessed.
- The Windows installer is still unsigned and macOS builds are ad-hoc signed;
  the second batch of supported tools has not been checked against real logs
  yet.

## 0.3.0

- **Cache ledger.** A new Cache page shows, for every request that should
  have hit the prompt cache, whether it did — and when it didn't, why: the
  conversation was compacted, the cache expired, the model changed, or no
  visible reason (often a relay that doesn't pass caching through). It adds
  up the tokens sent again and what they cost, by day, by provider (with a
  cache health figure) and by session. Each session gets a cache timeline.
  Works for every tool from token counts alone; `mytoken cache` does the
  same in the terminal.
- **Six more tools:** Amp, Kiro, Zed, Antigravity, Codebuff and Mux. None of
  them has been checked against real logs yet — tell us if numbers look off.
- Compaction markers are now read from Pi and OpenCode logs too, and Pi's
  compaction requests are counted.
- The first launch after updating rebuilds the cache ledger once (a few
  seconds on a large history).

## 0.2.3

- **Lighter in the tray.** Closing the main window now frees its graphics
  memory instead of keeping it hidden: MyToken drops from about 210 MB to
  about 90–100 MB while it sits in the menu bar. Opening the window again
  builds it anew in a few milliseconds.
- **Less CPU while your agents work.** A change to one log no longer reopens
  every log: unchanged files are skipped, and project paths are tidied from
  a small table instead of every request. Background CPU while agents write
  went from about 19% to about 7% on a 200K-request history.
- The pricing page shows your own multiplier for each provider. Before, it
  could show one synced from a relay or a past dated rule, and saving the
  page then turned that into a rule of yours.
- Relays add prices only for the models you use with them, not for every
  model in their price list (one site added 210 rules for 3 models).
- Droid and Forge show their own logos, and OpenClaude its green mark. Forge
  data in the older `~/forge` folder is read too.

## 0.2.2

- **Fixed: no data and failing update checks on a fresh install.** MyToken
  watched every folder under each tool's home for changes, including whole
  programs installed there (Hermes keeps its own code in `~/.hermes`). On
  macOS that held tens of thousands of files open until the system refused
  to open more, so logs could not be read and the update check could not
  connect. It now watches only the folders that hold logs.
- Settings › Sources lists only the tools that have data, with their request
  counts, and counts the other supported tools in one line.
- WorkBuddy shows its own logo.

## 0.2.1

- **Automatic updates.** MyToken checks github.com once a day and offers new
  versions with their notes; install, skip or be reminded later. Settings
  turns the check off, and has a button to check now. Updates are signed, and
  from a recent version only the changed parts are downloaded.
- The sidebar's "Up to date" now reads "Data up to date": it was always about
  the scan of your logs, not the app's version.

## 0.2.0

Relay reconciliation: see what your relay charged next to what your own logs
say it should have, and where the difference comes from.

- **Relay reconciliation** for new-api and sub2api relays, plus balances for
  some official APIs, off until you turn it on site by site: ratios become
  dated price rules, balances, and charges (new-api per request, sub2api per
  day) matched to your local requests.
- **Reconciliation page**: local estimate vs. charged, the implied multiplier,
  and a waterfall of where the difference comes from; by model and by day.
- **Tray** shows the balances of the sites you turned on.
- **CLI:** `mytoken relay list|enable|disable|sync` and `mytoken reconcile`.
- **12 more tools** (22 in total).
- **macOS:** the window buttons no longer overlap the sidebar.

First launch upgrades the database one way and reprices every request once.

## 0.1.3

See the [release](https://github.com/zzstar101/mytoken/releases/tag/v0.1.3).
