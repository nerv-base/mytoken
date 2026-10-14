package reporttime

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"
)

// TestBoundariesAgreeWithBruteForceOracle compares localBoundary against an
// independent bisection oracle over the whole host zone database: every zone in
// zone.tab, every day from 1970 through 2035, restricted to dates whose
// neighbours do not share the same UTC offset (the only dates where a midnight
// can be missing or repeated).
//
// The oracle bisects the predicate "the wall clock has reached this date's
// midnight" to nanosecond precision instead of reasoning about transitions.
// That predicate is not monotone where a fall-back repeats local midnight, so
// the oracle can land on the later of two crossings: such a disagreement is
// counted as genuineEarlier (this implementation returned an earlier instant
// that is itself a valid crossing, which is the contract) and is not a defect.
// A result that is later than the oracle, or that is not a crossing at all, is
// a defect and fails the test.
//
// A date that never existed has an empty period by design (start == next
// start); those are counted as skippedDate. The test needs the host zone
// database, which is where Go's loader gets zones without an explicit tzdata
// dependency; run with -short to skip it.
func TestBoundariesAgreeWithBruteForceOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("zone-database sweep skipped in short mode")
	}
	names := zoneTableNames(t)
	if len(names) == 0 {
		t.Skip("no zone database on this host")
	}

	oracle := func(y int, m time.Month, d int, loc *time.Location) (time.Time, bool) {
		target := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		lo := target.Add(-72 * time.Hour)
		hi := target.Add(72 * time.Hour)
		if !wallClock(lo, loc).Before(target) || wallClock(hi, loc).Before(target) {
			return time.Time{}, false
		}
		for hi.Sub(lo) > time.Nanosecond {
			mid := lo.Add(hi.Sub(lo) / 2)
			if wallClock(mid, loc).Before(target) {
				lo = mid
			} else {
				hi = mid
			}
		}
		if !wallClock(hi.Add(-time.Nanosecond), loc).Before(target) || wallClock(hi, loc).Before(target) {
			return time.Time{}, false // the oracle's own predicate was not a clean crossing
		}
		return hi.In(loc), true
	}
	crossing := func(t, target time.Time, loc *time.Location) bool {
		return !wallClock(t, loc).Before(target) && wallClock(t.Add(-time.Nanosecond), loc).Before(target)
	}

	var zones, compared, genuineEarlier, notMinimal, gotLater, notCrossing, skippedDate, emptyOther int
	examples := []string{}
	for _, name := range names {
		loc, err := time.LoadLocation(name)
		if err != nil {
			continue
		}
		zones++
		for y := 1970; y <= 2035; y++ {
			for mo := time.January; mo <= time.December; mo++ {
				last := time.Date(y, mo+1, 0, 0, 0, 0, 0, time.UTC).Day()
				for d := 1; d <= last; d++ {
					mid := time.Date(y, mo, d, 0, 0, 0, 0, loc)
					prev := time.Date(y, mo, d-1, 0, 0, 0, 0, loc)
					next := time.Date(y, mo, d+1, 0, 0, 0, 0, loc)
					if offsetAt(mid) == offsetAt(prev) && offsetAt(mid) == offsetAt(next) {
						continue
					}
					for k := -1; k <= 1; k++ {
						ty, tm, td := calendarDate(y, mo, d+k)
						got := localBoundary(ty, tm, td, loc)
						want, ok := oracle(ty, tm, td, loc)
						if !ok {
							continue
						}
						compared++
						if !got.Equal(want) {
							target := time.Date(ty, tm, td, 0, 0, 0, 0, time.UTC)
							switch {
							case got.Before(want) && crossing(got, target, loc):
								// The oracle can land on the later of two crossings
								// when a fall-back repeats midnight. Prove that got
								// really is the earliest by scanning back 3 hours at
								// one-second granularity: post-1970 offsets are whole
								// seconds, so a crossing cannot be skipped.
								minimal := true
								for s := time.Second; s <= 3*time.Hour; s += time.Second {
									if !wallClock(got.Add(-s), loc).Before(target) {
										minimal = false
										break
									}
								}
								if minimal {
									genuineEarlier++
								} else {
									notMinimal++
								}
							case got.After(want):
								gotLater++
							default:
								notCrossing++
							}
							if len(examples) < 10 {
								examples = append(examples, name+" "+target.Format("2006-01-02")+
									" got="+got.Format(time.RFC3339)+" oracle="+want.Format(time.RFC3339))
							}
						}
						ny, nm, nd := calendarDate(ty, tm, td+1)
						nextBoundary := localBoundary(ny, nm, nd, loc)
						if !got.Before(nextBoundary) {
							if got.Equal(nextBoundary) {
								skippedDate++
							} else {
								emptyOther++
							}
						}
					}
				}
			}
		}
	}

	t.Logf("zones=%d compared=%d oracleArtifactEarlierVerifiedEarliest=%d notEarliest=%d gotLater=%d notCrossing=%d skippedDate=%d emptyOther=%d",
		zones, compared, genuineEarlier, notMinimal, gotLater, notCrossing, skippedDate, emptyOther)
	for _, e := range examples {
		t.Logf("oracle disagrees (earlier crossing returned): %s", e)
	}
	if gotLater > 0 || notCrossing > 0 || notMinimal > 0 || emptyOther > 0 {
		t.Errorf("boundary defects: laterThanOracle=%d notACrossing=%d notEarliest=%d nonPositivePeriod=%d",
			gotLater, notCrossing, notMinimal, emptyOther)
	}
	if compared == 0 {
		t.Error("sweep compared no boundaries; zone database or clock assumptions changed")
	}
}

// zoneTableNames reads the canonical zone names from the host's zone.tab.
func zoneTableNames(t *testing.T) []string {
	t.Helper()
	for _, path := range []string{"/usr/share/zoneinfo/zone.tab", "/var/db/timezone/zoneinfo/zone.tab"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		var names []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if fields := strings.Fields(line); len(fields) >= 3 {
				names = append(names, fields[2])
			}
		}
		f.Close()
		if len(names) > 0 {
			return names
		}
	}
	return nil
}
