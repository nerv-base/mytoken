package plan

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWindowNormalize(t *testing.T) {
	balance := Window{Name: "Credits", Balance: true, UsedPercent: 42, Used: 10, Limit: 100, Remaining: 7}
	balance.normalize()
	if balance.UsedPercent != 0 || balance.Remaining != 7 {
		t.Fatalf("balance window = %+v", balance)
	}

	capped := Window{Name: "week", UsedPercent: 10, Used: 10, Limit: 100}
	capped.normalize()
	if capped.Remaining != 90 {
		t.Fatalf("capped remaining = %v", capped.Remaining)
	}

	over := Window{Name: "week", UsedPercent: 120, Used: 120, Limit: 100}
	over.normalize()
	if over.Remaining != 0 {
		t.Fatalf("over remaining = %v", over.Remaining)
	}

	percentOnly := Window{Name: "5h", UsedPercent: 12}
	percentOnly.normalize()
	if percentOnly.Remaining != 0 || percentOnly.UsedPercent != 12 {
		t.Fatalf("percent-only window = %+v", percentOnly)
	}
}

func TestSnapshotValidateBalanceAndDaily(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	day := func(offset int) time.Time { return DayStart(now).AddDate(0, 0, offset) }

	var days []DayUse
	for i := -15; i <= 0; i++ {
		days = append(days, DayUse{Day: day(i), Used: float64(i + 100), Known: true})
	}
	// A duplicate day and an unknown day with a stale amount.
	days = append(days, DayUse{Day: day(0), Used: 99, Known: true})
	days = append(days, DayUse{Day: day(-20), Used: 1, Known: false})

	s := Snapshot{
		Provider: "trae-cn",
		Windows: []Window{{
			Name: "Credits", Balance: true, UsedPercent: 33,
			Used: 10, Limit: 100, Remaining: 90,
		}},
		Daily: days,
	}
	if err := s.validate(now); err != nil {
		t.Fatal(err)
	}
	if len(s.Daily) != 14 {
		t.Fatalf("daily days = %d, want 14", len(s.Daily))
	}
	for i, d := range s.Daily {
		if i > 0 && !d.Day.After(s.Daily[i-1].Day) {
			t.Fatalf("daily not ascending at %d: %v then %v", i, s.Daily[i-1].Day, d.Day)
		}
		if !d.Known {
			t.Fatalf("daily[%d] lost Known", i)
		}
	}
	if got := s.Daily[len(s.Daily)-1]; got.Day != day(0) || got.Used != 99 {
		t.Fatalf("today = %+v, want duplicate replaced by 99", got)
	}
	if s.Windows[0].UsedPercent != 0 || s.Windows[0].Remaining != 90 {
		t.Fatalf("balance normalized = %+v", s.Windows[0])
	}
	if s.Source != SourceAPI || !s.FetchedAt.Equal(now.UTC()) {
		t.Fatalf("snapshot defaults = %+v", s)
	}
}

func TestSnapshotCloneCopiesDaily(t *testing.T) {
	s := Snapshot{
		Windows: []Window{{Name: "Credits", Balance: true, Remaining: 5}},
		Daily:   []DayUse{{Day: DayStart(time.Now()), Used: 1, Known: true}},
	}
	c := s.Clone()
	c.Windows[0].Remaining = 0
	c.Daily[0].Used = 0
	if s.Windows[0].Remaining != 5 || s.Daily[0].Used != 1 {
		t.Fatalf("clone shares storage: %+v %+v", s.Windows, s.Daily)
	}
}

func TestSnapshotRejectsNonAPISource(t *testing.T) {
	s := Snapshot{
		Provider: "codex",
		Windows:  []Window{{Name: "5h", UsedPercent: 10}},
		Source:   "local-log",
	}
	if err := s.validate(time.Now()); err == nil {
		t.Fatal("validate accepted a snapshot whose source is a local log")
	}
	s.Source = ""
	if err := s.validate(time.Now()); err != nil {
		t.Fatalf("validate with an unset source: %v", err)
	}
	if s.Source != SourceAPI {
		t.Fatalf("unset source became %q, want %q", s.Source, SourceAPI)
	}
}

func TestWindowJSONBackwardsCompatible(t *testing.T) {
	old := []byte(`{"name":"week","used_percent":10,"used":1,"limit":10,"unit":"requests","resets_at":"2026-10-20T00:00:00Z"}`)
	var w Window
	if err := json.Unmarshal(old, &w); err != nil {
		t.Fatal(err)
	}
	if w.Balance || w.Remaining != 0 || !w.ExpiresAt.IsZero() {
		t.Fatalf("old window decoded as %+v", w)
	}

	empty, err := json.Marshal(Window{Name: "week", UsedPercent: 10})
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{"name":"week","used_percent":10}` {
		t.Fatalf("new zero fields leaked into JSON: %s", empty)
	}

	full, err := json.Marshal(Window{
		Name: "Credits", Balance: true, Remaining: 7,
		ExpiresAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	var round Window
	if err := json.Unmarshal(full, &round); err != nil {
		t.Fatal(err)
	}
	if !round.Balance || round.Remaining != 7 || !round.ExpiresAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("round trip = %+v", round)
	}
}

func TestSnapshotJSONWithoutDaily(t *testing.T) {
	raw := []byte(`{"provider":"a","windows":[{"name":"week","used_percent":1}],"fetched_at":"2026-10-19T00:00:00Z"}`)
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Daily != nil {
		t.Fatalf("daily = %+v, want nil", s.Daily)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"provider":"a","windows":[{"name":"week","used_percent":1}],"fetched_at":"2026-10-19T00:00:00Z"}`; string(out) != want {
		t.Fatalf("json = %s, want %s", out, want)
	}
}

func TestDayStart(t *testing.T) {
	got := DayStart(time.Date(2026, 10, 19, 23, 59, 59, 0, time.Local))
	want := time.Date(2026, 10, 19, 0, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("DayStart = %v, want %v", got, want)
	}
}
