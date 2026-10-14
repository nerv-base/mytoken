package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zzstar101/mytoken/internal/claudequota"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/model"
)

// ErrClaudeQuotaUnavailable is returned by the Claude quota methods when the
// app has no quota service (never in a successful Open/OpenLocal).
var ErrClaudeQuotaUnavailable = errors.New("claude quota service is unavailable")

// isDefaultClaudeProfile reports whether src is the actual default Claude Code
// profile: the platform fallback instance, whose credential origin is the
// documented default config directory. An explicit entry, a config directory
// taken from CLAUDE_CONFIG_DIR and any custom profile are never it.
func isDefaultClaudeProfile(src Source) bool {
	return src.Harness == model.ClaudeCode &&
		src.ID == instance.DefaultID &&
		src.Source == instance.PathDefault
}

// claudeQuotaSources derives the credential sources this build may offer for
// the resolved instances. It only inspects non-secret fields: the instance's
// stable id, its provenance, its roots and the Claude CLI config directory
// those imply. Nothing here reads a credential.
//
// Binding rules that must not drift:
//
//   - A disabled entry or a backup-copy is never scanned, so it offers no
//     source at all.
//   - The actual default profile uses the documented default config directory
//     and, where a runner exists, may also offer the account-global macOS
//     keychain entry as an explicit alternative.
//   - Any other profile must derive its own credential origin from its own
//     roots. An unknown or ambiguous root set yields no source: it must never
//     fall back to the default ~/.claude credential, and it must never be
//     offered the default keychain service, which would bind whichever account
//     the keychain happens to hold.
//   - The file source is listed first, so it is the default a consent selects.
//     Ref and Profile name the exact origin a UI can confirm before enabling.
func claudeQuotaSources(sources []Source) []claudequota.Source {
	var out []claudequota.Source
	for _, src := range sources {
		if src.Harness != model.ClaudeCode || !src.Scannable() {
			continue
		}
		if isDefaultClaudeProfile(src) {
			home := harness.Home()
			if home == "" {
				// Without a home directory the documented default origin is
				// unknown: offer nothing rather than a relative path.
				continue
			}
			cfgDir := claudequota.DefaultConfigDir(home)
			out = append(out, claudeFileSource(src.ID, src.Label,
				filepath.Join(cfgDir, claudequota.CredentialsFileName)))
			if run := claudequota.DefaultKeychainRunner(); run != nil {
				out = append(out, claudequota.KeychainSource(
					src.ID, src.Label, claudequota.ClaudeCodeKeychainService, "", run))
			}
			continue
		}
		cfgDir, ok := claudequota.ConfigDirFromRoots(src.Roots)
		if !ok || !filepath.IsAbs(cfgDir) {
			continue
		}
		out = append(out, claudeFileSource(src.ID, src.Label,
			filepath.Join(cfgDir, claudequota.CredentialsFileName)))
	}
	return out
}

// claudeFileSource builds a file source and records whether its credentials
// file exists right now, so a card can say so before an enable instead of only
// after a failed read. It stats the path and reads nothing; a path that exists
// but cannot be read is left to the read path, which reports its own reason.
func claudeFileSource(id, label, path string) claudequota.Source {
	src := claudequota.FileSource(id, label, path)
	if _, err := os.Stat(path); err != nil && errors.Is(err, fs.ErrNotExist) {
		src.FileMissing = true
	}
	return src
}

// newClaudeQuota builds the quota service for this app. refresh mirrors the
// price worker: only the online Open starts anything automatic, so OpenLocal
// (offline) has no auto refresh and every request is refused.
func newClaudeQuota(store claudequota.Settings, sources []Source, refresh bool) *claudequota.Service {
	return claudequota.New(claudequota.Options{
		Store:     store,
		Sources:   claudeQuotaSources(sources),
		Transport: claudequota.DefaultTransport(),
		Offline:   !refresh,
	})
}

// ClaudeQuota returns the quota service, or nil when the app has none.
func (a *App) ClaudeQuota() *claudequota.Service {
	if a == nil {
		return nil
	}
	a.claudeMu.Lock()
	defer a.claudeMu.Unlock()
	return a.claudeQuota
}

// reloadClaudeQuotaSources rebuilds the quota service over the sources a
// successful reload resolved.
//
// A consent is bound to the exact source reference it was granted for
// (claudequota.Consent.ValidFor), and the service resolves a stored consent by
// that reference. Replacing the service is what makes a moved path take
// effect: the old reference no longer exists in the new source set, so that
// instance reports ReasonNoSource and fails closed instead of reading a new
// path under an old grant. A consent whose source did not move keeps working,
// because consents live in the store, not in the service.
func (a *App) reloadClaudeQuotaSources(sources []Source) {
	if a == nil || a.Store == nil {
		return
	}
	a.claudeMu.Lock()
	defer a.claudeMu.Unlock()
	old := a.claudeQuota
	a.claudeQuota = newClaudeQuota(a.Store, sources, a.claudeRefresh)
	a.claudeQuota.Start(context.Background())
	if old != nil {
		old.Close()
	}
}

func (a *App) quotaService() (*claudequota.Service, error) {
	if a == nil {
		return nil, ErrClaudeQuotaUnavailable
	}
	a.claudeMu.Lock()
	defer a.claudeMu.Unlock()
	if a.claudeQuota == nil {
		return nil, ErrClaudeQuotaUnavailable
	}
	return a.claudeQuota, nil
}

// ClaudeQuotaInfo reports one instance's consent and cached reading. It reads
// no credential and uses no network, so a GUI can render the confirmation of
// exactly which source and target a consent would authorize, and whether one
// already exists, before anything is enabled.
//
// A missing consent is not an error: Enabled is false.
func (a *App) ClaudeQuotaInfo(ctx context.Context, instance string) (claudequota.Info, error) {
	svc, err := a.quotaService()
	if err != nil {
		return claudequota.Info{}, err
	}
	return svc.ConsentInfo(ctx, instance)
}

// EnableClaudeQuota records the explicit, persisted consent for one instance
// and credential source and wakes the auto refresh. ref may be empty for the
// instance's default (credentials file) source; see ClaudeQuotaSources. It
// reads no credential itself.
func (a *App) EnableClaudeQuota(ctx context.Context, instance, ref string) (claudequota.Info, error) {
	svc, err := a.quotaService()
	if err != nil {
		return claudequota.Info{}, err
	}
	return svc.Enable(ctx, instance, ref)
}

// DisableClaudeQuota revokes consent, cancels any in-flight request for the
// instance and clears its cached reading. Nothing is published afterwards.
func (a *App) DisableClaudeQuota(ctx context.Context, instance string) error {
	svc, err := a.quotaService()
	if err != nil {
		return err
	}
	return svc.Disable(ctx, instance)
}

// ClaudeQuotaSnapshot returns the cached reading only: no credential read, no
// network. A never-fetched instance is StatusUnavailable, not 0%.
func (a *App) ClaudeQuotaSnapshot(ctx context.Context, instance string) (claudequota.Quota, error) {
	svc, err := a.quotaService()
	if err != nil {
		return claudequota.Quota{}, err
	}
	return svc.Snapshot(ctx, instance)
}

// RefreshClaudeQuota performs one authorized refresh. It is refused offline,
// without a valid consent and inside the throttle window (ErrThrottled carries
// RetryAfter). An expired Claude CLI token yields ReasonTokenExpired and must
// be renewed with the Claude CLI; this app never refreshes it.
func (a *App) RefreshClaudeQuota(ctx context.Context, instance string) (claudequota.Quota, error) {
	svc, err := a.quotaService()
	if err != nil {
		return claudequota.Quota{}, err
	}
	return svc.Refresh(ctx, instance)
}

// ClaudeQuotaSources lists the credential sources configured for an instance,
// in preference order, for a confirmation UI. Only Kind, Ref, Profile and Label
// are meaningful; they are non-secret.
func (a *App) ClaudeQuotaSources(instance string) []claudequota.Source {
	svc := a.ClaudeQuota()
	if svc == nil {
		return nil
	}
	return svc.SourceRefs(instance)
}
