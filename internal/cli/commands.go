package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"time"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/store"
)

type doctorRoot struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

type doctorHarness struct {
	Harness    string       `json:"harness"`
	Enabled    bool         `json:"enabled"`
	Detected   bool         `json:"detected"`
	Roots      []doctorRoot `json:"roots"`
	Problem    string       `json:"problem,omitempty"`
	Events     int64        `json:"events"`
	CursorRows int64        `json:"cursorRows"`
}

type doctorSchema struct {
	State   string   `json:"state"`
	Missing []string `json:"missing,omitempty"`
	Detail  string   `json:"detail,omitempty"`
}

type doctorReport struct {
	DataDir          string          `json:"dataDir"`
	DBPath           string          `json:"dbPath"`
	DBBytes          int64           `json:"dbBytes"`
	DBUnavailable    bool            `json:"dbUnavailable"`
	DBReason         string          `json:"dbReason,omitempty"`
	DBDetail         string          `json:"dbDetail,omitempty"`
	PricingSource    string          `json:"pricingSource"`
	PricingFetchedAt *time.Time      `json:"pricingFetchedAt,omitempty"`
	PricingAgeSecs   *int64          `json:"pricingAgeSeconds,omitempty"`
	Schema           doctorSchema    `json:"schema"`
	Harnesses        []doctorHarness `json:"harnesses"`
}

// runDoctor reports the index and the harness sources without writing anything.
// The index is opened read-only: doctor never creates, migrates or reprices it.
func runDoctor(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(errout)
	js := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errout, "usage: mytoken doctor [--json]")
		return 2
	}
	dir, err := paths.ResolveDataDir()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	db, err := paths.ResolveDBPath()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	report := doctorReport{
		DataDir: dir, DBPath: db, PricingSource: "embedded",
		Schema: doctorSchema{State: string(store.SchemaUnreadable)},
	}
	if info, e := os.Stat(db); e == nil {
		report.DBBytes = info.Size()
	} else if !os.IsNotExist(e) {
		fmt.Fprintln(errout, e)
		return 1
	}
	ro, roErr := store.OpenReadOnly(ctx, db)
	if roErr != nil {
		report.DBUnavailable = true
		report.DBDetail = roErr.Error()
		reason, _ := store.ReadOnlyReasonOf(roErr)
		if reason == "" {
			reason = store.ReasonUnavailable
		}
		report.DBReason = string(reason)
		if rerr, ok := roErr.(*store.ReadOnlyError); ok && rerr.Schema.State != "" {
			report.Schema = doctorSchema{State: string(rerr.Schema.State), Missing: rerr.Schema.Missing, Detail: rerr.Schema.Detail}
		} else {
			report.Schema.Detail = roErr.Error()
		}
	} else {
		defer ro.Close()
		sr := ro.ReadOnlySchema()
		report.Schema = doctorSchema{State: string(sr.State), Missing: sr.Missing, Detail: sr.Detail}
	}
	if raw, e := os.ReadFile(filepath.Join(dir, "prices.json")); e == nil {
		var cache struct {
			Fetched time.Time `json:"fetched"`
		}
		if json.Unmarshal(raw, &cache) == nil && !cache.Fetched.IsZero() {
			report.PricingSource = "cache"
			report.PricingFetchedAt = &cache.Fetched
			age := int64(time.Since(cache.Fetched).Seconds())
			if age < 0 {
				age = 0
			}
			report.PricingAgeSecs = &age
		}
	}
	// Two grouped reads beat two counts per harness: doctor must stay a
	// read-only snapshot, not a full-table scan per row.
	eventsByHarness := map[string]int64{}
	cursorsByHarness := map[string]int64{}
	if ro != nil {
		for querySQL, dest := range map[string]map[string]int64{
			"SELECT harness,count(*) FROM event_data GROUP BY harness": eventsByHarness,
			"SELECT harness,count(*) FROM cursors GROUP BY harness":    cursorsByHarness,
		} {
			rows, e := ro.DB().QueryContext(ctx, querySQL)
			if e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
			for rows.Next() {
				var name string
				var n int64
				if e = rows.Scan(&name, &n); e != nil {
					rows.Close()
					fmt.Fprintln(errout, e)
					return 1
				}
				dest[name] = n
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
		}
	}
	for _, p := range harness.All() {
		h := doctorHarness{Harness: string(p.Harness()), Roots: []doctorRoot{}, Enabled: true}
		if ro != nil {
			if v, e := ro.Setting(ctx, "harness."+string(p.Harness())+".enabled"); e == nil && v == "false" {
				h.Enabled = false
			}
		}
		for _, root := range p.Roots() {
			_, e := os.Stat(root)
			h.Roots = append(h.Roots, doctorRoot{Path: root, Exists: e == nil})
			if e == nil {
				h.Detected = true
			}
		}
		h.Events = eventsByHarness[string(p.Harness())]
		h.CursorRows = cursorsByHarness[string(p.Harness())]
		report.Harnesses = append(report.Harnesses, h)
	}
	if *js {
		if err = json.NewEncoder(out).Encode(report); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(out, "Data directory: %s\nDatabase: %s (%d bytes)\nPricing: %s", report.DataDir, report.DBPath, report.DBBytes, report.PricingSource)
	if report.PricingAgeSecs != nil {
		fmt.Fprintf(out, " (age %ds)", *report.PricingAgeSecs)
	} else {
		fmt.Fprint(out, " (age unknown)")
	}
	fmt.Fprintln(out)
	if report.DBUnavailable {
		fmt.Fprintf(out, "Index: unavailable (%s) — read-only, nothing was created, migrated or repriced\n", report.DBReason)
	} else {
		fmt.Fprintf(out, "Index: read-only, schema %s", report.Schema.State)
		if len(report.Schema.Missing) > 0 {
			fmt.Fprintf(out, " (missing: %s)", report.Schema.Missing)
		}
		fmt.Fprintln(out)
	}
	w := newTable(out)
	fmt.Fprintln(w, "HARNESS\tENABLED\tDETECTED\tEVENTS\tROOTS\tPROBLEM")
	for _, h := range report.Harnesses {
		roots := make([]string, 0, len(h.Roots))
		for _, r := range h.Roots {
			label := r.Path
			if !r.Exists {
				label += " (missing)"
			}
			roots = append(roots, label)
		}
		if len(roots) == 0 {
			roots = []string{"-"}
		}
		fmt.Fprintf(w, "%s\t%t\t%t\t%d\t%s\t%s\n", h.Harness, h.Enabled, h.Detected, h.Events, roots, h.Problem)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

func runScan(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(errout)
	cpu := fs.String("cpuprofile", "", "CPU profile path")
	mem := fs.String("memprofile", "", "allocation profile path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errout, "usage: mytoken scan")
		return 2
	}
	if *cpu != "" {
		f, err := os.Create(*cpu)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		if err = pprof.StartCPUProfile(f); err != nil {
			f.Close()
			fmt.Fprintln(errout, err)
			return 1
		}
		defer func() { pprof.StopCPUProfile(); f.Close() }()
	}
	a, err := app.OpenLocal()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer a.Close()
	start := time.Now()
	if err = a.Scanner.Scan(ctx); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	var events, sessions int64
	err = a.Store.DB().QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&events)
	if err == nil {
		err = a.Store.DB().QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE parent_id='' AND EXISTS (SELECT 1 FROM events WHERE events.harness=sessions.harness AND events.session_id=sessions.session_id)").Scan(&sessions)
	}
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	done, total := a.Scanner.Progress()
	fmt.Fprintf(out, "Scanned %d/%d sources: %d events, %d sessions in %s\n", done, total, events, sessions, time.Since(start).Round(time.Millisecond))
	if *mem != "" {
		f, e := os.Create(*mem)
		if e == nil {
			runtime.GC()
			e = pprof.Lookup("allocs").WriteTo(f, 0)
			closeErr := f.Close()
			if e == nil {
				e = closeErr
			}
		}
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
	}
	return 0
}
