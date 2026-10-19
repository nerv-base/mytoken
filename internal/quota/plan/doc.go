// Package plan reports subscription-plan quota windows for the coding tools a
// user has installed and signed in to.
//
// It is the online counterpart of the offline Codex rollout parser in
// package quota: Codex quota still comes from the local rollout log, while the
// other providers ask the vendor's usage endpoint with the credential the tool
// itself already stored. Nothing here needs a separate consent step — a
// credential that exists on disk is treated as the user's decision to let the
// tool it belongs to report its own quota.
//
// # Contract
//
//   - Missing is not zero. A window the payload did not state is omitted; a
//     provider that returns no window at all returns an error, never 0%.
//   - Credentials are read into memory for one request. They are never logged,
//     never persisted, and never included in an error, a Status or a Snapshot.
//   - Only quota numbers, their reset instants and the fetch instant are
//     persisted (Service, key prefix quota.plan.<provider>).
//   - Providers are polled while the app runs (default every five minutes),
//     with exponential backoff on failure and the endpoint's Retry-After on
//     HTTP 429. The last good Snapshot is kept and served while a fetch fails.
//
// # Supported providers
//
// Codex (local rollout log), Claude Code (OAuth usage endpoint), Cursor
// (DashboardService Connect RPC, token from the Cursor state database), GitHub
// Copilot (copilot_internal/user, token from the legacy Copilot hosts.json) and
// Z.ai / BigModel GLM (monitor/usage/quota/limit, API key from the environment
// or a known key file).
//
// # Not supported
//
//   - Gemini CLI: Google stopped serving individual / AI Pro / Ultra consumer
//     tiers from the Cloud Code quota endpoint on 2026-06-18; only Workspace
//     and Enterprise accounts remain, and their short-lived tokens need a
//     refresh this package will not perform. Local credentials alone are
//     therefore not enough to show a quota.
//   - Kimi Code: the only discoverable local credential is the CLI's access
//     token, which lives about fifteen minutes and is rotated by the CLI. The
//     API key that would be stable is only available from the environment.
//   - MiniMax Coding Plan: the quota endpoint is driven by a browser session
//     cookie; no coding-plan credential is stored in a documented local file.
//   - Factory Droid: no documented or widely used quota endpoint is reachable
//     with a locally stored credential.
package plan
