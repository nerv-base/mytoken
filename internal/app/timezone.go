package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/reporttime"
)

// StatsTimezoneKey is the settings key holding the reporting timezone every
// calendar period (budget periods, day buckets, session dates) is resolved in.
// It is a fixed persisted setting: the app writes it once, on first use, and a
// snapshot worker reads the same key without ever writing it.
//
// The value is either an IANA zone name (the fixed calendar) or absent. The
// legacy value "Local" is never written by this build and is not a fixed
// calendar: it is treated as unresolved (see EnsureStatsTimezone), because a
// stored "Local" would silently follow the operating system's zone on every
// later launch, which contradicts a persisted fixed reporting timezone.
const StatsTimezoneKey = "stats_timezone"

// systemZone is the legacy value meaning "follow the operating system's zone".
// It is no longer a persistable setting: it is reported as unresolved and
// needs an explicit IANA choice, and an explicit --timezone Local is a
// one-invocation override only.
const systemZone = "Local"

// ErrNoIANAZone reports that no stable IANA name could be discovered for the
// machine's zone. There is no silent fallback in that case: the calendar stays
// unconfigured until the user chooses a zone.
var ErrNoIANAZone = errors.New("mytoken: no stable IANA timezone could be identified for this machine")

// TimezoneState is the read side of the persisted reporting timezone. It
// distinguishes a fixed, user-chosen calendar from an unresolved one, so no
// caller has to guess whether a zone name is a real setting or the system zone
// standing in for a missing one. The type lives in reporttime so the GUI, the
// snapshot metadata and the budget gate describe the state identically.
type TimezoneState = reporttime.TimezoneState

// defaultStatsTimezone is the discovery seam. Tests replace it to simulate a
// machine where no stable IANA name exists (Windows with an unidentifiable
// local zone, a container with a bare /etc/localtime).
var defaultStatsTimezone = DefaultStatsTimezone

// DefaultStatsTimezone is the most stable IANA name for the machine's current
// zone: an explicit TZ, else the /etc/localtime target, else /etc/timezone,
// else the Windows TZ key, else the name Go reports. It returns "" when none
// of those yields a loadable IANA name, and never invents "Local" or an offset:
// an offset would silently drop daylight saving, and "Local" would silently
// follow the OS on a later launch.
func DefaultStatsTimezone() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		if name, ok := IANAZoneName(tz); ok {
			return name
		}
	}
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			name := strings.TrimPrefix(target[i+len("zoneinfo/"):], "posix/")
			if name != "" && name != "posixrules" {
				if n, ok := IANAZoneName(name); ok {
					return n
				}
			}
		}
	}
	if raw, err := os.ReadFile("/etc/timezone"); err == nil {
		if name := strings.TrimSpace(string(raw)); name != "" {
			if n, ok := IANAZoneName(name); ok {
				return n
			}
		}
	}
	// Windows has no zoneinfo: Go exports the Windows zone key through TZ when
	// the process environment carries it.
	if runtime.GOOS == "windows" {
		if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
			if n, ok := IANAZoneName(tz); ok {
				return n
			}
		}
	}
	if name := time.Now().Location().String(); name != "" {
		if n, ok := IANAZoneName(name); ok {
			return n
		}
	}
	return ""
}

// IANAZoneName reports whether name resolves to a zone with a stable name, and
// returns that canonical name. A bare offset ("" on some hosts, "+08:00")
// loses daylight saving, so it is refused, as is the legacy "Local".
func IANAZoneName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == systemZone {
		return "", false
	}
	loc, err := time.LoadLocation(name)
	if err != nil || loc == nil {
		return "", false
	}
	canonical := loc.String()
	if canonical == "" || canonical == systemZone ||
		strings.HasPrefix(canonical, "+") || strings.HasPrefix(canonical, "-") {
		return "", false
	}
	return canonical, true
}

// ParseStatsTimezone resolves a persisted or typed fixed zone name. Anything
// other than a loadable IANA name is refused: "Local" is refused on purpose,
// because persisting it would let the calendar silently follow the operating
// system. A one-invocation CLI override that wants the system zone uses
// ParseTimezoneOverride instead.
func ParseStatsTimezone(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("empty timezone")
	}
	if name == systemZone {
		return nil, fmt.Errorf("timezone %q follows the operating system and is not a fixed calendar; choose an IANA name", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil || loc == nil {
		return nil, fmt.Errorf("unknown timezone %q", name)
	}
	if n := loc.String(); n == "" || n == systemZone || strings.HasPrefix(n, "+") || strings.HasPrefix(n, "-") {
		return nil, fmt.Errorf("timezone %q has no stable name", name)
	}
	return loc, nil
}

// ParseTimezoneOverride resolves an explicit one-invocation override. It is the
// only path that accepts the literal "Local", which means the operating
// system's zone for this invocation only; it is never persisted.
func ParseTimezoneOverride(name string) (*time.Location, error) {
	if strings.TrimSpace(name) == systemZone {
		return time.Local, nil
	}
	return ParseStatsTimezone(name)
}

// EnsureStatsTimezone loads the persisted stats timezone, discovering and
// writing a stable IANA name on first use. It runs on normal app startup,
// before anything reads a calendar period.
//
// It never writes a value that cannot be used as a fixed calendar:
//
//   - no stable IANA name can be discovered: nothing is written, the calendar
//     stays unconfigured, and the GUI must ask for an explicit choice;
//   - the stored value is the legacy "Local" or cannot be loaded: it is left
//     exactly as it is (no destructive rewrite, no silent UTC or guessed
//     offset) and reported as needing an explicit choice.
func (a *App) EnsureStatsTimezone(ctx context.Context) error {
	if a == nil || a.Store == nil {
		return errors.New("mytoken: no store")
	}
	a.zoneMu.Lock()
	defer a.zoneMu.Unlock()
	stored, err := a.Store.Setting(ctx, StatsTimezoneKey)
	if err != nil {
		a.setUnconfiguredLocked(err.Error())
		return err
	}
	name := strings.TrimSpace(stored)
	if name == "" {
		name = defaultStatsTimezone()
		if name == "" {
			a.setUnconfiguredLocked(ErrNoIANAZone.Error())
			return ErrNoIANAZone
		}
		if err := a.Store.SetSetting(ctx, StatsTimezoneKey, name); err != nil {
			a.setUnconfiguredLocked(err.Error())
			return err
		}
		return a.setZoneLocked(name, false)
	}
	if _, err := ParseStatsTimezone(name); err != nil {
		a.setUnconfiguredLocked(fmt.Sprintf("%s: %v", StatsTimezoneKey, err))
		return fmt.Errorf("%s: %w", StatsTimezoneKey, err)
	}
	return a.setZoneLocked(name, true)
}

// Location is the fixed stats timezone, or nil while the calendar is
// unconfigured. A caller that needs calendar boundaries must treat nil as
// "ask the user for a timezone", never as "use the system zone": silently
// following the OS is exactly the bug this setting exists to prevent.
func (a *App) Location() *time.Location {
	if a == nil {
		return nil
	}
	a.zoneMu.Lock()
	defer a.zoneMu.Unlock()
	return a.zoneLoc
}

// Timezone reports the effective stats timezone and any problem with it. An
// unconfigured calendar reports an empty name with a non-nil error, so a caller
// cannot mistake it for a fixed zone.
func (a *App) Timezone(ctx context.Context) (string, error) {
	if a == nil {
		return "", errors.New("mytoken: no app")
	}
	a.zoneMu.Lock()
	defer a.zoneMu.Unlock()
	return a.zoneName, a.zoneErr
}

// TimezoneState reports the calendar's configuration state for the GUI and the
// read-only metadata. It is always safe to call and never writes.
func (a *App) TimezoneState() TimezoneState {
	if a == nil {
		return TimezoneState{NeedsSelection: true, Problem: "mytoken: no app"}
	}
	a.zoneMu.Lock()
	defer a.zoneMu.Unlock()
	st := TimezoneState{
		Configured:     a.zoneConfigured,
		Name:           a.zoneName,
		NeedsSelection: !a.zoneConfigured,
		SystemPreview:  a.zoneSystemPreview,
	}
	if a.zoneErr != nil {
		st.Problem = a.zoneErr.Error()
	}
	return st
}

// SetTimezone applies an explicit change: the name is validated as a fixed
// calendar, persisted and adopted, then the budget is asked to re-evaluate
// because moving the zone can move a period boundary. This is the only path
// that both persists and enables calendar calculations.
func (a *App) SetTimezone(ctx context.Context, name string) error {
	if a == nil || a.Store == nil {
		return errors.New("mytoken: no store")
	}
	loc, err := ParseStatsTimezone(name)
	if err != nil {
		return err
	}
	canonical := loc.String()
	if err := a.Store.SetSetting(ctx, StatsTimezoneKey, canonical); err != nil {
		return err
	}
	a.zoneMu.Lock()
	a.setZoneLockedLocked(canonical, loc, true)
	a.zoneMu.Unlock()
	if a.Budget != nil {
		a.Budget.Wake()
	}
	return nil
}

// setZoneLocked adopts a fixed calendar and reports any other stored-value
// problem. The caller holds zoneMu.
func (a *App) setZoneLocked(name string, clearProblem bool) error {
	loc, err := ParseStatsTimezone(name)
	if err != nil {
		a.setUnconfiguredLocked(fmt.Sprintf("%s: %v", StatsTimezoneKey, err))
		return err
	}
	a.setZoneLockedLocked(loc.String(), loc, clearProblem)
	return nil
}

// setZoneLockedLocked is setZoneLocked without re-parsing. The caller holds
// zoneMu.
func (a *App) setZoneLockedLocked(name string, loc *time.Location, clearProblem bool) {
	a.zoneName, a.zoneLoc, a.zoneConfigured = name, loc, true
	a.zoneSystemPreview = ""
	if clearProblem {
		a.zoneErr = nil
	}
}

// setUnconfiguredLocked leaves the calendar unresolved and keeps a preview of
// the machine's zone, clearly marked as unconfigured. Nothing is persisted.
// The caller holds zoneMu.
func (a *App) setUnconfiguredLocked(problem string) {
	a.zoneName, a.zoneLoc, a.zoneConfigured = "", nil, false
	a.zoneErr = errors.New(problem)
	a.zoneSystemPreview = systemZonePreview()
}

// systemZonePreview names the machine's current zone for an unconfigured
// calendar, or "" when even that has no stable IANA name. It is a preview
// only: it is never persisted and never used for calendar boundaries.
func systemZonePreview() string {
	if name := time.Now().Location().String(); name != "" && name != systemZone {
		return name
	}
	return ""
}
