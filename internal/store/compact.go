package store

import (
	"database/sql"
	"strings"
)

// Two schema generations share this file.
//
// The *legacy* column sets are what every release before the instance dimension
// wrote. The base migrations (compact, signals, cache, daily) keep using them so
// an index written by any earlier release keeps its exact shape until the
// instance migration rebuilds it in one transaction. That migration, and the
// runtime write surface, use the *instance* column sets below.
//
// instance_id is a dimension: it is part of the event_dimensions dictionary key
// and it is carried on event_data so the fact primary key can hold the same raw
// dedup key independently in two instances. The stored dedup key itself is never
// prefixed or hashed; instance identity lives in its own column.
const legacyDimensionColumns = "harness,session_id,parent_id,project,model,provider,base_url,resolved_provider,attrib,raw_project"
const legacyFactColumns = "harness,dedup_key,dimension_id,timestamp,input,output,cache_read,cache_write,reasoning,log_cost,cost,priced"
const legacySignalFactColumns = legacyFactColumns + ",request_id,boundary,bill_amount,bill_unit"

const dimensionColumns = "harness,instance_id,session_id,parent_id,project,model,provider,base_url,resolved_provider,attrib,raw_project"
const factColumns = "harness,instance_id,dedup_key,dimension_id,timestamp,input,output,cache_read,cache_write,cache_write_1h,reasoning,log_cost,cost,priced,request_id,boundary,bill_amount,bill_unit"

// withInstance inserts the instance column directly after harness in a
// comma-separated dimension list. Every dimension list in the schema starts with
// harness, so one helper keeps both generations aligned.
func withInstance(columns string) string {
	return strings.Replace(columns, "harness,", "harness,instance_id,", 1)
}

// migrateCompact dictionaries repeated strings, retaining exact dedup keys and
// a stable integer rowid. The events view preserves the SQL read/update surface.
// All DDL and the copy run in the caller's migration transaction.
func migrateCompact(tx *sql.Tx) (bool, error) {
	var exists int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='event_data' AND type='table'`).Scan(&exists); err != nil {
		return false, err
	}
	if exists != 0 {
		return false, nil
	}
	_, err := tx.Exec(`
 DROP TRIGGER IF EXISTS daily_insert; DROP TRIGGER IF EXISTS daily_update; DROP TRIGGER IF EXISTS daily_delete;
 CREATE TABLE event_dimensions(id INTEGER PRIMARY KEY,harness TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL,project TEXT NOT NULL,model TEXT NOT NULL,provider TEXT NOT NULL,base_url TEXT NOT NULL,resolved_provider TEXT NOT NULL,attrib TEXT NOT NULL,raw_project TEXT NOT NULL,UNIQUE(` + legacyDimensionColumns + `));
 INSERT INTO event_dimensions(` + legacyDimensionColumns + `) SELECT DISTINCT ` + legacyDimensionColumns + ` FROM events;
 CREATE INDEX dimensions_session ON event_dimensions(harness,session_id);
 CREATE INDEX dimensions_raw_project ON event_dimensions(raw_project,project);
 CREATE TABLE event_data(harness TEXT NOT NULL,dedup_key TEXT NOT NULL,dimension_id INTEGER NOT NULL REFERENCES event_dimensions(id),timestamp INTEGER NOT NULL,input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL,log_cost REAL,cost REAL NOT NULL,priced INTEGER NOT NULL,PRIMARY KEY(harness,dedup_key));
 INSERT INTO event_data(rowid,` + legacyFactColumns + `) SELECT e.rowid,e.harness,e.dedup_key,d.id,e.timestamp,e.input,e.output,e.cache_read,e.cache_write,e.reasoning,e.log_cost,e.cost,e.priced FROM events e JOIN event_dimensions d ON ` + dimensionMatch("e", "d") + `;
 DROP TABLE events;
 CREATE VIEW events AS SELECT e.rowid AS rowid,e.harness,e.dedup_key,d.session_id,d.parent_id,d.project,e.timestamp,d.model,d.provider,d.base_url,e.input,e.output,e.cache_read,e.cache_write,e.reasoning,e.log_cost,d.resolved_provider,d.attrib,e.cost,e.priced,d.raw_project FROM event_data e JOIN event_dimensions d ON d.id=e.dimension_id;
 `)
	return true, err
}

// The compatibility view is assembled from these fragments so the legacy and
// instance generations expose the same read surface, with instance_id added
// directly after dedup_key. request_id onward are the signals columns.
const legacyEventsViewHead = `CREATE VIEW events AS SELECT e.rowid AS rowid,e.harness,e.dedup_key,d.session_id,d.parent_id,d.project,e.timestamp,d.model,d.provider,d.base_url,e.input,e.output,e.cache_read,e.cache_write,e.cache_write_1h,e.reasoning,e.log_cost,d.resolved_provider,d.attrib,e.cost,e.priced,d.raw_project`
const instanceEventsViewHead = `CREATE VIEW events AS SELECT e.rowid AS rowid,e.harness,e.instance_id,e.dedup_key,d.session_id,d.parent_id,d.project,e.timestamp,d.model,d.provider,d.base_url,e.input,e.output,e.cache_read,e.cache_write,e.cache_write_1h,e.reasoning,e.log_cost,d.resolved_provider,d.attrib,e.cost,e.priced,d.raw_project`
const eventsViewSignalsTail = `,e.request_id,e.boundary,e.bill_amount,e.bill_unit`
const eventsViewCacheTail = eventsViewSignalsTail + `,e.cache_expected,e.cache_miss,e.miss_cause,e.miss_cost`
const eventsViewJoin = ` FROM event_data e JOIN event_dimensions d ON d.id=e.dimension_id;`

// eventsViewHead picks the view head for one schema generation.
func eventsViewHead(instance bool) string {
	if instance {
		return instanceEventsViewHead
	}
	return legacyEventsViewHead
}

// migrateSignals upgrades already-compacted databases without rewriting facts.
func migrateSignals(tx *sql.Tx, sc rollupSchema) error {
	rows, err := tx.Query(`PRAGMA table_info(event_data)`)
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var id, nn, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&id, &name, &typ, &nn, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	changed := false
	for _, col := range []struct{ name, decl string }{{"request_id", "TEXT NOT NULL DEFAULT ''"}, {"boundary", "TEXT NOT NULL DEFAULT ''"}, {"bill_amount", "REAL"}, {"bill_unit", "TEXT NOT NULL DEFAULT ''"}} {
		if !columns[col.name] {
			if _, err = tx.Exec("ALTER TABLE event_data ADD COLUMN " + col.name + " " + col.decl); err != nil {
				return err
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if _, err = tx.Exec(`DROP VIEW events; ` + eventsViewHead(sc.instance) + eventsViewSignalsTail + eventsViewJoin); err != nil {
		return err
	}
	return createEventsTriggers(tx, sc)
}

// createEventsTriggers (re)creates the events INSTEAD OF triggers that decode the
// compatibility view into event_data. Shared by migrateSignals and migrateCache
// so the write surface stays identical when the view gains columns. Driven by the
// schema's fact columns; cache columns are never written through the view (they
// are derived and maintained internally), so they are intentionally absent here.
func createEventsTriggers(tx *sql.Tx, sc rollupSchema) error {
	dims := strings.Split(sc.dimensionColumns, ",")
	var dimVals, matches []string
	for _, c := range dims {
		dimVals = append(dimVals, newColExpr(c))
		matches = append(matches, newColExpr(c)+"=d."+c)
	}
	dimInsert := `INSERT INTO event_dimensions(` + sc.dimensionColumns + `) VALUES(` + strings.Join(dimVals, ",") + `) ON CONFLICT DO NOTHING;`
	dimID := `(SELECT id FROM event_dimensions d WHERE ` + strings.Join(matches, " AND ") + `)`
	var vals, updates []string
	for _, c := range strings.Split(sc.factColumns, ",") {
		v := newColExpr(c)
		if c == "dimension_id" {
			v = dimID
		} else if c == "request_id" || c == "boundary" || c == "bill_unit" {
			v = "coalesce(" + v + ",'')"
		}
		vals = append(vals, v)
		updates = append(updates, c+"="+v)
	}
	insert := `INSERT INTO event_data(` + sc.factColumns + `) VALUES(` + strings.Join(vals, ",") + `) ON CONFLICT(` + sc.conflict + `) DO UPDATE SET dimension_id=excluded.dimension_id,timestamp=excluded.timestamp,input=excluded.input,output=excluded.output,cache_read=excluded.cache_read,cache_write=excluded.cache_write,reasoning=excluded.reasoning,log_cost=excluded.log_cost,cost=excluded.cost,priced=excluded.priced,request_id=coalesce(nullif(excluded.request_id,''),event_data.request_id),boundary=coalesce(nullif(excluded.boundary,''),event_data.boundary),bill_amount=coalesce(excluded.bill_amount,event_data.bill_amount),bill_unit=CASE WHEN excluded.bill_amount IS NOT NULL THEN excluded.bill_unit ELSE event_data.bill_unit END WHERE (SELECT session_id=` + newColExpr("session_id") + ` OR (parent_id!='' AND ` + newColExpr("parent_id") + `='') FROM event_dimensions WHERE id=event_data.dimension_id);`
	_, err := tx.Exec(`CREATE TRIGGER events_insert INSTEAD OF INSERT ON events BEGIN ` + dimInsert + insert + ` END;
 CREATE TRIGGER events_update INSTEAD OF UPDATE ON events BEGIN ` + dimInsert + `UPDATE event_data SET ` + strings.Join(updates, ",") + ` WHERE rowid=OLD.rowid; END;
 CREATE TRIGGER events_delete INSTEAD OF DELETE ON events BEGIN DELETE FROM event_data WHERE rowid=OLD.rowid; END;`)
	return err
}

// newColExpr is the NEW-row expression for one column. instance_id is coalesced
// so a write through the compatibility view that predates the instance dimension
// (or an UPDATE that does not mention it) still lands on the reserved default
// instance instead of violating NOT NULL.
func newColExpr(c string) string {
	if c == "instance_id" {
		return "coalesce(nullif(NEW.instance_id,''),'" + DefaultInstanceID + "')"
	}
	return "NEW." + c
}

func dimensionMatch(a, b string) string {
	var equal []string
	for _, c := range strings.Split(legacyDimensionColumns, ",") {
		equal = append(equal, a+"."+c+"="+b+"."+c)
	}
	return strings.Join(equal, " AND ")
}
