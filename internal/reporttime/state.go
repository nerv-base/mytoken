package reporttime

// TimezoneState is the read side of the machine's reporting calendar: whether a
// fixed zone is actually configured, and what to tell a caller when it is not.
//
// It exists so every surface (the GUI card, the read-only snapshot metadata,
// the statusline and the budget gate) describes the same state in the same
// words. A caller must not present SystemPreview as the calendar: when
// Configured is false there is no fixed calendar, and calendar periods are then
// either an explicit one-invocation override or an unconfigured preview.
type TimezoneState struct {
	// Configured is true only when a fixed IANA zone is persisted and in use.
	Configured bool
	// Name is the configured IANA zone; empty while unconfigured.
	Name string
	// NeedsSelection asks for an explicit IANA choice. It is set when no stable
	// zone could be identified, or when the persisted value cannot be used: the
	// app never substitutes the ambient system zone for a fixed calendar.
	NeedsSelection bool
	// Problem explains what has to be fixed, in one actionable sentence; empty
	// when nothing is wrong.
	Problem string
	// SystemPreview names the machine's ambient zone for display only, so a
	// user can see what "unconfigured" would follow. It is never the persisted
	// choice and never claimed as one.
	SystemPreview string
}
