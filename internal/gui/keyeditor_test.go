package gui

import (
	"context"
	"errors"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/traekey"
)

// keySources is a one-tool source list whose database is encrypted: it reports
// NeedsKey and remembers what the user supplied.
type keySources struct {
	key        string
	keyset     int
	captured   string
	captures   int
	captureErr error
}

func (k *keySources) List(context.Context) ([]Source, error) {
	return []Source{{
		Harness:  model.Trae,
		Detected: true,
		Enabled:  true,
		Roots:    []string{"/tmp/trae"},
		NeedsKey: true,
		HasKey:   k.key != "",
	}}, nil
}

func (k *keySources) SetEnabled(context.Context, model.Harness, bool) error { return nil }
func (k *keySources) SetRoot(context.Context, model.Harness, string) error  { return nil }
func (k *keySources) Rescan(context.Context) error                          { return nil }

func (k *keySources) CaptureKey(_ context.Context, _ model.Harness, progress func(string)) error {
	if k.captureErr != nil {
		return k.captureErr
	}
	if progress != nil {
		progress(traekey.StepScan)
	}
	k.key = k.captured
	k.captures++
	return nil
}

func (k *keySources) SetKey(_ context.Context, _ model.Harness, key string) error {
	k.key = key
	k.keyset++
	return nil
}

// TestAuthorizeDatabaseKey walks the path a user is meant to take: one click
// that asks the tool for its own key, with no field to fill in.
func TestAuthorizeDatabaseKey(t *testing.T) {
	const key = "aa11bb22cc33dd44ee55ff6600778899aa11bb22cc33dd44ee55ff6600778899"
	src := &keySources{captured: key}
	h := newHarnessWith(t, src)
	tt := ui.NewTester(h.app.MainView, 1280, 1000)
	h.app.goTo("settings")
	h.app.set.group = "sources"
	settle(t, h, tt)

	if !tt.HasText(tr("authHint")) {
		t.Fatalf("a missing key is not explained, texts=%v", tt.Texts())
	}
	if err := tt.Click(tr("authRead")); err != nil {
		t.Fatalf("no way to authorize an encrypted source: %v", err)
	}
	h.drain(t)
	settle(t, h, tt)
	if src.captures != 1 || src.key != key {
		t.Fatalf("capture ran %d times and stored %q, want the captured key once", src.captures, src.key)
	}
	if !tt.HasText(tr("dbKeyStored")) {
		t.Fatalf("captured key is not reported as stored, texts=%v", tt.Texts())
	}
	// A stored key offers a change instead of another authorization.
	if !tt.HasText(tr("changeDbKey")) || tt.HasText(tr("authRead")) {
		t.Fatalf("wrong actions after a key is stored, texts=%v", tt.Texts())
	}
}

// TestDatabaseKeyEditor drives the fallback field the way a user does: open the
// editor, paste, save, then clear.
func TestDatabaseKeyEditor(t *testing.T) {
	const old = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	src := &keySources{key: old}
	h := newHarnessWith(t, src)
	tt := ui.NewTester(h.app.MainView, 1280, 1000)
	h.app.goTo("settings")
	h.app.set.group = "sources"
	settle(t, h, tt)

	if err := tt.Click(tr("changeDbKey")); err != nil {
		t.Fatal(err)
	}
	settle(t, h, tt)
	if !tt.HasText(tr("dbKeyHint")) {
		t.Fatalf("editor did not open, texts=%v", tt.Texts())
	}

	const key = "aa11bb22cc33dd44ee55ff6600778899aa11bb22cc33dd44ee55ff6600778899"
	h.app.set.keyValue = key
	settle(t, h, tt)
	if err := tt.Click(tr("save")); err != nil {
		t.Fatal(err)
	}
	h.drain(t)
	settle(t, h, tt)
	if src.key != key || src.keyset != 1 {
		t.Fatalf("SetKey got %q after %d calls, want %q once", src.key, src.keyset, key)
	}
	if !tt.HasText(tr("dbKeyStored")) {
		t.Fatalf("stored key is not reported, texts=%v", tt.Texts())
	}

	if err := tt.Click(tr("changeDbKey")); err != nil {
		t.Fatal(err)
	}
	settle(t, h, tt)
	if err := tt.Click(tr("clearDbKey")); err != nil {
		t.Fatal(err)
	}
	h.drain(t)
	if src.key != "" || src.keyset != 2 {
		t.Fatalf("clearing got %q after %d calls, want empty", src.key, src.keyset)
	}
}

// TestAuthorizeFailureOpensEditor checks that a machine where a capture cannot
// work still leaves a way in: the failure is reported and the field opens so a
// key recovered elsewhere can be pasted.
func TestAuthorizeFailureOpensEditor(t *testing.T) {
	src := &keySources{captureErr: errors.New("no key found in memory")}
	h := newHarnessWith(t, src)
	tt := ui.NewTester(h.app.MainView, 1280, 1000)
	h.app.goTo("settings")
	h.app.set.group = "sources"
	settle(t, h, tt)

	if err := tt.Click(tr("authRead")); err != nil {
		t.Fatal(err)
	}
	h.drain(t)
	settle(t, h, tt)
	if h.app.set.err == "" {
		t.Fatalf("the capture failure was not reported")
	}
	if !tt.HasText(tr("dbKeyHint")) {
		t.Fatalf("the field did not open as a fallback, texts=%v", tt.Texts())
	}
}
