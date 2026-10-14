package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/store"
)

// QuotaObservation is one persisted Codex quota observation, read read-only.
//
// The observation keeps the instance it was stored under. Instances are never
// combined: two instances are two accounts, each with its own credit units, so
// a reader that summed or merged them would invent a number nobody reported.
type QuotaObservation struct {
	// Instance is the instance the observation was stored under, after
	// normalisation. A requested instance with nothing stored is still
	// reported, with Missing set, so an instance filter cannot hide it.
	Instance string `json:"instance"`
	// Key is the exact settings key the observation came from.
	Key string `json:"key,omitempty"`
	// Missing is true when the caller asked for this instance and no
	// quota.codex.<instance> setting exists. Report is nil and IngestedAt zero.
	Missing bool `json:"missing,omitempty"`
	// Corrupt is true when a setting exists but cannot be decoded into a
	// usable observation. The instance is reported unavailable rather than
	// dropped, and Detail carries the decode error.
	Corrupt bool `json:"corrupt,omitempty"`
	// Detail explains Missing/Corrupt when there is something to say.
	Detail string `json:"detail,omitempty"`
	// IngestedAt is when the writer persisted the observation.
	IngestedAt time.Time `json:"ingested_at,omitempty"`
	// IngestedKnown is false when the stored envelope carried no ingest time.
	IngestedKnown bool `json:"ingested_known"`
	// Source is the provenance label the writer recorded (e.g. a rollout path).
	Source string `json:"source,omitempty"`
	// Report is the stored quota report; nil when the instance is missing or
	// corrupt, which is exactly when no window may be shown.
	Report *quota.Report `json:"report,omitempty"`
	// Forecast is the pace forecast the writer retained in the stored
	// envelope. It is nil when the envelope carried none (every value written
	// before observation retention existed), and a reader must treat that as
	// "no forecast yet": it must not derive a pace here, because a pace needs
	// retained comparable observations and is never inferred from token counts.
	Forecast *quota.Forecast `json:"forecast,omitempty"`
}

// ReadQuota reads the persisted Codex quota observations inside one read-only
// transaction, so every instance is read from the same index state.
//
// instanceIDs selects which stored instances to return. When it is empty every
// stored quota.codex.* key is enumerated. Enumeration reads the settings table
// with plain readonly SQL: the CLI has no running scanner, so it must never ask
// Scanner.Instances which instances "exist" — the stored keys are the only
// truthful answer.
//
// A per-instance decode failure is reported in that observation instead of
// failing the whole read, so one bad key cannot hide every other instance.
func ReadQuota(ctx context.Context, ro *store.Store, instanceIDs []string) ([]QuotaObservation, error) {
	if ro == nil {
		return nil, errors.New("query: nil store")
	}
	tx, err := ro.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out, err := readQuotaTx(ctx, tx, instanceIDs)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func readQuotaTx(ctx context.Context, tx *sql.Tx, instanceIDs []string) ([]QuotaObservation, error) {
	if len(instanceIDs) == 0 {
		rows, err := tx.QueryContext(ctx, `SELECT key FROM settings WHERE key LIKE ? ESCAPE '\' ORDER BY key`, likePrefix(quota.SettingPrefix))
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
		if err != nil {
			return nil, err
		}
		out := make([]QuotaObservation, 0, len(keys))
		for _, key := range keys {
			obs, err := readQuotaKey(ctx, tx, key)
			if err != nil {
				return nil, err
			}
			out = append(out, obs)
		}
		return out, nil
	}
	// An explicit selection reports exactly the instances asked for, in the
	// order asked, including the ones with nothing stored.
	seen := map[string]bool{}
	out := make([]QuotaObservation, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		instance := store.NormalizeInstanceID(id)
		if seen[instance] {
			continue
		}
		seen[instance] = true
		key := quota.SettingKey(instance)
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			out = append(out, QuotaObservation{
				Instance: instance,
				Key:      key,
				Missing:  true,
				Detail:   "no stored Codex quota observation for this instance",
			})
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, decodeQuotaObservation(instance, key, raw))
	}
	return out, nil
}

func readQuotaKey(ctx context.Context, tx *sql.Tx, key string) (QuotaObservation, error) {
	instance := strings.TrimPrefix(key, quota.SettingPrefix)
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&raw); err != nil {
		return QuotaObservation{}, err
	}
	return decodeQuotaObservation(instance, key, raw), nil
}

func decodeQuotaObservation(instance, key, raw string) QuotaObservation {
	obs := QuotaObservation{Instance: instance, Key: key}
	var latest quota.Latest
	if err := json.Unmarshal([]byte(raw), &latest); err != nil {
		obs.Corrupt = true
		obs.Detail = "stored observation is not valid JSON: " + err.Error()
		return obs
	}
	if latest.Report == nil {
		obs.Corrupt = true
		obs.Detail = "stored observation carries no report"
		return obs
	}
	obs.IngestedAt = latest.IngestedAt
	obs.IngestedKnown = !latest.IngestedAt.IsZero()
	obs.Source = latest.Source
	obs.Report = latest.Report
	obs.Forecast = latest.Forecast
	return obs
}

// MissingQuotaObservations returns one missing observation per requested
// instance, normalised and deduplicated exactly like ReadQuota's explicit
// selection. A caller that cannot read the index at all still reports the
// instances it was asked about, so an instance filter never silently returns
// nothing. detail explains why nothing could be read.
func MissingQuotaObservations(instanceIDs []string, detail string) []QuotaObservation {
	seen := map[string]bool{}
	out := make([]QuotaObservation, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		instance := store.NormalizeInstanceID(id)
		if seen[instance] {
			continue
		}
		seen[instance] = true
		out = append(out, QuotaObservation{
			Instance: instance,
			Key:      quota.SettingKey(instance),
			Missing:  true,
			Detail:   detail,
		})
	}
	return out
}

// likePrefix escapes a literal prefix for a LIKE ... ESCAPE '\' pattern.
func likePrefix(prefix string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(prefix) + "%"
}

// SortQuotaObservations sorts in place by instance, then key. It is exported so
// the snapshot and the CLI agree on one order.
func SortQuotaObservations(obs []QuotaObservation) {
	sort.SliceStable(obs, func(i, j int) bool {
		if obs[i].Instance != obs[j].Instance {
			return obs[i].Instance < obs[j].Instance
		}
		return obs[i].Key < obs[j].Key
	})
}
