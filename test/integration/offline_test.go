//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

// offlineRun starts an offline migration and waits for its operation.
func (e *Env) offlineRun(id string, body map[string]any) *orch.Operation {
	e.t.Helper()
	e.do("POST", "/api/v1/migrations/"+id+"/start", body, nil, 200)
	c := e.migration(id).Migration.Flags.Cutover
	if c == nil || c.OpID == "" {
		e.t.Fatal("the offline start recorded no operation")
	}
	op := e.waitOp(c.OpID, 15*time.Minute)
	for _, s := range op.Steps {
		e.t.Logf("step %-10s %-8s %s", s.Key, s.State, s.Detail)
	}
	return op
}

// TestOfflineMigrationGO (overnight item 2, bug bash 7): an offline
// migration copies once with writes confirmed stopped, verifies, ends GO with
// identical data, and creates no slot, origin, publication or heartbeat.
func TestOfflineMigrationGO(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_offline")
	seedDB(t, db, 5000)
	exec1(t, srcConn, db, `CREATE TABLE nokey(a int, b text); INSERT INTO nokey SELECT g, 'x' FROM generate_series(1, 100) g;`)
	id := e.defineMigration("Offline", []string{db}, map[string]any{"mode": "offline"})
	var plan any
	e.do("GET", "/api/v1/migrations/"+id+"/plan", nil, &plan, 200)
	pb, _ := json.Marshal(plan)
	if strings.Contains(string(pb), "--follow") || strings.Contains(string(pb), "--slot-name") {
		t.Fatalf("the offline plan streams: %s", pb)
	}
	pv := e.preflight(id)
	for _, r := range pv.Results {
		if r.CheckID == "replica_identity" {
			t.Logf("replica_identity offline: %s %s", r.Level, r.Message)
			if r.Level != "info" || !strings.Contains(r.Message, "Not needed for an offline migration") {
				t.Errorf("a streaming-only check blocks an offline migration: %s", r.Level)
			}
		}
	}
	// Writes must be confirmed stopped (or frozen) before an offline copy.
	if c := e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil); c != 409 {
		t.Fatalf("an offline start without stopped writes was accepted (%d)", c)
	}
	op := e.offlineRun(id, map[string]any{"warnings_reviewed": true, "confirm_writers_stopped": true})
	m := e.migration(id).Migration
	if op.State != "done" || m.Flags.Verdict != "GO" {
		t.Fatalf("offline verdict %q: %s", m.Flags.Verdict, op.Error)
	}
	for _, tbl := range []string{"orders", "items", "nokey"} {
		s, d := q(t, srcConn, db, "SELECT count(*)::text FROM "+tbl), q(t, dstConn, db, "SELECT count(*)::text FROM "+tbl)
		if s != d {
			t.Errorf("%s: source %s rows, target %s", tbl, s, d)
		}
	}
	short := m.ShortID
	if n := q(t, srcConn, "defaultdb", "SELECT count(*)::text FROM pg_replication_slots WHERE slot_name LIKE 'upwell_"+short+"%'"); n != "0" {
		t.Error("an offline migration created a replication slot")
	}
	if n := q(t, dstConn, "defaultdb", "SELECT count(*)::text FROM pg_replication_origin WHERE roname LIKE 'upwell_"+short+"%'"); n != "0" {
		t.Error("an offline migration created a replication origin")
	}
	if n := q(t, srcConn, db, "SELECT count(*)::text FROM pg_namespace WHERE nspname='upwell'"); n != "0" {
		t.Error("an offline migration created the heartbeat schema")
	}
	res := verifyResults(t, e, id)
	if !strings.HasPrefix(res["writes_stopped"], "ok") || !strings.HasPrefix(res["checksums"], "ok") {
		t.Fatalf("verification: writes_stopped %s; checksums %s", res["writes_stopped"], res["checksums"])
	}
	if _, ok := res["origin_progress"]; ok {
		t.Error("an offline verification checked a replication origin")
	}
	e.cleanup(id, "Offline")
}

// TestOfflineWritesDuringCopyNoGo: a writer that starts after the writers
// step (during the copy) makes the offline migration NO-GO.
func TestOfflineWritesDuringCopyNoGo(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_offwr")
	seedDB(t, db, 20000)
	id := e.defineMigration("Offline writes", []string{db}, map[string]any{"mode": "offline"})
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true, "confirm_writers_stopped": true}, nil, 200)
	e.waitFor(2*time.Minute, "the copy step", func() bool {
		c := e.migration(id).Migration.Flags.Cutover
		return c != nil && (c.Phase == "copy" || c.EndedAt != 0)
	})
	w := startAppWriter(t, db, false, false)
	time.Sleep(3 * time.Second)
	c := e.migration(id).Migration.Flags.Cutover
	op := e.waitOp(c.OpID, 15*time.Minute)
	w.Stop()
	v := e.migration(id).Migration.Flags.Verdict
	t.Logf("operation %s: %s; verdict %s; the writer wrote %d rows", op.State, op.Error, v, w.ok.Load())
	if v == "GO" {
		t.Fatal("GO although the source changed during the offline copy")
	}
	if !blockerWith(t, e, id, "writes_stopped", "changed") {
		t.Fatal("NO-GO without the writes_stopped blocker")
	}
	e.cleanup(id, "Offline writes")
}

// TestOfflineWithFreeze: the write freeze before an offline copy ends a
// running application's writes, the migration ends GO, and cleanup undoes
// the freeze.
func TestOfflineWithFreeze(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_offfz")
	seedDB(t, db, 5000)
	// A property of the customer's: copied to the target, unlike the freeze.
	exec1(t, srcConn, db, `ALTER DATABASE `+db+` SET work_mem = '8MB'`)
	id := e.defineMigration("Offline freeze", []string{db}, map[string]any{"mode": "offline"})
	e.preflight(id)
	app := startAppWriter(t, db, false, false)
	defer app.Stop()
	time.Sleep(time.Second)
	var pv orch.FreezePreview
	e.do("GET", "/api/v1/migrations/"+id+"/cutover/freeze", nil, &pv, 200)
	op := e.offlineRun(id, map[string]any{"warnings_reviewed": true, "write_freeze": true, "freeze_confirm": pv.Confirm})
	app.Stop()
	m := e.migration(id).Migration
	t.Logf("verdict %s (%s); application %d ok, %d refused", m.Flags.Verdict, op.Error, app.ok.Load(), app.failed.Load())
	if m.Flags.Verdict != "GO" || app.failed.Load() == 0 {
		t.Fatalf("offline with the freeze: verdict %s, refused writes %d", m.Flags.Verdict, app.failed.Load())
	}
	if frozenSetting(t, db) != "on" {
		t.Fatal("the freeze is not kept after GO")
	}
	// The target is writable and has the customer's own property.
	props := q(t, dstConn, "defaultdb", `SELECT COALESCE(string_agg(s, ','), '') FROM pg_db_role_setting r, unnest(r.setconfig) s WHERE r.setdatabase = (SELECT oid FROM pg_database WHERE datname = '`+db+`') AND r.setrole = 0`)
	t.Logf("target database properties: %q", props)
	if strings.Contains(props, "default_transaction_read_only") || !strings.Contains(props, "work_mem=8MB") {
		t.Fatalf("target database properties: %q", props)
	}
	e.cleanup(id, "Offline freeze")
	if s := frozenSetting(t, db); s != "" {
		t.Fatalf("cleanup left the freeze applied: %q", s)
	}
}
