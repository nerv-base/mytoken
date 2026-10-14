package claudequota

import (
	"context"
	"sync"
	"time"
)

const (
	// defaultInterval is the auto-refresh interval when none is configured.
	defaultInterval = 15 * time.Minute
	// minInterval is the shortest accepted auto-refresh interval.
	minInterval = 1 * time.Minute
	// minAttemptGap is the shortest gap between two attempts on one instance,
	// whether manual or automatic.
	minAttemptGap = 30 * time.Second
	// baseBackoff is the wait after a rate limit that stated no Retry-After.
	baseBackoff = 1 * time.Minute
	// maxBackoff caps the escalating rate-limit backoff.
	maxBackoff = time.Hour
)

// Options configures a Service. Store, Sources and Transport are required for
// anything to actually happen; a nil Transport means no request can be made.
type Options struct {
	// Store is the shared settings store (satisfied by *store.Store).
	Store Settings
	// Sources are the credential sources per instance. Nothing is read from
	// them until a matching consent exists and Refresh runs.
	Sources []Source
	// Transport performs the authorized request. Tests inject a fake.
	Transport Transport
	// Offline disables the service entirely: no auto refresh and no request.
	// It is set for app.OpenLocal.
	Offline bool
	// Interval is the auto-refresh interval (default 15m, minimum 1m).
	Interval time.Duration
	// Now is the clock, injectable for tests. Defaults to time.Now.
	Now func() time.Time
	// OnChange, when set, is called after a reading is published so a UI can
	// re-read it. It is called outside all internal locks and never receives a
	// credential.
	OnChange func(instance string)
}

// Service is the opt-in Claude quota service. The zero value is unusable; use
// New.
//
// Lifecycle: Start once, Close once. Every network request belongs to either
// the auto-refresh loop or an explicit Refresh, and every request is gated on a
// valid consent check both before the credential read and again before the
// result is published.
type Service struct {
	store     Settings
	transport Transport
	offline   bool
	interval  time.Duration
	now       func() time.Time
	onChange  func(string)

	base       context.Context
	baseCancel context.CancelFunc
	wake       chan struct{}

	mu      sync.Mutex
	sources map[string]Source
	order   []string
	states  map[string]*state
	started bool
	closed  bool
	cancel  context.CancelFunc
	workers sync.WaitGroup
}

type state struct {
	attempt     uint64
	inFlight    bool
	failed      bool
	nextAllowed time.Time
	backoff     time.Duration
	cancel      context.CancelFunc
}

// New builds a service. It performs no I/O.
func New(o Options) *Service {
	interval := o.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	if interval < minInterval {
		interval = minInterval
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	base, baseCancel := context.WithCancel(context.Background())
	s := &Service{
		store:      o.Store,
		transport:  o.Transport,
		offline:    o.Offline,
		interval:   interval,
		now:        now,
		onChange:   o.OnChange,
		base:       base,
		baseCancel: baseCancel,
		wake:       make(chan struct{}, 1),
		sources:    make(map[string]Source, len(o.Sources)),
		states:     make(map[string]*state, len(o.Sources)),
	}
	for _, src := range o.Sources {
		if src.Instance == "" {
			continue
		}
		key := sourceKey(src.Instance, src.Ref)
		if _, dup := s.sources[key]; dup {
			continue
		}
		s.sources[key] = src
		s.order = append(s.order, key)
	}
	return s
}

// Offline reports whether this service is in offline mode.
func (s *Service) Offline() bool { return s.offline }

// Sources returns the configured sources in a stable order. Their Ref, Profile
// and Kind fields are non-secret and safe to show in a confirmation UI.
func (s *Service) Sources() []Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Source, 0, len(s.order))
	for _, key := range s.order {
		out = append(out, s.sources[key])
	}
	return out
}

// Start begins auto refresh. In offline mode it starts nothing and returns
// immediately; Close still works.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started || s.closed {
		s.mu.Unlock()
		return
	}
	s.started = true
	if s.offline {
		s.mu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.workers.Add(1)
	s.mu.Unlock()
	go s.loop(loopCtx)
}

// Wake asks for an immediate pass when the loop is running. It never blocks and
// never starts a pass on its own.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Close stops auto refresh, cancels in-flight requests and joins the loop. It
// is idempotent and safe on a service that was never started.
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	cancel := s.cancel
	cancels := make([]context.CancelFunc, 0, len(s.states))
	for _, st := range s.states {
		if st.cancel != nil {
			cancels = append(cancels, st.cancel)
		}
	}
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.baseCancel()
	for _, c := range cancels {
		c()
	}
	s.workers.Wait()
}

func (s *Service) loop(ctx context.Context) {
	defer s.workers.Done()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.base.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		}
		s.pass(ctx)
	}
}

// pass refreshes every instance that is enabled and due. It never touches a
// credential for an instance without a valid consent.
func (s *Service) pass(ctx context.Context) {
	for _, key := range s.sourceKeys() {
		if ctx.Err() != nil || s.base.Err() != nil {
			return
		}
		src := s.sources[key]
		consent, err := loadConsent(ctx, s.store, src.Instance)
		if err != nil || !consent.ValidFor(src.Instance, src.Kind, src.Ref) {
			continue
		}
		if s.fresh(src.Instance) {
			continue
		}
		_, _ = s.Refresh(ctx, src.Instance)
	}
}

// fresh reports whether an instance already has a reading new enough that a
// scheduled pass should skip it.
func (s *Service) fresh(instance string) bool {
	s.mu.Lock()
	st := s.states[instance]
	if st != nil && (st.inFlight || s.now().Before(st.nextAllowed)) {
		s.mu.Unlock()
		return true
	}
	s.mu.Unlock()
	if s.store == nil {
		return true
	}
	raw, err := s.store.Setting(s.base, LatestKey(instance))
	if err != nil {
		return false
	}
	q, ok := decodeQuota(raw)
	if !ok {
		return false
	}
	return q.Status == StatusOK && !q.FetchedAt.IsZero() && s.now().Sub(q.FetchedAt) < s.interval
}

// Snapshot returns the cached reading for an instance. It never reads a
// credential and never uses the network. An enabled instance with no reading
// yet yields StatusUnavailable with no error, so "not fetched yet" stays
// distinct from 0%.
func (s *Service) Snapshot(ctx context.Context, instance string) (Quota, error) {
	if s.store == nil {
		return Quota{}, errReason(ReasonStoreUnavailable)
	}
	raw, err := s.store.Setting(ctx, LatestKey(instance))
	if err != nil {
		return Quota{}, withInstance(errReason(ReasonStoreUnavailable), instance, "")
	}
	q, ok := decodeQuota(raw)
	if !ok {
		return Quota{Instance: instance, Target: Target, Status: StatusUnavailable}, nil
	}
	q.Instance = instance
	q.Status = freshness(q, s.now())
	// The stored forecast was computed when the reading was published. Refresh
	// it against the reading clock so a stale reading cannot present an
	// actionable pace: the samples did not change, only their age did.
	q.Forecast = Forecasts(q, s.now())
	return q, nil
}

func freshness(q Quota, now time.Time) Status {
	if q.FetchedAt.IsZero() {
		return StatusUnavailable
	}
	if now.Sub(q.FetchedAt) > StaleAfter {
		return StatusStale
	}
	if q.Status == StatusStale {
		return StatusStale
	}
	return StatusOK
}

// Info describes an instance's consent and last reading for a UI.
type Info struct {
	// Instance is the instance id.
	Instance string
	// Enabled is whether a usable consent exists right now.
	Enabled bool
	// Target is the request target a consent would authorize.
	Target string
	// ConsentVersion is the consent schema version.
	ConsentVersion int
	// GrantedAt is when the current consent was granted.
	GrantedAt time.Time
	// SourceKind and SourceRef are the authorized credential source, safe to
	// show without reading the credential.
	SourceKind SourceKind
	SourceRef  string
	// SourceLabel is a human label for the source, when one is configured.
	SourceLabel string
	// SourceAvailable is whether that source is still configured here.
	SourceAvailable bool
	// Quota is the cached reading.
	Quota Quota
	// Reason is the last failure reason, when any.
	Reason Reason
	// RetryAfter is how long until another attempt is allowed, when one was
	// refused or is still in flight.
	RetryAfter time.Duration
}

// ConsentInfo reports an instance's consent and cached reading. A missing
// consent is not an error: it yields Enabled=false. Errors are reserved for an
// unreadable store.
func (s *Service) ConsentInfo(ctx context.Context, instance string) (Info, error) {
	info := Info{Instance: instance, Target: Target}
	if src, ok := s.defaultSource(instance); ok {
		info.SourceKind, info.SourceRef, info.SourceLabel = src.Kind, src.Ref, src.Label
		info.SourceAvailable = src.Read != nil
	}
	if s.store == nil {
		return info, errReason(ReasonStoreUnavailable)
	}
	consent, err := loadConsent(ctx, s.store, instance)
	if err != nil {
		if reasonOf(err) == ReasonConsentInvalid {
			info.Reason = ReasonConsentInvalid
		}
		if q, qerr := s.Snapshot(ctx, instance); qerr == nil {
			info.Quota = q
		}
		return info, nil
	}
	info.Enabled = true
	info.ConsentVersion = consent.Version
	info.GrantedAt = consent.GrantedAt
	info.SourceKind = consent.SourceKind
	info.SourceRef = consent.SourceRef
	// A consent only authorizes the exact source it recorded. If that source is
	// no longer configured here, or cannot be read on this platform, the
	// consent is unusable and must not read as enabled.
	if src, ok := s.source(instance, consent.SourceRef); ok && src.Read != nil {
		info.SourceLabel = src.Label
		info.SourceAvailable = true
	} else {
		info.Enabled = false
		info.SourceAvailable = false
		info.Reason = ReasonNoSource
	}
	q, _ := s.Snapshot(ctx, instance)
	info.Quota = q
	if q.Reason != "" {
		info.Reason = q.Reason
	}
	if d, throttled := s.retryAfter(instance); throttled {
		info.RetryAfter = d
	}
	return info, nil
}

// Enable records the explicit consent for one instance and source. ref may be
// empty to select the instance's default source. It reads no credential and
// makes no request: the confirmation text a UI needs comes from ConsentInfo,
// which reports the same non-secret source and target without a secret read.
func (s *Service) Enable(ctx context.Context, instance, ref string) (Info, error) {
	if s.store == nil {
		return Info{}, errReason(ReasonStoreUnavailable)
	}
	src, ok := s.source(instance, ref)
	if !ok || src.Read == nil {
		return Info{}, withInstance(errReason(ReasonNoSource), instance, ref)
	}
	consent := Consent{
		Version:    ConsentVersion,
		Instance:   instance,
		SourceKind: src.Kind,
		SourceRef:  src.Ref,
		Target:     Target,
		GrantedAt:  s.now().UTC(),
	}
	enc, err := encodeConsent(consent)
	if err != nil {
		return Info{}, withInstance(err, instance, src.Ref)
	}
	if err := s.store.SetSetting(ctx, ConsentKey(instance), enc); err != nil {
		return Info{}, withInstance(errReason(ReasonStoreUnavailable), instance, src.Ref)
	}
	s.reset(instance)
	s.Wake()
	return s.ConsentInfo(ctx, instance)
}

// Disable revokes consent for one instance: it cancels any in-flight request
// for that instance, forbids further automatic attempts, and clears the cached
// reading so nothing is published afterwards.
func (s *Service) Disable(ctx context.Context, instance string) error {
	if s.store == nil {
		return errReason(ReasonStoreUnavailable)
	}
	// Revoke first so any concurrent publish fails its consent re-check.
	if err := s.store.SetSetting(ctx, ConsentKey(instance), ""); err != nil {
		return withInstance(errReason(ReasonStoreUnavailable), instance, "")
	}
	s.mu.Lock()
	st := s.state(instance)
	st.attempt++
	st.failed = false
	st.backoff = 0
	st.nextAllowed = time.Time{}
	cancel := st.cancel
	st.inFlight = false
	st.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if err := s.store.SetSetting(ctx, LatestKey(instance), ""); err != nil {
		return withInstance(errReason(ReasonStoreUnavailable), instance, "")
	}
	return nil
}

// Refresh performs one authorized refresh for an instance: it re-reads the
// consent, then reads the credential, then performs exactly one bounded request
// to the fixed target, then re-checks the consent before publishing.
//
// It refuses to run offline, without a valid consent, while another attempt for
// the instance is in flight, or inside the throttle window (ErrThrottled
// carries RetryAfter). It never refreshes or rewrites the OAuth token: an
// expired token yields ReasonTokenExpired and the caller must renew it with the
// Claude CLI.
func (s *Service) Refresh(ctx context.Context, instance string) (Quota, error) {
	if s.offline {
		return Quota{}, withInstance(errReason(ReasonOffline), instance, "")
	}
	if s.store == nil {
		return Quota{}, withInstance(errReason(ReasonStoreUnavailable), instance, "")
	}
	consent, err := loadConsent(ctx, s.store, instance)
	if err != nil {
		return Quota{}, withInstance(err, instance, "")
	}
	src, ok := s.source(instance, consent.SourceRef)
	if !ok || src.Read == nil {
		return Quota{}, withInstance(errReason(ReasonNoSource), instance, consent.SourceRef)
	}
	if !consent.ValidFor(instance, src.Kind, src.Ref) {
		return Quota{}, withInstance(errReason(ReasonNotEnabled), instance, src.Ref)
	}
	attempt, wait, ok := s.begin(instance)
	if !ok {
		return Quota{}, &Error{Reason: ReasonThrottled, Instance: instance, Ref: src.Ref, RetryAfter: wait}
	}
	defer s.finish(instance)

	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.base, cancel)
	defer func() {
		stop()
		cancel()
	}()
	s.mu.Lock()
	s.state(instance).cancel = cancel
	s.mu.Unlock()

	if s.base.Err() != nil {
		return s.fail(ctx, instance, attempt, src, errReason(ReasonCanceled))
	}

	cred, err := src.Read(ctx)
	if err != nil {
		if reasonOf(err) == ReasonNone {
			err = errReason(ReasonSourceUnreadable)
		}
		return s.fail(ctx, instance, attempt, src, err)
	}
	now := s.now()
	if cred.Expired(now) {
		return s.fail(ctx, instance, attempt, src, errReason(ReasonTokenExpired))
	}
	if cred.token == "" {
		return s.fail(ctx, instance, attempt, src, errReason(ReasonTokenMissing))
	}
	windows, err := fetchUsage(ctx, s.transport, cred)
	if err != nil {
		return s.fail(ctx, instance, attempt, src, err)
	}
	q := Quota{
		Instance:    instance,
		Target:      Target,
		SourceKind:  src.Kind,
		SourceRef:   src.Ref,
		FetchedAt:   now,
		AttemptedAt: now,
		Status:      StatusOK,
		Windows:     windows,
	}
	published, err := s.publish(ctx, instance, attempt, q)
	if err != nil {
		return Quota{}, err
	}
	s.succeed(instance)
	return published, nil
}

// fail records the failure status for an instance, keeping any previous windows
// so stale data stays visibly stale rather than becoming zero. It is
// best-effort: a revoked consent wins over the recorded failure.
func (s *Service) fail(ctx context.Context, instance string, attempt uint64, src Source, cause error) (Quota, error) {
	reason := reasonOf(cause)
	if reason == ReasonNone {
		reason = ReasonNetwork
	}
	prev, _ := s.Snapshot(ctx, instance)
	q := prev
	q.Instance = instance
	q.Target = Target
	q.SourceKind = src.Kind
	q.SourceRef = src.Ref
	q.AttemptedAt = s.now()
	q.Status = StatusUnavailable
	q.Reason = reason
	published, err := s.publish(ctx, instance, attempt, q)
	if err != nil {
		return Quota{}, err
	}
	s.retryLater(instance, retryDelay(cause))
	return published, withInstance(cause, instance, src.Ref)
}

// publish writes a reading, but only if the consent that authorized the attempt
// is still present and unchanged and no revocation happened meanwhile. The
// consent is re-read from the shared store both before and after the write, so
// a revocation performed by another process (the CLI) between those two reads
// discards the reading this call just stored instead of leaving it published.
//
// It also folds the reading's samples into the bounded retained set and
// recomputes the pace forecast. The prior cached reading is read only after the
// consent checks, and only a successful reading contributes a sample. The
// returned reading is the one that was stored, so a caller sees the same
// samples and forecast the cache holds.
func (s *Service) publish(ctx context.Context, instance string, attempt uint64, q Quota) (Quota, error) {
	s.mu.Lock()
	stale := s.state(instance).attempt != attempt
	s.mu.Unlock()
	if stale {
		return Quota{}, withInstance(errReason(ReasonDisabled), instance, q.SourceRef)
	}
	consent, err := loadConsent(ctx, s.store, instance)
	if err != nil {
		return Quota{}, withInstance(err, instance, q.SourceRef)
	}
	if !consent.ValidFor(instance, q.SourceKind, q.SourceRef) {
		return Quota{}, withInstance(errReason(ReasonDisabled), instance, q.SourceRef)
	}
	s.mu.Lock()
	stale = s.state(instance).attempt != attempt
	s.mu.Unlock()
	if stale {
		return Quota{}, withInstance(errReason(ReasonDisabled), instance, q.SourceRef)
	}
	// The cached reading is read only here, after both consent checks have
	// passed: retention compares this attempt's samples against what was
	// already published, and must not read quota state the attempt is not
	// authorized to write.
	var prior Quota
	if raw, rerr := s.store.Setting(ctx, LatestKey(instance)); rerr == nil {
		if p, pok := decodeQuota(raw); pok {
			prior = p
		}
	}
	q = withForecast(prior, q, s.now())
	enc, err := encodeQuota(q)
	if err != nil {
		return Quota{}, withInstance(err, instance, q.SourceRef)
	}
	if err := s.store.SetSetting(ctx, LatestKey(instance), enc); err != nil {
		return Quota{}, withInstance(errReason(ReasonStoreUnavailable), instance, q.SourceRef)
	}
	// A revocation that landed from another process after the re-check above
	// would otherwise leave a reading stored under a consent that is already
	// gone. Re-read the shared consent and discard the reading when it no
	// longer authorizes it.
	if after, aerr := loadConsent(ctx, s.store, instance); aerr != nil ||
		!after.ValidFor(instance, q.SourceKind, q.SourceRef) {
		if cerr := s.store.SetSetting(ctx, LatestKey(instance), ""); cerr != nil {
			return Quota{}, withInstance(errReason(ReasonStoreUnavailable), instance, q.SourceRef)
		}
		return Quota{}, withInstance(errReason(ReasonDisabled), instance, q.SourceRef)
	}
	if s.onChange != nil {
		s.onChange(instance)
	}
	return q, nil
}

// SourceFor reports the source an instance would use, without reading it.
func (s *Service) SourceFor(instance string) (Source, bool) { return s.defaultSource(instance) }

// SourceRefs lists the non-secret source references configured for an
// instance, for a confirmation UI.
func (s *Service) SourceRefs(instance string) []Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Source
	for _, key := range s.order {
		src := s.sources[key]
		if src.Instance == instance {
			out = append(out, src)
		}
	}
	return out
}

func sourceKey(instance, ref string) string { return instance + "\x00" + ref }

func (s *Service) sourceKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

func (s *Service) defaultSource(instance string) (Source, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range s.order {
		if src := s.sources[key]; src.Instance == instance {
			return src, true
		}
	}
	return Source{}, false
}

func (s *Service) source(instance, ref string) (Source, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref != "" {
		if src, ok := s.sources[sourceKey(instance, ref)]; ok {
			return src, true
		}
		return Source{}, false
	}
	for _, key := range s.order {
		if src := s.sources[key]; src.Instance == instance {
			return src, true
		}
	}
	return Source{}, false
}

// state returns the mutable throttle state for an instance. Callers must hold
// s.mu.
func (s *Service) state(instance string) *state {
	st := s.states[instance]
	if st == nil {
		st = &state{}
		s.states[instance] = st
	}
	return st
}

func (s *Service) retryAfter(instance string) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[instance]
	if st == nil || !st.failed {
		return 0, false
	}
	if st.inFlight {
		return 0, true
	}
	if d := st.nextAllowed.Sub(s.now()); d > 0 {
		return d, true
	}
	return 0, false
}

// begin reserves the single attempt slot for an instance.
func (s *Service) begin(instance string) (attempt uint64, wait time.Duration, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(instance)
	if st.inFlight {
		return 0, 0, false
	}
	if d := st.nextAllowed.Sub(s.now()); d > 0 {
		return 0, d, false
	}
	st.inFlight = true
	st.nextAllowed = s.now().Add(minAttemptGap)
	return st.attempt, 0, true
}

func (s *Service) finish(instance string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(instance)
	st.inFlight = false
	st.cancel = nil
}

func (s *Service) succeed(instance string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(instance)
	st.failed = false
	st.backoff = 0
	st.nextAllowed = s.now().Add(minAttemptGap)
}

// retryLater applies backoff, escalating across consecutive failures.
func (s *Service) retryLater(instance string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(instance)
	st.failed = true
	if d <= 0 {
		if st.backoff == 0 {
			st.backoff = baseBackoff
		} else {
			st.backoff *= 2
		}
		if st.backoff > maxBackoff {
			st.backoff = maxBackoff
		}
		d = st.backoff
	} else if d > maxBackoff {
		d = maxBackoff
	}
	st.nextAllowed = s.now().Add(d)
}

// reset clears throttle state after an explicit enable.
func (s *Service) reset(instance string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(instance)
	st.failed = false
	st.backoff = 0
	st.nextAllowed = time.Time{}
}

func retryDelay(cause error) time.Duration {
	var e *Error
	if asError(cause, &e) && e.Reason == ReasonRateLimited {
		return e.RetryAfter
	}
	return 0
}

func reasonOf(err error) Reason {
	var e *Error
	if asError(err, &e) {
		return e.Reason
	}
	return ReasonNone
}
