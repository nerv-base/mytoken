package budget

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// The persisted state document. It is a versioned, explicit JSON format: the
// Go field names are not the wire format, and unknown fields are rejected so a
// document written by a different schema version fails loudly instead of being
// silently half-read.
type stateDoc struct {
	Version      int                       `json:"version"`
	Rules        map[string]ruleStateDoc   `json:"rules"`
	Reservations map[string]reservationDoc `json:"reservations"`
}

type ruleStateDoc struct {
	Version      int       `json:"version"`
	Fingerprint  string    `json:"fingerprint"`
	LastPeriodID string    `json:"last_period_id"`
	Watermark    time.Time `json:"watermark"`
}

type reservationDoc struct {
	ID          string    `json:"id"`
	RuleID      string    `json:"rule_id"`
	RuleVersion int       `json:"rule_version"`
	PeriodID    string    `json:"period_id"`
	Threshold   float64   `json:"threshold"`
	ReservedAt  time.Time `json:"reserved_at"`
}

// EncodeState serializes the reserve/dedup ledger. A zero-valued State (fresh
// install) and an already-populated State are both written as a document at the
// current SchemaVersion. The input is not modified.
func EncodeState(s State) ([]byte, error) {
	if s.Version != 0 && s.Version != SchemaVersion {
		return nil, fmt.Errorf("%w: cannot encode state version %d (want %d)", ErrStateVersion, s.Version, SchemaVersion)
	}
	doc := stateDoc{
		Version:      SchemaVersion,
		Rules:        make(map[string]ruleStateDoc, len(s.Rules)),
		Reservations: make(map[string]reservationDoc, len(s.Reservations)),
	}
	for id, rs := range s.Rules {
		// A rule entry without a watermark would silently disable the rollback
		// guard, so it is refused rather than written.
		if rs.Watermark.IsZero() {
			return nil, fmt.Errorf("%w: rule entry %q has no evaluation watermark", ErrInvalidState, id)
		}
		doc.Rules[id] = ruleStateDoc{
			Version:      rs.Version,
			Fingerprint:  rs.Fingerprint,
			LastPeriodID: rs.LastPeriodID,
			Watermark:    rs.Watermark,
		}
	}
	for id, res := range s.Reservations {
		doc.Reservations[id] = reservationDoc{
			ID:          res.ID,
			RuleID:      res.RuleID,
			RuleVersion: res.RuleVersion,
			PeriodID:    res.PeriodID,
			Threshold:   res.Threshold,
			ReservedAt:  res.ReservedAt,
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	return raw, nil
}

// DecodeState parses a document written by EncodeState.
//
// A document from another schema version is reported as ErrStateVersion so the
// caller can migrate or discard it; anything undecodable, unknown-fielded or
// trailing is ErrInvalidState. The result always has non-nil maps and carries
// the current SchemaVersion, so it can be passed straight to Evaluate.
func DecodeState(raw []byte) (State, error) {
	var doc stateDoc
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return State{}, fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return State{}, fmt.Errorf("%w: trailing data after the state document", ErrInvalidState)
	}
	if doc.Version != SchemaVersion {
		return State{}, fmt.Errorf("%w: document version %d (want %d)", ErrStateVersion, doc.Version, SchemaVersion)
	}

	state := State{
		Version:      SchemaVersion,
		Rules:        make(map[string]RuleState, len(doc.Rules)),
		Reservations: make(map[string]Reservation, len(doc.Reservations)),
	}
	for id, rs := range doc.Rules {
		// The watermark is mandatory: a document that lacks it cannot prove the
		// once-per-period contract survived a rollback or a restart, so it is
		// rejected instead of being silently trusted.
		if id == "" || rs.Version < 1 || rs.Fingerprint == "" {
			return State{}, fmt.Errorf("%w: rule entry %q is incomplete", ErrInvalidState, id)
		}
		if rs.Watermark.IsZero() {
			return State{}, fmt.Errorf("%w: rule entry %q has no evaluation watermark", ErrInvalidState, id)
		}
		state.Rules[id] = RuleState{
			Version:      rs.Version,
			Fingerprint:  rs.Fingerprint,
			LastPeriodID: rs.LastPeriodID,
			Watermark:    rs.Watermark,
		}
	}
	for key, res := range doc.Reservations {
		// The reservation key is the dedup identity: a document whose key and
		// payload disagree would silently break dedup, so it is rejected.
		if key == "" || res.ID != key || res.RuleID == "" || res.PeriodID == "" {
			return State{}, fmt.Errorf("%w: reservation %q does not match its key or is incomplete", ErrInvalidState, key)
		}
		if invalidThreshold(res.Threshold) {
			return State{}, fmt.Errorf("%w: reservation %q has threshold %v", ErrInvalidState, key, res.Threshold)
		}
		state.Reservations[key] = Reservation{
			ID:          res.ID,
			RuleID:      res.RuleID,
			RuleVersion: res.RuleVersion,
			PeriodID:    res.PeriodID,
			Threshold:   res.Threshold,
			ReservedAt:  res.ReservedAt,
		}
	}
	return state, nil
}
