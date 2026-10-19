package codex

import (
	"context"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/quota/plan"
)

type fakeStore struct {
	vals map[string]string
}

func (s *fakeStore) Setting(_ context.Context, key string) (string, error) {
	return s.vals[key], nil
}

func (s *fakeStore) SetSetting(_ context.Context, key, value string) error {
	s.vals[key] = value
	return nil
}

func TestFetchConvertsPersistedReading(t *testing.T) {
	store := &fakeStore{vals: map[string]string{}}
	used := 9.0
	remaining := 91.0
	minutes := int64(10080)
	resets := time.Date(2026, 9, 30, 2, 12, 17, 0, time.UTC)
	observed := time.Date(2026, 9, 23, 8, 3, 54, 0, time.UTC)
	planType := "self_serve_business_prolite"
	rep := &quota.Report{
		ObservedAt: &observed,
		PlanType:   &planType,
		Primary: &quota.Window{
			Kind:             quota.WindowPrimary,
			Status:           quota.StatusOK,
			UsedPercent:      &used,
			RemainingPercent: &remaining,
			WindowMinutes:    &minutes,
			ResetsAt:         &resets,
		},
		Status: quota.StatusOK,
	}
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	if err := quota.PublishLatest(context.Background(), store, quota.DefaultInstance, rep, now); err != nil {
		t.Fatal(err)
	}

	p := New(Options{Store: store, Now: func() time.Time { return now }})
	if !p.Local() || p.ID() != ProviderID {
		t.Fatalf("provider = %+v", p)
	}
	if !p.Detect(context.Background()) {
		t.Fatal("not detected")
	}
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Plan != planType || snap.Source != plan.SourceLocalLog || !snap.FetchedAt.Equal(observed) {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap.Windows) != 1 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	w := snap.Windows[0]
	if w.Name != "week" || w.UsedPercent != 9 || !w.ResetsAt.Equal(resets) {
		t.Fatalf("window = %+v", w)
	}
}

func TestFetchDerivesUsedFromRemaining(t *testing.T) {
	store := &fakeStore{vals: map[string]string{}}
	remaining := 75.0
	minutes := int64(300)
	rep := &quota.Report{
		Primary: &quota.Window{Kind: quota.WindowPrimary, RemainingPercent: &remaining, WindowMinutes: &minutes},
		Status:  quota.StatusOK,
	}
	if err := quota.PublishLatest(context.Background(), store, "default", rep, time.Now()); err != nil {
		t.Fatal(err)
	}
	snap, err := New(Options{Store: store}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Windows[0].Name != "5h" || snap.Windows[0].UsedPercent != 25 {
		t.Fatalf("window = %+v", snap.Windows[0])
	}
}

func TestMissingReading(t *testing.T) {
	p := New(Options{Store: &fakeStore{vals: map[string]string{}}})
	if p.Detect(context.Background()) {
		t.Fatal("detected without a reading")
	}
	if _, err := p.Fetch(context.Background()); !plan.IsReason(err, plan.ReasonNoData) {
		t.Fatalf("err = %v", err)
	}
	if p := New(Options{}); p.Detect(context.Background()) {
		t.Fatal("nil store detected")
	}
}

func TestWindowName(t *testing.T) {
	cases := []struct {
		kind quota.WindowKind
		mins *int64
		want string
	}{
		{quota.WindowPrimary, ptr(int64(300)), "5h"},
		{quota.WindowPrimary, ptr(int64(10080)), "week"},
		{quota.WindowPrimary, ptr(int64(1440)), "day"},
		{quota.WindowPrimary, ptr(int64(2880)), "2d"},
		{quota.WindowPrimary, ptr(int64(90)), "90m"},
		{quota.WindowPrimary, nil, "primary"},
		{quota.WindowSecondary, nil, "secondary"},
	}
	for _, tc := range cases {
		if got := windowName(tc.kind, tc.mins); got != tc.want {
			t.Fatalf("%s/%v = %q, want %q", tc.kind, tc.mins, got, tc.want)
		}
	}
}

func ptr[T any](v T) *T { return &v }
