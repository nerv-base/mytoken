# Changelog

The section of each version is also what the app's update window shows, so
every release needs one here (`## 0.2.1`), written for the people updating.

## Unreleased

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
