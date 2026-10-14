package budget_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/budget"
	"github.com/zzstar101/mytoken/internal/reporttime"
	"github.com/zzstar101/mytoken/internal/store"
)

// testStore is an in-memory store: the settings table is all these tests need,
// and nothing touches the user's real database.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestConfigRoundTrips(t *testing.T) {
	cfg := budget.Config{Rules: []budget.Rule{
		globalRule(),
		{
			ID:         "proj",
			Version:    3,
			Scope:      budget.Scope{Kind: budget.ScopeProject, ID: "/Users/me/code/app"},
			Period:     reporttime.Week,
			Currency:   "USD",
			Limit:      250.5,
			Thresholds: []float64{50, 80, 100},
			Disabled:   true,
			Label:      "weekly app",
		},
	}}
	raw, err := budget.EncodeConfig(cfg)
	if err != nil {
		t.Fatalf("EncodeConfig: %v", err)
	}
	got, err := budget.DecodeConfig(raw)
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("round trip = %+v, want %+v", got, cfg)
	}
}

// TestDecodeConfigIsStrict pins the wire contract: a stored document that this
// version cannot read exactly is refused rather than guessed at, so a rule
// never silently changes scope, period or currency.
func TestDecodeConfigIsStrict(t *testing.T) {
	good := `{"version":1,"rules":[{"id":"r","version":1,"scope":"global","period":"day","currency":"USD","limit":10}]}`
	if _, err := budget.DecodeConfig([]byte(good)); err != nil {
		t.Fatalf("DecodeConfig(good): %v", err)
	}
	cases := map[string]string{
		"unknown field":  `{"version":1,"rules":[],"extra":true}`,
		"wrong version":  `{"version":2,"rules":[]}`,
		"no version":     `{"rules":[]}`,
		"trailing data":  good + `{}`,
		"duplicate ids":  `{"version":1,"rules":[{"id":"r","version":1,"scope":"global","period":"day","currency":"USD","limit":10},{"id":"r","version":1,"scope":"global","period":"week","currency":"USD","limit":10}]}`,
		"empty id":       `{"version":1,"rules":[{"id":"","version":1,"scope":"global","period":"day","currency":"USD","limit":10}]}`,
		"zero version":   `{"version":1,"rules":[{"id":"r","version":0,"scope":"global","period":"day","currency":"USD","limit":10}]}`,
		"lowercase ccy":  `{"version":1,"rules":[{"id":"r","version":1,"scope":"global","period":"day","currency":"usd","limit":10}]}`,
		"zero limit":     `{"version":1,"rules":[{"id":"r","version":1,"scope":"global","period":"day","currency":"USD","limit":0}]}`,
		"bad threshold":  `{"version":1,"rules":[{"id":"r","version":1,"scope":"global","period":"day","currency":"USD","limit":10,"thresholds":[0]}]}`,
		"global with id": `{"version":1,"rules":[{"id":"r","version":1,"scope":"global","scope_id":"x","period":"day","currency":"USD","limit":10}]}`,
		"unknown period": `{"version":1,"rules":[{"id":"r","version":1,"scope":"global","period":"quarter","currency":"USD","limit":10}]}`,
		"unknown scope":  `{"version":1,"rules":[{"id":"r","version":1,"scope":"team","scope_id":"x","period":"day","currency":"USD","limit":10}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := budget.DecodeConfig([]byte(raw)); !errors.Is(err, budget.ErrCorruptConfig) {
				t.Fatalf("DecodeConfig(%s) error = %v, want ErrCorruptConfig", name, err)
			}
		})
	}
}

func TestConfigPersistsThroughSettings(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	empty, err := budget.LoadConfig(ctx, st)
	if err != nil {
		t.Fatalf("LoadConfig(empty): %v", err)
	}
	if len(empty.Rules) != 0 {
		t.Fatalf("LoadConfig(empty) = %+v, want no rules", empty)
	}

	cfg := budget.Config{Rules: []budget.Rule{globalRule()}}
	if err := budget.SaveConfig(ctx, st, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err := budget.LoadConfig(ctx, st)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("LoadConfig = %+v, want %+v", got, cfg)
	}
	if r, ok := got.Find("rule-1"); !ok || r.ID != "rule-1" {
		t.Fatalf("Find(rule-1) = %+v, %v", r, ok)
	}

	// A corrupt value is reported, never repaired or overwritten silently.
	if err := st.SetSetting(ctx, budget.ConfigKey, "{not json"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if _, err := budget.LoadConfig(ctx, st); !errors.Is(err, budget.ErrCorruptConfig) {
		t.Fatalf("LoadConfig(corrupt) error = %v, want ErrCorruptConfig", err)
	}
	if raw, _ := st.Setting(ctx, budget.ConfigKey); !strings.HasPrefix(raw, "{not json") {
		t.Fatalf("corrupt value was rewritten: %q", raw)
	}
}

func TestEncodeConfigRefusesInvalidRules(t *testing.T) {
	bad := globalRule()
	bad.Currency = "usd"
	if _, err := budget.EncodeConfig(budget.Config{Rules: []budget.Rule{bad}}); !errors.Is(err, budget.ErrInvalidCurrency) {
		t.Fatalf("EncodeConfig(bad currency) error = %v, want ErrInvalidCurrency", err)
	}
	dup := budget.Config{Rules: []budget.Rule{globalRule(), globalRule()}}
	if _, err := budget.EncodeConfig(dup); !errors.Is(err, budget.ErrDuplicateRule) {
		t.Fatalf("EncodeConfig(duplicate) error = %v, want ErrDuplicateRule", err)
	}
}
