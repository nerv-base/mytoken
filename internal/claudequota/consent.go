package claudequota

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Consent is the persisted, explicit authorization to read one credential
// source for one instance and to send it to one fixed request target. It holds
// no secret: only the non-secret source reference, the target, the schema
// version, the instance identity and the grant instant.
//
// Consent is shared: the GUI and the CLI read and write the same record, so
// granting it once in either place enables it in both, and revoking it in
// either place disables it in both.
type Consent struct {
	// Version is the consent schema version this record was written with.
	Version int `json:"version"`
	// Instance is the instance identity this consent is bound to.
	Instance string `json:"instance"`
	// SourceKind is the credential source kind authorized.
	SourceKind SourceKind `json:"source_kind"`
	// SourceRef is the exact, non-secret source reference authorized (a file
	// path or a keychain service/account).
	SourceRef string `json:"source_ref"`
	// Target is the exact request target authorized.
	Target string `json:"target"`
	// GrantedAt is when the consent was recorded.
	GrantedAt time.Time `json:"granted_at"`
}

// ValidFor reports whether this consent authorizes exactly this instance,
// source and target, under the current schema version.
func (c Consent) ValidFor(instance string, kind SourceKind, ref string) bool {
	if c.Version != ConsentVersion || c.Target != Target {
		return false
	}
	if c.Instance == "" || c.Instance != instance {
		return false
	}
	if c.SourceRef == "" || c.SourceRef != ref {
		return false
	}
	return c.SourceKind == kind
}

// ConsentKey is the settings key holding an instance's consent.
func ConsentKey(instance string) string { return SettingConsentPrefix + instance }

// LatestKey is the settings key holding an instance's cached quota windows.
func LatestKey(instance string) string { return SettingLatestPrefix + instance }

// loadConsent reads and decodes an instance's consent.
//
// It returns ErrNotEnabled when nothing is stored (the default), and
// ReasonConsentInvalid when something is stored that cannot be used, so a
// corrupt record fails closed instead of silently granting.
func loadConsent(ctx context.Context, store SettingReader, instance string) (Consent, error) {
	if store == nil {
		return Consent{}, errReason(ReasonStoreUnavailable)
	}
	raw, err := store.Setting(ctx, ConsentKey(instance))
	if err != nil {
		return Consent{}, errReason(ReasonStoreUnavailable)
	}
	if strings.TrimSpace(raw) == "" {
		return Consent{}, errReason(ReasonNotEnabled)
	}
	var c Consent
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Consent{}, errReason(ReasonConsentInvalid)
	}
	if c.Version != ConsentVersion {
		return Consent{}, errReason(ReasonConsentInvalid)
	}
	if c.Target != Target || c.SourceRef == "" || c.SourceKind == "" || c.Instance == "" {
		return Consent{}, errReason(ReasonConsentInvalid)
	}
	return c, nil
}

// SettingReader is the read half of the settings store this package needs.
type SettingReader interface {
	Setting(ctx context.Context, key string) (string, error)
}

// Settings is the read/write store this package needs. It is satisfied by
// *store.Store. No schema is added or changed.
type Settings interface {
	SettingReader
	SetSetting(ctx context.Context, key, value string) error
}

// encodeConsent renders a consent record. It never contains a credential.
func encodeConsent(c Consent) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", errReason(ReasonStoreUnavailable)
	}
	return string(b), nil
}
