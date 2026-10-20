package dsh

import (
	"encoding/json"
)

// Stream timings.
//
// Every assistant/message record carries data.stream: the compact journal of the
// token stream that produced the message, with a millisecond time on every
// record. DSH itself derives its own tok/s from it — output tokens over the
// window from the first streamed token to the moment the message settled — so
// that is the window this reader records as model.UsageEvent.DecodeMS. Nothing
// is inferred: either the stream names a first token or the event reports no
// measurement at all.
//
// The stream is also the bulk of the file two-thirds of the way down, so it is
// never decoded wholesale: the search stops at the first token, which is at the
// head of the stream in practice.

// decodeWindow returns the generation window of one record: the time from its
// first streamed token to the record's own time, which is when DSH wrote the
// settled message. Zero when the stream names no token.
//
// The record line is scanned rather than decoded: the fast path deliberately
// skips the conversation body and the journal, which are most of the file.
func decodeWindow(at num, line []byte) int64 {
	if at <= 0 {
		return 0
	}
	data, ok := objectValue(line, "data")
	if !ok {
		return 0
	}
	first := firstTokenTime(data)
	if first <= 0 || first > int64(at) {
		return 0
	}
	return int64(at) - first
}

// streamRecord is one record of data.stream.
type streamRecord struct {
	Type  string       `json:"type"`
	Time  num          `json:"time"`  // chunk records: absolute time
	Time0 num          `json:"time0"` // packed runs: time of the first member
	Name  *string      `json:"name"`  // tool-call runs name the call up front
	Chunk *streamChunk `json:"chunk"`
	DT    []num        `json:"dt"`    // packed runs: per-member time deltas
	Texts []string     `json:"texts"` // text-chunks, reasoning-chunks
	Args  []string     `json:"args"`  // tool-call-chunks
}

// streamChunk is the payload of a chunk record.
type streamChunk struct {
	Type           string  `json:"type"`
	Text           *string `json:"text"`
	Name           *string `json:"name"`
	ArgumentsDelta *string `json:"argumentsDelta"`
}

// firstTokenTime returns the millisecond time of the first output token in one
// assistant/message data object, or 0 when its stream carries no token at all.
//
// It follows DSH's own rule (assistantStreamFirstTokenTime) so the reading is
// the same number the tool displays: a text or reasoning delta with a non-empty
// fragment, any tool-call delta, or the first non-empty member of a packed run
// (a tool-call run that already names its call starts at its first member).
func firstTokenTime(data []byte) int64 {
	stream, ok := objectValue(data, "stream")
	if !ok {
		return 0
	}
	i := skipSpace(stream, 0)
	if i >= len(stream) || stream[i] != '[' {
		return 0
	}
	i++
	for {
		i = skipSpace(stream, i)
		if i >= len(stream) || stream[i] == ']' {
			return 0
		}
		end := skipValue(stream, i)
		if end <= i {
			return 0
		}
		var rec streamRecord
		// Only the record under exam is decoded, and only until the first token.
		if json.Unmarshal(stream[i:end], &rec) == nil {
			if t := rec.firstToken(); t > 0 {
				return t
			}
		}
		i = skipSpace(stream, end)
		if i < len(stream) && stream[i] == ',' {
			i++
		}
	}
}

// firstToken reports the record's own first token time according to the rule in
// firstTokenTime, or 0 when the record carries none.
func (r *streamRecord) firstToken() int64 {
	if r.Type == "chunk" {
		if r.Chunk != nil && r.Chunk.tokenDelta() {
			return int64(r.Time)
		}
		return 0
	}
	switch r.Type {
	case "text-chunks", "reasoning-chunks", "tool-call-chunks":
	default:
		return 0
	}
	if r.Type == "tool-call-chunks" && r.Name != nil {
		return int64(r.Time0)
	}
	frags := r.Texts
	if r.Type == "tool-call-chunks" {
		frags = r.Args
	}
	at := int64(r.Time0)
	for i, frag := range frags {
		if i > 0 && i-1 < len(r.DT) {
			at += int64(r.DT[i-1])
		}
		if frag != "" {
			return at
		}
	}
	return 0
}

// tokenDelta reports whether the chunk carries the model's first output token:
// a non-empty text or reasoning fragment, or a tool call in any form.
func (c *streamChunk) tokenDelta() bool {
	switch c.Type {
	case "text-delta", "reasoning-delta":
		return c.Text != nil && *c.Text != ""
	case "tool-call-delta":
		return (c.ArgumentsDelta != nil && *c.ArgumentsDelta != "") || c.Name != nil
	}
	return false
}

// objectValue returns the raw JSON of one top-level member of a JSON object. It
// scans the object without decoding it, so a large member the caller skipped
// costs no allocation.
func objectValue(obj []byte, key string) ([]byte, bool) {
	i := skipSpace(obj, 0)
	if i >= len(obj) || obj[i] != '{' {
		return nil, false
	}
	i++
	for {
		i = skipSpace(obj, i)
		if i >= len(obj) || obj[i] != '"' {
			return nil, false
		}
		nameEnd := skipString(obj, i)
		if nameEnd < 0 {
			return nil, false
		}
		match := keyIs(obj[i:nameEnd], key)
		i = skipSpace(obj, nameEnd)
		if i >= len(obj) || obj[i] != ':' {
			return nil, false
		}
		i = skipSpace(obj, i+1)
		end := skipValue(obj, i)
		if end <= i {
			return nil, false
		}
		if match {
			return obj[i:end], true
		}
		i = skipSpace(obj, end)
		if i < len(obj) && obj[i] == ',' {
			i++
			continue
		}
		return nil, false
	}
}

// keyIs compares a raw JSON string token with a plain key name.
func keyIs(raw []byte, key string) bool {
	if len(raw) != len(key)+2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return false
	}
	for i := 0; i < len(key); i++ {
		if raw[i+1] != key[i] {
			return false
		}
	}
	return true
}

// skipSpace returns the index of the first non-space byte at or after i.
func skipSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// skipString returns the index just past the string token starting at i, or -1.
func skipString(b []byte, i int) int {
	if i >= len(b) || b[i] != '"' {
		return -1
	}
	for i++; i < len(b); {
		switch b[i] {
		case '\\':
			i += 2 // an escape, including \uXXXX, never contains a quote
		case '"':
			return i + 1
		default:
			i++
		}
	}
	return -1
}

// skipValue returns the index just past the JSON value starting at i, or -1.
func skipValue(b []byte, i int) int {
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				next := skipString(b, i)
				if next < 0 {
					return -1
				}
				i = next
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return -1
	default:
		for i < len(b) {
			switch b[i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return i
			}
			i++
		}
		return i
	}
}
