package quota

import (
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// These literals stand in for the two things a rollout log may contain that
// must never surface in a quota report, an error string, or a JSON payload:
// free-form conversation content and arbitrary future field values.
const (
	syntheticSecretPrompt = "SYNTHETIC-SECRET-PROMPT-9f2c"
	syntheticSecretValue  = "SYNTHETIC-SECRET-VALUE-4a71"
)

func TestReadCodexReportNeverCarriesRawContent(t *testing.T) {
	in := jsonl(
		`{"timestamp":"2026-01-02T03:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"`+syntheticSecretPrompt+`"}]}}`,
		tokenCount("2026-01-02T03:00:01Z", `{"primary":{"used_percent":10,"future_note":"`+syntheticSecretValue+`"},"plan_type":"plus"}`),
	)
	rep := readCodex(t, in, CodexOptions{Source: "synthetic.jsonl"})

	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	out := string(raw)
	if strings.Contains(out, syntheticSecretPrompt) {
		t.Fatalf("report JSON leaked prompt content: %s", out)
	}
	if strings.Contains(out, syntheticSecretValue) {
		t.Fatalf("report JSON leaked a future field value: %s", out)
	}
	if rep.UnknownFields != 1 {
		t.Errorf("UnknownFields = %d, want 1 (the unknown window field)", rep.UnknownFields)
	}
}

type secretThenErrorReader struct {
	line string
	sent bool
}

func (r *secretThenErrorReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errors.New("synthetic read failure")
	}
	r.sent = true
	return copy(p, r.line+"\n"), nil
}

func TestReadCodexErrorsNeverCarryInput(t *testing.T) {
	secretLine := `{"timestamp":"2026-01-02T03:00:00Z","type":"response_item","payload":{"type":"message","content":[{"type":"input_text","text":"` + syntheticSecretPrompt + `"}]}}`
	_, err := ReadCodex(context.Background(), &secretThenErrorReader{line: secretLine}, CodexOptions{})
	if err == nil {
		t.Fatal("ReadCodex = nil error, want the reader failure")
	}
	if strings.Contains(err.Error(), syntheticSecretPrompt) {
		t.Fatalf("error leaked input content: %v", err)
	}
	if strings.Contains(err.Error(), "input_text") {
		t.Fatalf("error echoed raw JSON: %v", err)
	}
}

func TestReadCodexCapsLabelsFromLog(t *testing.T) {
	long := strings.Repeat("x", 1000)
	in := jsonl(tokenCount("2026-01-02T03:04:05Z",
		`{"primary":{"used_percent":1,"window_minutes":300},"plan_type":"`+long+`","limit_id":"`+long+`"}`))
	rep := readCodex(t, in, CodexOptions{})

	if rep.PlanType != nil && len(*rep.PlanType) > maxLabelLen {
		t.Errorf("PlanType length = %d, want <= %d", len(*rep.PlanType), maxLabelLen)
	}
	if len(rep.LimitID) > maxLabelLen {
		t.Errorf("LimitID length = %d, want <= %d", len(rep.LimitID), maxLabelLen)
	}
	if rep.Status != StatusOK {
		t.Errorf("status = %q/%q, want a capped but usable observation", rep.Status, rep.Reason)
	}
}

// TestQuotaPackageStaysOffline is the structural guarantee that the offline
// Codex reader cannot reach the network, the filesystem, credentials, or the
// rest of this module. It parses the package's own non-test sources.
func TestQuotaPackageStaysOffline(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parser.ParseDir: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("parser.ParseDir found no package")
	}

	forbidden := []string{
		"net",
		"crypto/tls",
		"os",
		"os/exec",
		"syscall",
		"plugin",
		"github.com/zzstar101/mytoken/",
	}
	seen := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("%s: unquote %s: %v", name, imp.Path.Value, err)
				}
				seen++
				for _, bad := range forbidden {
					if path == bad || strings.HasPrefix(path, bad) {
						t.Errorf("%s imports %q, which the offline quota reader must not use", name, path)
					}
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("parsed no imports; the isolation check would pass vacuously")
	}
}
