package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/ingestrate"
)

// IngestRateSettingKey is the exact settings key the newest committed
// live-ingestion rate is persisted under. A statusline that cannot call
// Scanner.IngestRate reads it from the index instead.
const IngestRateSettingKey = "ingest_rate.latest.v1"

// maxIngestRateSignalBytes bounds the persisted signal, like the quota one: a
// statusline must never be handed an unbounded blob.
const maxIngestRateSignalBytes = 4096

// ErrCorruptIngestRateSignal reports a stored value that is not a valid signal.
var ErrCorruptIngestRateSignal = errors.New("scan: stored ingest rate signal is corrupt")

// IngestRateSettings is the settings read side LoadIngestRateSignal needs.
// *store.Store satisfies it.
type IngestRateSettings interface {
	Setting(ctx context.Context, key string) (string, error)
}

// IngestRateSignal is the bounded persisted form of one rate snapshot.
type IngestRateSignal struct {
	// Timestamp is when the snapshot was taken. It is the freshness reference:
	// a reader more than Window past it must treat the signal as stale.
	Timestamp time.Time `json:"timestamp"`
	// Available is false when there is no usable live measurement; Reason says
	// why.
	Available bool `json:"available"`
	// Reason is the tracker's reason code, empty when Available.
	Reason string `json:"reason,omitempty"`
	// Window is the trailing window length in seconds.
	Window float64 `json:"window_seconds"`
	// Tokens is the output tokens committed inside the window.
	Tokens int64 `json:"tokens"`
	// Rate is Tokens divided by the window, per second.
	Rate float64 `json:"rate"`
}

// ingestReasonCodes is the closed set of tracker reason codes a stored
// unavailable signal may carry. Anything else did not come from this producer.
var ingestReasonCodes = map[string]bool{
	string(ingestrate.ReasonNoLiveSignal):     true,
	string(ingestrate.ReasonStale):            true,
	string(ingestrate.ReasonClockRegression):  true,
	string(ingestrate.ReasonOverflow):         true,
	string(ingestrate.ReasonCapacityExceeded): true,
}

// corruptIngestRate builds an error that identifies the stored signal as
// unusable without echoing whatever bytes were stored.
func corruptIngestRate(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorruptIngestRateSignal, fmt.Sprintf(format, args...))
}

// validateIngestRateSignal enforces the producer's contract on a decoded
// signal. A plain decode accepts an empty object and impossible counts, so the
// bounded reader must check the shape itself: a truthful timestamp, the
// tracker's own window, non-negative counts, a rate that agrees with the tokens
// it claims, and an available/reason pair that cannot both be set.
func validateIngestRateSignal(sig IngestRateSignal) error {
	if sig.Timestamp.IsZero() {
		return corruptIngestRate("timestamp is missing or zero")
	}
	if want := ingestrate.Window.Seconds(); sig.Window != want {
		return corruptIngestRate("window_seconds must be %v", want)
	}
	if sig.Tokens < 0 {
		return corruptIngestRate("tokens is negative")
	}
	if sig.Rate < 0 {
		return corruptIngestRate("rate is negative")
	}
	if sig.Available {
		if sig.Reason != "" {
			return corruptIngestRate("available signal must not carry a reason")
		}
		if want := float64(sig.Tokens) / ingestrate.Window.Seconds(); sig.Rate != want {
			return corruptIngestRate("rate does not match tokens over the window")
		}
		return nil
	}
	if sig.Reason == "" {
		return corruptIngestRate("unavailable signal has no reason")
	}
	if !ingestReasonCodes[sig.Reason] {
		return corruptIngestRate("unavailable signal has an unknown reason")
	}
	if sig.Tokens != 0 || sig.Rate != 0 {
		return corruptIngestRate("unavailable signal must not carry tokens or a rate")
	}
	return nil
}

// LoadIngestRateSignal reads the persisted signal. It reports false when
// nothing was ever published, and ErrCorruptIngestRateSignal when a value was
// published but cannot be trusted. The stored timestamp is returned as it was
// written: reading a signal never re-dates it.
func LoadIngestRateSignal(ctx context.Context, s IngestRateSettings) (IngestRateSignal, bool, error) {
	if s == nil {
		return IngestRateSignal{}, false, nil
	}
	raw, err := s.Setting(ctx, IngestRateSettingKey)
	if err != nil {
		return IngestRateSignal{}, false, err
	}
	if strings.TrimSpace(raw) == "" {
		return IngestRateSignal{}, false, nil
	}
	// Bound before decode: a reader must never parse an unbounded blob, and a
	// value over the producer's own bound is corrupt by construction.
	if len(raw) > maxIngestRateSignalBytes {
		return IngestRateSignal{}, false, corruptIngestRate("value is %d bytes, over the %d byte bound", len(raw), maxIngestRateSignalBytes)
	}
	var sig IngestRateSignal
	if err := json.Unmarshal([]byte(raw), &sig); err != nil {
		return IngestRateSignal{}, false, fmt.Errorf("%w: %v", ErrCorruptIngestRateSignal, err)
	}
	if err := validateIngestRateSignal(sig); err != nil {
		return IngestRateSignal{}, false, err
	}
	return sig, true, nil
}

// signalOf renders a snapshot in its persisted form.
func signalOf(at time.Time, snap ingestrate.Snapshot) IngestRateSignal {
	return IngestRateSignal{
		Timestamp: at.UTC(),
		Available: snap.Available,
		Reason:    string(snap.Reason),
		Window:    snap.Window.Seconds(),
		Tokens:    snap.Tokens,
		Rate:      snap.Rate,
	}
}

// newIngestTracker builds the tracker with the window itself as the freshness
// horizon, so telemetry older than one window reads unavailable.
func newIngestTracker() *ingestrate.Tracker {
	tr, err := ingestrate.New(ingestrate.Window)
	if err != nil {
		// Unreachable while Window satisfies New's contract; a nil tracker
		// only makes IngestRate report no live signal.
		return nil
	}
	return tr
}

func (s *Scanner) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// stampIngestLocked clamps at forward onto the scanner's ingest clock and
// returns it. Every observation and every snapshot passes through here, so the
// scanner's own calls can never reach the tracker out of order or in the past.
func (s *Scanner) stampIngestLocked(at time.Time) time.Time {
	if at.Before(s.ingestAt) {
		at = s.ingestAt
	}
	s.ingestAt = at
	return at
}

// observeIngestAt records newly committed live output tokens that were filtered
// to the window ending at the commit instant at. It passes the same instant the
// commit used through the scanner's monotonic stamp, so the observation and the
// window it describes cannot disagree about when "now" was.
func (s *Scanner) observeIngestAt(tokens int64, at time.Time) {
	if s.ingest == nil {
		return
	}
	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()
	_ = s.ingest.Observe(tokens, s.stampIngestLocked(at), ingestrate.ClassLive)
}

// heartbeatIngest records a live zero-token observation and returns the
// snapshot taken at the same instant. A live window in which nothing was
// appended is a real zero, not a stale signal.
func (s *Scanner) heartbeatIngest() (time.Time, ingestrate.Snapshot) {
	if s.ingest == nil {
		return time.Time{}, ingestrate.Snapshot{}
	}
	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()
	at := s.stampIngestLocked(s.now())
	_ = s.ingest.Observe(0, at, ingestrate.ClassLive)
	return at, s.ingest.Snapshot(at)
}

// IngestRate reports committed live-ingestion output tokens over the trailing
// window ending at now. It is safe to call concurrently with scanning and never
// writes anything: a read must not re-date an idle signal.
func (s *Scanner) IngestRate(now time.Time) ingestrate.Snapshot {
	if s.ingest == nil {
		return ingestrate.Snapshot{Reason: ingestrate.ReasonNoLiveSignal, Window: ingestrate.Window}
	}
	if now.IsZero() {
		now = s.now()
	}
	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()
	return s.ingest.Snapshot(s.stampIngestLocked(now))
}

// publishIngestRate persists the latest signal for readers that only have the
// index.
func (s *Scanner) publishIngestRate(ctx context.Context, at time.Time, snap ingestrate.Snapshot) error {
	if s.st == nil {
		return nil
	}
	raw, err := json.Marshal(signalOf(at, snap))
	if err != nil {
		return err
	}
	if len(raw) > maxIngestRateSignalBytes {
		return fmt.Errorf("scan: ingest rate signal is %d bytes, over the %d byte bound", len(raw), maxIngestRateSignalBytes)
	}
	return s.st.SetSetting(ctx, IngestRateSettingKey, string(raw))
}

// addOutput sums output tokens without wrapping past MaxInt64; a wrapped sum
// would reach the tracker as a negative delta.
func addOutput(total, add int64) int64 {
	if add <= 0 {
		return total
	}
	if total > math.MaxInt64-add {
		return math.MaxInt64
	}
	return total + add
}
