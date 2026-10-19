# 0.4.0 Multi-Instance Data Sources Plan

Read-only architecture planning. No code, shared planning file, harness config, database, credential or real log was modified while producing this document. Scope: task_plan.md phase 4 (unified multi-instance paths, watcher/cursor lifecycle, collection diagnostics) plus the phase-3 default-instance migration it depends on, bounded by docs/ROADMAP.md lines 192–196.

## 1. Binding contract

- GUI and CLI resolve data roots through **one shared implementation**; neither owns a private copy of the rules.
- Resolution precedence per harness: **explicit user config > that tool's own env/config > platform default**. Only known or user-specified locations are read; no default full-disk search; harness config files are never modified.
- An **invalid explicit path fails loudly** (non-zero exit / error card) and does **not** silently fall back to a different dataset. Missing tier-2/tier-3 defaults keep today's silent-skip behavior but must appear in diagnostics with a reason.
- Every configured root has a **stable opaque instance ID that survives a move**; the path is a mutable attribute, never the identity.
- New vs move vs backup copy is an **explicit user declaration**, never inferred; suspect copied logs are surfaced for a user decision, never auto-merged.
- Independent instances whose content is byte-identical **must not dedup into one request**; watchers, cursors, session identities and reports must not collide.
- Diagnostics show actual scanned path, path source, last successful scan, recognized file/session counts and a machine-readable reason; they distinguish "nothing collected" from "usage is zero", never print credentials, and never echo raw env/config values.
- Existing data becomes the reserved **default instance** through a consistent backup + single-transaction migration; per-request tokens, request counts, historical cost and billing `Origin`+`KeyID` are unchanged, and failure rolls back.

## 2. Verified interface inventory

| Surface | Location | Today | Gap for multi-instance |
|---|---|---|---|
| Data dir | `internal/paths/paths.go:10` `DataDir()`; `:43` `DBPath()` | `MYTOKEN_HOME` else platform default; no config file | No place for a persistent, GUI-readable source config |
| App wiring | `internal/app/app.go` `open(refresh bool)` | `paths.DataDir()`/`DBPath()`; `scan.New(st, resolver, prices)` | Single global parser set; no instance input |
| Parser interface | `internal/harness/harness.go` `Parser{Harness,Roots,Discover,Parse}`; `Register/All/Get` | One parser per harness, built in package `init()` from env; `Register` overwrites by harness id | No way to construct a second rooted parser without mutating the global registry |
| Rooted constructors | e.g. `internal/harness/claude/claude.go:46-70` `New()/NewWithRoots(roots ...string) *Parser` | Every package has the function, none exposes it through the registry | Need a registry-level rooted factory |
| Env overrides | 21 `MYTOKEN_*_DIRS` lists, cited in section 11 | Highest precedence inside each `defaultRoots`, env-only | Inconsistent across harnesses; cannot express two instances |
| Scanner | `internal/scan/scanner.go` `New(st,resolver,pricer, parsers ...harness.Parser)` | `seen` key `harness+"\x00"+path`; `dirs` keyed by path; `st.Cursors(ctx,h)` keyed by path; `maxWatchDirs=2048`, `newDirBudget=32` | No instance dimension in any key |
| Fact schema | `internal/store/compact.go:29` `event_data ... PRIMARY KEY(harness,dedup_key)` | One row per (harness, dedup key) | Identical copied logs collapse |
| Dimensions | `internal/store/compact.go:8` `dimensionColumns = "harness,session_id,parent_id,project,model,provider,base_url,resolved_provider,attrib,raw_project"`; `:25` `UNIQUE(...)` | Session identity is a dimension string only | Same session id across instances merges |
| Cursors | `internal/store/store.go:70` `cursors(harness,path) PRIMARY KEY`; `:99 Cursor`, `:114 Cursors` | Path-keyed per harness | Same absolute path across instances collides |
| Write | `internal/store/store.go:144` `Write{Harness,Path,Batch,Resolutions}`; `:151` positional literal | No instance field | Instance must travel to the transaction |
| Rollups | `internal/store/aggregate.go:45` `daily_usage PRIMARY KEY(day,harness,session_id,resolved_provider,model,project,attrib)`; `internal/store/cache.go:128` `cache_daily` | Session-keyed | Rollups would merge instances and drift reports |
| Backup | `internal/store/backup.go` `Backup(ctx, destination)` (`O_CREATE\|O_EXCL` 0600, `VACUUM INTO`, fsync, quick_check) | Ready to use | — |
| Migration pattern | `internal/store/migration.go`, `internal/store/compact.go:15 migrateCompact`, `internal/store/provider_migration.go` | Additive columns, digest before/after, single transaction, rollback | Precedent for the instance-scope rebuild |
| Doctor | `internal/cli/commands.go:173 runDoctor`; types `:140-171` | `doctorHarness{Harness,Roots[]{Path,Exists},Sources,Events}` | No path source, kind, last scan, file/session count, reason |
| GUI sources | `internal/gui/sources.go` (`loadSources`, `sourcesCard`), `internal/gui/pages.go:221` | Shows existing roots only | Same gap; no validate/rescan affordance |
| Settings | `internal/query/settings.go:66 Settings` (store `settings` k/v), `internal/query/settings_service.go` | Write side used by GUI and CLI | Natural home for per-root status, not for bootstrapping paths |
| Query filter | SPEC §5 `Filter{Range,Harnesses,Providers,Models,Project}` | No instance selector | Reports sum across instances; no per-instance drilldown |
| Session detail | SPEC §5 `Session(ctx, harness, id)` | No instance argument | Ambiguous once two instances share a session id |

## 3. Architecture decision

**Chosen: one database, instances as an explicit dimension in facts, cursors and rollups.**

- `instance_id` joins `event_data`'s primary key, `event_dimensions`' unique key, `cursors`' primary key and both rollup keys. Identical copied logs are different rows because identity is part of the key, not inferred from content.
- The reserved id `default` (constant `instance.DefaultID`) is used for every legacy row and for all tier-2/tier-3 roots. Existing dedup keys are stored **verbatim**, so the phase-3 history invariants hold with no key rewriting.
- Instance paths and IDs live in a versioned `sources.json` under `paths.DataDir()`, read before the database opens (the database path itself must not depend on data already in the database).

**Rejected alternatives and why:**

- *Namespace the stored `dedup_key` with an instance prefix.* Avoids a primary-key rebuild, but `events.dedup_key` is read verbatim into `query.AttributedEvent.DedupKey` (`internal/query/service.go:143`) and SPEC §5.1 requires the original key untruncated and unhashed; a prefix would surface through the query/view surface. Rejected.
- *One database per instance.* Trivially isolated, but `query.Service` is built over one `*store.Store` (SPEC §5, `internal/app/app.go`), so cross-instance reports, the GUI's single window and `Subscribe` would each need multi-DB aggregation. Rejected for 0.4.0.
- *Derive instance identity from the root path or a content hash.* A move changes the path and a copy has identical content, so both violate "stable across move" and "copies must not dedup". Rejected.
- *Auto-merge a root whose content matches a known instance.* Explicitly forbidden by ROADMAP line 195; rejected even as a default.

## 4. Config contract (`sources.json`)

Location `filepath.Join(paths.DataDir(), "sources.json")`, mode 0600 in the existing 0700 dir, written atomically (temp file + rename) by the app only.

```json
{
  "version": 1,
  "instances": [
    {
      "id": "inst_7f3k2m9q4t",
      "label": "work laptop",
      "harness": "claude-code",
      "roots": ["/Users/me/work/.claude/projects"],
      "kind": "instance",
      "origin": "user",
      "movedFrom": ["/Volumes/old/.claude/projects"],
      "createdAt": "2026-10-13T00:00:00Z"
    }
  ]
}
```

- `id`: opaque, generated once (`inst_` + 128 random bits, base32); never derived from path, content, label or harness. `"default"` is reserved and rejected in config.
- `kind`: `instance` | `backup-copy`; `origin`: `user` | `default`.
- Rules: `harness` must be `model.Known()`; `roots` may be empty only for a disabled entry; exactly one enabled entry per existing instance id; a *replace* entry (`"disabled": true`) suppresses that harness's tier-2/tier-3 roots.
- Semantics: if any enabled user entry names harness `H`, that set **replaces** the legacy chain for `H` (two entries = two instances). With no user entry, the legacy chain runs unchanged and every root belongs to `default`.
- Tier-2/tier-3 roots keep today's silent skip when missing, but resolution returns a `Problem` for each so diagnostics can print a reason.
- `Load(dir)` returns an empty config when the file is absent; malformed JSON, an unknown `version`, a duplicate id or a reserved id is a fatal `Problem`, not a fallback.

Proposed interface (`internal/instance`):

```go
const DefaultID = "default"

type Kind string    // KindInstance, KindBackupCopy
type Origin string  // OriginUser, OriginDefault
type PathSource string // PathUser, PathHarnessEnv, PathHarnessConfig, PathDefault

type Entry struct{ ID, Label string; Harness model.Harness; Roots []string; Kind Kind; Disabled bool }

type Problem struct{ Code, Path, Harness, Message string } // codes: invalid-config, missing-dir,
// not-dir, permission, duplicate-root, overlap, unsupported-harness, no-factory, reserved-id

type Resolved struct {
    ID, Label string
    Harness   model.Harness
    Roots     []string
    Source    PathSource
    Kind      Kind
    Disabled  bool
}

func Load(dataDir string) (*Config, error)
func (c *Config) Save(dataDir string) error
func Resolve(cfg *Config, parsers []harness.Parser) ([]Resolved, []Problem)
func Validate(roots []string, known []Resolved) []Problem // stat + containment + overlap, no parsing
func Add(cfg *Config, harness model.Harness, roots []string, kind Kind) (Entry, error)
func Move(cfg *Config, id string, roots []string) (Entry, error) // keeps ID, appends movedFrom
func Remove(cfg *Config, id string) error
```

Rooted factory (proposed in `internal/harness`):

```go
type Rooted interface{ WithRoots(roots ...string) Parser }
func RegisterRooted(h model.Harness, f func(roots ...string) Parser)
func NewRooted(h model.Harness, roots ...string) (Parser, bool)
```

`NewRooted` must build a fresh parser and must not touch the global registry map (parallel tests and GUI/CLI resolution both depend on this).

## 5. Undecided product choices (user decision, not blocking this plan)

These are separate from the technical choices in section 6; each has a proposed slice-1 default recorded so work can proceed, but the default is a product call.

1. **Backup-copy semantics — confirmed by user in round 9.** A declared `backup-copy` is stored and shown in diagnostics but **not scanned or counted**. Promotion to an independent instance is explicit; independent instances count independently even with identical contents. Moving a source preserves its existing instance identity.
2. **Default report presentation.** Sum instances into one total with an optional per-instance drilldown, versus instance as a first-class grouping/selector. Affects ranking/overview/CLI defaults.
3. **Replace-vs-append for explicit config.** Section 4 proposes replace, so adding a custom root stops scanning the default root. Append semantics would be the opposite; the choice changes what an existing user sees after adding one path.
4. **How visible instances are.** First-run/setup affordance versus Settings-only, and whether the GUI can add/move/remove in 0.4.0 or slice 1 stays CLI-first with read-only GUI.
5. **Instance labels.** User-editable name versus a label derived from the path (with a privacy note: paths can contain user names).
6. **Relay-bill matching with instances.** May one relay bill match at most one instance (first/most-specific wins), or is a copied instance's request allowed to match the same bill? Both outcomes are defensible; a rule must be explicit.
7. **Downgrade after migration.** A 0.4.0 database is not readable by 0.3.x. Is restoring the pre-migration backup sufficient, or is an export/`--downgrade` path required?
8. **Passive suspect detection.** Whether a root whose manifest matches a known instance blocks scanning until resolved (safest, may stall collection) or scans under a new instance pending resolution.
9. **Legacy resolver watches.** Today's scanner always watches `~/.claude` and `~/.codex` for the attribution timeline when a resolver exists. Whether user-configured instances should suppress those watches.

## 6. Proposed technical choices consistent with the contract (pending lead review)

- Single DB with an instance dimension as in section 3.
- `sources.json` in `paths.DataDir()` as the shared GUI/CLI config, versioned, atomic, 0600.
- Reserved `default` id for legacy rows and tier-2/tier-3 roots; user ids are random and opaque.
- Move = update `roots` in place for the existing id, append `movedFrom`, drop that instance's path-keyed cursors and rescan; events are preserved and the stable dedup keys upsert, so no double count.
- Copy = explicit declaration at add time; no heuristic merge and no heuristic identity.
- Path-keyed cursor/`seen` keys gain the instance id; the watcher stays path-keyed (one OS watch per directory) with a global budget.
- Per-root scan status stored under a `settings` key (additive, no new table) and rendered by both doctor and the sources card.
- A migration that rebuilds `event_data`/`event_dimensions`/`cursors` and the two derived rollups in one transaction behind `Store.Backup`, following `migrateCompact`/`migration.go` precedent.

## 7. Workstreams, write scopes and dependencies

Write scopes are advisory but disjoint so future work can run in parallel. `internal/query`, `internal/store`, `internal/reporttime` and `internal/ingestrate` are being edited by other agents right now — B and G must not start until the provider-identity migration in `docs/superpowers/plans/2026-10-13-v040-provider-identity.md` lands and those files are quiet.

| ID | Scope | Primary files | Depends on |
|---|---|---|---|
| A | Config, ids, resolution, validation, add/move/remove | `internal/instance/*` (new), `internal/paths/paths.go` (read-only use) | — |
| B | Instance dimension in schema + migration | `internal/store/instance_migration.go` (new), `internal/store/compact.go`, `store.go`, `aggregate.go`, `cache.go`, `migration.go` | provider-identity migration landed |
| C | Rooted registry factory | `internal/harness/harness.go`, one `WithRoots` method per `internal/harness/<h>/*.go` (mechanical, 28 packages) | — |
| D | Per-instance scanner | `internal/scan/scanner.go`, `scanner_test.go` | A, B, C |
| E | App wiring + shared resolution | `internal/app/app.go` | A, C |
| F | CLI diagnostics and source commands | `internal/cli/commands.go`, `commands_test.go`, `internal/cli/options.go` | A, E |
| G | GUI read-only sources/settings surface | `internal/gui/sources.go`, `pages.go`, `state.go`, `data.go` | A, E |
| H | Query instance dimension | `internal/query/query.go`, `service.go`, `settings.go` | B, E |

Suggested order: A and C in parallel → B → D → E → F/G in parallel → H.

## 8. First vertical slice (bounded, end-to-end)

Goal: a user can declare a second root for a harness that already has a default root; the app ingests it under a distinct stable instance id; byte-identical copied logs produce two independent stored request sets; cursors and reports stay separate; existing data migrates to `default` with a consistent backup and unchanged invariants; diagnostics show path source, last scan, counts and reason. Surface is CLI plus a read-only GUI row; no GUI add/move dialogs, no passive copy detection, no per-instance query selectors.

**V1 — Config, ids, resolution (A).**
- [ ] Tests first in `internal/instance/config_test.go` and `resolve_test.go`: absent file; malformed JSON; unknown version; duplicate id; reserved `default` id; roots with spaces and Chinese; atomic save leaves no temp file; file mode 0600; explicit > `MYTOKEN_*_DIRS` > tool env > default; explicit invalid root yields `Problem{Code: "missing-dir"}` and no resolved root; replace semantics; two entries on one harness; disabled entry suppresses defaults.
- [ ] Observe failures, then implement `internal/instance/config.go`, `resolve.go`, `validate.go`.
- [ ] `go test ./internal/instance -count=1` green; add an idempotence and a "no secret in `Problem.Message`" regression.

**V2 — Rooted registry factory (C).**
- [ ] Add a conformance test (in `internal/harness` or `harnesstest`) asserting every registered parser implements `Rooted`, that `NewRooted(h, roots...).Roots()` equals the given roots, and that `harness.All()` is unchanged after `NewRooted` (no global mutation).
- [ ] Add `RegisterRooted`/`NewRooted` and the one-line `WithRoots` method per harness package; a harness without it must report `no-factory`, never fall back to defaults.
- [ ] `go test ./internal/harness/... -count=1`.

**V3 — Schema and migration (B).**
- [ ] Disk-backed tests in `internal/store/instance_migration_test.go` before implementation, reusing the legacy fixtures of `migration_legacy_test.go`/`migration_integrity_test.go`: backup created and verified before mutation; `instance_id='default'` for every pre-existing row; per-request tokens/requests/cost digest identical before/after; `Origin`+`KeyID` rows untouched; two identical dedup keys under two instance ids both stored; cursor and rollup keys separated by instance; reopen idempotent; injected failure rolls back and leaves the pre-migration schema.
- [ ] Implement `migrateInstanceScope(tx)` calling into `migrate()`; rebuild `event_data` PK `(harness,instance_id,dedup_key)`, add `instance_id` to `dimensionColumns` and the dimensions unique key, rebuild `cursors` PK `(harness,instance_id,path)`, rebuild `daily_usage` and `cache_daily` keys, recreate the `events` view and INSTEAD OF triggers.
- [ ] Add `Write.InstanceID`, `Store.Cursor(ctx,h,instance,path)`, `Cursors(ctx,h,instance)`; fix the positional literal at `internal/store/store.go:151`.
- [ ] `go test ./internal/store -count=1` and `go test -race ./internal/store`; measure with `MYTOKEN_MIGRATION_PERF=1 go test ./internal/store -run TestCopiedDatabaseMigrationPerformance` on a `/tmp` copy only.

**V4 — Per-instance scanner (D).**
- [ ] Tests in `internal/scan/instance_test.go`: two targets, same harness, identical content → both event sets stored (no dedup); `seen`/cursor keys distinct per instance; path switch clears cursors only for the changed instance and keeps events; `Progress`/status reported per instance; watcher directory count unchanged versus one instance on the same roots.
- [ ] Add `scan.Target{ID string; Parser harness.Parser}` and `NewWithTargets(st, resolver, pricer, targets ...Target)`; keep `New(..., parsers ...harness.Parser)` wrapping each parser as `Target{ID: instance.DefaultID, ...}` so existing tests compile.
- [ ] Thread the instance id through `scanOne` → `store.Write`, cursor loads and the per-root status written at the end of `scan()`.
- [ ] `go test -race ./internal/scan -count=1`.

**V5 — App wiring and shared resolution (E).**
- [ ] `open(refresh bool)` loads `sources.json`, resolves targets, passes them to `scan.NewWithTargets`; a fatal `Problem` from an explicit root returns an error so GUI and CLI both fail the same way.
- [ ] `app.App` exposes the resolved instances for GUI/CLI diagnostics.
- [ ] `go test ./internal/app -count=1`.

**V6 — CLI diagnostics and commands (F).**
- [ ] Extend `doctorReport`/`doctorHarness`/`doctorRoot` additively with `pathSource`, `kind`, `instanceId`, `lastScanAt`, `files`, `sessions`, `reason`; assert existing JSON fields are unchanged in `internal/cli/commands_test.go`.
- [ ] Add `mytoken sources list|validate|rescan` and `sources add --harness H --root P --kind new|move|backup-copy [--id ID]`, plus `sources move`/`sources remove`.
- [ ] Assert a bad explicit path exits non-zero with a clear message and no fallback scan, and that no output contains a raw env value or credential.
- [ ] `go test ./internal/cli -count=1`.

**V7 — GUI read-only row (G).**
- [ ] `sourcesCard` renders per root: instance id/label, harness, path, path source, exists/reason, last scan, files/sessions; `Hooks` gains `Validate(root)` and `Rescan(instanceID)` wired to scanner status.
- [ ] Re-baseline the affected snapshots and record the diff in `docs/verification/` (see risk R7).
- [ ] `go test ./internal/gui -count=1` and the snapshot command recorded in the mygo-baseline plan.

Slice-level evidence: `go vet ./...`, `go test ./... -count=1`, `go test -race ./internal/scan ./internal/store`, plus the migration benchmark above on a `/tmp` copy. No real credential, real user log or production database is used.

## 9. Deferred beyond the first slice

- Passive suspect detection (manifest of relative path + size + head sha1 per root) and the pending-decision surface.
- GUI add/move/remove dialogs and first-run instance setup.
- Instance selectors in query/CLI/overview, and the relay-bill instance rule.
- `backup-copy` dedup-against-source.
- Symlink-policy hardening beyond containment checks.
- Windows/Linux behavior beyond cross-compile; real-machine evidence stays a release gate.

## 10. Compatibility plan

- **Existing install:** no `sources.json` → resolution is byte-for-byte today's behavior; all roots resolve to `default`; the first open migrates rows to `instance_id='default'` after a verified backup. No user action required and no re-scan is forced.
- **Existing DB:** read-only compatibility through the `events` view; the view keeps its current column set plus `instance_id`; `AttributedEvent`/`SessionRow` gain an additive instance field. SPEC §3 (model) and §5 (query) note that interface-signature changes need lead approval, so the plan deliberately avoids touching `model` types and only proposes additive query fields.
- **Existing env overrides:** all 21 `MYTOKEN_*_DIRS` variables and tool env vars keep tier-2 precedence; none are removed.
- **Existing CLI/GUI output:** `doctor` JSON only gains fields; `stats`/`sessions`/`cache` output is unchanged unless the user opts into an instance selector (deferred to slice 2).
- **Downgrade:** 0.3.x cannot read the migrated schema; the pre-migration backup is the documented restore path (product choice P7 covers whether an export is also required).
- **Snapshots/tests:** the plan re-baselines only the screenshots its own UI rows change, keeping the MyGo-upgrade baseline evidence intact and documented separately.

## 11. Zero-path/config discovery IO privacy assessment

With no `sources.json` and no env override (the state of every existing install), startup reaches `app.open` → `scan.New(st, resolver, prices)` → `harness.All()`: 28 registered parsers, each constructed in its package `init()` from env. What that costs and touches:

- **Discovery IO is bounded, not a disk scan.** 27 `Discover` implementations `os.Stat` each declared root and walk only inside it (`internal/harness/opencode/opencode.go:177-184` is representative); none walks outside its roots. Confirmed for the sqlite-backed harnesses (goose, crush, zed, cline, codebuff, workbuddy) and the JSONL/JSON ones. There is no home-wide or full-disk search, so today's behavior already satisfies "only read known locations".
- **Extra reads during the same window.** Directory listings and file metadata; read-only opens of harness SQLite databases; `internal/harness/kimi/kimi.go:519` reads `<share>/config.toml` for a default model name; `internal/harness/claudedesktop/claudedesktop.go:134` uses `os.UserConfigDir()`. Discovery does **not** read credential files. `source.NewHarnessConfig(home)` (`internal/source/harnessconfig.go:88,186`) reads `~/.claude/settings.json` and `~/.codex/config.toml` endpoints during resolver refresh, and `source.Credentials` reads only env tokens on demand (`ANTHROPIC_AUTH_TOKEN`, codex `env_key`) — never during `Discover`.
- **Watcher IO.** `Scanner.Run` watches `p.Roots()` plus `~/.claude` and `~/.codex` when a resolver exists, bounded by `maxWatchDirs = 2048` and `newDirBudget = 32`. That is up to 28 harness root chains plus two home directories, i.e. file-descriptor pressure, not content exposure (contrast `internal/scan/watch_test.go` which exists because watching a harness home exhausts descriptors).
- **Privacy risks introduced by user paths.** (1) Diagnostics print absolute paths, which can contain user or tenant names — required by ROADMAP line 194, so keep paths visible but never print raw `MYTOKEN_*_DIRS` values, config file bodies or credential material. (2) `os.Stat`/SQLite error strings can embed paths; scrub secrets, not paths. (3) A user root can point at any directory; discovery must read only files matching that parser's patterns and must refuse to follow `..`/symlinks that escape the declared root (containment check in `instance.Validate`). (4) `sources.json` must be 0600 in the existing 0700 data dir and must contain no tokens. (5) A new hardcoded `os.UserConfigDir()` use would widen the read set; slice 1 adds no new implicit locations, it only narrows defaults when the user opts in.
- **Assessment:** zero-config discovery is privacy-acceptable and bounded; the plan's changes must preserve that property (explicit replace suppresses defaults rather than adding a second implicit read path) and must not turn a validation action into a parsing action (`validate` stats/lists; only `rescan` parses).

## 12. Critical risks

- **R1 — Fact-table rebuild cost.** `event_data`'s primary key cannot be altered in place, so the migration rebuilds a 200K+-event table inside one transaction. Precedent `migrateCompact` exists, but a fresh open already costs ~9.2s on the real-schema copy; a naive rebuild can push first-open past the budget. Mitigate by rebuilding in the same transaction without an extra VACUUM, rebuilding derived rollups from facts, and measuring on a `/tmp` copy before committing.
- **R2 — Silent rollup drift.** `daily_usage` and `cache_daily` triggers must be recreated with the instance key, or totals drift while individual queries still look right. The before/after digest test in V3 must cover rollups, not just facts.
- **R3 — Frozen surfaces.** SPEC §3 freezes `internal/model` and SPEC §5 note requires lead approval for query signatures. The plan carries instance identity in `store.Write`, `store` cursors and additive query fields only; any temptation to add `InstanceID` to `model.UsageEvent` or to change `Session(ctx, harness, id)` must go back for approval.
- **R4 — Global registry mutation.** `harness.Register` overwrites by harness id and every parser is built in `init()`; a rooted factory that mutates the registry would make GUI/CLI resolution order-dependent. V2's test must assert `harness.All()` is unchanged after `NewRooted`.
- **R5 — 28 mechanical factory methods.** A missed `WithRoots` must fail as `no-factory`, not fall back to default roots; otherwise one harness silently ignores the user's explicit path, which is the exact failure ROADMAP line 193 forbids.
- **R6 — Watcher budget.** `maxWatchDirs = 2048` is global; two instances of a broad harness can starve later instances. Watch directories must be deduplicated by resolved path across instances and the budget enforced globally with a diagnostic when hit.
- **R7 — Snapshot churn.** Adding settings/sources rows changes the 46 screenshot fixtures, ten of which already differ by 7–13px from the MyGo upgrade. Re-baseline only the affected images and keep the two baselines documented separately, or the phase-2 evidence becomes ambiguous.
- **R8 — Path identity subtlety.** Overlap/containment comparisons must use resolved absolute paths (`filepath.EvalSymlinks` + case folding on darwin/windows); string prefix comparison misses a symlinked or case-variant duplicate and double counts.
- **R9 — In-flight collisions.** `internal/query`, `internal/store`, `internal/reporttime` and `internal/ingestrate` are under active edit; V3 and V8 must wait for the provider-identity migration to land, and the identity migration touches the same `event_dimensions`/`event_data` structures this plan rebuilds. Sequencing, not parallelism, is the mitigation.
- **R10 — Secret leakage through roots.** A user can paste a URL or token into a root field; validation must reject non-directory roots with a message that echoes the path but never a credential-shaped value, and `doctor --json` must stay free of env values.
- **R11 — Ambiguous legacy session display.** With two instances sharing a session id, `Service.Session(harness, id)` cannot stay correct without an instance argument; the GUI must pass the instance id from the row it selected rather than letting the first match win.

## Checkpoint

Slice V1–V7 is a proposed implementation checkpoint, not a user-approved reduction of the complete 0.4.0 scope: it delivers a working second instance end-to-end (config → resolution → rooted parser → scanner keys → store dimension → migration → diagnostics) rather than new unused types. Stop for a user decision before any of section 5's product defaults is treated as settled, before touching `internal/model` or a `query.Service` signature, and before any destructive or migration-affecting change to a real database. Record actual evidence and outstanding scope in progress.md when the slice executes; no commits, releases, servers or real-data runs are part of this plan.
