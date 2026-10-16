//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

// TestRerunInPlace (D105): a migration that reached GO runs again with the
// same definition. Its slots and origins go, the target database is reset,
// the run is kept in the history, and the second run reaches GO with the same
// data, after a change to the database selection. A migration whose
// applications were switched to the target is never reset.
func TestRerunInPlace(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	a, b := uniq("it_rerun_a"), uniq("it_rerun_b")
	seedDB(t, a, 5000)
	seedDB(t, b, 2000)
	name := "Rerun in place"
	id := e.defineMigration(name, []string{a}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, a, 3*time.Minute, orch.DInSync)
	if op := e.startCutover(id); op.State != "done" {
		t.Fatalf("first cutover: %s", op.Error)
	}
	short := e.migration(id).Migration.ShortID

	// While applications are marked switched to the target: refused.
	e.do("POST", "/api/v1/migrations/"+id+"/switched", map[string]any{"confirm": name}, nil, 200)
	var er struct{ Code string }
	e.do("POST", "/api/v1/migrations/"+id+"/rerun", map[string]any{"confirm": name}, &er, 409)
	if er.Code != "apps_switched" {
		t.Fatalf("rerun after switching applications: %q", er.Code)
	}
	e.Store.DB.Exec(`UPDATE migrations SET flags = json_set(flags, '$.apps_switched', json('false')) WHERE id=?`, id)

	// Write on the target, as an application would; the reset must remove it.
	exec1(t, dstConn, a, "INSERT INTO orders(id, customer, note) VALUES (999999901, 1, 'written on the target')")
	var r struct {
		OperationID string `json:"operation_id"`
	}
	e.do("POST", "/api/v1/migrations/"+id+"/rerun", map[string]any{"confirm": name}, &r, 202)
	if op := e.waitOp(r.OperationID, 5*time.Minute); op.State != "done" {
		for _, s := range op.Steps {
			t.Logf("step %s %s %s", s.Key, s.State, s.Detail)
		}
		t.Fatalf("rerun: %s", op.Error)
	}
	v := e.migration(id)
	if v.Migration.Flags.Started || v.Migration.Flags.Verdict != "" || v.Migration.StartedAt != nil {
		t.Fatalf("after rerun: flags %+v started_at %v", v.Migration.Flags, v.Migration.StartedAt)
	}
	if d := e.db(id, a); d.State != orch.DPending || d.BaseCopyDone {
		t.Fatalf("database after rerun: %s, base copy done %v", d.State, d.BaseCopyDone)
	}
	noLeftovers(t, short)
	if n := q(t, dstConn, "defaultdb", "SELECT count(*)::text FROM pg_database WHERE datname='"+a+"'"); n != "0" {
		t.Fatalf("the target database was not reset (%s)", n)
	}
	var runs []orch.Run
	e.do("GET", "/api/v1/runs", nil, &runs, 200)
	mine := func() []orch.Run {
		var out []orch.Run
		for _, x := range runs {
			if x.MigrationID == id {
				out = append(out, x)
			}
		}
		return out
	}
	if m := mine(); len(m) != 1 || m[0].Outcome != "GO" || m[0].Summary.BaseCopyMs <= 0 || len(m[0].Summary.Databases) != 1 || !m[0].HasReport {
		t.Fatalf("history after the first run: %+v", m)
	}
	code, rep := e.raw("GET", "/api/v1/runs/"+mine()[0].ID+"/report")
	if code != 200 || !strings.Contains(rep, name) {
		t.Fatalf("stored report: %d", code)
	}

	// The definition is editable again: add the second database, then run.
	e.do("PUT", "/api/v1/migrations/"+id+"/databases", []map[string]any{
		{"source_name": a, "target_name": a, "include": true}, {"source_name": b, "target_name": b, "include": true}}, nil, 200)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, a, 3*time.Minute, orch.DInSync)
	e.waitDBState(id, b, 3*time.Minute, orch.DInSync)
	exec1(t, srcConn, a, "INSERT INTO orders(customer, note) SELECT g, 'second run' FROM generate_series(1, 500) g")
	if op := e.startCutover(id); op.State != "done" {
		t.Fatalf("second cutover: %s", op.Error)
	}
	for _, db := range []string{a, b} {
		if s, d := q(t, srcConn, db, "SELECT count(*)::text FROM orders"), q(t, dstConn, db, "SELECT count(*)::text FROM orders"); s != d {
			t.Fatalf("%s: source %s rows, target %s", db, s, d)
		}
	}
	if n := q(t, dstConn, a, "SELECT count(*)::text FROM orders WHERE id = 999999901"); n != "0" {
		t.Fatal("the row written on the target survived the reset")
	}
	e.waitFor(time.Minute, "the second run in the history", func() bool {
		e.do("GET", "/api/v1/runs", nil, &runs, 200)
		return len(mine()) == 2
	})
	if m := mine(); m[0].Outcome != "GO" || len(m[0].Summary.Databases) != 2 || m[0].Number != 2 {
		t.Fatalf("history after the second run: %+v", m[0])
	}
	e.cleanup(id, name)
}
