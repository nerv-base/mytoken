package codebuddycn

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nerv-base/mytoken/internal/harness"
	"github.com/nerv-base/mytoken/internal/model"
)

const (
	kindLog      = "log"
	fingerprintN = 4096
	creditUnit   = "credit"

	usageMarker   = "Agent execution successful with usage:"
	usageTag      = "[AgentReporter]"
	modelMarker   = "Model prepared:"
	modelTag      = "[CraftInvokableAgent]"
	workspaceMark = "Workspace Path:"
)

// Log timestamps are local wall-clock with milliseconds: the app writes
// "2006-01-02 15:04:05.000", the extension log writes "[2006/1/2 15:04:05.000]".
var (
	appTime  = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2}) (\d{2}):(\d{2}):(\d{2})\.(\d{3})`)
	extTime  = regexp.MustCompile(`^\[?(\d{4})/(\d{1,2})/(\d{1,2}) (\d{1,2}):(\d{2}):(\d{2})\.(\d{3})\]?`)
	agentTag = regexp.MustCompile(`^\[([A-Za-z0-9_-]+)\]`)
)

// logState is the per-source memory that survives between incremental reads:
// the workspace path (written once, at the top of the file) and the model each
// agent announced before its usage line.
type logState struct {
	Project string            `json:"p,omitempty"`
	Models  map[string]string `json:"m,omitempty"`
}

// parseLog reads the app's and the extension's request logs incrementally, one
// completed line at a time.
func (p *Parser) parseLog(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	f, err := os.Open(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return harness.Batch{}, err
	}
	// These logs start small and grow by a line at a time, so the fingerprint
	// must cover only what was already read: hashing the first 4 KB of a
	// 300-byte file would change on every append and force a full re-read.
	probe := int64(fingerprintN)
	if cur.Fingerprint != "" && cur.Offset < probe {
		probe = cur.Offset
	}
	fp, err := fingerprint(f, probe)
	if err != nil {
		return harness.Batch{}, err
	}
	if unchanged(cur, st, fp) {
		return harness.Batch{Next: cur}, nil
	}

	next := harness.Cursor{Size: st.Size(), ModTime: st.ModTime(), Fingerprint: fp, Extra: cur.Extra}
	off := cur.Offset
	if off < 0 || off > st.Size() || (cur.Fingerprint != "" && cur.Fingerprint != fp) {
		off = 0
		next.Extra = ""
	}
	state := decodeLogState(next.Extra)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return harness.Batch{}, err
	}

	var b harness.Batch
	sessions := map[string]*model.SessionMeta{}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return b, err
		}
		line, err := r.ReadBytes('\n')
		n := len(line)
		if n == 0 || line[n-1] != '\n' {
			// A trailing partial line is left for the next parse.
			break
		}
		off += int64(n)
		p.consumeLogLine(&b, sessions, state, strings.TrimRight(string(bytes.TrimPrefix(line[:n-1], []byte("\ufeff"))), "\r"), st.ModTime())
		if err == io.EOF {
			break
		}
		if err != nil {
			return b, err
		}
	}

	next.Offset = off
	next.Extra = encodeLogState(state)
	for _, s := range sortedSessions(sessions) {
		b.Sessions = append(b.Sessions, *s)
	}
	b.Next = next
	return b, nil
}

// consumeLogLine turns one log line into zero or one usage event.
func (p *Parser) consumeLogLine(b *harness.Batch, sessions map[string]*model.SessionMeta, state *logState, line string, mtime time.Time) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return
	}
	if state.Project == "" && strings.HasPrefix(trimmed, workspaceMark) {
		state.Project = strings.TrimSpace(strings.TrimPrefix(trimmed, workspaceMark))
		return
	}

	if strings.Contains(trimmed, modelTag) && strings.Contains(trimmed, modelMarker) {
		agent, ok := valueAfterTag(trimmed, modelTag)
		if !ok {
			return
		}
		if model := modelAfterMarker(trimmed); model != "" {
			if state.Models == nil {
				state.Models = map[string]string{}
			}
			state.Models[agent] = model
		}
		return
	}

	if !strings.Contains(trimmed, usageTag) || !strings.Contains(trimmed, usageMarker) {
		return
	}
	agent, ok := valueAfterTag(trimmed, usageTag)
	if !ok {
		return
	}
	usage, ok := usageJSON(trimmed)
	if !ok {
		return
	}
	tokens, ok := tokensFromUsage(usage)
	if !ok || tokens.IsZero() {
		return
	}

	ts := logTimestamp(trimmed, mtime)
	ev := model.UsageEvent{
		Harness:   model.CodeBuddyCN,
		DedupKey:  fmt.Sprintf("cbcn:%s:%d:%d:%d:%d:%d", agent, tokens.Input, tokens.Output, tokens.CacheRead, tokens.CacheWrite, tokens.Reasoning),
		SessionID: agent,
		Timestamp: ts.UTC(),
		Model:     state.Models[agent],
		Tokens:    tokens,
	}
	if state.Project != "" {
		ev.ProjectPath = state.Project
	}
	if credit, ok := floatFrom(usage, "credit", "credits", "cost"); ok && credit > 0 {
		ev.Bill = &model.Bill{Amount: credit, Unit: creditUnit}
	}

	// A single request is journalled by both the app log and the extension log;
	// DedupKey collapses the copy, so the same event is not added twice here.
	for _, existing := range b.Events {
		if existing.DedupKey == ev.DedupKey {
			return
		}
	}
	b.Events = append(b.Events, ev)

	s := sessions[agent]
	if s == nil {
		s = &model.SessionMeta{Harness: model.CodeBuddyCN, SessionID: agent, Project: state.Project, StartedAt: ts}
		sessions[agent] = s
	}
	if ts.Before(s.StartedAt) {
		s.StartedAt = ts
	}
	if ts.After(s.UpdatedAt) {
		s.UpdatedAt = ts
	}
}

// valueAfterTag returns the bracket group that follows tag, e.g. the agent id in
// "[AgentReporter] [1a7aac0766...]".
func valueAfterTag(line, tag string) (string, bool) {
	i := strings.Index(line, tag)
	if i < 0 {
		return "", false
	}
	rest := strings.TrimLeft(line[i+len(tag):], " ")
	if m := agentTag.FindStringSubmatch(rest); m != nil {
		return m[1], true
	}
	return "", false
}

// modelAfterMarker reads "Hy4 (hy4free)" and keeps the parenthesized id when the
// line carries one, which is the id the gateway actually billed.
func modelAfterMarker(line string) string {
	i := strings.Index(line, modelMarker)
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(line[i+len(modelMarker):])
	if open := strings.LastIndex(rest, "("); open != -1 {
		if closeIdx := strings.Index(rest[open+1:], ")"); closeIdx != -1 {
			if inner := strings.TrimSpace(rest[open+1 : open+1+closeIdx]); inner != "" {
				return inner
			}
		}
	}
	if fields := strings.Fields(rest); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// usageJSON decodes the object that ends the line.
func usageJSON(line string) (map[string]any, bool) {
	i := strings.Index(line, usageMarker)
	if i < 0 {
		return nil, false
	}
	raw := strings.TrimSpace(line[i+len(usageMarker):])
	end := strings.LastIndex(raw, "}")
	if end < 0 {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(raw[:end+1]))
	dec.UseNumber()
	var usage map[string]any
	if err := dec.Decode(&usage); err != nil {
		return nil, false
	}
	return usage, true
}

// tokensFromUsage normalizes the tools' several spellings into the five token
// classes MyToken keeps disjoint.
//
// inputTokens is the whole prompt, cache included, and outputTokens is the whole
// completion, thinking included, so both are reduced when the log states the
// parts: cachedMissTokens/cacheMissTokens give the uncached input directly, and
// thinkingTokens is the reasoning that already sits inside outputTokens.
func tokensFromUsage(usage map[string]any) (model.Tokens, bool) {
	input, hasMiss := numberFrom(usage,
		"cachedMissTokens", "cacheMissTokens", "prompt_cache_miss_tokens")
	if !hasMiss {
		if total, ok := numberFrom(usage, "input_tokens", "inputTokens", "prompt_tokens"); ok {
			input = total
		}
	}
	output, _ := numberFrom(usage, "output_tokens", "outputTokens", "completion_tokens")
	cacheRead, _ := numberFrom(usage,
		"cache_read_input_tokens", "cacheReadInputTokens", "cacheTokens", "prompt_cache_hit_tokens", "cached_tokens")
	cacheWrite, _ := numberFrom(usage,
		"cache_creation_input_tokens", "cacheCreationInputTokens", "cachedWriteTokens", "prompt_cache_write_tokens")
	reasoning, _ := numberFrom(usage,
		"completion_thinking_tokens", "completionThinkingTokens", "reasoningTokens", "thinkingTokens")

	if !hasMiss && cacheRead > 0 {
		input -= cacheRead
	}
	output -= reasoning

	return model.Tokens{
		Input:      maxInt(input),
		Output:     maxInt(output),
		CacheRead:  maxInt(cacheRead),
		CacheWrite: maxInt(cacheWrite),
		Reasoning:  maxInt(reasoning),
	}, true
}

// numberFrom returns the first present numeric key.
func numberFrom(usage map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		v, ok := usage[key]
		if !ok || v == nil {
			continue
		}
		switch n := v.(type) {
		case json.Number:
			f, err := n.Float64()
			if err != nil {
				continue
			}
			return int64(f), true
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
			if err != nil {
				continue
			}
			return int64(f), true
		}
	}
	return 0, false
}

// logTimestamp reads the leading wall-clock stamp; mtime is the fallback for a
// line the tool wrote without one.
func logTimestamp(line string, mtime time.Time) time.Time {
	for _, re := range []*regexp.Regexp{appTime, extTime} {
		if m := re.FindStringSubmatch(line); m != nil {
			y, _ := strconv.Atoi(m[1])
			mo, _ := strconv.Atoi(m[2])
			d, _ := strconv.Atoi(m[3])
			h, _ := strconv.Atoi(m[4])
			mi, _ := strconv.Atoi(m[5])
			se, _ := strconv.Atoi(m[6])
			ms, _ := strconv.Atoi(m[7])
			return time.Date(y, time.Month(mo), d, h, mi, se, ms*1e6, time.Local)
		}
	}
	return mtime
}

func sortedSessions(m map[string]*model.SessionMeta) []*model.SessionMeta {
	out := make([]*model.SessionMeta, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].SessionID < out[j-1].SessionID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func decodeLogState(raw string) *logState {
	state := &logState{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), state); err != nil {
			return &logState{}
		}
	}
	return state
}

func encodeLogState(state *logState) string {
	raw, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	return string(raw)
}

func unchanged(cur harness.Cursor, st os.FileInfo, fp string) bool {
	return cur.Fingerprint != "" && cur.Fingerprint == fp &&
		cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime())
}

// fingerprint hashes the first n bytes and leaves the file offset at 0.
func fingerprint(f *os.File, n int64) (string, error) {
	if n <= 0 {
		n = fingerprintN
	}
	buf := make([]byte, n)
	read, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	sum := sha1.Sum(buf[:read])
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum[:]), nil
}

func maxInt(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// floatFrom returns the first present numeric key as a float, which is what the
// tool's own credit charge needs.
func floatFrom(usage map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		v, ok := usage[key]
		if !ok || v == nil {
			continue
		}
		switch n := v.(type) {
		case json.Number:
			f, err := n.Float64()
			if err != nil {
				continue
			}
			return f, true
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
			if err != nil {
				continue
			}
			return f, true
		}
	}
	return 0, false
}
