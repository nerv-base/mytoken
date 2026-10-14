package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

func TestEventSignalsRoundTripAndReplay(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := model.UsageEvent{DedupKey: "signal", SessionID: "s", Tokens: model.Tokens{Input: 10}, RequestID: "req-exact", Boundary: model.BoundaryCompact, Bill: &model.Bill{Amount: 1.25, Unit: "credits"}}
	write := func() {
		t.Helper()
		if err := st.Commit(context.Background(), model.Codex, "source", harness.Batch{Events: []model.UsageEvent{e}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		t.Helper()
		var request, boundary, unit string
		var amount float64
		var cost sql.NullFloat64
		if err := st.DB().QueryRow(`SELECT request_id,boundary,bill_amount,bill_unit,log_cost FROM events`).Scan(&request, &boundary, &amount, &unit, &cost); err != nil {
			t.Fatal(err)
		}
		if request != "req-exact" || boundary != "compact" || amount != 1.25 || unit != "credits" || cost.Valid {
			t.Fatalf("signals %q %q %g %q cost=%v", request, boundary, amount, unit, cost)
		}
	}
	write()
	check()
	// Streaming replays with absent optional signals must not erase observations.
	e.RequestID = ""
	e.Boundary = ""
	e.Bill = nil
	e.Tokens.Output = 3
	write()
	check()
	if err := st.Exec(context.Background(), `UPDATE events SET resolved_provider='updated'`); err != nil {
		t.Fatal(err)
	}
	check()
	assertDailyRows(t, st)
}

func TestSignalsUpgradeExistingCompactDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	// The pre-signal compact generation: the dictionary-backed event_data is
	// keyed by (harness,dedup_key), the events view is the legacy read surface
	// and the signal columns do not exist yet. This generation predates the
	// instance dimension, so it carries no instance marker and no instance_id
	// columns; it also predates raw_project on sessions, which the migration
	// adds. Only event_data, event_dimensions and the view need to exist here,
	// because Open's base DDL and the migrations create everything else.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`
CREATE TABLE event_dimensions(id INTEGER PRIMARY KEY,harness TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL,project TEXT NOT NULL,model TEXT NOT NULL,provider TEXT NOT NULL,base_url TEXT NOT NULL,resolved_provider TEXT NOT NULL,attrib TEXT NOT NULL,raw_project TEXT NOT NULL,UNIQUE(harness,session_id,parent_id,project,model,provider,base_url,resolved_provider,attrib,raw_project));
CREATE TABLE event_data(harness TEXT NOT NULL,dedup_key TEXT NOT NULL,dimension_id INTEGER NOT NULL REFERENCES event_dimensions(id),timestamp INTEGER NOT NULL,input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL,log_cost REAL,cost REAL NOT NULL,priced INTEGER NOT NULL,PRIMARY KEY(harness,dedup_key));
INSERT INTO event_dimensions(harness,session_id,parent_id,project,model,provider,base_url,resolved_provider,attrib,raw_project) VALUES('codex','s','','/p','m','prov','https://api.example.com','resolved','log','/p');
INSERT INTO event_data(harness,dedup_key,dimension_id,timestamp,input,output,cache_read,cache_write,reasoning,log_cost,cost,priced) VALUES('codex','old',1,1700000000000000000,7,0,0,0,0,NULL,0,0);
CREATE VIEW events AS SELECT e.rowid AS rowid,e.harness,e.dedup_key,d.session_id,d.parent_id,d.project,e.timestamp,d.model,d.provider,d.base_url,e.input,e.output,e.cache_read,e.cache_write,e.reasoning,e.log_cost,d.resolved_provider,d.attrib,e.cost,e.priced,d.raw_project FROM event_data e JOIN event_dimensions d ON d.id=e.dimension_id;`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var input int
	var request, boundary, unit string
	var amount sql.NullFloat64
	if err = st.DB().QueryRow(`SELECT input,request_id,boundary,bill_amount,bill_unit FROM events`).Scan(&input, &request, &boundary, &amount, &unit); err != nil {
		t.Fatal(err)
	}
	if input != 7 || request != "" || boundary != "" || amount.Valid || unit != "" {
		t.Fatalf("migration changed signals: %d %q %q %v %q", input, request, boundary, amount, unit)
	}
	assertDailyRows(t, st)
}
