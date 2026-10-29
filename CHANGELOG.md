# Changelog

The section of each version is also what the app's update window shows, so
every release needs one here (`## 0.2.1`), written for the people updating.

## 0.5.6

- **Codex's older sessions are readable again.** Codex compresses a rollout
  once its session is over, and MyToken only ever looked at the plain files —
  so on a machine with a few months of Codex history, 1 299 of 1 313 rollouts
  were skipped and the sessions page showed a single day. Those files are read
  now, and their tokens are counted as they always were.
- **A subagent's work stays under its own session.** Codex writes the *parent*
  thread's id into every usage record of a subagent rollout, which is what the
  reader took, so a subagent's spend was attributed to the session that
  spawned it and its own session never appeared. 1 277 sessions instead of
  1 262 on the same history, with the same total.
- **A tool that needs a key no longer stops the scan.** Trae's database is
  encrypted with a key only a running Trae holds, and until it is supplied the
  reader could not open anything at all — which used to fail the whole scan, so
  every other tool's data was lost with it. MyToken now reports that tool as
  *not ready* and carries on; Settings → Sources still shows the reason.
- **Built on MyGo 0.7.1.** The dashed pace projection is now drawn with the
  framework's own path dashes rather than a hand-rolled loop, and every other
  drawing path is unchanged.

## 0.5.5

- **Trae's token usage is readable now.** Trae keeps its history in a
  SQLCipher 4 database and the key to it exists only inside the running app,
  which is why the tool used to appear with a quota card and no usage. MyToken
  decrypts that database itself — AES-256-CBC, per-page HMAC-SHA512, with the
  write-ahead log folded in — and reads every turn's prompt, completion, cache
  and reasoning tokens, the model it billed, and the workspace it came from.
  Nothing is written to Trae's files: the reader decrypts to a temporary
  database, reads it, and deletes it.
- **One click to authorize.** The key is proved against the database before it
  is accepted, and MyToken reads it from the running app itself: Settings →
  Sources offers *Authorize reading…*, and `mytoken key trae` does the same from
  a terminal, on macOS, Windows and Linux. A key recovered elsewhere can still
  be pasted, and either way it lives in a `0600` file beside the index rather
  than inside it.

## 0.5.4

- **The token card has an All range.** The card now offers the same four
  ranges as the other pages — the last 7, 30 and 90 days and all time. All
  time reads the whole history when it is chosen and folds it into weekly
  or monthly bars, so a long history stays readable.
- **ZCode plan quota.** The plan card now reads the ZCode Start Plan balance
  from the login the ZCode app already stored, so its grants and their reset
  times sit beside the other providers instead of leaving a gap.
- **Trae appears again.** The current Trae client stores its sign-in region
  as an object rather than a string, which made the whole sign-in unreadable
  and the Trae card never appear. Both shapes are read now, and a region the
  app does not recognise no longer takes the card down with it.
- **DeepSeek Harness speed is measured, not guessed.** The live card's tokens
  per second came from the gaps between requests, and a gap also carries the
  wait for the first token and the tool round trips, so DSH sessions read two
  to seven times slower than DeepSeek Harness's own figure. The reader now
  keeps the decode window from each reply and divides by it, so a DSH session
  reads 249.9 tok/s where it used to read 82. Tools that time nothing keep
  the old estimate.

## 0.5.3

- **Opening the app works again.** Since 0.5.0, opening MyToken!!!!! from
  Finder, the Dock or Spotlight printed the command-line summary into the
  void and quit, because the app took the empty output it gets there for a
  terminal. It now checks for a real terminal, so only a bare `mytoken` typed
  in a shell prints the summary.

## 0.5.2

- **Plan quotas always come from the provider's server.** Codex quota is now
  fetched from the ChatGPT backend (the same read the codex CLI makes) instead
  of the last rollout log, so a log that stopped in September can no longer
  show 0%. Every other provider already fetched; all of them cache only the
  last server response. If the stored Codex token is rejected, the app asks you
  to sign in to Codex again — it never refreshes the token, because that would
  sign you out of Codex.
- **In your language again.** 0.5.0 and 0.5.1 showed English on Chinese
  systems; the app now follows the system's languages again, and picking a
  language in Settings also switches the toolkit's own texts (menus, the
  update window).
- **Token usage on the overview.** Today, this month, the last 7/30/90 days
  and the daily average, over daily bars by input, output, cache write and
  cache read, above the spend card. Token counts everywhere now include
  cache reads.
- **DeepSeek balance.** When DeepSeek Harness holds a DeepSeek API key (or
  `DEEPSEEK_API_KEY` is set), the plan-quota card shows the account balance
  from DeepSeek's balance API.
- **Plan quota tiles fixed.** Names no longer collapse to "…", Chinese plan
  names are no longer garbled, long plans shorten instead of pushing the
  header out, four accounts sit two by two, and a provider that turns the
  saved login away says to sign in again.
- **Running now, calmer.** The running-sessions card sits in the page like
  the other cards: each session keeps its orb, now with a softer highlight,
  shows what it cost and its output speed (estimated from the gaps between
  requests), and a strip of its requests of the last 15 minutes, each as
  tall as what it cost, drifting left as time passes.
- **Running sessions are marked** in the session list and on the session's
  page.
- **Today's dial** keeps its hour labels outside, clear of the total.
- **Data sources list only the tools found here.** The others are under
  "Install other agents", with the install command (click to copy), the
  website, and a way to choose a folder if one is installed after all.
- Icons for Z Code, MiniMax Code, MiMo Code and Step Code.
- **The yearly heatmap breathes.** Square cells that fill the card, come in
  as a wave, a slow swell of light across the busy days, today's cell
  pulsing, and a hover card per day.
- **Wide screens.** Pages grow wider and stay centred instead of leaving a
  big gap on the right.
- "1 day", "1 request" and friends in English instead of "1 days".
- A new README with fresh screenshots in English and Chinese, plus
  CONTRIBUTING.md and docs/ARCHITECTURE.md for anyone who wants to help.

## 0.5.1

- **Check for Updates is back.** Settings → General → About has the button
  and the switch for the daily automatic check again (0.5.0 dropped both),
  and the menu bar menu has a Check for Updates item.
- **Uses the system proxy.** Update checks, plan-quota requests and price
  refreshes now go through the proxy set in macOS or Windows settings, as a
  browser does, so they work where GitHub or a tool's API is only reachable
  through a proxy. `HTTPS_PROXY` / `HTTP_PROXY` still win when set.
- Apple-silicon-only and Intel-only builds of MyToken (made locally) now find
  updates too; they update to the universal build.

## 0.5.0

MyToken is now a spend and plan-quota dashboard for the AI coding tools on
this machine. The relay-reconciliation direction of 0.2–0.4 is gone.

- **A new app, rebuilt on MyGo 0.5.5.** Five pages: Overview, Sessions,
  Ranking (with the yearly heatmap and streaks), Projects and Settings, in a
  calmer palette with motion: cards rise into view, figures count, selections
  slide. Overview leads with today's spend and the sessions running now, as a
  deck of cards that turns by itself, each with a breathing orb; then this
  month's spend against last month with a month-end forecast, or today as a
  24-hour clock; then plan quotas and the breakdown by tool, model and token
  type. The menu-bar panel shows today's spend, the sessions running now with
  their cost per hour and cache hit rate, and every quota. The menu-bar text
  can show any of today's spend, spend per hour, hit rate and quota. New icon.
- **Costs are official API prices, labelled "API equivalent".** Models
  without a price show as unpriced instead of $0; Settings → Prices can price
  them as another model or with your own rates. Headline token counts no
  longer include cache reads, and there is one cache hit rate:
  `cache read / (input + cache read + cache write)`. One-hour cache writes are
  now priced at 2× input (five-minute writes stay 1.25×).
- **Claude totals are right.** Claude Desktop's Code tab no longer counts
  Claude Code sessions a second time, Cowork turns are not counted twice, and
  ledger rows keep millisecond timestamps. The tools whose reading changed
  (Claude Code, Claude Desktop, OpenClaude, Step Code, Pi) are read again
  once after the update, in a few seconds; requests whose logs those tools
  have since deleted are kept as they were.
- **Plan quota without setup.** Codex is read from its logs; Claude, Cursor,
  Copilot, GLM, Trae, WorkBuddy and Qoder are queried with the login those
  tools already have on this machine (Qoder may ask once for Keychain
  access). Percent quotas show as rings with a pace mark; credit balances,
  which have no fixed limit, show what is left, the last 14 days of use and
  about how many days it lasts. Tools without a detected plan show nothing.
- **Sources are switches.** Every supported tool is detected automatically
  and can be turned off; one that is installed somewhere unusual can be
  pointed at its folder, which is checked on the spot.
- **Much faster and lighter.** A fresh index of ~255K requests takes about
  13 seconds instead of 23 minutes; the app watches a bounded set of folders
  (~125 open files instead of 5,400), refreshes in about 10 ms and is
  nearly idle in the background. Command-line reads open the index read-only.
- **Removed:** relays, reconciliation, provider multipliers, budgets and
  alerts, multiple instances, the cache ledger and the Cache page. The index
  is migrated in place — your history is kept, repriced and compacted, with
  one backup beside it.
- **Command line trimmed** to `mytoken` (summary), `daily`, `quota`, `scan`,
  `statusline`, `doctor` and `version`. A bare `mytoken` opens the app when it
  is not run from a terminal.
- **Windows:** right-click the tray icon for Open, Rescan and Quit.
- **More tools.** ZCode, MiniMax Code, MiMo Code and Step Code are counted
  offline; Kimi Code is covered by the Kimi source; Pi and OpenCode recognise
  their compatible orchestrators (Oh My Pi, Oh My OpenAgent/senpi, OMO)
  without counting aggregate logs twice. Trae and Qoder appear for their plan
  credits only: they keep no usage record to count requests from.

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
