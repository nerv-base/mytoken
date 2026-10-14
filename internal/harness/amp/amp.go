// Package amp parses Amp (Sourcegraph's coding agent, ampcode.com) usage.
//
// Amp mirrors every thread it serves as one JSON document per thread at
// <amp-data>/threads/<id>.json, where <amp-data> is $AMP_DATA_DIR
// (comma-separated for extra roots) or ~/.local/share/amp:
//
//	{ id, messages: [...], usageLedger?: { events: [...] } }
//
// usageLedger.events is the authoritative per-request record: { id, timestamp,
// model, tokens: { input, output, total }, toMessageId, credits }. Cache tokens
// are not on the event — they live on the thread message the event billed for
// (toMessageId -> message.messageId -> usage.cacheCreationInputTokens /
// usage.cacheReadInputTokens). Threads without a ledger fall back to the
// per-assistant-message usage blocks. For ledger events, any positive remainder
// of tokens.total after input, output and both cache counts is added to output;
// a message block with no individual counts uses its totalTokens as the output
// count.
//
// Ported from codeburn (MIT, Copyright (c) 2026 AgentSeal),
// src/providers/amp.ts; see THIRD_PARTY_NOTICES. Deviations from that reader:
// costs are left to MyToken's price table instead of codeburn's own catalog,
// Amp's credits are reported as Bill{Unit: "credit"} rather than converted, and
// RequestID stays empty — the ledger's event id is Amp's local record id, not
// an upstream request id (it is used as the DedupKey instead). Thread files
// carry no cwd, so sessions group under the "Amp" project label, as upstream
// does.
//
// Not verified against real Amp data: this machine has none. The conformance
// fixture was written from codeburn's reader and Amp's documented thread shape.
package amp

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	// threadsDirName is the directory Amp keeps one JSON document per thread in.
	threadsDirName = "threads"
	// kindJSON is the source kind of a whole-file JSON thread document.
	kindJSON = "json"
	// providerName is Amp's provider id in the logs.
	providerName = "amp"
	// projectLabel groups Amp sessions: thread files carry no working directory.
	projectLabel = "Amp"
	// creditsUnit is Amp's native billing unit.
	creditsUnit = "credit"

	// fingerprintBytes is how much of a file the change fingerprint covers.
	fingerprintBytes = 4096
	// maxSafeInteger mirrors codeburn's clamp on hostile token fields.
	maxSafeInteger = float64(1<<53 - 1)
)

// Parser implements harness.Parser for Amp.
type Parser struct {
	roots []string
	// prov recomputes this parser's default-root provenance from the current
	// environment. It is set only on the registry singleton; a parser built for
	// explicit roots reports those roots instead.
	prov func() ([]harness.Root, error)
}

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// New returns a parser over the default roots.
func New() *Parser {
	p := NewWithRoots(defaultRoots()...)
	p.prov = rootProvenance
	return p
}

// NewWithRoot returns a parser over a single root.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser over the given roots.
func NewWithRoots(roots ...string) *Parser {
	p := &Parser{}
	seen := make(map[string]bool, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		p.roots = append(p.roots, root)
	}
	return p
}

// rootProvenance reports Amp's default roots and where each came from. It reads
// the environment on every call so an override set after startup is honoured.
func rootProvenance() ([]harness.Root, error) {
	if v, ok := harness.Getenv("MYTOKEN_AMP_DIRS"); ok {
		dirs, err := harness.SplitEnvList("MYTOKEN_AMP_DIRS", v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots("MYTOKEN_AMP_DIRS", dirs...), nil
	}
	if v, ok := harness.Getenv("AMP_DATA_DIR"); ok {
		var dirs []string
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				dirs = append(dirs, filepath.Clean(part))
			}
		}
		if len(dirs) == 0 {
			return nil, &harness.EnvListError{Env: "AMP_DATA_DIR", Value: v}
		}
		return harness.EnvRoots("AMP_DATA_DIR", dirs...), nil
	}
	return harness.BuiltinRoots(builtinRoots()...), nil
}

// builtinRoots is Amp's platform default, used when no explicit override is set.
func builtinRoots() []string {
	return []string{filepath.Join(harness.Home(), ".local", "share", "amp")}
}

// defaultRoots returns the paths of rootProvenance. A malformed explicit
// override yields no roots: it is never replaced by the built-in default.
func defaultRoots() []string {
	roots, err := rootProvenance()
	if err != nil {
		return nil
	}
	return harness.RootPaths(roots)
}

// RootProvenance implements harness.Provenance.
func (p *Parser) RootProvenance() ([]harness.Root, error) {
	if p.prov == nil {
		return harness.ExplicitRoots(p.roots...), nil
	}
	return p.prov()
}

// Harness reports the harness this parser reads.
func (p *Parser) Harness() model.Harness { return model.Amp }

// WithRoots implements harness.Rooted. It returns a fresh parser over exactly
// the given roots; an explicit empty list stays empty instead of falling back
// to this harness's default roots.
func (p *Parser) WithRoots(roots ...string) harness.Parser {
	for _, r := range roots {
		if strings.TrimSpace(r) != "" {
			return NewWithRoots(roots...)
		}
	}
	return &Parser{}
}

// Roots reports the configured roots.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover finds every thread document under <root>/threads. Discovery walks
// names only; the parser validates each document once it reads it.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := make(map[string]bool)
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		threads := filepath.Join(root, threadsDirName)
		err := filepath.WalkDir(threads, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".json") || seen[path] {
				return nil
			}
			seen[path] = true
			out = append(out, harness.Source{Path: path, Kind: kindJSON})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Parse reads one thread document. A thread is a single JSON value, so it is
// reparsed in full whenever the file changes; an unchanged file is a no-op and
// duplicate records are collapsed by DedupKey.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{}, err
	}
	st, err := os.Stat(src.Path)
	if err != nil || st.IsDir() {
		// Amp deletes threads it no longer needs; keep the cursor.
		return harness.Batch{Next: cur}, nil
	}
	fp, err := fingerprint(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	if cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime()) {
		return harness.Batch{Next: cur}, nil
	}
	next := harness.Cursor{Offset: st.Size(), Size: st.Size(), ModTime: st.ModTime(), Fingerprint: fp}
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return harness.Batch{Next: next}, nil
	}
	thread, ok := decodeThread(raw)
	if !ok {
		// A partially written or foreign document yields nothing; the cursor
		// still advances so the next append is reparsed.
		return harness.Batch{Next: next}, nil
	}

	byKey := make(map[string]model.UsageEvent)
	var started, updated time.Time
	for _, rec := range thread.calls() {
		if !rec.hasTokens() {
			continue
		}
		ts := parseTimestamp(rec.timestamp)
		if ts.IsZero() {
			continue
		}
		key := rec.dedupKey(thread.ID)
		if _, dup := byKey[key]; dup {
			continue
		}
		event := model.UsageEvent{
			Harness:     model.Amp,
			DedupKey:    key,
			SessionID:   thread.ID,
			ProjectPath: projectLabel,
			Timestamp:   ts,
			Model:       rec.model,
			Provider:    providerName,
			Tokens: model.Tokens{
				Input:      round(rec.input),
				Output:     round(rec.output),
				CacheRead:  round(rec.cacheRead),
				CacheWrite: round(rec.cacheWrite),
			},
		}
		if rec.credits > 0 {
			event.Bill = &model.Bill{Amount: rec.credits, Unit: creditsUnit}
		}
		byKey[key] = event
		if started.IsZero() || ts.Before(started) {
			started = ts
		}
		if ts.After(updated) {
			updated = ts
		}
	}
	if len(byKey) == 0 {
		return harness.Batch{Next: next}, nil
	}

	events := make([]model.UsageEvent, 0, len(byKey))
	for _, event := range byKey {
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].DedupKey < events[j].DedupKey })
	sessions := []model.SessionMeta{{
		Harness:   model.Amp,
		SessionID: thread.ID,
		Title:     harness.Title(firstUserText(thread.Messages)),
		Project:   projectLabel,
		StartedAt: started,
		UpdatedAt: updated,
	}}
	return harness.Batch{Events: events, Sessions: sessions, Next: next}, nil
}

// ampThread is the subset of a thread document MyToken reads.
type ampThread struct {
	ID          string          `json:"id"`
	Messages    []ampMessage    `json:"messages"`
	UsageLedger *ampUsageLedger `json:"usageLedger"`
	firstUser   string
}

type ampMessage struct {
	Role      string           `json:"role"`
	Content   json.RawMessage  `json:"content"`
	Text      string           `json:"text"`
	MessageID json.RawMessage  `json:"messageId"`
	Model     string           `json:"model"`
	Timestamp string           `json:"timestamp"`
	Usage     *ampMessageUsage `json:"usage"`
}

type ampMessageUsage struct {
	InputTokens              json.RawMessage `json:"inputTokens"`
	OutputTokens             json.RawMessage `json:"outputTokens"`
	CacheCreationInputTokens json.RawMessage `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     json.RawMessage `json:"cacheReadInputTokens"`
	TotalTokens              json.RawMessage `json:"totalTokens"`
	Timestamp                string          `json:"timestamp"`
	Model                    string          `json:"model"`
}

type ampUsageLedger struct {
	Events []ampLedgerEvent `json:"events"`
}

type ampLedgerEvent struct {
	ID          json.RawMessage `json:"id"`
	Timestamp   string          `json:"timestamp"`
	Model       string          `json:"model"`
	Tokens      *ampLedgerToken `json:"tokens"`
	ToMessageID json.RawMessage `json:"toMessageId"`
	Credits     json.RawMessage `json:"credits"`
}

type ampLedgerToken struct {
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
	Total  json.RawMessage `json:"total"`
}

// ampCall is one per-request usage record, from either the ledger or the
// per-message fallback.
type ampCall struct {
	model      string
	timestamp  string
	input      float64
	output     float64
	cacheWrite float64
	cacheRead  float64
	eventID    string
	credits    float64
}

func (c ampCall) hasTokens() bool {
	return c.input > 0 || c.output > 0 || c.cacheWrite > 0 || c.cacheRead > 0
}

// dedupKey mirrors codeburn: the ledger event id when there is one, otherwise
// the record's own timestamp, model and token pair.
func (c ampCall) dedupKey(threadID string) string {
	if c.eventID != "" {
		return providerName + ":" + threadID + ":" + c.eventID
	}
	return fmt.Sprintf("%s:%s:%s:%s:%dx%d", providerName, threadID, c.timestamp, c.model, round(c.input), round(c.output))
}

// decodeThread parses a thread document. Documents without an id are ignored,
// like upstream.
func decodeThread(raw []byte) (*ampThread, bool) {
	var thread ampThread
	if err := json.Unmarshal(raw, &thread); err != nil {
		return nil, false
	}
	if thread.ID == "" {
		return nil, false
	}
	return &thread, true
}

// calls returns the thread's usage records: the ledger when it has any events,
// otherwise the per-assistant-message usage blocks.
func (t *ampThread) calls() []ampCall {
	cacheByMessageID := make(map[int64]ampMessageUsage, len(t.Messages))
	for i := range t.Messages {
		message := &t.Messages[i]
		if message.Role != "assistant" || message.Usage == nil {
			continue
		}
		id, ok := integer(message.MessageID)
		if !ok {
			continue
		}
		cacheByMessageID[id] = *message.Usage
	}

	var calls []ampCall
	if t.UsageLedger != nil {
		for _, event := range t.UsageLedger.Events {
			if event.Timestamp == "" || event.Model == "" || event.Tokens == nil {
				continue
			}
			input := floatValue(event.Tokens.Input)
			output := floatValue(event.Tokens.Output)
			total, hasTotal := number(event.Tokens.Total)
			var cacheWrite, cacheRead float64
			if id, ok := integer(event.ToMessageID); ok {
				if usage, found := cacheByMessageID[id]; found {
					cacheWrite = floatValue(usage.CacheCreationInputTokens)
					cacheRead = floatValue(usage.CacheReadInputTokens)
				}
			}
			// Any positive remainder of the total goes to output, after the
			// explicit counts and both joined cache counts.
			accounted := input + output + cacheWrite + cacheRead
			billed := output
			if hasTotal && total > accounted {
				billed += total - accounted
			}
			calls = append(calls, ampCall{
				model:      event.Model,
				timestamp:  event.Timestamp,
				input:      input,
				output:     billed,
				cacheWrite: cacheWrite,
				cacheRead:  cacheRead,
				eventID:    rawString(event.ID),
				credits:    floatValue(event.Credits),
			})
		}
	}
	if len(calls) > 0 {
		return calls
	}

	for i := range t.Messages {
		message := &t.Messages[i]
		if message.Role != "assistant" || message.Usage == nil {
			continue
		}
		usage := message.Usage
		timestamp := firstNonEmpty(usage.Timestamp, message.Timestamp)
		model := firstNonEmpty(usage.Model, message.Model)
		if timestamp == "" || model == "" {
			continue
		}
		input := floatValue(usage.InputTokens)
		output := floatValue(usage.OutputTokens)
		cacheWrite := floatValue(usage.CacheCreationInputTokens)
		cacheRead := floatValue(usage.CacheReadInputTokens)
		hasParts := input > 0 || output > 0 || cacheWrite > 0 || cacheRead > 0
		if !hasParts {
			if total, ok := number(usage.TotalTokens); ok {
				output = total
			} else {
				output = 0
			}
		}
		calls = append(calls, ampCall{
			model:      model,
			timestamp:  timestamp,
			input:      input,
			output:     output,
			cacheWrite: cacheWrite,
			cacheRead:  cacheRead,
		})
	}
	return calls
}

// firstUserText is the first user message's text; the title only ever comes
// from a user message (upstream falls back to any message text).
func firstUserText(messages []ampMessage) string {
	for i := range messages {
		if !strings.EqualFold(messages[i].Role, "user") {
			continue
		}
		if text := messages[i].text(); text != "" {
			return text
		}
	}
	return ""
}

// text returns the message's text: a plain string content, the text blocks of
// an array content, or the message's own text field.
func (m *ampMessage) text() string {
	if text := contentText(m.Content); text != "" {
		return text
	}
	return m.Text
}

// contentText extracts text from a content field that is either a string or an
// array of {text} blocks.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, " ")
}

// number reads a non-negative finite number, like codeburn's finiteNonNegative.
func number(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	if value > maxSafeInteger {
		value = maxSafeInteger
	}
	return value, true
}

// floatValue is number with a zero default.
func floatValue(raw json.RawMessage) float64 {
	value, _ := number(raw)
	return value
}

// integer reads a JSON integer, the only form codeburn joins cache tokens on.
func integer(raw json.RawMessage) (int64, bool) {
	value, ok := number(raw)
	if !ok || value != math.Trunc(value) {
		return 0, false
	}
	return int64(value), true
}

// rawString reads a JSON string, or a JSON number rendered as text (the ledger
// event id may be either).
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err == nil {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// round clamps a float token count to int64.
func round(value float64) int64 {
	if value <= 0 {
		return 0
	}
	if value > maxSafeInteger {
		return int64(maxSafeInteger)
	}
	return int64(value)
}

// parseTimestamp accepts the RFC 3339 timestamps Amp writes, plus numeric
// seconds or milliseconds.
func parseTimestamp(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return ts.UTC()
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return time.Time{}
	}
	if value >= 1e12 {
		return time.UnixMilli(int64(value)).UTC()
	}
	seconds := int64(value)
	return time.Unix(seconds, int64((value-float64(seconds))*1e9)).UTC()
}

// fingerprint hashes the first 4KB of a file.
func fingerprint(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	buf := make([]byte, fingerprintBytes)
	n, err := io.ReadFull(file, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	sum := sha1.Sum(buf[:n])
	return hex.EncodeToString(sum[:]), nil
}
