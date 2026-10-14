package gui

import (
	"fmt"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/scan"
)

// The settings page draws the data-source card in its right column and the
// instances card in its left one, and each card owns its own load state. While
// the two shared one flag, the left column rendered first, marked the shared
// flag, and the right card then skipped its own loader: the page showed the
// data-source subtitle and the count of tools without data even though the
// query layer held real rows. This drives the real page entry point, in the
// order it builds its columns.
func TestSettingsPageShowsDataSourcesWithUsage(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)

	f := &fakeSources{view: SourceView{Sources: []SourceInfo{
		sourceInfoFor("inst_live", model.Codex, "instance", []string{"/tmp/a"}, true),
	}}}
	read := &diagReader{records: map[string]scan.Diagnostics{"inst_live": {
		LastAttemptAt: now.Add(-time.Minute), LastSuccessAt: now.Add(-time.Minute),
		Reason: scan.ReasonOK, DiscoveredFiles: 1, NewEvents: 1,
		Sources: []scan.SourceDiagnostics{{Path: "/tmp/a/one.jsonl", Reason: scan.ReasonOK}},
	}}}
	s := NewState(NewDemoService(now), Hooks{
		Sources:     f,
		Diagnostics: read.read,
		Now:         func() time.Time { return now },
	}, nil)
	tt := renderPanel(t, 900, 1100, func(c *ui.Context, pal palette) { s.settingsPage(c, pal) })

	// Both loaders ran, and neither marked the other as already done.
	if !s.src.rowsLoaded {
		t.Fatal("the settings page never loaded the data sources")
	}
	if !s.src.cfgLoaded {
		t.Fatal("the settings page never loaded the source configuration")
	}
	if f.configs != 1 {
		t.Errorf("the configuration loaded %d times while drawing, want once", f.configs)
	}
	if len(s.src.rows) == 0 {
		t.Fatal("the data-source card loaded no rows")
	}

	// The card shows the rows themselves: the harness label and its request
	// count, not only the subtitle and the tools-without-data line.
	if !tt.HasText(tr("sourcesSub")) {
		t.Error("the data-source card lost its subtitle")
	}
	b := s.src.rows[0]
	if !tt.HasText(b.Label) {
		t.Errorf("the data-source card did not show the label %q", b.Label)
	}
	usage := fmt.Sprintf(tr("sourceRequests"), fmtInt(b.Requests))
	if !tt.HasText(usage) {
		t.Errorf("the data-source card did not show %q for %s", usage, b.Key)
	}
}
