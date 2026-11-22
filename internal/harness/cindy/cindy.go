// Package cindy reads Cindy's local SQLite database.
//
// Cindy is a desktop agent host that runs Claude Code, Codex and Pi under one
// roof. It keeps one SQLite database per local profile in its Electron userData
// directory, named cindy-<owner>.db. Token usage is written per assistant
// message in messages.agent_meta as a turnUsageDetails object; the database also
// keeps daily_model_usage, an aggregate that survives message pruning. The
// reader emits the message-level events first and then the difference between
// the daily aggregate and those events, so history that Cindy has already
// slimmed away still counts.
//
// The database is opened read-only and never modified. It is a snapshot parser:
// Cindy fills agent_meta after inserting a message row, and messages has no
// updated_at column, so the reader re-reads the whole database whenever its
// database or WAL signature changes.
package cindy

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nerv-base/mytoken/internal/harness"
	"github.com/nerv-base/mytoken/internal/model"
	"github.com/nerv-base/mytoken/internal/sqlitedsn"
	_ "modernc.org/sqlite"
)

const (
	fingerprintBytes = 4096
	busyTimeoutMS    = 5000
	dailySessionID   = "cindy-daily"
	dailySessionName = "Cindy daily totals"
)

var slashCommandRE = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9_-]*(\s|$)`)

// Parser reads every Cindy profile database under its roots.
type Parser struct {
	roots []string
	prov  func() ([]harness.Root, error)
}

// New returns a parser rooted at Cindy's platform user-data directory.
func New() *Parser { return &Parser{prov: rootProvenance} }

// NewWithRoots returns a parser rooted at explicit directories or database
// files. An empty argument list uses the default roots, like the other readers.
func NewWithRoots(roots ...string) *Parser {
	if len(roots) == 0 {
		return New()
	}
	return &Parser{roots: append([]string(nil), roots...)}
}

func init() { harness.Register(New()) }

func rootProvenance() ([]harness.Root, error) {
	if value, ok := harness.Getenv("MYTOKEN_CINDY_DIRS"); ok {
		roots, err := harness.SplitEnvList("MYTOKEN_CINDY_DIRS", value)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots("MYTOKEN_CINDY_DIRS", roots...), nil
	}
	if value, ok := harness.Getenv("CINDY_HOST_DATA_DIR"); ok {
		return harness.EnvRoots("CINDY_HOST_DATA_DIR", filepath.Clean(value)), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return nil, nil
	}
	return harness.BuiltinRoots(filepath.Join(dir, "Cindy")), nil
}

// DefaultRoots returns the platform user-data directory unless an environment
// override names explicit roots.
func (p *Parser) DefaultRoots() []string {
	roots, err := p.RootProvenance()
	if err != nil {
		return nil
	}
	return harness.RootPaths(roots)
}

// RootProvenance implements harness.Provenance.
func (p *Parser) RootProvenance() ([]harness.Root, error) {
	if len(p.roots) > 0 {
		return harness.ExplicitRoots(p.roots...), nil
	}
	if p.prov != nil {
		return p.prov()
	}
	return harness.ExplicitRoots(), nil
}

// Harness implements harness.Parser.
func (p *Parser) Harness() model.Harness { return model.Cindy }

// WithRoots implements harness.Rooted.
func (p *Parser) WithRoots(roots ...string) harness.Parser {
	return &Parser{roots: append([]string(nil), roots...)}
}

// Roots implements harness.Parser.
func (p *Parser) Roots() []string {
	roots, err := p.RootProvenance()
	if err != nil {
		return nil
	}
	return harness.RootPaths(roots)
}

// Discover finds cindy-*.db files directly under each root. A root may itself
// be a database file, which makes MYTOKEN_CINDY_DIRS usable for one archive.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	_ = ctx
	seen := map[string]bool{}
	out := make([]harness.Source, 0)
	add := func(path string) {
		if !isCindyDB(filepath.Base(path)) {
			return
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		if seen[abs] {
			return
		}
		seen[abs] = true
		out = append(out, harness.Source{Path: abs, Kind: "sqlite"})
	}
	for _, root := range p.Roots() {
		if root == "" {
			continue
		}
		info, err := os.Stat(root)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() {
			add(root)
			continue
		}
		if !info.IsDir() {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(root, "cindy-*.db"))
		for _, match := range matches {
			add(match)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Parse reads one Cindy database. Anything but a sqlite source is a no-op.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if src.Kind != "sqlite" {
		return harness.Batch{Next: cur}, nil
	}
	return parseDB(ctx, src.Path, cur)
}

func isCindyDB(name string) bool {
	return strings.HasPrefix(name, "cindy-") && strings.HasSuffix(name, ".db")
}

type dbSignature struct {
	size int64
	mod  time.Time
}

// signatureOf combines the database with its -wal companion: Cindy can commit
// usage into the WAL before any checkpoint touches the main file. -shm is
// deliberately excluded because every reader writes its read mark there.
func signatureOf(path string) (dbSignature, error) {
	info, err := os.Stat(path)
	if err != nil {
		return dbSignature{}, err
	}
	sig := dbSignature{size: info.Size(), mod: info.ModTime()}
	for _, suffix := range []string{"-wal"} {
		wal, err := os.Stat(path + suffix)
		if err != nil {
			continue
		}
		sig.size += wal.Size()
		if wal.ModTime().After(sig.mod) {
			sig.mod = wal.ModTime()
		}
	}
	return sig, nil
}

func fingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.CopyN(h, f, fingerprintBytes); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	q := url.Values{
		"mode":      {"ro"},
		"immutable": {"0"},
		"_pragma":   {fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS)},
	}
	db, err := sql.Open("sqlite", sqlitedsn.URI(abs, q.Encode()))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func parseDB(ctx context.Context, path string, cur harness.Cursor) (harness.Batch, error) {
	sig, err := signatureOf(path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	fp, err := fingerprint(path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	if cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == sig.size && cur.ModTime.Equal(sig.mod) {
		return harness.Batch{Next: cur}, nil
	}
	db, err := openReadOnly(path)
	if err != nil {
		// Locked, unreadable or not a database: keep the cursor and retry.
		return harness.Batch{Next: cur}, nil
	}
	defer db.Close()

	tables := tableColumns(ctx, db)
	sessions, err := loadSessions(ctx, db, tables)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	messages, err := loadMessages(ctx, db, tables)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	daily, err := loadDailyUsage(ctx, db, tables)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}

	events, metas := buildBatch(sessions, messages, daily)
	return harness.Batch{
		Events:   events,
		Sessions: metas,
		Next: harness.Cursor{
			Offset:      0,
			Size:        sig.size,
			ModTime:     sig.mod,
			Fingerprint: fp,
		},
	}, nil
}

type sessionRow struct {
	ID           string
	ParentID     string
	Title        string
	Project      string
	Model        string
	Provider     string
	Created      float64
	Updated      float64
	LastActivity float64
}

type messageRow struct {
	ID        string
	SessionID string
	Role      string
	Content   string
	AgentMeta string
	Created   float64
}

type dailyRow struct {
	Day         string
	AgentKind   string
	Model       string
	Input       float64
	Output      float64
	CacheRead   float64
	CacheCreate float64
	Updated     float64
}

type messageMeta struct {
	TurnUsageDetails *usageDetails `json:"turnUsageDetails"`
}

type usageDetails struct {
	ProviderID        string            `json:"providerId"`
	ProviderName      string            `json:"providerName"`
	BillingSource     string            `json:"billingSource"`
	InputTokens       float64           `json:"inputTokens"`
	OutputTokens      float64           `json:"outputTokens"`
	CacheReadTokens   float64           `json:"cacheReadTokens"`
	CacheCreateTokens float64           `json:"cacheCreateTokens"`
	TotalTokens       float64           `json:"totalTokens"`
	Model             string            `json:"model"`
	Models            []json.RawMessage `json:"models"`
}

func loadSessions(ctx context.Context, db *sql.DB, tables map[string]map[string]bool) ([]sessionRow, error) {
	cols := tables["sessions"]
	if len(cols) == 0 {
		return nil, nil
	}
	query := fmt.Sprintf(
		`SELECT %s, %s, %s, %s, %s, %s, %s, %s, %s, %s FROM sessions ORDER BY %s`,
		textExpr(cols, "id"),
		textExpr(cols, "parent_session_id"),
		textExpr(cols, "title"),
		textExpr(cols, "working_dir"),
		textExpr(cols, "project_dir"),
		textExpr(cols, "model"),
		textExpr(cols, "provider_id"),
		numExpr(cols, "created_at"),
		numExpr(cols, "updated_at"),
		numExpr(cols, "last_activity_at"),
		textExpr(cols, "id"),
	)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var (
			s          sessionRow
			workingDir string
			projectDir string
		)
		if err := rows.Scan(
			&s.ID,
			&s.ParentID,
			&s.Title,
			&workingDir,
			&projectDir,
			&s.Model,
			&s.Provider,
			&s.Created,
			&s.Updated,
			&s.LastActivity,
		); err != nil {
			return nil, err
		}
		s.Project = firstNonEmpty(workingDir, projectDir)
		if s.ID != "" {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

func loadMessages(ctx context.Context, db *sql.DB, tables map[string]map[string]bool) ([]messageRow, error) {
	cols := tables["messages"]
	if len(cols) == 0 {
		return nil, nil
	}
	query := fmt.Sprintf(
		`SELECT %s, %s, %s, %s, %s, %s FROM messages ORDER BY %s, %s`,
		textExpr(cols, "id"),
		textExpr(cols, "session_id"),
		textExpr(cols, "role"),
		textExpr(cols, "content"),
		textExpr(cols, "agent_meta"),
		numExpr(cols, "created_at"),
		numExpr(cols, "created_at"),
		textExpr(cols, "id"),
	)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []messageRow
	for rows.Next() {
		var m messageRow
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.AgentMeta, &m.Created); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func loadDailyUsage(ctx context.Context, db *sql.DB, tables map[string]map[string]bool) ([]dailyRow, error) {
	cols := tables["daily_model_usage"]
	if len(cols) == 0 {
		return nil, nil
	}
	query := fmt.Sprintf(
		`SELECT %s, %s, %s, %s, %s, %s, %s, %s FROM daily_model_usage ORDER BY %s, %s`,
		textExpr(cols, "day"),
		textExpr(cols, "agent_kind"),
		textExpr(cols, "model"),
		numExpr(cols, "input_tokens"),
		numExpr(cols, "output_tokens"),
		numExpr(cols, "cache_read_tokens"),
		numExpr(cols, "cache_create_tokens"),
		numExpr(cols, "updated_at"),
		textExpr(cols, "day"),
		textExpr(cols, "model"),
	)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dailyRow
	for rows.Next() {
		var d dailyRow
		if err := rows.Scan(&d.Day, &d.AgentKind, &d.Model, &d.Input, &d.Output, &d.CacheRead, &d.CacheCreate, &d.Updated); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func buildBatch(sessions []sessionRow, messages []messageRow, daily []dailyRow) ([]model.UsageEvent, []model.SessionMeta) {
	byID := make(map[string]sessionRow, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
	}
	titles := map[string]string{}
	firstSeen := map[string]time.Time{}
	lastSeen := map[string]time.Time{}
	bump := func(m map[string]time.Time, id string, at time.Time) {
		if id == "" || at.IsZero() {
			return
		}
		if old, ok := m[id]; !ok || at.Before(old) {
			m[id] = at
		}
	}
	bumpLast := func(id string, at time.Time) {
		if id == "" || at.IsZero() {
			return
		}
		if old, ok := lastSeen[id]; !ok || at.After(old) {
			lastSeen[id] = at
		}
	}

	var events []model.UsageEvent
	messageTotals := map[string]model.Tokens{}
	for _, m := range messages {
		if m.SessionID == "" {
			continue
		}
		at := unixMillis(m.Created)
		bump(firstSeen, m.SessionID, at)
		bumpLast(m.SessionID, at)
		if m.Role == "user" && titles[m.SessionID] == "" {
			if title := titleText(m.Content); title != "" {
				titles[m.SessionID] = title
			}
		}
		if m.Role != "assistant" {
			continue
		}
		details, ok := parseUsage(m.AgentMeta)
		if !ok {
			continue
		}
		tokens := model.Tokens{
			Input:      count(details.InputTokens),
			Output:     count(details.OutputTokens),
			CacheRead:  count(details.CacheReadTokens),
			CacheWrite: count(details.CacheCreateTokens),
		}
		if tokens.Total() == 0 {
			continue
		}
		s := byID[m.SessionID]
		modelName := normalizeModel(firstNonEmpty(details.Model, detailModel(details), s.Model))
		if modelName == "" {
			continue
		}
		if at.IsZero() {
			at = unixMillis(s.Created)
		}
		events = append(events, model.UsageEvent{
			Harness:     model.Cindy,
			DedupKey:    messageKey(m),
			SessionID:   m.SessionID,
			ParentID:    s.ParentID,
			ProjectPath: s.Project,
			Timestamp:   at,
			Model:       modelName,
			Provider:    firstNonEmpty(details.ProviderID, details.ProviderName, s.Provider),
			Tokens:      tokens,
		})
		if !at.IsZero() {
			key := dayKey(at) + "|" + modelName
			messageTotals[key] = addTokens(messageTotals[key], tokens)
		}
	}

	// daily_model_usage has one row per (day, agent kind, model). Group by
	// day and normalized model first: a model can appear under more than one
	// agent kind, and subtracting the same message totals from each row would
	// undercount the day.
	dailyAgg := map[string]*dailyRow{}
	for _, d := range daily {
		modelName := normalizeModel(d.Model)
		if d.Day == "" || modelName == "" {
			continue
		}
		key := d.Day + "|" + modelName
		a := dailyAgg[key]
		if a == nil {
			a = &dailyRow{Day: d.Day, Model: modelName}
			dailyAgg[key] = a
		}
		a.Input += d.Input
		a.Output += d.Output
		a.CacheRead += d.CacheRead
		a.CacheCreate += d.CacheCreate
		if d.Updated > a.Updated {
			a.Updated = d.Updated
		}
	}
	dailyKeys := make([]string, 0, len(dailyAgg))
	for key := range dailyAgg {
		dailyKeys = append(dailyKeys, key)
	}
	sort.Strings(dailyKeys)

	var dailyEvents []model.UsageEvent
	var dailyStart, dailyEnd time.Time
	for _, key := range dailyKeys {
		d := dailyAgg[key]
		want := model.Tokens{
			Input:      count(d.Input),
			Output:     count(d.Output),
			CacheRead:  count(d.CacheRead),
			CacheWrite: count(d.CacheCreate),
		}
		if want.Total() == 0 {
			continue
		}
		got := messageTotals[d.Day+"|"+d.Model]
		residual := subtractTokens(want, got)
		if residual.Total() == 0 {
			continue
		}
		at := dailyTimestamp(d.Day, d.Updated)
		dailyEvents = append(dailyEvents, model.UsageEvent{
			Harness:   model.Cindy,
			DedupKey:  "daily:" + d.Day + ":" + d.Model,
			SessionID: dailySessionID,
			Timestamp: at,
			Model:     d.Model,
			Tokens:    residual,
		})
		if !at.IsZero() {
			if dailyStart.IsZero() || at.Before(dailyStart) {
				dailyStart = at
			}
			if dailyEnd.IsZero() || at.After(dailyEnd) {
				dailyEnd = at
			}
		}
	}
	events = append(events, dailyEvents...)

	var metas []model.SessionMeta
	seen := map[string]bool{}
	for _, s := range sessions {
		seen[s.ID] = true
		started := unixMillis(s.Created)
		if started.IsZero() {
			started = firstSeen[s.ID]
		}
		updated := maxTime(unixMillis(s.Updated), unixMillis(s.LastActivity), lastSeen[s.ID])
		title := s.Title
		if title == "" {
			title = titles[s.ID]
		}
		metas = append(metas, model.SessionMeta{
			Harness:   model.Cindy,
			SessionID: s.ID,
			ParentID:  s.ParentID,
			Title:     harness.Title(title),
			Project:   s.Project,
			StartedAt: started,
			UpdatedAt: updated,
		})
	}
	for id, first := range firstSeen {
		if seen[id] {
			continue
		}
		metas = append(metas, model.SessionMeta{
			Harness:   model.Cindy,
			SessionID: id,
			Title:     harness.Title(titles[id]),
			StartedAt: first,
			UpdatedAt: lastSeen[id],
		})
	}
	if len(dailyEvents) > 0 {
		metas = append(metas, model.SessionMeta{
			Harness:   model.Cindy,
			SessionID: dailySessionID,
			Title:     dailySessionName,
			StartedAt: dailyStart,
			UpdatedAt: dailyEnd,
		})
	}
	return events, metas
}

func parseUsage(raw string) (*usageDetails, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, false
	}
	var meta messageMeta
	if err := json.Unmarshal([]byte(raw), &meta); err != nil || meta.TurnUsageDetails == nil {
		return nil, false
	}
	return meta.TurnUsageDetails, true
}

func detailModel(details *usageDetails) string {
	for _, raw := range details.Models {
		var name string
		if err := json.Unmarshal(raw, &name); err == nil && strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name)
		}
		var obj struct {
			Model string `json:"model"`
			Name  string `json:"name"`
			ID    string `json:"id"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil {
			if name := firstNonEmpty(obj.Model, obj.Name, obj.ID); name != "" {
				return name
			}
		}
	}
	return ""
}

func messageKey(m messageRow) string {
	id := strings.TrimSpace(m.ID)
	if id == "" {
		return fmt.Sprintf("msg:%s:%d", m.SessionID, int64(math.Round(m.Created)))
	}
	return "msg:" + m.SessionID + ":" + id
}

func normalizeModel(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.Index(name, "#billing="); i >= 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}

func dayKey(t time.Time) string { return t.In(time.Local).Format("2006-01-02") }

func dailyTimestamp(day string, updated float64) time.Time {
	if at := unixMillis(updated); !at.IsZero() {
		return at
	}
	t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(day), time.Local)
	if err != nil {
		return time.Time{}
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 12, 0, 0, 0, time.Local)
}

func unixMillis(value float64) time.Time {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return time.Time{}
	}
	ms := int64(math.Round(value))
	if ms < 1e12 {
		ms *= 1000
	}
	return time.UnixMilli(ms).UTC()
}

func addTokens(a, b model.Tokens) model.Tokens {
	return model.Tokens{
		Input:      a.Input + b.Input,
		Output:     a.Output + b.Output,
		CacheRead:  a.CacheRead + b.CacheRead,
		CacheWrite: a.CacheWrite + b.CacheWrite,
	}
}

func subtractTokens(want, got model.Tokens) model.Tokens {
	return model.Tokens{
		Input:      positiveSubtract(want.Input, got.Input),
		Output:     positiveSubtract(want.Output, got.Output),
		CacheRead:  positiveSubtract(want.CacheRead, got.CacheRead),
		CacheWrite: positiveSubtract(want.CacheWrite, got.CacheWrite),
	}
}

func positiveSubtract(a, b int64) int64 {
	if b >= a {
		return 0
	}
	return a - b
}

func maxTime(values ...time.Time) time.Time {
	var out time.Time
	for _, v := range values {
		if v.After(out) {
			out = v
		}
	}
	return out
}

// tableColumns maps every table and view to its column set so the queries can
// degrade gracefully across schema generations.
func tableColumns(ctx context.Context, db *sql.DB) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type IN ('table','view')`)
	if err != nil {
		return out
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		names = append(names, name)
	}
	rows.Close()
	for _, name := range names {
		cols := map[string]bool{}
		rs, err := db.QueryContext(ctx, `PRAGMA table_info(`+quoteIdent(name)+`)`)
		if err != nil {
			continue
		}
		for rs.Next() {
			var (
				cid, notnull, pk int
				col, ctype       string
				dflt             sql.NullString
			)
			if err := rs.Scan(&cid, &col, &ctype, &notnull, &dflt, &pk); err != nil {
				continue
			}
			cols[col] = true
		}
		rs.Close()
		out[name] = cols
	}
	return out
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func textExpr(cols map[string]bool, name string) string {
	if cols[name] {
		return "COALESCE(" + quoteIdent(name) + ",'')"
	}
	return "''"
}

func numExpr(cols map[string]bool, name string) string {
	if cols[name] {
		return "COALESCE(" + quoteIdent(name) + ",0)"
	}
	return "0"
}

func count(v float64) int64 {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	return int64(math.Round(v))
}

func titleText(raw string) string {
	text := strings.TrimSpace(textOf(raw))
	if text == "" || !titleCandidate(text) {
		return ""
	}
	return harness.Title(text)
}

func titleCandidate(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "<") {
		return false
	}
	return !slashCommandRE.MatchString(s)
}

// textOf flattens a string-or-blocks message body. Cindy stores user content as
// either a plain string or a JSON array of text blocks.
func textOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal([]byte(raw), &text); err == nil {
			return text
		}
	}
	if raw[0] != '[' {
		return raw
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
		return raw
	}
	var b strings.Builder
	for _, blk := range blocks {
		text := strings.TrimSpace(blk.Text)
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(text)
	}
	if b.Len() == 0 {
		return raw
	}
	return b.String()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
