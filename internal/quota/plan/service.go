package plan

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults and bounds of the polling Service.
const (
	// DefaultInterval is how often a provider is polled.
	DefaultInterval = 5 * time.Minute
	// MinInterval is the shortest poll interval a caller may configure.
	MinInterval = 30 * time.Second
	// FetchTimeout bounds one provider's Detect+Fetch.
	FetchTimeout = 20 * time.Second
	// SnapshotPrefix is the settings key prefix of a cached snapshot:
	// quota.plan.<provider>.
	SnapshotPrefix = "quota.plan."
	// maxSnapshotBytes bounds a persisted snapshot.
	maxSnapshotBytes = 64 << 10
	// maxHistoryBytes bounds a persisted credit-balance history.
	maxHistoryBytes = 64 << 10
	// balanceHistoryDays is how many local days of balance readings are kept.
	balanceHistoryDays = 30
	// dailyDays is how many local days Snapshot.Daily covers.
	dailyDays = 14
	// baseBackoff is the first retry delay after a failed fetch.
	baseBackoff = time.Minute
	// maxBackoff caps exponential retry delays.
	maxBackoff = 30 * time.Minute
	// maxRetryDelay caps a provider's Retry-After.
	maxRetryDelay = time.Hour
	// defaultTick is how often the loop checks whether a provider is due.
	defaultTick = 15 * time.Second
)

// Store is the subset of the application settings store the Service needs.
type Store interface {
	Setting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Options configures a Service.
type Options struct {
	// Providers is the source list. Ids must be unique.
	Providers []Provider
	// Store persists the last good snapshot of each provider. Optional.
	Store Store
	// Offline disables every network fetch; only the stored snapshots are read.
	Offline bool
	// Interval overrides DefaultInterval for every provider.
	Interval time.Duration
	// Intervals overrides Interval for individual provider ids.
	Intervals map[string]time.Duration
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// OnChange is called with the latest snapshots after a provider reports a
	// new reading. It runs outside the Service lock and must not block.
	OnChange func([]Snapshot)
	// Tick is how often the loop wakes to check due providers. Tests shorten it.
	Tick time.Duration
}

// Status is the last known state of one provider.
type Status struct {
	Provider string
	// Detected reports whether the provider's credential was present at the
	// last attempt.
	Detected bool
	// Offline reports that the attempt was skipped because the Service is
	// offline and the provider needs the network.
	Offline bool
	// LastAttempt is when Detect+Fetch last ran (or was skipped).
	LastAttempt time.Time
	// NextAttempt is when the provider becomes due again.
	NextAttempt time.Time
	// Reason is the last failure cause, or ReasonNone after a success.
	Reason Reason
	// Attempts is the number of consecutive failures.
	Attempts int
	// FetchedAt is the FetchedAt of the cached snapshot, when one exists.
	FetchedAt time.Time
}

// Service polls providers, caches their last good snapshot and serves it.
type Service struct {
	providers []Provider
	order     []string
	store     Store
	offline   bool
	interval  time.Duration
	intervals map[string]time.Duration
	now       func() time.Time
	onChange  atomic.Pointer[func([]Snapshot)]
	tick      time.Duration

	mu      sync.Mutex
	snaps   map[string]Snapshot
	status  map[string]*Status
	backoff map[string]time.Duration
	next    map[string]time.Time
	history map[string]*BalanceHistory
	started bool
	closed  bool

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
}

// SnapshotKey returns the settings key of a provider's cached snapshot.
func SnapshotKey(id string) string { return SnapshotPrefix + id }

// HistoryKey returns the settings key of a provider's credit-balance history.
func HistoryKey(id string) string { return SnapshotPrefix + id + ".history" }

// BalancePoint is one local day's first and last reading of a credit balance.
// The first reading is what the balance was at the start of the observed day;
// the last is the latest reading of that day.
type BalancePoint struct {
	Day     time.Time `json:"day"`
	First   float64   `json:"first"`
	Last    float64   `json:"last"`
	FirstAt time.Time `json:"first_at,omitzero"`
	LastAt  time.Time `json:"last_at,omitzero"`
}

// BalanceHistory is one provider's credit-balance ledger, keyed by window
// name. It is what the Service persists under HistoryKey.
type BalanceHistory struct {
	Windows map[string][]BalancePoint `json:"windows,omitempty"`
}

// NewService validates the options, loads cached snapshots and returns a
// Service. Call Start to begin polling.
func NewService(opts Options) (*Service, error) {
	reg, err := NewRegistry(opts.Providers...)
	if err != nil {
		return nil, err
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	interval := opts.Interval
	if interval < MinInterval {
		interval = DefaultInterval
	}
	tick := opts.Tick
	if tick <= 0 {
		tick = defaultTick
	}
	if tick > interval {
		tick = interval
	}
	s := &Service{
		providers: reg.Providers(),
		store:     opts.Store,
		offline:   opts.Offline,
		interval:  interval,
		intervals: make(map[string]time.Duration, len(opts.Intervals)),
		now:       now,
		tick:      tick,
		snaps:     make(map[string]Snapshot, len(opts.Providers)),
		status:    make(map[string]*Status, len(opts.Providers)),
		backoff:   make(map[string]time.Duration, len(opts.Providers)),
		next:      make(map[string]time.Time, len(opts.Providers)),
		history:   make(map[string]*BalanceHistory, len(opts.Providers)),
		wake:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	for _, p := range s.providers {
		s.order = append(s.order, p.ID())
		s.status[p.ID()] = &Status{Provider: p.ID()}
		if v := opts.Intervals[p.ID()]; v >= MinInterval {
			s.intervals[p.ID()] = v
		}
	}
	if opts.OnChange != nil {
		s.SetOnChange(opts.OnChange)
	}
	s.load()
	return s, nil
}

// load restores the persisted snapshots, ignoring unreadable or stale-format
// entries. A missing key is not an error.
func (s *Service) load() {
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, p := range s.providers {
		s.loadHistory(ctx, p.ID())
		raw, err := s.store.Setting(ctx, SnapshotKey(p.ID()))
		if err != nil || strings.TrimSpace(raw) == "" || len(raw) > maxSnapshotBytes {
			continue
		}
		var snap Snapshot
		if err := json.Unmarshal([]byte(raw), &snap); err != nil {
			continue
		}
		if snap.Provider != p.ID() {
			continue
		}
		if err := snap.validate(s.now()); err != nil {
			continue
		}
		s.snaps[p.ID()] = snap
		s.status[p.ID()].FetchedAt = snap.FetchedAt
	}
}

// loadHistory restores a provider's balance ledger. A missing or malformed
// entry is ignored.
func (s *Service) loadHistory(ctx context.Context, id string) {
	raw, err := s.store.Setting(ctx, HistoryKey(id))
	if err != nil || strings.TrimSpace(raw) == "" || len(raw) > maxHistoryBytes {
		return
	}
	var h BalanceHistory
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		return
	}
	normalizeHistory(&h, s.now())
	s.history[id] = &h
}

// Start begins the polling loop. It is idempotent.
func (s *Service) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closed {
		return
	}
	s.started = true
	go s.loop()
}

// Wake asks the loop to check for due providers immediately.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Close stops the loop and waits for it to return. It is idempotent.
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	started := s.started
	s.mu.Unlock()
	if !started {
		return nil
	}
	close(s.stop)
	<-s.done
	return nil
}

// Refresh fetches every detected provider now, ignoring backoff. It returns
// when all attempts have finished or ctx is done.
func (s *Service) Refresh(ctx context.Context) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.pass(ctx, true)
}

// Latest returns the cached snapshots in provider order. It keeps a provider's
// last good snapshot across fetch failures and while the credential is missing,
// so the caller can keep showing it as stale; Detected reports which providers
// are currently signed in.
func (s *Service) Latest() []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Snapshot, 0, len(s.snaps))
	for _, id := range s.order {
		if snap, ok := s.snaps[id]; ok {
			out = append(out, snap.Clone())
		}
	}
	sortSnapshots(s.order, out)
	return out
}

// Detected returns the cached snapshots whose provider currently reports a
// credential, in provider order. This is the set a quota dashboard should show.
func (s *Service) Detected() []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Snapshot, 0, len(s.snaps))
	for _, id := range s.order {
		snap, ok := s.snaps[id]
		if !ok {
			continue
		}
		if st, ok := s.status[id]; !ok || !st.Detected {
			continue
		}
		out = append(out, snap.Clone())
	}
	sortSnapshots(s.order, out)
	return out
}

// Snapshot returns one provider's cached snapshot.
func (s *Service) Snapshot(id string) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.snaps[id]
	if !ok {
		return Snapshot{}, false
	}
	return snap.Clone(), true
}

// Statuses returns the last known state of every provider, in provider order.
func (s *Service) Statuses() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.order))
	for _, id := range s.order {
		if st, ok := s.status[id]; ok {
			out = append(out, *st)
		}
	}
	return out
}

func (s *Service) loop() {
	defer close(s.done)
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	s.pass(context.Background(), false)
	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
			s.pass(context.Background(), false)
		case <-ticker.C:
			s.pass(context.Background(), false)
		}
	}
}

func (s *Service) pass(ctx context.Context, force bool) {
	for _, p := range s.providers {
		if ctx.Err() != nil {
			return
		}
		s.fetchOne(ctx, p, force)
	}
}

func (s *Service) fetchOne(ctx context.Context, p Provider, force bool) {
	id := p.ID()
	now := s.now()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if !force {
		if next, ok := s.next[id]; ok && now.Before(next) {
			s.mu.Unlock()
			return
		}
	}
	offline := s.offline
	if offline {
		st := s.status[id]
		st.Detected = false
		st.Offline = true
		st.Reason = ReasonNone
		st.LastAttempt = now
		st.NextAttempt = now.Add(s.intervalFor(id))
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	s.mu.Lock()
	s.status[id].Offline = false
	s.mu.Unlock()

	detected := p.Detect(ctx)
	if !detected {
		s.mu.Lock()
		st := s.status[id]
		st.Detected = false
		st.LastAttempt = now
		st.Reason = ReasonNone
		st.Attempts = 0
		st.NextAttempt = now.Add(s.intervalFor(id))
		s.mu.Unlock()
		return
	}

	fctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	snap, err := p.Fetch(fctx)
	cancel()

	if err != nil {
		s.fail(id, now, err)
		return
	}
	snap.Provider = id
	if err := snap.validate(s.now()); err != nil {
		s.fail(id, now, err)
		return
	}
	s.succeed(id, now, snap)
}

func (s *Service) succeed(id string, attempted time.Time, snap Snapshot) {
	snap = s.recordBalance(id, snap, attempted)
	interval := s.intervalFor(id)
	s.mu.Lock()
	s.snaps[id] = snap
	st := s.status[id]
	st.Detected = true
	st.Offline = false
	st.LastAttempt = attempted
	st.NextAttempt = attempted.Add(interval)
	st.Reason = ReasonNone
	st.Attempts = 0
	st.FetchedAt = snap.FetchedAt
	delete(s.backoff, id)
	s.mu.Unlock()

	s.persist(snap)
	s.notify()
}

func (s *Service) fail(id string, attempted time.Time, err error) {
	reason := errorReason(err)
	s.mu.Lock()
	delay := s.backoff[id] * 2
	if delay <= 0 {
		delay = baseBackoff
	}
	var rd RetryDelayer
	if errors.As(err, &rd) {
		if d := rd.RetryDelay(); d > delay {
			delay = d
		}
	}
	if delay > maxBackoff {
		delay = maxBackoff
	}
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	s.backoff[id] = delay
	st := s.status[id]
	st.Detected = true
	st.LastAttempt = attempted
	st.Reason = reason
	st.Attempts++
	st.NextAttempt = attempted.Add(delay)
	s.mu.Unlock()
}

// persist writes the snapshot back to the settings store; failure to persist is
// not a fetch failure.
func (s *Service) persist(snap Snapshot) {
	raw, err := json.Marshal(snap)
	if err != nil || len(raw) > maxSnapshotBytes {
		return
	}
	s.persistRaw(SnapshotKey(snap.Provider), raw)
}

func (s *Service) persistRaw(key string, raw []byte) {
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.store.SetSetting(ctx, key, string(raw))
}

// recordBalance appends the current reading of every Balance window to the
// provider's ledger and, when the provider did not supply Snapshot.Daily,
// derives it from the first Balance window's ledger. The updated snapshot is
// persisted by the caller.
func (s *Service) recordBalance(id string, snap Snapshot, attempted time.Time) Snapshot {
	var balance []Window
	for _, w := range snap.Windows {
		if w.Balance {
			balance = append(balance, w)
		}
	}
	if len(balance) == 0 {
		return snap
	}
	at := snap.FetchedAt
	if at.IsZero() {
		at = attempted
	}

	s.mu.Lock()
	h := s.history[id]
	if h == nil {
		h = &BalanceHistory{}
		s.history[id] = h
	}
	if h.Windows == nil {
		h.Windows = make(map[string][]BalancePoint, len(balance))
	}
	for _, w := range balance {
		h.Windows[w.Name] = recordBalancePoint(h.Windows[w.Name], at, w.Remaining)
	}
	if len(snap.Daily) == 0 {
		snap.Daily = deriveDaily(h.Windows[balance[0].Name], s.now())
	}
	raw, err := json.Marshal(h)
	s.mu.Unlock()
	if err == nil && len(raw) <= maxHistoryBytes {
		s.persistRaw(HistoryKey(id), raw)
	}
	return snap
}

// recordBalancePoint folds one reading into a day's first/last point. Points
// stay sorted by Day, one per local day.
func recordBalancePoint(points []BalancePoint, at time.Time, remaining float64) []BalancePoint {
	day := DayStart(at)
	i := sort.Search(len(points), func(i int) bool { return !points[i].Day.Before(day) })
	if i < len(points) && points[i].Day.Equal(day) {
		p := &points[i]
		if p.FirstAt.IsZero() || at.Before(p.FirstAt) {
			p.First, p.FirstAt = remaining, at
		}
		if p.LastAt.IsZero() || !at.Before(p.LastAt) {
			p.Last, p.LastAt = remaining, at
		}
		return pruneBalance(points, at)
	}
	points = append(points, BalancePoint{})
	copy(points[i+1:], points[i:])
	points[i] = BalancePoint{Day: day, First: remaining, Last: remaining, FirstAt: at, LastAt: at}
	return pruneBalance(points, at)
}

// pruneBalance keeps the most recent balanceHistoryDays local days.
func pruneBalance(points []BalancePoint, at time.Time) []BalancePoint {
	cutoff := DayStart(at).AddDate(0, 0, -(balanceHistoryDays - 1))
	i := 0
	for i < len(points) && points[i].Day.Before(cutoff) {
		i++
	}
	points = points[i:]
	if len(points) > balanceHistoryDays {
		points = points[len(points)-balanceHistoryDays:]
	}
	return points
}

// deriveDaily computes the last dailyDays local days of consumption from a
// balance ledger: the drops within a day plus the drop from the previous day's
// last reading to this day's first, ignoring increases (top-ups).
func deriveDaily(points []BalancePoint, now time.Time) []DayUse {
	if len(points) == 0 {
		return nil
	}
	byDay := make(map[int64]BalancePoint, len(points))
	for _, p := range points {
		byDay[p.Day.Unix()] = p
	}
	today := DayStart(now)
	out := make([]DayUse, 0, dailyDays)
	for i := dailyDays - 1; i >= 0; i-- {
		day := today.AddDate(0, 0, -i)
		p, ok := byDay[day.Unix()]
		if !ok {
			out = append(out, DayUse{Day: day})
			continue
		}
		used := math.Max(0, p.First-p.Last)
		if prev, ok := byDay[day.AddDate(0, 0, -1).Unix()]; ok {
			used += math.Max(0, prev.Last-p.First)
		}
		out = append(out, DayUse{Day: day, Used: used, Known: true})
	}
	return out
}

// normalizeHistory sorts and de-duplicates a loaded ledger and drops entries
// outside the retention window.
func normalizeHistory(h *BalanceHistory, now time.Time) {
	for name, points := range h.Windows {
		sort.Slice(points, func(i, j int) bool { return points[i].Day.Before(points[j].Day) })
		kept := points[:0]
		for _, p := range points {
			if p.Day.IsZero() {
				continue
			}
			p.Day = DayStart(p.Day)
			if n := len(kept); n > 0 && kept[n-1].Day.Equal(p.Day) {
				kept[n-1] = p
				continue
			}
			kept = append(kept, p)
		}
		h.Windows[name] = pruneBalance(kept, now)
	}
}

// SetOnChange replaces the callback run with the latest snapshots after a
// provider reports a new one; nil removes it.
func (s *Service) SetOnChange(fn func([]Snapshot)) {
	if fn == nil {
		s.onChange.Store(nil)
		return
	}
	s.onChange.Store(&fn)
}

func (s *Service) notify() {
	cb := s.onChange.Load()
	if cb == nil {
		return
	}
	(*cb)(s.Latest())
}

func (s *Service) intervalFor(id string) time.Duration {
	if v, ok := s.intervals[id]; ok {
		return v
	}
	return s.interval
}

func errorReason(err error) Reason {
	var r Reasoner
	if errors.As(err, &r) {
		if code := strings.TrimSpace(r.QuotaReason()); code != "" {
			return Reason(code)
		}
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ReasonNetwork
}
