package budget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zzstar101/mytoken/internal/reporttime"
	"github.com/zzstar101/mytoken/internal/store"
)

// Store settings keys holding the persisted budget documents. The store
// already keeps key/value settings, so nothing about its schema changes.
const (
	// ConfigKey holds the encoded Config (the configured rules).
	ConfigKey = "budget_rules"
	// StateKey holds the encoded reserve/dedup State. It is written before a
	// notification is handed to the operating system, which is what makes
	// delivery at-most-once.
	StateKey = "budget_state"
)

// ConfigVersion is the schema version of the encoded Config document.
const ConfigVersion = 1

// ErrCorruptConfig marks a stored configuration that is present but cannot be
// decoded. It is never silently replaced: the value stays in the store and the
// caller decides.
var ErrCorruptConfig = errors.New("budget: corrupt configuration")

// Config is the persisted rule set. Rules are independent: each one has its own
// scope, period, currency and thresholds, and each one is evaluated on its own.
type Config struct {
	Rules []Rule
}

// Validate checks every rule and rejects duplicate rule ids. A configuration
// with one unusable rule is refused as a whole: a partially applied budget is
// worse than none.
func (c Config) Validate() error {
	seen := make(map[string]struct{}, len(c.Rules))
	for i, r := range c.Rules {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("rule %d: %w", i, err)
		}
		if _, dup := seen[r.ID]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateRule, r.ID)
		}
		seen[r.ID] = struct{}{}
	}
	return nil
}

// Find returns the rule with id and whether it is present.
func (c Config) Find(id string) (Rule, bool) {
	for _, r := range c.Rules {
		if r.ID == id {
			return r, true
		}
	}
	return Rule{}, false
}

// ruleDoc and configDoc are the wire format. Fields are snake_case and every
// field the decoder sees must be known (see DecodeConfig): a document written
// by a newer build fails loudly instead of losing the fields it cannot read.
type configDoc struct {
	Version int       `json:"version"`
	Rules   []ruleDoc `json:"rules"`
}

type ruleDoc struct {
	ID         string    `json:"id"`
	Version    int       `json:"version"`
	Scope      string    `json:"scope"`
	ScopeID    string    `json:"scope_id,omitempty"`
	Period     string    `json:"period"`
	Currency   string    `json:"currency"`
	Limit      float64   `json:"limit"`
	Thresholds []float64 `json:"thresholds,omitempty"`
	Disabled   bool      `json:"disabled,omitempty"`
	Label      string    `json:"label,omitempty"`
}

func (d ruleDoc) rule() (Rule, error) {
	kind, err := ParseScopeKind(d.Scope)
	if err != nil {
		return Rule{}, err
	}
	r := Rule{
		ID:         d.ID,
		Version:    d.Version,
		Scope:      Scope{Kind: kind, ID: d.ScopeID},
		Period:     reporttime.Kind(d.Period),
		Currency:   d.Currency,
		Limit:      d.Limit,
		Thresholds: d.Thresholds,
		Disabled:   d.Disabled,
		Label:      d.Label,
	}
	if err := r.Validate(); err != nil {
		return Rule{}, err
	}
	return r, nil
}

func (r Rule) doc() ruleDoc {
	return ruleDoc{
		ID:         r.ID,
		Version:    r.Version,
		Scope:      string(r.Scope.Kind),
		ScopeID:    r.Scope.ID,
		Period:     string(r.Period),
		Currency:   r.Currency,
		Limit:      r.Limit,
		Thresholds: r.Thresholds,
		Disabled:   r.Disabled,
		Label:      r.Label,
	}
}

// EncodeConfig encodes cfg as the persisted configuration document. The
// configuration is validated first: nothing invalid is ever written.
func EncodeConfig(cfg Config) ([]byte, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	doc := configDoc{Version: ConfigVersion, Rules: make([]ruleDoc, 0, len(cfg.Rules))}
	for _, r := range cfg.Rules {
		doc.Rules = append(doc.Rules, r.doc())
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptConfig, err)
	}
	return raw, nil
}

// DecodeConfig decodes a persisted configuration document strictly: unknown
// fields, a trailing value, an unsupported version or any invalid rule is an
// error. It is never lenient about a rule's identity, scope or currency.
func DecodeConfig(raw []byte) (Config, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc configDoc
	if err := dec.Decode(&doc); err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrCorruptConfig, err)
	}
	if dec.More() {
		return Config{}, fmt.Errorf("%w: trailing data", ErrCorruptConfig)
	}
	if doc.Version != ConfigVersion {
		return Config{}, fmt.Errorf("%w: unsupported version %d (want %d)", ErrCorruptConfig, doc.Version, ConfigVersion)
	}
	cfg := Config{Rules: make([]Rule, 0, len(doc.Rules))}
	for _, d := range doc.Rules {
		r, err := d.rule()
		if err != nil {
			return Config{}, fmt.Errorf("%w: %v", ErrCorruptConfig, err)
		}
		cfg.Rules = append(cfg.Rules, r)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrCorruptConfig, err)
	}
	return cfg, nil
}

// LoadConfig reads the persisted rules. A missing key is an empty
// configuration, not an error. A present but undecodable value returns
// ErrCorruptConfig and is left in the store untouched.
func LoadConfig(ctx context.Context, st *store.Store) (Config, error) {
	if st == nil {
		return Config{}, errors.New("budget: no store")
	}
	raw, err := st.Setting(ctx, ConfigKey)
	if err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(raw) == "" {
		return Config{}, nil
	}
	return DecodeConfig([]byte(raw))
}

// SaveConfig validates and persists cfg. The write goes through the store's
// existing SetSetting, which does not notify subscribers, so saving a rule
// cannot trigger another evaluation by itself.
func SaveConfig(ctx context.Context, st *store.Store, cfg Config) error {
	if st == nil {
		return errors.New("budget: no store")
	}
	raw, err := EncodeConfig(cfg)
	if err != nil {
		return err
	}
	return st.SetSetting(ctx, ConfigKey, string(raw))
}

// LoadState reads the persisted reserve/dedup ledger. A missing key is a fresh
// ledger, not an error. A present but undecodable value returns the decode
// error and is left in the store untouched.
func LoadState(ctx context.Context, st *store.Store) (State, error) {
	if st == nil {
		return State{}, errors.New("budget: no store")
	}
	raw, err := st.Setting(ctx, StateKey)
	if err != nil {
		return State{}, err
	}
	if strings.TrimSpace(raw) == "" {
		return State{}, nil
	}
	return DecodeState([]byte(raw))
}

// SaveState persists the reserve/dedup ledger. Callers must save before
// delivering: a delivery without a durable reservation is what would make a
// duplicate possible after a restart.
func SaveState(ctx context.Context, st *store.Store, s State) error {
	if st == nil {
		return errors.New("budget: no store")
	}
	raw, err := EncodeState(s)
	if err != nil {
		return err
	}
	return st.SetSetting(ctx, StateKey, string(raw))
}
