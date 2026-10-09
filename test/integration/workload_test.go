//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

// Bug bash 2: complex workloads. Each scenario must end with identical data,
// or be caught early (preflight, an alert, readiness) or at the verdict.

const complexSchema = `
	CREATE MATERIALIZED VIEW mv_totals AS SELECT customer, sum(amount) AS total, count(*) AS n FROM orders GROUP BY customer;
	CREATE TABLE readings(id bigint, region text, val float8, PRIMARY KEY (id, region)) PARTITION BY LIST (region);
	CREATE TABLE readings_east PARTITION OF readings FOR VALUES IN ('east');
	CREATE TABLE readings_west PARTITION OF readings FOR VALUES IN ('west');
	INSERT INTO readings SELECT g, CASE WHEN g%2=0 THEN 'east' ELSE 'west' END, g/7.0 FROM generate_series(1, 2000) g;
	CREATE TABLE audit_full(at timestamptz, who text, amount float8, payload jsonb, tags text[]);
	ALTER TABLE audit_full REPLICA IDENTITY FULL;
	INSERT INTO audit_full SELECT now() - g*interval '1 min', 'user'||(g%50), g*1.1, jsonb_build_object('g', g), ARRAY['a'||g] FROM generate_series(1, 1000) g;
	SELECT lo_from_bytea(0, convert_to(repeat('blob '||g, 200), 'UTF8')) FROM generate_series(1, 20) g;`

func TestComplexWorkloadGO(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_cplx")
	seedDB(t, db, 5000)
	exec1(t, srcConn, db, complexSchema)
	id := e.defineMigration("Complex workload", []string{db}, nil)
	pv := e.preflight(id)
	for _, r := range pv.Results {
		if r.CheckID == "identity_full_duplicates" || r.CheckID == "large_objects" {
			t.Logf("preflight %s: %s %s", r.CheckID, r.Level, r.Message)
		}
	}
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	// While streaming: rows move between partitions, a transaction with 200
	// savepoints (an ORM pattern), updates and deletes found by every column
	// (float8, jsonb, arrays), new orders, and a refresh of the view.
	exec1(t, srcConn, db, `UPDATE readings SET region = 'west' WHERE region = 'east' AND id % 10 = 0;`)
	var sp strings.Builder
	sp.WriteString("BEGIN;")
	for i := 0; i < 200; i++ {
		sp.WriteString("SAVEPOINT s; INSERT INTO orders(customer, amount, note) VALUES (7, 1.5, 'savepoint'); ")
		if i%3 == 0 {
			sp.WriteString("ROLLBACK TO SAVEPOINT s; ")
		} else {
			sp.WriteString("RELEASE SAVEPOINT s; ")
		}
	}
	sp.WriteString("COMMIT;")
	exec1(t, srcConn, db, sp.String())
	exec1(t, srcConn, db, `UPDATE audit_full SET amount = amount * 3, payload = payload || '{"x":1}' WHERE who IN ('user1','user2');
		DELETE FROM audit_full WHERE who = 'user3';
		INSERT INTO orders(customer, amount, note) SELECT g, g, 'late' FROM generate_series(1, 300) g;
		REFRESH MATERIALIZED VIEW mv_totals;`)
	if v := outcome(t, e, id, db); v != "GO" {
		verifyResults(t, e, id)
		t.Fatalf("verdict %s, want GO", v)
	}
	res := verifyResults(t, e, id)
	for _, c := range []string{"large_objects", "materialized_views"} {
		if !strings.HasPrefix(res[c], "ok") {
			t.Errorf("%s: %s", c, res[c])
		}
	}
	if got, want := q(t, dstConn, db, `SELECT count(*) FROM orders WHERE note='savepoint'`), "133"; got != want {
		t.Errorf("savepoint rows on the target: %s, want %s", got, want)
	}
	e.cleanup(id, "Complex workload")
}

func TestLargeObjectChangedWhileStreaming(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_lo")
	seedDB(t, db, 2000)
	exec1(t, srcConn, db, `SELECT lo_from_bytea(0, 'before') FROM generate_series(1, 5);`)
	id := e.defineMigration("Large object", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	exec1(t, srcConn, db, `SELECT lo_from_bytea(0, 'created while streaming');`)
	if v := outcome(t, e, id, db); v != "NO-GO" {
		t.Fatalf("verdict %s with a large object created while streaming, want NO-GO", v)
	}
	if r := verifyResults(t, e, id)["large_objects"]; !strings.HasPrefix(r, "blocker") {
		t.Fatalf("large_objects: %s", r)
	}
	e.abortAndClean(id, "Large object")
}

// TestSchemaDriftCaughtWhileStreaming: next month's partition created by a
// job, and an enum value added by a deploy. Both would break or invalidate
// the migration; Upwell must say so within minutes, not at the cutover.
func TestSchemaDriftCaughtWhileStreaming(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_drift")
	seedDB(t, db, 2000)
	exec1(t, srcConn, db, `CREATE TYPE status AS ENUM ('new', 'paid');
		CREATE TABLE events(id bigint, month date, s status, PRIMARY KEY (id, month)) PARTITION BY RANGE (month);
		CREATE TABLE events_2026_10 PARTITION OF events FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
		INSERT INTO events SELECT g, '2026-10-15', 'new' FROM generate_series(1, 100) g;`)
	id := e.defineMigration("Schema drift", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	exec1(t, srcConn, db, `CREATE TABLE events_2026_11 PARTITION OF events FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');
		ALTER TYPE status ADD VALUE 'refunded';`)
	var rd struct {
		Ready      bool             `json:"ready"`
		Conditions []orch.Condition `json:"conditions"`
	}
	e.waitFor(3*time.Minute, "readiness to report the schema change", func() bool {
		e.do("GET", "/api/v1/migrations/"+id+"/cutover/readiness", nil, &rd, 200)
		for _, c := range rd.Conditions {
			if c.Key == "schema" && !c.OK {
				t.Logf("readiness: %s: %s", c.Label, c.Detail)
				return !rd.Ready && strings.Contains(c.Detail, "events_2026_11") && strings.Contains(c.Detail, "status")
			}
		}
		return false
	})
	var alerts []map[string]any
	e.waitFor(time.Minute, "the schema drift alert", func() bool {
		e.do("GET", "/api/v1/alerts?state=firing", nil, &alerts, 200)
		for _, a := range alerts {
			if a["rule"] == "schema_drift" {
				t.Logf("alert: %v", a["message"])
				return true
			}
		}
		return false
	})
	e.abortAndClean(id, "Schema drift")
}

func TestPreflightIdentityFullTypes(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_idfull")
	seedDB(t, db, 100)
	exec1(t, srcConn, db, `CREATE TABLE docs(body json, at point); ALTER TABLE docs REPLICA IDENTITY FULL;`)
	id := e.defineMigration("Identity full types", []string{db}, nil)
	pv := e.preflight(id)
	found := false
	for _, r := range pv.Results {
		if r.CheckID == "identity_full_types" {
			t.Logf("preflight %s: %s hard=%v %s", r.CheckID, r.Level, r.Hard, r.Message)
			found = r.Level == "blocker" && r.Hard && strings.Contains(r.Message, "docs")
		}
	}
	if !found {
		t.Fatal("no hard blocker for json and point columns under REPLICA IDENTITY FULL")
	}
	if c := e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil); c < 400 {
		t.Fatalf("start accepted (%d)", c)
	}
}
