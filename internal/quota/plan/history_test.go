package plan

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type historyHarness struct {
	svc   *Service
	store *fakeStore
	now   time.Time
	bal   float64
}

func newHistoryHarness(t *testing.T, store Store) *historyHarness {
	t.Helper()
	h := &historyHarness{
		store: newFakeStore(),
		now:   time.Date(2026, 10, 1, 8, 0, 0, 0, time.Local),
		bal:   100,
	}
	if store != nil {
		h.store = store.(*fakeStore)
	}
	p := &fakeProvider{id: "trae-cn", detected: true}
	p.fetch = func(context.Context) (Snapshot, error) {
		return Snapshot{
			Provider:  "trae-cn",
			Plan:      "Pro",
			Windows:   []Window{{Name: "Credits", Balance: true, Remaining: h.bal, Unit: "credits"}},
			FetchedAt: h.now,
		}, nil
	}
	var err error
	h.svc, err = NewService(Options{
		Providers: []Provider{p},
		Store:     h.store,
		Now:       func() time.Time { return h.now },
		Tick:      time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *historyHarness) refresh(t *testing.T) Snapshot {
	t.Helper()
	h.svc.Refresh(context.Background())
	snap, ok := h.svc.Snapshot("trae-cn")
	if !ok {
		t.Fatal("no snapshot")
	}
	return snap
}

func (h *historyHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

func TestBalanceHistoryDerivesDaily(t *testing.T) {
	h := newHistoryHarness(t, nil)

	snap := h.refresh(t)
	if len(snap.Daily) != dailyDays {
		t.Fatalf("daily days = %d", len(snap.Daily))
	}
	today := snap.Daily[len(snap.Daily)-1]
	if !today.Known || today.Used != 0 || !today.Day.Equal(DayStart(h.now)) {
		t.Fatalf("today = %+v", today)
	}

	// Same day: 100 -> 90.
	h.advance(4 * time.Hour)
	h.bal = 90
	snap = h.refresh(t)
	if got := snap.Daily[len(snap.Daily)-1]; got.Used != 10 || !got.Known {
		t.Fatalf("day one = %+v", got)
	}

	// Next morning: 90 -> 85, the overnight drop belongs to day two.
	h.advance(20 * time.Hour)
	h.bal = 85
	snap = h.refresh(t)
	if got := snap.Daily[len(snap.Daily)-1]; got.Used != 5 || !got.Known {
		t.Fatalf("day two first = %+v", got)
	}
	if got := snap.Daily[len(snap.Daily)-2]; got.Used != 10 || !got.Known {
		t.Fatalf("day one final = %+v", got)
	}

	// A top-up is ignored, then another drop is still counted within the day.
	h.advance(4 * time.Hour)
	h.bal = 95
	snap = h.refresh(t)
	if got := snap.Daily[len(snap.Daily)-1]; got.Used != 5 {
		t.Fatalf("after top-up = %+v", got)
	}
	h.advance(6 * time.Hour)
	h.bal = 80
	snap = h.refresh(t)
	if got := snap.Daily[len(snap.Daily)-1]; got.Used != 10 {
		t.Fatalf("after drop = %+v", got)
	}

	// The ledger has one point per day: first 85, last 80.
	raw, ok := h.store.get(HistoryKey("trae-cn"))
	if !ok {
		t.Fatal("history not persisted")
	}
	var hist BalanceHistory
	if err := json.Unmarshal([]byte(raw), &hist); err != nil {
		t.Fatal(err)
	}
	pts := hist.Windows["Credits"]
	if len(pts) != 2 {
		t.Fatalf("points = %+v", pts)
	}
	last := pts[len(pts)-1]
	if last.First != 85 || last.Last != 80 || !last.Day.Equal(DayStart(h.now)) {
		t.Fatalf("last point = %+v", last)
	}
}

func TestBalanceHistoryMemoryWithoutStore(t *testing.T) {
	h := newHistoryHarness(t, nil)
	h.refresh(t)
	h.advance(24 * time.Hour)
	h.bal = 70
	snap := h.refresh(t)
	if got := snap.Daily[len(snap.Daily)-1]; got.Used != 30 {
		t.Fatalf("derived without store = %+v", got)
	}
}

func TestBalanceHistoryKeepsThirtyDays(t *testing.T) {
	h := newHistoryHarness(t, nil)
	start := h.now
	for i := 0; i < 35; i++ {
		h.now = start.AddDate(0, 0, i)
		h.bal = 100 - float64(i)
		h.refresh(t)
	}
	raw, ok := h.store.get(HistoryKey("trae-cn"))
	if !ok {
		t.Fatal("history not persisted")
	}
	var hist BalanceHistory
	if err := json.Unmarshal([]byte(raw), &hist); err != nil {
		t.Fatal(err)
	}
	if got := len(hist.Windows["Credits"]); got != balanceHistoryDays {
		t.Fatalf("points = %d, want %d", got, balanceHistoryDays)
	}
	snap, _ := h.svc.Snapshot("trae-cn")
	if got := len(snap.Daily); got != dailyDays {
		t.Fatalf("daily = %d", got)
	}
}

func TestProviderDailyWinsButHistoryRecorded(t *testing.T) {
	h := newHistoryHarness(t, nil)
	p := h.svc.providers[0].(*fakeProvider)
	day := DayStart(h.now)
	p.fetch = func(context.Context) (Snapshot, error) {
		return Snapshot{
			Provider:  "trae-cn",
			Windows:   []Window{{Name: "Credits", Balance: true, Remaining: 50}},
			Daily:     []DayUse{{Day: day, Used: 12, Known: true}},
			FetchedAt: h.now,
		}, nil
	}
	snap := h.refresh(t)
	if len(snap.Daily) != 1 || snap.Daily[0].Used != 12 {
		t.Fatalf("provider daily replaced = %+v", snap.Daily)
	}
	if raw, ok := h.store.get(HistoryKey("trae-cn")); !ok || raw == "" {
		t.Fatal("history not recorded")
	}
}

func TestCappedWindowHasNoHistory(t *testing.T) {
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.Local)
	p := &fakeProvider{id: "workbuddy", detected: true}
	p.fetch = func(context.Context) (Snapshot, error) {
		return Snapshot{
			Provider:  "workbuddy",
			Windows:   []Window{{Name: "Credits", UsedPercent: 25, Used: 25, Limit: 100, Unit: "credits"}},
			FetchedAt: now,
		}, nil
	}
	store := newFakeStore()
	s, err := NewService(Options{Providers: []Provider{p}, Store: store, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	s.Refresh(context.Background())
	snap, _ := s.Snapshot("workbuddy")
	if snap.Daily != nil {
		t.Fatalf("daily = %+v", snap.Daily)
	}
	if _, ok := store.get(HistoryKey("workbuddy")); ok {
		t.Fatal("history written for a capped window")
	}
}

func TestHistoryLoadAndNormalize(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)
	raw, _ := json.Marshal(BalanceHistory{Windows: map[string][]BalancePoint{
		"Credits": {
			{Day: now.AddDate(0, 0, -1), First: 90, Last: 80},
			{Day: now.AddDate(0, 0, -3), First: 100, Last: 90},
			{Day: now.AddDate(0, 0, -1), First: 85, Last: 70},
		},
	}})
	store := newFakeStore()
	store.vals[HistoryKey("trae-cn")] = string(raw)
	p := &fakeProvider{id: "trae-cn", detected: true}
	s, err := NewService(Options{Providers: []Provider{p}, Store: store, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	pts := s.history["trae-cn"].Windows["Credits"]
	if len(pts) != 2 {
		t.Fatalf("points = %+v", pts)
	}
	if got := pts[len(pts)-1]; got.First != 85 || got.Last != 70 {
		t.Fatalf("dedupe kept %+v", got)
	}
}
