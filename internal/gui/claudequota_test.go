package gui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/claudequota"
)

// TestClaudeQuotaDisabledCardSaysNoConsentRecorded pins the honesty of the
// default state: with no consent stored the card must say in words that nothing
// is authorized and nothing was read. An "Off" chip alone reads like a panel
// that failed to load.
func TestClaudeQuotaDisabledCardSaysNoConsentRecorded(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	const ref = "/home/u/.claude/.credentials.json"
	f := &fakeClaudeQuota{
		info: claudequota.Info{Instance: "default", Target: claudequota.Target},
		sources: []claudequota.Source{
			{Instance: "default", Label: "Claude", Kind: claudequota.SourceFile, Ref: ref,
				Read: func(context.Context) (claudequota.Credential, error) { return claudequota.Credential{}, nil }},
		},
	}
	s := NewState(NewDemoService(now), Hooks{ClaudeQuota: f, Now: func() time.Time { return now }}, nil)
	tt := renderPanel(t, 900, 700, func(c *ui.Context, pal palette) { s.claudeQuotaCard(c, pal) })

	if f.info.Enabled {
		t.Fatal("the panel enabled itself")
	}
	if !tt.HasText(tr("claudeQuotaNoConsent")) {
		t.Errorf("the disabled card does not say that no consent is recorded; want %q", tr("claudeQuotaNoConsent"))
	}
	// The disclosure stays: it is what an enable would authorize.
	if !tt.HasText(tr("claudeQuotaTarget")) || !tt.HasText(ref) {
		t.Error("the disabled card must still disclose what enabling would read and request")
	}
}

// unreadableSnapshotClaudeQuota answers like the real service when the cached
// reading cannot be read: info still resolves, the snapshot does not.
type unreadableSnapshotClaudeQuota struct{ *fakeClaudeQuota }

func (f unreadableSnapshotClaudeQuota) Snapshot(context.Context, string) (claudequota.Quota, error) {
	return claudequota.Quota{}, errors.New("claude quota: the index could not be read")
}

// TestClaudeQuotaEnabledUnreadableReadingSaysWhy covers the other half of the
// honesty rule: once enabled, a cached reading that cannot be read must say so
// and keep naming the stored reason instead of showing a blank chip.
func TestClaudeQuotaEnabledUnreadableReadingSaysWhy(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	base := &fakeClaudeQuota{
		info: claudequota.Info{
			Instance: "default",
			Enabled:  true,
			Target:   claudequota.Target,
			Quota: claudequota.Quota{
				Instance: "default",
				Status:   claudequota.StatusUnavailable,
				Reason:   claudequota.ReasonOffline,
			},
		},
	}
	s := NewState(NewDemoService(now), Hooks{ClaudeQuota: unreadableSnapshotClaudeQuota{base}, Now: func() time.Time { return now }}, nil)
	tt := renderPanel(t, 900, 700, func(c *ui.Context, pal palette) { s.claudeQuotaCard(c, pal) })

	if base.info.Enabled != true {
		t.Fatal("test setup: the instance must be enabled")
	}
	if !tt.HasText(tr("claudeQuotaNoSnapshot")) {
		t.Errorf("an unreadable cached reading must say so; want %q", tr("claudeQuotaNoSnapshot"))
	}
	if want := claudeReasonText(claudequota.ReasonOffline); !tt.HasText(want) {
		t.Errorf("the card must keep naming the stored reason; want %q", want)
	}
}

// TestClaudeQuotaMissingCredentialsFileIsSaidBeforeEnable pins the candidate
// list's honesty about a file that is not there yet: the row says so before an
// enable, instead of looking ready and only failing on the first read.
func TestClaudeQuotaMissingCredentialsFileIsSaidBeforeEnable(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	const ref = "/home/u/.claude/.credentials.json"
	f := &fakeClaudeQuota{
		info: claudequota.Info{Instance: "default", Target: claudequota.Target},
		sources: []claudequota.Source{
			{Instance: "default", Label: "Claude", Kind: claudequota.SourceFile, Ref: ref,
				FileMissing: true,
				Read: func(context.Context) (claudequota.Credential, error) {
					return claudequota.Credential{}, nil
				}},
		},
	}
	s := NewState(NewDemoService(now), Hooks{ClaudeQuota: f, Now: func() time.Time { return now }}, nil)
	tt := renderPanel(t, 900, 700, func(c *ui.Context, pal palette) { s.claudeQuotaCard(c, pal) })

	if !tt.HasText(tr("claudeQuotaFileMissing")) {
		t.Errorf("the candidate list does not say the credentials file is missing; want %q", tr("claudeQuotaFileMissing"))
	}
}
