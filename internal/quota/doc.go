// Package quota reports provider quota windows from data that is already on
// disk, without any network access, credentials, or provider SDK.
//
// The package is deliberately isolated: it does not import net, os, syscall,
// or any other package in this module, so it cannot fetch quota, read
// credentials, or write anything. Callers hand it an io.Reader (for example a
// file opened by the scanner) and get back a Report describing what the log
// actually said.
//
// Reporting rules, in order of importance:
//
//   - Missing is not zero. A window field that the log did not carry stays nil
//     and is reported with ReasonMissing, never as 0.
//   - Nothing is inferred. Quota limits are never derived from locally counted
//     tokens, context-window sizes, or API billing. If the log has no
//     rate_limits payload, the answer is ReasonNoData.
//   - Every observation carries provenance: the source name the caller
//     supplied, the 1-based line, the record type, the observed instant when
//     the log carried a trustworthy one, and the limit identity that record
//     itself stated. A Report describes exactly one selected record: labels
//     from other records are never merged into it. Limit IDs seen elsewhere
//     are listed separately in Report.OtherLimitIDs instead of being
//     attributed to the selected window.
//   - The newest window-bearing record governs currency. A rollout is
//     append-ordered, so if the newest record that states (or fails to state)
//     a window is unusable, an older usable observation is retained only as
//     history: Status is stale with ReasonNewerInvalid, SupersededByLine names
//     the newer record, and ObservedAt stays the retained record's own exact
//     instant. An old value is never reported as fresh.
//   - Unsupported shapes are explicit. Fields the parser does not understand
//     are reported through Issue values rather than guessed at, and unknown
//     future fields are counted but tolerated.
//   - Raw log content never escapes. Errors, Issues, and the JSON encoding of
//     Report contain only values this package generated or explicitly
//     allowlisted labels, never free-form text from the log. A failure in the
//     caller's reader is returned as a *ReadError whose message is generated
//     by this package; the reader's own error text is arbitrary caller input
//     and is reachable only through Unwrap.
//
// The reader is bounded: it caps per-line size, total bytes read, and honors
// context cancellation, so a corrupt or enormous rollout log cannot exhaust
// memory.
package quota
