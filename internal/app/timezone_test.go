package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // pin the IANA database for a machine without zoneinfo

	"github.com/zzstar101/mytoken/internal/store"
)

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", path, err)
	}
	return st
}

// TestStatsTimezoneIsPersistedOnFirstUse walks the contract the snapshot
// worker depends on: the first normal app startup stores an exact
// 'stats_timezone' value that a later startup reuses unchanged.
func TestStatsTimezoneIsPersistedOnFirstUse(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mytoken.db")

	st := openStore(t, path)
	a := &App{Store: st}
	if err := a.EnsureStatsTimezone(ctx); err != nil {
		t.Fatalf("EnsureStatsTimezone: %v", err)
	}
	name, err := a.Timezone(ctx)
	if err != nil {
		t.Fatalf("Timezone: %v", err)
	}
	if name == "" {
		t.Fatal("Timezone = empty, want a usable default")
	}
	if _, err := time.LoadLocation(name); err != nil {
		t.Fatalf("default %q is not loadable: %v", name, err)
	}
	if a.Location() == nil {
		t.Fatal("Location() = nil, want the system zone at worst")
	}
	stored, err := st.Setting(ctx, StatsTimezoneKey)
	if err != nil {
		t.Fatalf("Setting(%s): %v", StatsTimezoneKey, err)
	}
	if stored != name {
		t.Fatalf("stored %q, want %q", stored, name)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A restart over the same database keeps the stored zone: the value is
	// persisted, not recomputed from the machine's clock settings.
	st2 := openStore(t, path)
	defer st2.Close()
	a2 := &App{Store: st2}
	if err := a2.EnsureStatsTimezone(ctx); err != nil {
		t.Fatalf("EnsureStatsTimezone (second run): %v", err)
	}
	again, err := a2.Timezone(ctx)
	if err != nil {
		t.Fatalf("Timezone (second run): %v", err)
	}
	if again != name {
		t.Fatalf("second startup uses %q, want the stored %q", again, name)
	}
}

func TestSetTimezoneIsExplicitAndValidated(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "mytoken.db"))
	defer st.Close()
	a := &App{Store: st}
	if err := a.EnsureStatsTimezone(ctx); err != nil {
		t.Fatalf("EnsureStatsTimezone: %v", err)
	}

	if err := a.SetTimezone(ctx, "America/New_York"); err != nil {
		t.Fatalf("SetTimezone: %v", err)
	}
	got, err := a.Timezone(ctx)
	if err != nil || got != "America/New_York" {
		t.Fatalf("Timezone = %q, %v; want America/New_York", got, err)
	}
	if stored, _ := st.Setting(ctx, StatsTimezoneKey); stored != "America/New_York" {
		t.Fatalf("stored %q, want America/New_York", stored)
	}
	// A named zone keeps its DST rules: January and July differ.
	loc := a.Location()
	_, jan := time.Date(2026, 1, 15, 12, 0, 0, 0, loc).Zone()
	_, jul := time.Date(2026, 7, 15, 12, 0, 0, 0, loc).Zone()
	if jan == jul {
		t.Fatalf("America/New_York has no DST here (offsets %d, %d)", jan, jul)
	}

	// A name that cannot be resolved is refused and changes nothing.
	for _, bad := range []string{"", "  ", "Mars/Olympus", "+08:00", "-05:00"} {
		if err := a.SetTimezone(ctx, bad); err == nil {
			t.Fatalf("SetTimezone(%q) = nil, want an error", bad)
		}
	}
	if stored, _ := st.Setting(ctx, StatsTimezoneKey); stored != "America/New_York" {
		t.Fatalf("a rejected value was written: %q", stored)
	}
	if got, _ := a.Timezone(ctx); got != "America/New_York" {
		t.Fatalf("Timezone = %q after rejections, want the last good zone", got)
	}
}

// TestEnsureStatsTimezoneKeepsUnreadableValue: a stored value this build cannot
// parse is reported, left in place and never silently replaced by the process
// zone. There is no fixed calendar in that state, so no caller may be handed
// one.
func TestEnsureStatsTimezoneKeepsUnreadableValue(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "mytoken.db"))
	defer st.Close()
	if err := st.SetSetting(ctx, StatsTimezoneKey, "Not/AZone"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	a := &App{Store: st}
	err := a.EnsureStatsTimezone(ctx)
	if err == nil {
		t.Fatal("EnsureStatsTimezone = nil, want the stored value reported")
	}
	if !strings.Contains(err.Error(), StatsTimezoneKey) {
		t.Fatalf("error %q does not name the key", err)
	}
	name, err := a.Timezone(ctx)
	if err == nil {
		t.Fatal("Timezone error = nil, want the stored value reported")
	}
	if name != "" {
		t.Fatalf("Timezone = %q, want no name: the value is unusable, not a fallback", name)
	}
	if !strings.Contains(err.Error(), "Not/AZone") {
		t.Fatalf("error %q does not name the stored value", err)
	}
	if a.Location() != nil {
		t.Fatalf("Location() = %v, want nil: no fixed calendar is configured", a.Location())
	}
	stt := a.TimezoneState()
	if stt.Configured || !stt.NeedsSelection {
		t.Fatalf("TimezoneState = %+v, want unconfigured and needing a selection", stt)
	}
	if !strings.Contains(stt.Problem, "Not/AZone") {
		t.Fatalf("TimezoneState.Problem = %q, want the stored value named", stt.Problem)
	}
	if stored, _ := st.Setting(ctx, StatsTimezoneKey); stored != "Not/AZone" {
		t.Fatalf("stored value was rewritten to %q", stored)
	}
}

// TestEnsureStatsTimezoneWithoutStableZone: when no stable IANA name can be
// identified, nothing is persisted and the state asks for an explicit choice.
// Writing "Local" here is the bug this guards: it would follow the OS on the
// next launch and contradict a fixed calendar.
func TestEnsureStatsTimezoneWithoutStableZone(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "mytoken.db"))
	defer st.Close()
	prev := defaultStatsTimezone
	defaultStatsTimezone = func() string { return "" }
	defer func() { defaultStatsTimezone = prev }()

	a := &App{Store: st}
	if err := a.EnsureStatsTimezone(ctx); !errors.Is(err, ErrNoIANAZone) {
		t.Fatalf("EnsureStatsTimezone = %v, want ErrNoIANAZone", err)
	}
	if stored, _ := st.Setting(ctx, StatsTimezoneKey); stored != "" {
		t.Fatalf("stored %q, want nothing written without a stable zone", stored)
	}
	if a.Location() != nil {
		t.Fatalf("Location() = %v, want nil", a.Location())
	}
	stt := a.TimezoneState()
	if stt.Configured || !stt.NeedsSelection || stt.Name != "" {
		t.Fatalf("TimezoneState = %+v, want unconfigured/needs selection", stt)
	}
	if stt.Problem == "" {
		t.Fatal("TimezoneState.Problem = empty, want an actionable reason")
	}
	// The setter still works once the user chooses explicitly, and only then.
	if err := a.SetTimezone(ctx, "Asia/Shanghai"); err != nil {
		t.Fatalf("SetTimezone: %v", err)
	}
	if stored, _ := st.Setting(ctx, StatsTimezoneKey); stored != "Asia/Shanghai" {
		t.Fatalf("stored %q after an explicit choice", stored)
	}
	if !a.TimezoneState().Configured {
		t.Fatal("TimezoneState is still unconfigured after an explicit choice")
	}
}

// TestLegacyLocalIsUnresolvedNotTheProcessZone: a stored "Local" is a legacy
// value that follows the OS. It must be reported as needing a choice rather
// than resolved to whatever time.Local happens to be now, and it must not be
// rewritten.
func TestLegacyLocalIsUnresolvedNotTheProcessZone(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "mytoken.db"))
	defer st.Close()
	if err := st.SetSetting(ctx, StatsTimezoneKey, systemZone); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	// The OS zone moves between launches; the stored value must not move with
	// it.
	prevLocal := time.Local
	time.Local = time.FixedZone("Shifted/Zone", 3*3600)
	defer func() { time.Local = prevLocal }()

	a := &App{Store: st}
	if err := a.EnsureStatsTimezone(ctx); err == nil {
		t.Fatal("EnsureStatsTimezone = nil, want the legacy value reported")
	}
	name, err := a.Timezone(ctx)
	if err == nil {
		t.Fatal("Timezone error = nil, want the legacy value reported")
	}
	if name != "" {
		t.Fatalf("Timezone = %q, want no name: Local is not a fixed calendar", name)
	}
	if a.Location() != nil {
		t.Fatalf("Location() = %v, want nil", a.Location())
	}
	stt := a.TimezoneState()
	if stt.Configured || !stt.NeedsSelection {
		t.Fatalf("TimezoneState = %+v, want unresolved", stt)
	}
	if stt.Name == time.Local.String() || stt.SystemPreview != time.Local.String() {
		t.Fatalf("system zone %q was adopted: %+v", time.Local.String(), stt)
	}
	if stored, _ := st.Setting(ctx, StatsTimezoneKey); stored != systemZone {
		t.Fatalf("stored value was rewritten to %q", stored)
	}
}

func TestDefaultStatsTimezoneIsUsable(t *testing.T) {
	name := DefaultStatsTimezone()
	if name == "" {
		t.Fatal("DefaultStatsTimezone = empty")
	}
	loc, err := ParseStatsTimezone(name)
	if err != nil {
		t.Fatalf("ParseStatsTimezone(%q): %v", name, err)
	}
	if loc == nil {
		t.Fatalf("ParseStatsTimezone(%q) = nil", name)
	}
}

func TestParseStatsTimezone(t *testing.T) {
	ok := map[string]string{"UTC": "UTC", "Asia/Shanghai": "Asia/Shanghai", "Etc/UTC": "Etc/UTC"}
	for in, want := range ok {
		loc, err := ParseStatsTimezone(in)
		if err != nil {
			t.Fatalf("ParseStatsTimezone(%q): %v", in, err)
		}
		if loc.String() != want {
			t.Fatalf("ParseStatsTimezone(%q) = %q, want %q", in, loc, want)
		}
	}
	for _, bad := range []string{"", "  ", "Mars/Olympus", "+08:00", "-0500", "GMT+8", systemZone} {
		if _, err := ParseStatsTimezone(bad); err == nil {
			t.Fatalf("ParseStatsTimezone(%q) = nil error, want a rejection", bad)
		}
	}
	// Only the explicit one-invocation override accepts a zone that follows the
	// OS, and it is never the persisted setting.
	loc, err := ParseTimezoneOverride(systemZone)
	if err != nil {
		t.Fatalf("ParseTimezoneOverride(%q): %v", systemZone, err)
	}
	if loc != time.Local {
		t.Fatalf("ParseTimezoneOverride(%q) = %v, want the process zone", systemZone, loc)
	}
	if _, err := ParseTimezoneOverride("Mars/Olympus"); err == nil {
		t.Fatal("ParseTimezoneOverride accepted an unknown zone")
	}
}
