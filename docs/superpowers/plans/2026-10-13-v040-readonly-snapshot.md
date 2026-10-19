# MyToken 0.4.0 Read-Only Versioned Snapshot & Statusline — Implementation Plan

> **For agentic workers:** This file is **design only**. It authorizes no product code, no agents, no commits. It freezes nothing until the open decisions in §9 are confirmed by the user. Steps use checkbox syntax for the eventual implementation.

**Goal:** A versioned, read-only snapshot surface (`--json --once`) that statusline, tray and (later) MCP share, plus the statusline fields (session cost, relay balance, cache hit rate). Read-only must mean *no* scan, *no* network, *no* credentials, *no* write, *no* schema migration — and missing data must surface as an explicit unavailable reason, never as zeros.

**Architecture:** A separate read-only SQLite handle plus a small `internal/snapshot` envelope that reuses the frozen `query.Service` instead of the writable `internal/app` wiring. Freshness and schema state travel inside the same versioned payload.

**Tech Stack:** Go 1.27.1, modernc.org/sqlite v1.60.1 (`go.mod:10`), mygo v0.4.8, existing `internal/query` / `internal/store` / `internal/pricing` / `internal/reconcile`.

**Scope authority:** `task_plan.md:7-18` (snapshot defaults read-only, no implicit scan/network, freshness + missing reasons included, no data must not masquerade as zero, refresh explicit), `task_plan.md:15` (fixed persisted stats timezone, default = first system timezone, week starts Monday), `docs/ROADMAP.md:200-201` (`--json --once` shared by statusline/tray/MCP; statusline = session cost + relay balance + cache hit rate), `docs/ROADMAP.md:64` (<100 ms, peak memory <50 MB), `docs/SPEC.md` §5.1 (timezone inside the read snapshot; read-only fallback to facts).

---

## 1. Verified starting point — why every design choice below is forced

### 1.1 Every current open path writes (this is the central finding)

| # | Site | Write |
| --- | --- | --- |
| 1 | `internal/paths/paths.go:10` `DataDir()` | `os.MkdirAll(dir, 0700)` — resolving the data dir already creates it. `DBPath()` at `:43`. |
| 2 | `internal/store/store.go:59` `Open(path)` | `MkdirAll` parent, `db.SetMaxOpenConns(1)` (`:73`), one `db.Exec` (`:77-86`) running `PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; …` **plus `CREATE TABLE IF NOT EXISTS events/sessions/cursors/provider_configs/settings`**, then `st.migrate()` (`:90`). |
| 3 | `internal/store/migration.go:8` `migrate()` | `prepareProviderMigration` → `prepareProviderRollup` → `prepareInstanceMigration`; one tx of ALTER TABLE / `migrateCompact` / `migrateSignals` / `migrateCache` / `migrateEventIndexes` / `migrateDaily` / relay schema / endpoint rollups / instance re-key; invariant check; optional `VACUUM`; then `s.projects = newProjectResolver()`, `s.EnsureLocalDays(ctx)` (`:178`), `s.NormalizeProjects(ctx)` (`:181`). |
| 4 | `internal/store/aggregate.go:253` `EnsureLocalDays` | When `settings.daily_timezone` ≠ `LocalDayZone()` (`:238`): `DELETE FROM daily_usage` + reinsert, `DELETE FROM cache_daily` + reinsert, `rebuildProviderRollups`, `INSERT settings` — a write performed *at every DB open*. |
| 5 | `internal/store/pricing.go:36` `EnsurePrices(ctx, fingerprint, cost)` | Reprices event rows. |
| 6 | `internal/app/app.go:64` `open(refresh)` | `store.Open` (`:73`) → `attrib.New` → `pricing.New` → `query.NewSettings` → `LoadPricingSettings` → `scan.New` → `query.NewService` → `newRelays` → **`st.EnsurePrices(...)` unconditionally (`:92`)**; `Open()` (`:59`) additionally spawns a 24 h price-refresh goroutine. `OpenLocal()` (`:62`) skips only the goroutine, not `EnsurePrices`. |
| 7 | `internal/attrib/attrib.go:47` `New` | Calls `Resolver.Refresh` (`:287`), whose `backfillConfigs` (`:332`) **writes `provider_configs`**. So the attribution resolver is not read-only either. |
| 8 | `internal/cli/commands.go:215` `runDoctor` | `app.OpenLocal()` first; the subsequent `os.Stat(db)` therefore can never observe a missing database — the open already created it. |

**Consequence:** a read-only snapshot must not reuse `app.Open`/`app.OpenLocal`, `paths.DataDir()`, `attrib.New`, `store.Open`, `store.migrate`, `EnsurePrices`, or `EnsureLocalDays`. Each of those is a separate hook to bypass, not one flag.

### 1.2 What is already safely read-only

- `internal/query/service.go:18` `NewService(st *store.Store) Service`; `load` at `:40` opens `BeginTx(ctx, &sql.TxOptions{ReadOnly: true})`. Read-only at the SQL level already, but over a read-write handle.
- `internal/query/aggregate_sql.go:127` and `internal/query/cache_query.go:130`: `persistentDays := zone == store.LocalDayZone()`. When the stored zone differs — system TZ changed, or a `--timezone` override via `runInLocation` (`internal/cli/options.go:372-379`) — queries fall back to fact SQL (`eventAggregateSQLMode`, `cacheRollupFromEvents`). **This is the SPEC §5.1 read-only fallback and it already exists**: skipping `EnsureLocalDays` yields correct-but-slower numbers, never wrong ones.
- `internal/store/aggregate.go:14-31` registers `mytoken_day` / `mytoken_hour` with `sqlite.MustRegisterScalarFunction` in `init()` — process-global, so a second (read-only) connection in the same process has them. Both functions read `time.Local`; that is what makes `runInLocation` work and what makes a concurrent `--timezone` override unsafe in-process (§9 R2).
- `internal/store/store.go:96` `DB() *sql.DB`; `:460` `Setting`; `:121` `LatestRelayBalance`; `internal/query/query.go:133` `Service` interface.
- `internal/reconcile/sync.go:337` `SiteStatus{relay.Site; Providers; HasKey; Balance *relay.Balance; LastSync; LastError}`; `Sites(ctx)` is documented offline (`internal/gui/relays.go:15-16`) — the balance source for statusline without network.
- `internal/gui/data.go` `Overview.ProviderErr` is the existing precedent for "a failed query never reads as zero usage".

### 1.3 What does not exist

- `--once`: no match in any `.go` file.
- No `Snapshot` service type in `internal/query`; no statusline code — only `docs/ROADMAP.md:64,200-201`, `docs/CLI.md`, and the `internal/ingestrate/ingestrate.go:3` comment ("readout (tray, statusline)").
- **No global schema version.** No `PRAGMA user_version`, no `application_id`. Generations are recorded only as `settings` markers: `provider.identity.v1` (`internal/store/provider_migration.go:14`), `provider.endpoint-rollup.v1` (`internal/store/provider_rollup.go:18`), `instance_schema_version` = `instance.v1` (`internal/store/instance_migration.go:16-17`), `ledger_version` (`internal/store/cache.go:21,25`), `daily_timezone` (`internal/store/aggregate.go:286`), plus `attribution.*`, `price-rules`, `model-aliases`.
- **No persisted "last scan at".** `internal/harness/harness.go:24` `Cursor{Offset, Size, ModTime, Fingerprint, Extra}` carries only the source file's mtime; the bootstrap `cursors` table is `(harness, path, cursor)` inside the DDL at `internal/store/store.go:77-86` and has no timestamp column.
- `internal/store/store.go:97-106` `Close()` only closes subscriber channels and calls `s.db.Close()` — **no WAL checkpoint**. There is no close-time write, but uncheckpointed WAL is the normal steady state, which is exactly the case §4 must handle.

---

## 2. Contract to freeze: snapshot v1 envelope

Field names below are proposals needing confirmation (§9 D1/D2); the *semantics* come from `task_plan.md:7-18`.

```json
{
  "snapshotVersion": 1,
  "generatedAt": "2026-10-13T19:40:00+08:00",
  "schema": { "state": "ok|upgrade_required|newer|unreadable", "markers": {}, "missing": [], "detail": "" },
  "sources": {
    "index":   { "available": true, "reason": "", "detail": "" },
    "pricing": { "available": true, "reason": "", "fetchedAt": null },
    "balance": { "available": false, "reason": "no_relay_enabled", "detail": "" },
    "tokRate": { "available": false, "reason": "no_live_signal", "detail": "" }
  },
  "freshness": {
    "eventsThrough": null, "sessionsThrough": null, "sourceModTime": null,
    "pricingFetchedAt": null, "rollupZone": "Local", "localZone": "Local", "rollupsCurrent": true
  },
  "data": { "totals": { }, "cache": { }, "balances": [ ] }
}
```

Rules:

1. **Read-only by default.** No scan, no network, no credential read, no write, no migration. `--once` prints exactly one document and exits.
2. **Unavailable ≠ zero.** A missing DB, a missing table, an unreadable path, an unmigrated index, or a newer schema must emit a machine code in `sources.*`/`schema` and leave `data` absent or null. Zeros are reserved for "we read the index and the user really has zero usage".
3. **Priority order:** unavailable reason > zero > value. This mirrors `Overview.ProviderErr`.
4. **Freshness and schema version travel in the payload**, not in stderr.
5. **Version is explicit**: consumers switch on `snapshotVersion`; additive fields are allowed, semantic changes bump it.
6. **Legacy output preserved:** the existing `stats` table/CSV/JSON shapes (`internal/cli/cli.go:89-142`, `statsReport{Totals, By, Rows, UnpricedModels}`) are untouched. The snapshot is additive surface.
7. **Exit codes** stay `0` success / `1` operational error / `2` invalid arguments (`docs/CLI.md:17`). Whether "index not yet created" is `0` + reason (statusline renders "未采集") or `1` is open (§9 D1).

---

## 3. Read-only open design

Files: new `internal/store/readonly.go` (or a `ReadOnly` type in `internal/store`), plus a small addition to `internal/sqlitedsn`.

- DSN: `sqlitedsn.URI(path, "mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)")`. **Do not** pass `_pragma=journal_mode=WAL` — that is a write, and the file is already WAL by construction. `sqlitedsn.URI` already renders `mode=ro` correctly (`URI("/a b/x.db","mode=ro")` → `file:///a%20b/x.db?mode=ro`).
- **No** `MkdirAll`, **no** bootstrap DDL, **no** `migrate()`, **no** `EnsureLocalDays`, **no** `NormalizeProjects`, **no** `EnsurePrices`, **no** `attrib.New`. If the database file does not exist, return a typed error before touching the filesystem beyond `os.Stat`.
- `paths` needs a **non-creating** resolver: `DataDir()` (`internal/paths/paths.go:10`) calls `os.MkdirAll`, so the snapshot needs either a `ResolveDataDir(create bool)`-style variant or to replicate the env/platform default lookup (`MYTOKEN_HOME` → darwin `~/Library/Application Support/MyToken` → `%APPDATA%\MyToken` → `$XDG_DATA_HOME/mytoken`) without the mkdir. **Decision needed:** new exported function vs. an unexported resolver in `internal/snapshot` (§9 D8).
- **WAL visibility rule:** a read-only SQLite connection needs the `-wal`/`-shm` sidecars present and readable; a read-only opener must not be the process that creates `-shm`. The test matrix in §6 T4 must therefore cover: (a) a writer connection open with live uncheckpointed WAL, (b) a cleanly closed DB with no `-wal`, (c) `-wal` present, `-shm` absent, (d) file mode read-only on disk.
- `immutable=1` is **not** the default: it asserts the file cannot change, which is false while a scanner runs. Keep it as a documented fallback only if the read-only WAL open proves unreliable (§9 D6).
- `Close()` on the read-only handle calls only `db.Close()`. Do **not** reuse `store.Store.Close` — it assumes ownership of the subscriber map.
- `SetMaxOpenConns(1)` + one read transaction per snapshot matches the existing single-snapshot/single-writer model (SPEC §5.1).

---

## 4. Unknown / newer schema handling

A pure read-only validator (no migration) computes `schema.state` from the marker set:

- All required markers present with known values → `ok`.
- Any required marker absent, or a marker/table mismatch (the partial states the write-path preflights already reject) → `upgrade_required`, with a message telling the user to run a normal open once (`mytoken scan` / `mytoken doctor`) so the migration runs. The snapshot must not migrate.
- Any marker present with a value this build does not know (e.g. `instance_schema_version` = `instance.v2`) → `newer`, message "本机数据库由更新版本的 MyToken 写入，请升级 MyToken", exit `1`. Never guess.
- Legacy pre-0.3 file with no `settings` table at all → `upgrade_required`; no read path may assume `event_dimensions` exists.

Reuse the *shape* of `checkProviderIndex` (`internal/store/provider_migration.go:27`), `checkProviderRollupIndex` (`internal/store/provider_rollup.go:135`) and `checkInstanceSchema` (`internal/store/instance_migration.go:77`), but as read-only validators — the existing ones run inside the write migration. The required-marker table must be data-driven, because workflow-24 is still landing `instance.v1` and the set will move.

---

## 5. Freshness metadata

Nothing today records a successful scan. Read-only signals available:

- `max(events.timestamp)` — index covers usage through here (a quiet user legitimately looks stale).
- `max(sessions.updated_at)`.
- Per-source `harness.Cursor.ModTime` (`internal/harness/harness.go:27`).
- `prices.json`.`fetched` (`internal/pricing/pricing.go:73`; the read pattern already exists at `internal/cli/commands.go:228`).
- Stored `settings.daily_timezone` vs `store.LocalDayZone()` → `rollupsCurrent` (SPEC §5.1).

Open decision (§9 D2): freshness defined as **derived signals only** (slice 1 recommendation — no new write anywhere) versus a persisted `last_scan_at` written by the scan/refresh path (a write, deliberately *not* part of the snapshot).

---

## 6. Small test-first sequence

- [ ] **Step 0 — freeze the envelope.** Confirm this file's §9 decisions. No code.
- [ ] **Step 1 — `internal/sqlitedsn`.** Table test that `URI(p, "mode=ro&_pragma=query_only(1)")` renders a correctly escaped `file:` URI with both pragmas.
- [ ] **Step 2 — `internal/store` read-only handle (`readonly_test.go`, new).** All tests run on temp dirs / `:memory:` fixtures; never a real DB.
  - [ ] T1 missing DB → typed error, and **the directory listing is unchanged** (no file, no `-wal`, no `-shm` created).
  - [ ] T2 marker-less legacy DB → `upgrade_required`; sha256 of `mytoken.db` **and** `settings` row count unchanged.
  - [ ] T3 unknown/newer marker value → `newer`; DB bytes/mtime unchanged.
  - [ ] T4 live WAL visibility: a writer commits rows and stays open; the read-only handle sees them. Plus subtests (b) closed DB without `-wal`, (c) `-wal` without `-shm`, (d) read-only file mode.
  - [ ] T5 no-write proof: sha256 of the DB before/after open + full query sweep; `-wal` size/mtime not truncated by close.
  - [ ] T6 zone drift: set a stale `settings.daily_timezone`, run snapshot queries, assert totals match the facts-based fallback **and** `settings.daily_timezone` is untouched.
- [ ] **Step 3 — `internal/query` parity.** Reuse `NewService(st)` over the read-only handle; a regression test asserting `Totals`/`CacheTotals`/`Daily` equal the read-write handle on the same fixture.
- [ ] **Step 4 — `internal/snapshot` envelope.** Golden test at `internal/snapshot/testdata/snapshot_v1.json`; a test asserting every unavailable path emits a code and never a zero-only payload.
- [ ] **Step 5 — `internal/cli`.** `snapshot --json --once` emits exactly one document; missing DB → reason code; corrupt/newer → exit 1; existing `stats` golden tests must pass **unmodified**.
- [ ] **Step 6 — performance.** `MYTOKEN_STRICT_PERF=1 go test ./internal/query -run TestQueryPerformanceBudget -v -count=1` plus a new snapshot budget test (<100 ms end-to-end, <50 MiB) per `docs/ROADMAP.md:64`.
- [ ] **Step 7 — GUI (only after D3).** Tray statusline strip reads the shared snapshot; re-baseline `internal/gui/snapshot_test.go` and record the PNG diff (multi-instance plan R7: 46 fixtures, ten already differ 7–13 px).

---

## 7. Suggested first working slice (V1)

Narrow but does not weaken the full goal:

1. `internal/store` read-only handle + marker validation (Steps 1–2).
2. `internal/snapshot` envelope v1 carrying `totals`, `cache`, `freshness`, `sources`, `schema` only.
3. CLI `snapshot --json --once` (dispatch in `internal/cli/cli.go:26-59`, usage in `internal/cli/commands.go:25-82`, flags via `commonFlags` `internal/cli/options.go:27-44` and `resolveFormat` `:166-183`), reading through `query.NewService` over the read-only handle. `stats` untouched.
4. **No GUI change, no statusline session-id plumbing in V1.**

That proves read-only correctness, versioned output, unavailable semantics and the perf budget without committing the contested product choices.

---

## 8. Shared GUI/CLI snapshot and statusline

- **Placement:** new `internal/snapshot` depending on `store` (read-only handle), `query` (existing `Service`), `pricing` (`HasPrice` only) and `reconcile.Service.Sites` (offline balance). It must **not** import `internal/app`.
- **GUI:** `internal/gui/state.go:286-335` (`loadSpan`, `loadShared`, `loadSessions`, `loadDetail`) currently call `Q` directly. Options (§9 D3): (a) only the tray/statusline strip reads the snapshot, pages keep `Q`; (b) all GUI reads go through the snapshot. (a) is the smaller slice and satisfies "statusline、托盘、MCP 共用同一份" for the strip.
- **Statusline fields** (`docs/ROADMAP.md:201`): session cost — `query.Service.Session` / `Sessions` or `Totals` with a session filter; cache hit rate — `query.Service.CacheTotals` (`internal/query/cache_query.go`); relay balance — `reconcile.SiteStatus.Balance` / `store.LatestRelayBalance` (`internal/store/store.go:121`). Session id source is a product decision (§9 D4).
- **tok/s** already belongs to `internal/ingestrate` (`ingestrate.go:3`); it is a pure in-memory `Tracker` and fits as another `sources` entry.

---

## 9. Missing wiring this plan must not silently invent

### 9.1 Persisted stats timezone needs a separate write path

`task_plan.md:15` wants the persisted timezone defaulted from the first system timezone (week starts Monday). Today that initial write is a hidden side effect of `store.Open` → `migrate` → `EnsureLocalDays` (`internal/store/aggregate.go:178,253`). The snapshot must never perform it. Options, **not chosen here**:

1. keep it at the normal writable open; the snapshot reports `rollupsCurrent:false` and still returns correct fact-based numbers;
2. an explicit `mytoken init` / `stats --init-timezone` write command;
3. an explicit `--refresh` (separate from `--once`) that opens writable and runs `EnsurePrices`/`EnsureLocalDays`.

### 9.2 Open decisions (user must confirm)

- **D1** Exit code for "index not yet created": `0` + reason code, or `1`?
- **D2** Freshness definition: derived signals only, or a persisted `last_scan_at`?
- **D3** GUI read path: strip-only (a) or all reads (b)?
- **D4** Statusline session id source (harness stdin / env var / explicit flag).
- **D5** Timezone-init command shape (§9.1 options 1/2/3).
- **D6** `immutable=1` fallback for read-only WAL open.
- **D7** Command name: `snapshot` subcommand vs `stats --json --once`.
- **D8** Non-creating data-dir resolution: new exported `paths` function vs an internal helper in `internal/snapshot`.

---

## 10. Risks

- **R1** Read-only WAL open on a DB with no `-shm` is platform/state dependent; needs the §6 T4 matrix and a documented fallback (D6).
- **R2** `time.Local` is process-global and read by `mytoken_day`/`mytoken_hour` (`internal/store/aggregate.go:14-31`) and `LocalDayZone()` (`:238`). A `--timezone` override is not safe to run concurrently with in-process GUI readers.
- **R3** `internal/store` and `internal/query` are being edited right now (workflow-24: instance dimension). The read-only handle must land after that migration quiets, or it conflicts on `store.go`/`migration.go`/`aggregate.go`.
- **R4** The required-marker set changes as `instance.v1` lands; the validator must be table-driven.
- **R5** Statusline latency budget (ROADMAP:64) includes connection open — a fresh read-only connection per invocation vs a long-lived reader is a real design fork.
- **R6** `Close` performs no checkpoint today; a read-only connection must not leave `-shm` behind or change `-wal` state.
- **R7** 46 GUI screenshot fixtures churn (multi-instance plan R7); Step 7 must record the diff rather than claim pixel equivalence.

---

## 11. Non-claims and evidence gates

- This plan **does not** claim 0.4.0 completion, GUI/CLI parity, MCP, tray integration, budget/notify, or vendor-identity completion.
- Implementation gates: `go vet ./...` and `go test ./... -count=1` exit 0; `-race` on `internal/store` (SPEC §10); the strict perf gate above.
- Evidence, when implemented, goes to `docs/verification/v040-readonly-snapshot.md` with the WAL matrix, before/after hashes and strict-perf numbers. No release, commit or push is authorized here.

Self-review: covers the read-only contract, the versioned `--json --once` envelope, unavailable-vs-zero semantics, freshness and schema state, the separate timezone-init write, legacy `stats` preservation, exact files/APIs, a small test-first sequence and a first slice that proves the hard part without pre-empting the open product decisions.
