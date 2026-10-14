package store

import (
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

func TestIngestionMaintainsEndpointIdentity(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	batch := harness.Batch{Events: []model.UsageEvent{
		{DedupKey: "one", SessionID: "s", BaseURL: "https://API.example.com:443/v1/", Provider: "account-a"},
		{DedupKey: "two", SessionID: "s", BaseURL: "https://api.example.com/v1", Provider: "account-b"},
		{DedupKey: "three", SessionID: "s", BaseURL: "https://api.example.com/tenant-b", Provider: "account-c"},
		{DedupKey: "four", SessionID: "s", BaseURL: "http://localhost:8080", Provider: "local"},
	}}
	for i := 0; i < 2; i++ {
		if err = s.Commit(ctx, model.Codex, "fixture", batch, nil); err != nil {
			t.Fatal(err)
		}
	}
	var raw, ids int
	if err = s.DB().QueryRow("SELECT count(*),count(DISTINCT provider_id) FROM provider_endpoints").Scan(&raw, &ids); err != nil {
		t.Fatal(err)
	}
	if raw != 3 || ids != 2 {
		t.Fatalf("endpoint identities: raw=%d stable=%d; want 3/2", raw, ids)
	}
	var events, names int
	if err = s.DB().QueryRow("SELECT count(*),count(DISTINCT resolved_provider) FROM events").Scan(&events, &names); err != nil {
		t.Fatal(err)
	}
	if events != 4 || names != 4 {
		t.Fatalf("changed request/pricing boundaries: %d/%d", events, names)
	}
}

func TestEndpointIdentityFailureRollsBackIngestion(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	if err = s.Exec(ctx, `CREATE TRIGGER fail_endpoint BEFORE INSERT ON provider_endpoints BEGIN SELECT RAISE(ABORT,'endpoint write failed'); END`); err != nil {
		t.Fatal(err)
	}
	err = s.Commit(ctx, model.Codex, "fixture", harness.Batch{Events: []model.UsageEvent{{DedupKey: "one", SessionID: "s", BaseURL: "https://api.example.com/v1"}}}, nil)
	if err == nil {
		t.Fatal("ingestion succeeded without endpoint identity")
	}
	for _, table := range []string{"events", "event_dimensions", "sessions", "cursors", "provider_endpoints"} {
		var n int
		if err = s.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s retained %d rows after failure", table, n)
		}
	}
}
