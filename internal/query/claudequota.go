package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/zzstar101/mytoken/internal/claudequota"
	"github.com/zzstar101/mytoken/internal/store"
)

// ClaudeQuotaObservation is one instance's persisted Claude Code quota state:
// the explicit consent record and the cached reading, read read-only.
//
// The consent decides whether anything may be fetched at all, so it is reported
// separately from the reading: an instance can be enabled with no reading yet,
// and a reading can exist while the consent that produced it was already
// revoked (in which case it must not be shown as current). Nothing here reads a
// credential or the network; the credential source reference is carried only so
// an explicit disclosure command can print it.
type ClaudeQuotaObservation struct {
	// Instance is the instance the settings keys belong to, after
	// normalisation. A requested instance with nothing stored is still
	// reported, with nothing present, so an instance filter cannot hide it.
	Instance string `json:"instance"`
	// ConsentKey and LatestKey are the exact settings keys read.
	ConsentKey string `json:"consent_key,omitempty"`
	LatestKey  string `json:"latest_key,omitempty"`

	// ConsentPresent is true when a consent row exists. It stays true for a
	// cleared consent (Disable writes an empty value rather than deleting the
	// row), which is why ConsentValid is a separate bit.
	ConsentPresent bool `json:"consent_present"`
	// ConsentValid is true when the stored consent is usable for this instance
	// and the fixed target under the current schema version.
	ConsentValid bool `json:"consent_valid"`
	// Consent is the decoded record; its zero value is not a grant.
	Consent claudequota.Consent `json:"consent,omitempty"`
	// ConsentCorrupt is true when a row exists but is not valid JSON.
	ConsentCorrupt bool `json:"consent_corrupt,omitempty"`
	// ConsentDetail explains a present-but-unusable consent.
	ConsentDetail string `json:"consent_detail,omitempty"`

	// QuotaPresent is true when a cached reading row with content exists.
	QuotaPresent bool `json:"quota_present"`
	// Quota is the decoded cached reading; its windows are omitted when the
	// reading carries none, never reported as zero.
	Quota claudequota.Quota `json:"quota,omitempty"`
	// QuotaCorrupt is true when a reading row exists but cannot be decoded.
	QuotaCorrupt bool `json:"quota_corrupt,omitempty"`
	// QuotaDetail explains a missing or corrupt reading.
	QuotaDetail string `json:"quota_detail,omitempty"`
}

// ReadClaudeQuota reads the persisted Claude Code consent and cached readings
// inside one read-only transaction, so every instance is read from the same
// index state.
//
// instanceIDs selects which stored instances to return. When it is empty every
// stored quota.claude.* key is enumerated. Enumeration reads the settings table
// with plain read-only SQL: the CLI has no running service, so the stored keys
// are the only truthful answer, and nothing is inferred from the configured
// sources.
//
// A per-instance decode failure is reported in that observation instead of
// failing the whole read, so one bad key cannot hide every other instance.
func ReadClaudeQuota(ctx context.Context, ro *store.Store, instanceIDs []string) ([]ClaudeQuotaObservation, error) {
	if ro == nil {
		return nil, errors.New("query: nil store")
	}
	tx, err := ro.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out, err := readClaudeQuotaTx(ctx, tx, instanceIDs)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func readClaudeQuotaTx(ctx context.Context, tx *sql.Tx, instanceIDs []string) ([]ClaudeQuotaObservation, error) {
	if len(instanceIDs) == 0 {
		keys, err := enumerateClaudeQuotaKeys(ctx, tx)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		out := make([]ClaudeQuotaObservation, 0, len(keys))
		for _, key := range keys {
			instance := claudeQuotaInstanceOfKey(key)
			if instance == "" || seen[instance] {
				continue
			}
			seen[instance] = true
			obs, err := readClaudeQuotaInstance(ctx, tx, instance)
			if err != nil {
				return nil, err
			}
			out = append(out, obs)
		}
		SortClaudeQuotaObservations(out)
		return out, nil
	}
	// An explicit selection reports exactly the instances asked for, in the
	// order asked, including the ones with nothing stored.
	seen := map[string]bool{}
	out := make([]ClaudeQuotaObservation, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		instance := store.NormalizeInstanceID(id)
		if seen[instance] {
			continue
		}
		seen[instance] = true
		obs, err := readClaudeQuotaInstance(ctx, tx, instance)
		if err != nil {
			return nil, err
		}
		out = append(out, obs)
	}
	return out, nil
}

// enumerateClaudeQuotaKeys lists every consent or reading key. It is one query
// so both prefixes come from the same snapshot of the settings table.
func enumerateClaudeQuotaKeys(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT key FROM settings WHERE key LIKE ? ESCAPE '\' OR key LIKE ? ESCAPE '\' ORDER BY key`,
		likePrefix(claudequota.SettingConsentPrefix), likePrefix(claudequota.SettingLatestPrefix))
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	return keys, err
}

// claudeQuotaInstanceOfKey recovers the instance from either key shape. A key
// that matches neither prefix belongs to no instance and is skipped rather than
// guessed at.
func claudeQuotaInstanceOfKey(key string) string {
	if instance := strings.TrimPrefix(key, claudequota.SettingConsentPrefix); instance != key {
		return instance
	}
	if instance := strings.TrimPrefix(key, claudequota.SettingLatestPrefix); instance != key {
		return instance
	}
	return ""
}

// readClaudeQuotaInstance reads both keys of one instance. A missing row is not
// an error: the default state is "nothing stored", which is exactly "disabled".
func readClaudeQuotaInstance(ctx context.Context, tx *sql.Tx, instance string) (ClaudeQuotaObservation, error) {
	obs := ClaudeQuotaObservation{
		Instance:   instance,
		ConsentKey: claudequota.ConsentKey(instance),
		LatestKey:  claudequota.LatestKey(instance),
	}
	consentRaw, consentPresent, err := settingValue(ctx, tx, obs.ConsentKey)
	if err != nil {
		return ClaudeQuotaObservation{}, err
	}
	obs.ConsentPresent = consentPresent
	obs.Consent, obs.ConsentValid, obs.ConsentCorrupt, obs.ConsentDetail =
		decodeClaudeConsent(instance, consentRaw, consentPresent)

	quotaRaw, quotaPresent, err := settingValue(ctx, tx, obs.LatestKey)
	if err != nil {
		return ClaudeQuotaObservation{}, err
	}
	obs.Quota, obs.QuotaPresent, obs.QuotaCorrupt, obs.QuotaDetail =
		decodeClaudeQuotaReading(instance, quotaRaw, quotaPresent)
	return obs, nil
}

func settingValue(ctx context.Context, tx *sql.Tx, key string) (string, bool, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return raw, true, nil
}

// decodeClaudeConsent mirrors the service's fail-closed consent rules using
// only exported state: a record is usable only under the current schema version,
// bound to this instance and to the fixed target. A cleared row is present but
// not usable, which is what "disabled" means.
func decodeClaudeConsent(instance, raw string, present bool) (claudequota.Consent, bool, bool, string) {
	if !present {
		return claudequota.Consent{}, false, false, ""
	}
	if strings.TrimSpace(raw) == "" {
		return claudequota.Consent{}, false, false, "the stored consent is cleared; Claude quota is disabled for this instance"
	}
	var c claudequota.Consent
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return claudequota.Consent{}, false, true, "the stored consent is not valid JSON: " + err.Error()
	}
	switch {
	case c.Version != claudequota.ConsentVersion:
		return c, false, false, "the stored consent version is not the current schema version"
	case c.Target != claudequota.Target:
		return c, false, false, "the stored consent target is not the fixed Claude usage endpoint"
	case c.Instance == "" || c.Instance != instance:
		return c, false, false, "the stored consent is bound to a different instance"
	}
	return c, true, false, ""
}

// decodeClaudeQuotaReading decodes the cached reading. An empty value is a
// cleared reading, not a zero reading.
func decodeClaudeQuotaReading(instance, raw string, present bool) (claudequota.Quota, bool, bool, string) {
	if !present || strings.TrimSpace(raw) == "" {
		return claudequota.Quota{Instance: instance, Target: claudequota.Target, Status: claudequota.StatusUnavailable}, false, false, ""
	}
	var q claudequota.Quota
	if err := json.Unmarshal([]byte(raw), &q); err != nil {
		return claudequota.Quota{Instance: instance, Target: claudequota.Target, Status: claudequota.StatusUnavailable}, false, true,
			"the stored Claude quota reading is not valid JSON: " + err.Error()
	}
	if q.Instance == "" {
		q.Instance = instance
	}
	if q.Target == "" {
		q.Target = claudequota.Target
	}
	return q, true, false, ""
}

// MissingClaudeQuotaObservations returns one empty observation per requested
// instance, normalised and deduplicated exactly like ReadClaudeQuota's explicit
// selection. A caller that cannot read the index at all still reports the
// instances it was asked about, so an instance filter never silently returns
// nothing. detail explains why nothing could be read.
func MissingClaudeQuotaObservations(instanceIDs []string, detail string) []ClaudeQuotaObservation {
	seen := map[string]bool{}
	out := make([]ClaudeQuotaObservation, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		instance := store.NormalizeInstanceID(id)
		if seen[instance] {
			continue
		}
		seen[instance] = true
		out = append(out, ClaudeQuotaObservation{
			Instance:      instance,
			ConsentKey:    claudequota.ConsentKey(instance),
			LatestKey:     claudequota.LatestKey(instance),
			QuotaDetail:   detail,
			ConsentDetail: detail,
		})
	}
	return out
}

// SortClaudeQuotaObservations sorts in place by instance. It is exported so the
// snapshot and the CLI agree on one order.
func SortClaudeQuotaObservations(obs []ClaudeQuotaObservation) {
	sort.SliceStable(obs, func(i, j int) bool { return obs[i].Instance < obs[j].Instance })
}
