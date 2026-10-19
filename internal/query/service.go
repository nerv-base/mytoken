package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
	"sort"
	"strings"
	"sync"
	"time"
)

type service struct{ st *store.Store }

func NewService(st *store.Store) Service { return &service{st: st} }

var _ Service = (*service)(nil)

// The SQL-backed service is the instance-aware implementation.
var _ InstanceService = (*service)(nil)

// sessionKey identifies a session inside one data source instance. The instance
// is part of the identity: the same harness and session id in two instances are
// two independent sessions, and dropping the instance here would merge them.
type sessionKey struct {
	h        model.Harness
	instance string
	id       string
}
type data struct {
	events          []AttributedEvent
	meta            map[sessionKey]model.SessionMeta
	providerAliases map[string]string
}

type loadMode struct {
	hourly    bool
	identity  bool
	sessionID string
	// instance scopes a session detail read to one explicit data source when
	// scoped is set. The legacy Session call leaves scoped false so it can
	// detect an id that exists in several instances.
	instance string
	scoped   bool
	// branch reads the per-request branch metadata alongside the facts. Branch
	// is metadata, not a rollup dimension, so this mode always scans facts.
	branch bool
}

func (s *service) load(ctx context.Context, f Filter, mode ...loadMode) (data, error) {
	d := data{meta: map[sessionKey]model.SessionMeta{}}
	tx, e := s.st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return d, e
	}
	defer tx.Rollback()
	d, e = s.loadTx(ctx, tx, f, mode...)
	if e != nil {
		return d, e
	}
	return d, tx.Commit()
}

// loadTx does the read work inside a caller-owned transaction. ReadEnvelope
// uses it so the totals, cache and freshness fields of one envelope come from a
// single consistent read snapshot instead of three separate transactions.
func (s *service) loadTx(ctx context.Context, tx *sql.Tx, f Filter, mode ...loadMode) (data, error) {
	d := data{meta: map[sessionKey]model.SessionMeta{}}
	rows, e := tx.QueryContext(ctx, "SELECT harness,instance_id,session_id,parent_id,title,project,started_at,updated_at FROM sessions")
	if e != nil {
		return d, e
	}
	for rows.Next() {
		var m model.SessionMeta
		var instance string
		var start, end int64
		if e = rows.Scan(&m.Harness, &instance, &m.SessionID, &m.ParentID, &m.Title, &m.Project, &start, &end); e != nil {
			rows.Close()
			return d, e
		}
		if start != 0 {
			m.StartedAt = time.Unix(0, start).UTC()
		}
		if end != 0 {
			m.UpdatedAt = time.Unix(0, end).UTC()
		}
		d.meta[sessionKey{m.Harness, store.NormalizeInstanceID(instance), m.SessionID}] = m
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return d, e
	}
	var raw string
	var aliases []ModelAlias
	e = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='model-aliases'").Scan(&raw)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return d, e
	}
	if raw != "" {
		if e = json.Unmarshal([]byte(raw), &aliases); e != nil {
			return d, e
		}
	}
	if len(mode) > 0 && mode[0].identity {
		d.providerAliases = map[string]string{}
		rows, e = tx.QueryContext(ctx, "SELECT key,value FROM settings WHERE key LIKE 'provider.alias.%'")
		if e != nil {
			return d, e
		}
		for rows.Next() {
			var key, value string
			if e = rows.Scan(&key, &value); e != nil {
				rows.Close()
				return d, e
			}
			d.providerAliases[strings.TrimPrefix(key, "provider.alias.")] = value
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return d, e
		}
	}
	aliasLookup := aliasMap(aliases)
	models := f.Models
	f.Models = nil
	// Endpoint identity is hashed in Go, so an ID filter cannot be pushed into
	// SQL. Select base_url and filter here; this also forces the facts-based
	// aggregate because the legacy daily rollup has no endpoint column.
	selector := newProviderIDSelector(f.ProviderIDs)
	branchDim := len(mode) > 0 && mode[0].branch
	withEndpoint := selector != nil || (len(mode) > 0 && mode[0].identity) || branchDim
	var loaded []AttributedEvent
	if len(mode) == 0 || mode[0].hourly || mode[0].identity || branchDim {
		var err error
		loaded, err = scanUsageMode(ctx, tx, f, len(mode) > 0 && mode[0].hourly, withEndpoint, branchDim)
		if err != nil {
			return d, err
		}
	} else {
		root := mode[0].sessionID
		// The legacy instance-less call resolves the instance from the session
		// rows. When the same id exists in more than one instance there is no
		// correct answer, so it reports ErrAmbiguousInstance instead of reading
		// another data source's requests into this session.
		scope := mode[0].instance
		if !mode[0].scoped {
			scope = ""
			for k := range d.meta {
				if k.h != f.Harnesses[0] || k.id != root {
					continue
				}
				if scope == "" {
					scope = k.instance
				} else if scope != k.instance {
					return d, ErrAmbiguousInstance
				}
			}
		}
		// Limit detail I/O to this session and descendants (including grandchildren).
		ids := []string{root}
		seen := map[string]bool{root: true}
		for i := 0; i < len(ids); i++ {
			for k, m := range d.meta {
				if m.ParentID == ids[i] && k.h == f.Harnesses[0] && (scope == "" || k.instance == scope) && !seen[k.id] {
					ids = append(ids, k.id)
					seen[k.id] = true
				}
			}
		}
		// Drive the fact lookup through dimension IDs. A view predicate on
		// harness alone otherwise tempts SQLite into scanning its dedup index.
		where := " WHERE rowid IN (SELECT rowid FROM event_data WHERE dimension_id IN (SELECT id FROM event_dimensions WHERE harness=?"
		args := []any{f.Harnesses[0]}
		if scope != "" {
			where += " AND instance_id=?"
			args = append(args, scope)
		}
		where += " AND session_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")))"
		for _, id := range ids {
			args = append(args, id)
		}
		rows, e = tx.QueryContext(ctx, `SELECT harness,instance_id,dedup_key,session_id,parent_id,project,timestamp,model,provider,base_url,input,output,cache_read,cache_write,cache_write_1h,reasoning,log_cost,resolved_provider,attrib,cost,priced,request_id,boundary,bill_amount,bill_unit FROM events`+where+" ORDER BY timestamp,harness,dedup_key", args...)
		if e != nil {
			return d, e
		}
		loadedInstances := map[string]bool{}
		for rows.Next() {
			var v AttributedEvent
			var instance string
			var at int64
			var cost, billAmount sql.NullFloat64
			var billUnit string
			if e = rows.Scan(&v.Harness, &instance, &v.DedupKey, &v.SessionID, &v.ParentID, &v.ProjectPath, &at, &v.Model, &v.Provider, &v.BaseURL, &v.Tokens.Input, &v.Tokens.Output, &v.Tokens.CacheRead, &v.Tokens.CacheWrite, &v.Tokens.CacheWrite1h, &v.Tokens.Reasoning, &cost, &v.ResolvedProvider, &v.Attrib, &v.Cost, &v.Priced, &v.RequestID, &v.Boundary, &billAmount, &billUnit); e != nil {
				rows.Close()
				return d, e
			}
			v.InstanceID = store.NormalizeInstanceID(instance)
			v.Timestamp = time.Unix(0, at).UTC()
			if cost.Valid {
				x := cost.Float64
				v.CostUSD = &x
			}
			if billAmount.Valid {
				v.Bill = &model.Bill{Amount: billAmount.Float64, Unit: billUnit}
			}
			eventSource(&v)
			loadedInstances[v.InstanceID] = true
			loaded = append(loaded, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return d, e
		}
		// Without a known instance the facts themselves must agree on one.
		if scope == "" && len(loadedInstances) > 1 {
			return d, ErrAmbiguousInstance
		}
	}
	for _, v := range loaded {
		if target, ok := aliasLookup[[2]string{v.ResolvedProvider, v.Model}]; ok {
			v.Model = target
		} else if target, ok := aliasLookup[[2]string{"", v.Model}]; ok {
			v.Model = target
		}
		if len(models) > 0 {
			matches := false
			for _, name := range models {
				if name == v.Model {
					matches = true
					break
				}
			}
			if !matches {
				continue
			}
		}
		// ProviderIDs is exact and orthogonal to Providers: an ID that no
		// event carries removes every event rather than disabling the filter.
		if !selector.match(v) {
			continue
		}
		d.events = append(d.events, v)
	}
	return d, nil
}
func filterSQL(f Filter) (string, []any) {
	var clauses []string
	var args []any
	if !f.Range.From.IsZero() {
		clauses = append(clauses, "timestamp>=?")
		args = append(args, f.Range.From.UnixNano())
	}
	if !f.Range.To.IsZero() {
		clauses = append(clauses, "timestamp<?")
		args = append(args, f.Range.To.UnixNano())
	}
	add := func(col string, values []string) {
		if len(values) == 0 {
			return
		}
		clauses = append(clauses, col+" IN ("+strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")+")")
		for _, v := range values {
			args = append(args, v)
		}
	}
	hs := make([]string, len(f.Harnesses))
	for i, h := range f.Harnesses {
		hs[i] = string(h)
	}
	add("harness", hs)
	add("instance_id", f.InstanceIDs)
	add("resolved_provider", f.Providers)
	add("model", f.Models)
	if f.Project != "" {
		clauses = append(clauses, "project=?")
		args = append(args, f.Project)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}
func (d data) root(k sessionKey) sessionKey {
	seen := map[sessionKey]bool{}
	for !seen[k] {
		seen[k] = true
		m, ok := d.meta[k]
		if !ok || m.ParentID == "" {
			return k
		}
		k = sessionKey{k.h, k.instance, m.ParentID}
	}
	return k
}
func (s *service) Totals(ctx context.Context, f Filter) (Totals, error) {
	d, e := s.load(ctx, f)
	if e != nil {
		return Totals{}, e
	}
	return totalsOf(d), nil
}

// totalsOf folds loaded events into the aggregate. It is shared with
// ReadEnvelope so a snapshot's totals and a `stats` run cannot drift apart.
func totalsOf(d data) Totals {
	var out Totals
	sessions := map[sessionKey]bool{}
	for _, v := range d.events {
		out.Tokens = out.Tokens.Add(v.Tokens)
		out.CostUSD += v.Cost
		out.Unpriced += v.unpriced
		out.Requests += v.requests
		sessions[d.root(sessionKey{v.Harness, v.InstanceID, v.SessionID})] = true
	}
	out.Sessions = int64(len(sessions))
	denom := out.Tokens.Input + out.Tokens.CacheRead + out.Tokens.CacheWrite
	if denom > 0 {
		out.CacheHit = float64(out.Tokens.CacheRead) / float64(denom)
	}
	return out
}
func (s *service) Daily(ctx context.Context, f Filter) ([]Point, error) {
	return s.series(ctx, f, false)
}
func (s *service) Hourly(ctx context.Context, f Filter) ([]Point, error) {
	return s.series(ctx, f, true)
}
func floor(t time.Time, hourly bool) time.Time {
	t = t.In(time.Local)
	if hourly {
		return t.Add(-time.Duration(t.Minute())*time.Minute - time.Duration(t.Second())*time.Second - time.Duration(t.Nanosecond()))
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}
func step(t time.Time, hourly bool) time.Time {
	if hourly {
		return t.Add(time.Hour)
	}
	return t.AddDate(0, 0, 1)
}
func (s *service) series(ctx context.Context, f Filter, hourly bool) ([]Point, error) {
	var mode []loadMode
	if hourly {
		mode = []loadMode{{hourly: true}}
	}
	d, e := s.load(ctx, f, mode...)
	if e != nil {
		return nil, e
	}
	sort.Slice(d.events, func(i, j int) bool { return d.events[i].Timestamp.Before(d.events[j].Timestamp) })
	from, to := f.Range.From, f.Range.To
	if from.IsZero() {
		if len(d.events) == 0 {
			return []Point{}, nil
		}
		from = d.events[0].Timestamp
	}
	if to.IsZero() {
		if len(d.events) == 0 {
			return []Point{}, nil
		}
		to = step(floor(d.events[len(d.events)-1].Timestamp, hourly), hourly)
	}
	out := []Point{}
	indices := map[int64]int{}
	for at := floor(from, hourly); at.Before(to); at = step(at, hourly) {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		indices[at.UnixNano()] = len(out)
		out = append(out, Point{Day: at})
	}
	for _, v := range d.events {
		if i, ok := indices[floor(v.Timestamp, hourly).UnixNano()]; ok {
			out[i].Tokens = out[i].Tokens.Add(v.Tokens)
			out[i].CostUSD += v.Cost
		}
	}
	return out, nil
}
func (s *service) ByProvider(ctx context.Context, f Filter) ([]Bucket, error) {
	return s.buckets(ctx, f, "provider")
}
func (s *service) ByModel(ctx context.Context, f Filter) ([]Bucket, error) {
	return s.buckets(ctx, f, "model")
}
func (s *service) ByProject(ctx context.Context, f Filter) ([]Bucket, error) {
	return s.buckets(ctx, f, "project")
}
func (s *service) ByHarness(ctx context.Context, f Filter) ([]Bucket, error) {
	return s.buckets(ctx, f, "harness")
}
func (s *service) buckets(ctx context.Context, f Filter, kind string) ([]Bucket, error) {
	d, e := s.load(ctx, f)
	if e != nil {
		return nil, e
	}
	return bucketsOf(d, kind), nil
}

// bucketsOf ranks one dimension of an already-loaded data set. It is shared by
// Service.buckets and Envelope grouping so a snapshot can rank the same
// dimension inside the transaction that produced its totals.
func bucketsOf(d data, kind string) []Bucket {
	groups := map[string]*Bucket{}
	seen := map[string]map[sessionKey]bool{}
	for _, v := range d.events {
		var key, label string
		labelFallback := true
		switch kind {
		case "provider":
			key = v.ResolvedProvider
		case "model":
			key = v.Model
		case "project":
			key = v.ProjectPath
		case "harness":
			key = string(v.Harness)
			label = v.Harness.DisplayName()
		case "branch":
			// A branch name alone is not a group: two repositories can both sit
			// on "main". The key is the pair, so the ranking separates them, and
			// the label stays the bare branch. An empty branch means the log
			// stated none and is kept as its own explicit bucket rather than
			// being folded into another one, so no request is dropped.
			key = BranchKey(v.ProjectPath, v.GitBranch)
			label = v.GitBranch
			labelFallback = false
		}
		if labelFallback && label == "" {
			label = key
		}
		b := groups[key]
		if b == nil {
			b = &Bucket{Key: key, Label: label}
			groups[key] = b
			seen[key] = map[sessionKey]bool{}
		}
		b.Tokens = b.Tokens.Add(v.Tokens)
		b.CostUSD += v.Cost
		b.Unpriced += v.unpriced
		b.Requests += v.requests
		seen[key][d.root(sessionKey{v.Harness, v.InstanceID, v.SessionID})] = true
	}
	out := make([]Bucket, 0, len(groups))
	for k, b := range groups {
		b.Sessions = int64(len(seen[k]))
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tokens.Total() == out[j].Tokens.Total() {
			return out[i].Key < out[j].Key
		}
		return out[i].Tokens.Total() > out[j].Tokens.Total()
	})
	return out
}
func (d data) rows() map[sessionKey]*SessionRow {
	rows := map[sessionKey]*SessionRow{}
	breakdowns := map[sessionKey]map[[2]string]*ProviderModel{}
	for k, m := range d.meta {
		rows[k] = &SessionRow{SessionMeta: m, InstanceID: k.instance, Breakdown: []ProviderModel{}}
		breakdowns[k] = map[[2]string]*ProviderModel{}
	}
	for k, m := range d.meta {
		if m.ParentID != "" {
			if p := rows[sessionKey{k.h, k.instance, m.ParentID}]; p != nil {
				p.Children++
			}
		}
	}
	for _, e := range d.events {
		k := sessionKey{e.Harness, e.InstanceID, e.SessionID}
		seen := map[sessionKey]bool{}
		for !seen[k] {
			seen[k] = true
			r := rows[k]
			if r == nil {
				break
			}
			r.Tokens = r.Tokens.Add(e.Tokens)
			r.CostUSD += e.Cost
			r.Unpriced += e.unpriced
			r.Requests += e.requests
			if e.DedupKey != "" && e.Timestamp.After(r.UpdatedAt) {
				r.UpdatedAt = e.Timestamp
			}
			if e.DedupKey != "" && (r.StartedAt.IsZero() || e.Timestamp.Before(r.StartedAt)) {
				r.StartedAt = e.Timestamp
			}
			pk := [2]string{e.ResolvedProvider, e.Model}
			b := breakdowns[k][pk]
			if b == nil {
				b = &ProviderModel{Provider: pk[0], Model: pk[1], Attrib: e.Attrib}
				breakdowns[k][pk] = b
			}
			b.Tokens = b.Tokens.Add(e.Tokens)
			b.CostUSD += e.Cost
			b.Unpriced += e.unpriced
			b.Requests += e.requests
			if r.ParentID == "" {
				break
			}
			k = sessionKey{k.h, k.instance, r.ParentID}
		}
	}
	for k, r := range rows {
		for _, b := range breakdowns[k] {
			r.Breakdown = append(r.Breakdown, *b)
		}
		sort.Slice(r.Breakdown, func(i, j int) bool {
			a, b := r.Breakdown[i], r.Breakdown[j]
			if a.Tokens.Total() != b.Tokens.Total() {
				return a.Tokens.Total() > b.Tokens.Total()
			}
			if a.Provider != b.Provider {
				return a.Provider < b.Provider
			}
			return a.Model < b.Model
		})
	}
	return rows
}
func (s *service) Sessions(ctx context.Context, f Filter, order string, limit, offset int) ([]SessionRow, int, error) {
	d, e := s.load(ctx, f)
	if e != nil {
		return nil, 0, e
	}
	rows, total := sessionsOf(d, order, limit, offset)
	return rows, total, nil
}

// sessionsOf ranks and paginates the root sessions of an already-loaded data
// set. It is shared by Service.Sessions and Envelope grouping.
func sessionsOf(d data, order string, limit, offset int) ([]SessionRow, int) {
	out := []SessionRow{}
	for _, r := range d.rows() {
		if r.ParentID == "" && r.Requests > 0 {
			out = append(out, *r)
		}
	}
	sortRows(out, order)
	total := len(out)
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return []SessionRow{}, total
	}
	out = out[offset:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, total
}
func sortRows(rows []SessionRow, order string) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch order {
		case SortTokens:
			if a.Tokens.Total() != b.Tokens.Total() {
				return a.Tokens.Total() > b.Tokens.Total()
			}
		case SortCost:
			if a.CostUSD != b.CostUSD {
				return a.CostUSD > b.CostUSD
			}
		default:
			if !a.UpdatedAt.Equal(b.UpdatedAt) {
				return a.UpdatedAt.After(b.UpdatedAt)
			}
		}
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		return a.SessionID < b.SessionID
	})
}
func (s *service) Session(ctx context.Context, h model.Harness, id string) (SessionRow, []SessionRow, []AttributedEvent, error) {
	d, e := s.load(ctx, Filter{Harnesses: []model.Harness{h}}, loadMode{sessionID: id})
	if e != nil {
		return SessionRow{}, nil, nil, e
	}
	instance, e := sessionInstance(d, h, id)
	if e != nil {
		return SessionRow{}, nil, nil, e
	}
	return s.sessionIn(d, h, instance, id)
}

// SessionIn is Session for one explicit data source instance.
func (s *service) SessionIn(ctx context.Context, h model.Harness, instance, id string) (SessionRow, []SessionRow, []AttributedEvent, error) {
	instance = store.NormalizeInstanceID(instance)
	d, e := s.load(ctx, Filter{Harnesses: []model.Harness{h}, InstanceIDs: []string{instance}}, loadMode{sessionID: id, instance: instance, scoped: true})
	if e != nil {
		return SessionRow{}, nil, nil, e
	}
	return s.sessionIn(d, h, instance, id)
}

// sessionInstance resolves the one instance a legacy, instance-less lookup
// refers to. Zero instances is reported as the empty string (the caller then
// returns sql.ErrNoRows, as before); more than one is ErrAmbiguousInstance,
// never an arbitrary first match.
func sessionInstance(d data, h model.Harness, id string) (string, error) {
	found := ""
	for k := range d.meta {
		if k.h != h || k.id != id {
			continue
		}
		if found == "" {
			found = k.instance
		} else if found != k.instance {
			return "", ErrAmbiguousInstance
		}
	}
	for _, v := range d.events {
		if v.SessionID != id {
			continue
		}
		if found == "" {
			found = v.InstanceID
		} else if found != v.InstanceID {
			return "", ErrAmbiguousInstance
		}
	}
	return found, nil
}

func (s *service) sessionIn(d data, h model.Harness, instance, id string) (SessionRow, []SessionRow, []AttributedEvent, error) {
	rows := d.rows()
	r := rows[sessionKey{h, instance, id}]
	if r == nil {
		return SessionRow{}, nil, nil, sql.ErrNoRows
	}
	children := []SessionRow{}
	for _, v := range rows {
		if v.Harness == h && v.InstanceID == instance && v.ParentID == id {
			children = append(children, *v)
		}
	}
	sortRows(children, SortRecent)
	events := []AttributedEvent{}
	for _, v := range d.events {
		if v.SessionID == id && v.InstanceID == instance {
			events = append(events, v)
		}
	}
	return *r, children, events, nil
}
func (s *service) Subscribe() (<-chan struct{}, func()) {
	src, unsubscribe := s.st.Subscribe()
	out := make(chan struct{}, 1)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(out)
		var last time.Time
		var timer *time.Timer
		var tick <-chan time.Time
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()
		send := func() {
			select {
			case out <- struct{}{}:
			default:
			}
			last = time.Now()
		}
		for {
			select {
			case <-done:
				return
			case _, ok := <-src:
				if !ok {
					return
				}
				remaining := time.Second - time.Since(last)
				if remaining <= 0 {
					send()
				} else if tick == nil {
					timer = time.NewTimer(remaining)
					tick = timer.C
				}
			case <-tick:
				tick = nil
				send()
			}
		}
	}()
	return out, func() { once.Do(func() { close(done); unsubscribe() }) }
}

var _ = errors.Is
