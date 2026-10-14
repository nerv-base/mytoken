package query

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/scan"
	"github.com/zzstar101/mytoken/internal/store"
)

// DiagnosticsObservation is one harness instance's persisted collection
// diagnostics, read read-only: what the last scan of that instance attempted,
// what it reached, and why.
//
// A record is keyed by harness *and* instance, and both are reported unchanged.
// Two harnesses can share an instance id (the built-in default), so collapsing
// them would put two different sources into one line. A zero count is only ever
// reported from a stored record: when nothing was stored, Missing is true and
// Record is nil, so "no data collected" can never be read as "zero usage".
type DiagnosticsObservation struct {
	// Harness is the harness the record belongs to, decoded from the settings
	// key. It is empty only for a Missing entry, where no record proves it.
	Harness model.Harness `json:"harness,omitempty"`
	// Instance is the instance id after normalisation, in index spelling.
	Instance string `json:"instance"`
	// Key is the exact settings key the record was stored under.
	Key string `json:"key,omitempty"`
	// Missing is true when no record exists for a requested instance. The
	// counts below are then absent, not zero.
	Missing bool `json:"missing,omitempty"`
	// Corrupt is true when a record exists but is not a valid record for this
	// harness and instance. It is reported instead of dropping the row.
	Corrupt bool `json:"corrupt,omitempty"`
	// Detail explains Missing or Corrupt in words.
	Detail string `json:"detail,omitempty"`
	// Record is the decoded record; nil when Missing or Corrupt.
	Record *scan.Diagnostics `json:"record,omitempty"`
}

// ReadDiagnostics reads the stored collection diagnostics read-only.
//
// With no instance filter it returns every stored record, ordered by harness
// then instance. With a filter it reports exactly the requested instances, in
// the order asked and deduplicated after normalisation: an instance with
// nothing stored is still reported, with Missing true, so an instance filter
// cannot hide a source that never collected anything. A stored record that does
// not decode is reported as Corrupt rather than failing the whole read, but a
// failure of the index itself is returned, because it says nothing about any
// single instance.
func ReadDiagnostics(ctx context.Context, ro *store.Store, instanceIDs []string) ([]DiagnosticsObservation, error) {
	if ro == nil {
		return nil, errors.New("query: nil store")
	}
	tx, err := ro.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out, err := readDiagnosticsTx(ctx, tx, instanceIDs)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func readDiagnosticsTx(ctx context.Context, tx *sql.Tx, instanceIDs []string) ([]DiagnosticsObservation, error) {
	keys, err := storedDiagnosticsKeys(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(instanceIDs) == 0 {
		out := make([]DiagnosticsObservation, 0, len(keys))
		for _, key := range keys {
			obs, err := readDiagnosticsKey(ctx, tx, key)
			if err != nil {
				return nil, err
			}
			out = append(out, obs)
		}
		sortDiagnosticsObservations(out)
		return out, nil
	}

	// A selection reports exactly the instances asked for, in the order asked.
	// Instances are matched after normalisation, because that is the spelling
	// the writer used.
	byInstance := map[string][]string{}
	for _, key := range keys {
		h, instance, ok := decodeDiagnosticsKey(key)
		if !ok {
			// A key under the prefix that our own key builder would not
			// produce. Keep it visible rather than letting it vanish.
			byInstance[""] = append(byInstance[""], key)
			continue
		}
		_ = h
		byInstance[instance] = append(byInstance[instance], key)
	}

	out := make([]DiagnosticsObservation, 0, len(instanceIDs))
	seen := make(map[string]bool, len(instanceIDs))
	for _, id := range instanceIDs {
		instance := store.NormalizeInstanceID(id)
		if seen[instance] {
			continue
		}
		seen[instance] = true
		matched := byInstance[instance]
		if len(matched) == 0 {
			// No record: the harness is unknowable from an absent key, and the
			// counts are absent rather than zero.
			out = append(out, DiagnosticsObservation{
				Instance: instance,
				Missing:  true,
				Detail:   "no collection diagnostics stored for this instance; a scan writes them",
			})
			continue
		}
		for _, key := range matched {
			obs, err := readDiagnosticsKey(ctx, tx, key)
			if err != nil {
				return nil, err
			}
			out = append(out, obs)
		}
	}
	return out, nil
}

// storedDiagnosticsKeys lists every diagnostics key in the settings table,
// ordered, so one read is one stable picture.
func storedDiagnosticsKeys(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT key FROM settings WHERE key LIKE ? ESCAPE '\' ORDER BY key`,
		likePrefix(scan.DiagnosticsSettingPrefix))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// readDiagnosticsKey decodes one stored key through scan's own loader, so the
// validation rules are the writer's and never a second copy of them.
func readDiagnosticsKey(ctx context.Context, tx *sql.Tx, key string) (DiagnosticsObservation, error) {
	h, instance, ok := decodeDiagnosticsKey(key)
	if !ok {
		return DiagnosticsObservation{
			Key:     key,
			Corrupt: true,
			Detail:  "stored key is not a valid collection diagnostics key",
		}, nil
	}
	rec, present, err := scan.LoadDiagnostics(ctx, txSettings{tx: tx}, h, instance)
	if err != nil {
		if errors.Is(err, scan.ErrCorruptDiagnostics) {
			return DiagnosticsObservation{
				Harness: h, Instance: instance, Key: key,
				Corrupt: true, Detail: err.Error(),
			}, nil
		}
		return DiagnosticsObservation{}, err
	}
	if !present {
		return DiagnosticsObservation{
			Harness: h, Instance: instance, Key: key,
			Corrupt: true,
			Detail:  "stored record is empty",
		}, nil
	}
	return DiagnosticsObservation{Harness: h, Instance: instance, Key: key, Record: &rec}, nil
}

// txSettings reads one settings key inside the read-only transaction, so every
// observation in a read comes from the same index state.
type txSettings struct{ tx *sql.Tx }

func (t txSettings) Setting(ctx context.Context, key string) (string, error) {
	var raw string
	err := t.tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return raw, err
}

// decodeDiagnosticsKey inverts scan.DiagnosticsSettingKey: it splits at the
// first dot and unescapes each half.
//
// The result is verified by regenerating the key with scan's own function, so
// this decoder can never drift from the writer's escaping, and a key this
// package would not have written is never guessed into a harness or instance.
func decodeDiagnosticsKey(key string) (model.Harness, string, bool) {
	rest, ok := strings.CutPrefix(key, scan.DiagnosticsSettingPrefix)
	if !ok {
		return "", "", false
	}
	sep := strings.Index(rest, ".")
	if sep <= 0 || sep == len(rest)-1 {
		return "", "", false
	}
	harness, okH := unescapeDiagnosticsKeyPart(rest[:sep])
	instance, okI := unescapeDiagnosticsKeyPart(rest[sep+1:])
	if !okH || !okI || instance == "" {
		return "", "", false
	}
	h := model.Harness(harness)
	if scan.DiagnosticsSettingKey(h, instance) != key {
		return "", "", false
	}
	return h, instance, true
}

// unescapeDiagnosticsKeyPart inverts diagnosticsKeyPart ("%" then "." escapes).
// An escape sequence it does not know is a rejection, not a guess.
func unescapeDiagnosticsKeyPart(s string) (string, bool) {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '%' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+3 > len(s) {
			return "", false
		}
		switch s[i : i+3] {
		case "%25":
			b.WriteByte('%')
		case "%2E":
			b.WriteByte('.')
		default:
			return "", false
		}
		i += 3
	}
	return b.String(), true
}

// sortDiagnosticsObservations orders records by harness then instance so the
// snapshot and the CLI agree on one order regardless of key collation.
func sortDiagnosticsObservations(obs []DiagnosticsObservation) {
	sort.SliceStable(obs, func(i, j int) bool { return lessDiagnostics(obs[i], obs[j]) })
}

func lessDiagnostics(a, b DiagnosticsObservation) bool {
	if a.Harness != b.Harness {
		return a.Harness < b.Harness
	}
	if a.Instance != b.Instance {
		return a.Instance < b.Instance
	}
	return a.Key < b.Key
}
