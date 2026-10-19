# 0.4.0 Provider Identity and Migration Plan

## Contract
Display identity derives from a conservative normalized endpoint, NOT provider label and NOT billing Origin+KeyID. Preserve historical costs, token classes, request rows, original attribution, raw endpoint and price matching names. Unknown endpoints stay separate. No real database or credentials are used during implementation tests.

## Task 1 — Pure endpoint identity (completed; full suite/vet passed)

All four steps below executed in goal round 1; tests first failed for missing APIs, numeric-host regression then failed with four demonstrated cases, and all passed after implementation. Task 2's Backup API is also implemented/tested, but migration integration and rollback invariants remain open.
- [ ] Add table-driven tests in internal/provider/identity_test.go for host/scheme case, default ports, one trailing slash, path/tenant/case preservation, explicit scheme, invalid ports, local proxies and secret-bearing/ambiguous URLs. Test stable versioned ID independent of presentation labels. No inferred /v1 equivalence.
- [ ] Run `go test ./internal/provider` and observe failure for missing implementation.
- [ ] Implement `NormalizeEndpoint(raw string) (string, bool)` and `EndpointID(raw string) (string, bool)` in internal/provider/identity.go. Strict http(s), no userinfo/query/fragment, no loopback/unspecified hosts; refuse uncertainty rather than remove identifying components. Full SHA-256 namespace v1, no credential retention.
- [ ] Verify focused tests; add idempotence and distinct-tenant regression checks; run full test suite.

## Task 2 — Backup and transactional migration
- [ ] Inspect compact event dimensions, aggregate schema, pricing refresh and query integration before choosing schema changes.
- [ ] Add disk-backed migration tests before implementation: consistent WAL snapshot, existing backup collision, backup permission/failure, transaction rollback on invariant mismatch, token classes/request/cost preservation, idempotent reopen. Avoid copying only SQLite main file.
- [ ] Implement backup-before-mutation guard with contextual errors and restrictive permissions; never overwrite existing backup. Validate backup before migration. All derived identity changes in one transaction with before/after invariants. No pricing_fingerprint clearing due to display identity.
- [ ] Keep old attributable names and bill IDs untouched. Record migration version only after validation. Full regressions.

## Task 3 — New ingestion and shared query identity

Progress (round 5): ingestion metadata is transactional and tested. Additive `Service.ProviderGroups` preserves the legacy ByProvider/pricing API. It reads endpoint-aware fact aggregates because existing daily rollups discard base_url; this deliberately prioritizes correctness over unmeasured optimization. Stable endpoint IDs group cross-name/harness usage; unresolved identities are scoped to harness/session/original provider/raw endpoint and hashed to avoid exposing sensitive URLs. Members retain original attribution, not billing balances. Existing model aliases and name filters continue to apply before grouping.

Remaining exact steps:
- [ ] Add explicit ProviderIDs filtering consistently to totals/series/session/cache paths (do not silently treat IDs as price labels).
- [ ] Add display-only alias persistence; alias writes must not reprice or mutate historical attribution.
- [ ] Wire GUI/CLI provider pages and pickers with ID versus label distinctions; retain account/billing drilldown.
- [ ] Measure fact-query cost before choosing endpoint-aware rollup migration; include timezone and range-edge equivalence tests.
- [ ] Extend source-scoped unresolved IDs after stable multi-instance identity exists.

- [ ] Write tests for same endpoint different names/harnesses merging usage, path isolation, uncertain source separation, explicit aliases, independent key/account balances and historical price boundaries.
- [ ] Add identity persistence and aliases with a shared query implementation used by GUI/CLI; keep pricing on original provider labels and billing on Origin+KeyID. Add drilldown metadata.
- [ ] Run attrib/store/query/CLI/GUI tests and full suite; check migration invariants and snapshot prerequisites.

## Checkpoint
Record actual evidence and outstanding scope in progress.md. Tasks 2–3 need exact interface-level substeps after inspection; Task 1 alone is not feature completion. Continue automatically on passing checkpoints; stop for decisions affecting destructive data operations/privacy/scope. No commits or releases.
