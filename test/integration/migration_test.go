//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

// TestHappyPath: define, preflight, start, stream with writes, stop and
// resume one database, cut over to GO, report, clean up with nothing left.
func TestHappyPath(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	a, b := uniq("it_happy_a"), uniq("it_happy_b")
	seedDB(t, a, 50000)
	seedDB(t, b, 20000)
	id := e.defineMigration("Happy path", []string{a, b}, nil)
	pv := e.preflight(id)
	for _, r := range pv.Results {
		if r.Level == "blocker" {
			t.Fatalf("unexpected blocker %s %s: %s", r.CheckID, r.Database, r.Message)
		}
	}
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, a, 3*time.Minute, orch.DInSync)
	e.waitDBState(id, b, 3*time.Minute, orch.DInSync)
	e.waitFor(30*time.Second, "migration state streaming", func() bool { return e.migration(id).Migration.State == orch.MStreaming })
	w := startWriter(t, a)
	time.Sleep(3 * time.Second)
	// Stop and resume one database while writes continue.
	var d orch.Database
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+a+"/stop", nil, &d, 200)
	if d.State != orch.DStopped {
		t.Fatalf("after stop: %s", d.State)
	}
	if n := q(t, srcConn, "defaultdb", "SELECT count(*)::text FROM pg_replication_slots WHERE active AND database='"+a+"'"); n != "0" {
		t.Fatalf("slot still active after stop: %s", n)
	}
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+a+"/resume", nil, nil, 200)
	e.waitDBState(id, a, 2*time.Minute, orch.DInSync)
	time.Sleep(2 * time.Second)
	w.Stop()
	t.Logf("writer committed %d transactions", w.n)
	op := e.startCutover(id)
	if op.State != "done" {
		for _, st := range op.Steps {
			t.Logf("step %-10s %-8s %s", st.Key, st.State, st.Detail)
		}
		var ver struct {
			Results []orch.VerificationRow `json:"results"`
		}
		e.do("GET", "/api/v1/migrations/"+id+"/verification", nil, &ver, 200)
		for _, r := range ver.Results {
			t.Logf("verify %s %s %s %s", r.Database, r.CheckID, r.Result, r.Detail)
		}
		t.Fatalf("cutover failed: %s", op.Error)
	}
	v := e.migration(id)
	if v.Migration.Flags.Verdict != "GO" || v.Migration.State != orch.MCompleted {
		t.Fatalf("verdict %s state %s", v.Migration.Flags.Verdict, v.Migration.State)
	}
	if got, want := q(t, dstConn, a, "SELECT count(*)::text FROM orders"), q(t, srcConn, a, "SELECT count(*)::text FROM orders"); got != want {
		t.Fatalf("orders: target %s source %s", got, want)
	}
	code, html := e.raw("GET", "/api/v1/migrations/"+id+"/report?format=html")
	if code != 200 || !strings.Contains(html, "GO") || !strings.Contains(html, a) {
		t.Fatalf("report: %d", code)
	}
	code, md := e.raw("GET", "/api/v1/migrations/"+id+"/report?format=md")
	if code != 200 || !strings.Contains(md, "verdict **GO**") {
		t.Fatalf("markdown report: %d %s", code, md[:min(200, len(md))])
	}
	e.cleanup(id, "Happy path")
	noLeftovers(t, v.Migration.ShortID)
	if q(t, srcConn, a, "SELECT to_regclass('upwell.heartbeat')::text") != "" {
		t.Fatal("heartbeat table left on the source")
	}
}

// TestS02MissingSetPrivilege: without SET on session_replication_role the
// preflight blocks hard; forcing past preflight never reaches GO.
func TestS02MissingSetPrivilege(t *testing.T) {
	superSQL(t, dstPort, "postgres", "REVOKE SET ON PARAMETER session_replication_role FROM doadmin")
	t.Cleanup(func() { superSQL(t, dstPort, "postgres", "GRANT SET ON PARAMETER session_replication_role TO doadmin") })
	e := newEnv(t, "", orch.TestHooks{SkipPreflightGate: true})
	name := uniq("it_s02")
	seedDB(t, name, 5000)
	id := e.defineMigration("S02", []string{name}, nil)
	pv := e.preflight(id)
	found := false
	for _, r := range pv.Results {
		if r.CheckID == "target_apply_privileges" {
			found = true
			if r.Level != "blocker" || !r.Hard {
				t.Fatalf("target_apply_privileges is %s hard=%v: %s", r.Level, r.Hard, r.Message)
			}
		}
	}
	if !found {
		t.Fatal("target_apply_privileges did not run")
	}
	// Hard blockers cannot be accepted.
	for _, r := range pv.Results {
		if r.CheckID == "target_apply_privileges" {
			e.do("POST", "/api/v1/migrations/"+id+"/acceptances", map[string]any{"result_id": r.ID, "reason": "trying to accept a hard blocker"}, nil, 409)
		}
	}
	// Forced past preflight (test hook): the engine is never started without the privilege.
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	d := e.waitDBState(id, name, 2*time.Minute, orch.DFailed, orch.DRestartRequired)
	if !strings.Contains(d.LastError, "session_replication_role") {
		t.Fatalf("unexpected error: %s", d.LastError)
	}
	if v := e.migration(id).Migration.Flags.Verdict; v == "GO" {
		t.Fatal("GO without the apply privilege")
	}
	e.abortAndClean(id, "S02")
}

// TestS02PrivilegeLostMidStream: the privilege disappears after the engine
// started (F2 in its real form: pgcopydb applies nothing and reports success).
// The cutover must end NO-GO.
func TestS02PrivilegeLostMidStream(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	name := uniq("it_s02b")
	seedDB(t, name, 5000)
	id := e.defineMigration("S02 mid-stream", []string{name}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, name, 3*time.Minute, orch.DInSync)
	superSQL(t, dstPort, "postgres", "REVOKE SET ON PARAMETER session_replication_role FROM doadmin")
	t.Cleanup(func() { superSQL(t, dstPort, "postgres", "GRANT SET ON PARAMETER session_replication_role TO doadmin") })
	// New engine sessions now lack the privilege: restart the engine's apply
	// by stopping and resuming the database.
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+name+"/stop", nil, nil, 200)
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+name+"/resume", nil, nil, 200)
	w := startWriter(t, name)
	time.Sleep(6 * time.Second)
	w.Stop()
	e.waitDBState(id, name, 2*time.Minute, orch.DInSync)
	op := e.startCutover(id)
	v := e.migration(id)
	if v.Migration.Flags.Verdict != "NO-GO" {
		t.Fatalf("verdict %q (op %s: %s), want NO-GO", v.Migration.Flags.Verdict, op.State, op.Error)
	}
	superSQL(t, dstPort, "postgres", "GRANT SET ON PARAMETER session_replication_role TO doadmin")
	e.abortAndClean(id, "S02 mid-stream")
}

// TestS09LastBatchMissing: the last large transaction before the end
// position goes missing on the target together with the final heartbeat,
// which is what pgcopydb 0.18 did in Phase 0 (F9). The loss is injected after
// the drain; verification must end NO-GO and name the table.
func TestS09LastBatchMissing(t *testing.T) {
	name := uniq("it_s09")
	hook := func(ctx context.Context, d orch.Database) {
		exec1(t, dstConn, d.TargetName, "DELETE FROM orders WHERE note='last-batch'")
		exec1(t, dstConn, d.TargetName, "DELETE FROM upwell.heartbeat WHERE kind='final'")
	}
	e := newEnv(t, "", orch.TestHooks{BeforeVerify: hook})
	seedDB(t, name, 5000)
	id := e.defineMigration("S09", []string{name}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, name, 3*time.Minute, orch.DInSync)
	exec1(t, srcConn, name, "INSERT INTO orders(customer, amount, note) SELECT g%1000, 1, 'last-batch' FROM generate_series(1,20000) g")
	time.Sleep(3 * time.Second)
	e.waitDBState(id, name, 2*time.Minute, orch.DInSync)
	e.startCutover(id)
	v := e.migration(id)
	if v.Migration.Flags.Verdict != "NO-GO" {
		t.Fatalf("verdict %q, want NO-GO", v.Migration.Flags.Verdict)
	}
	var ver struct {
		Results []orch.VerificationRow `json:"results"`
	}
	e.do("GET", "/api/v1/migrations/"+id+"/verification", nil, &ver, 200)
	var counts, hb bool
	for _, r := range ver.Results {
		if r.CheckID == "row_counts" && r.Result == "blocker" && strings.Contains(string(r.Detail), "public.orders") {
			counts = true
		}
		if r.CheckID == "final_heartbeat" && r.Result == "blocker" {
			hb = true
		}
	}
	if !counts || !hb {
		t.Fatalf("expected row_counts and final_heartbeat blockers naming public.orders: %+v", ver.Results)
	}
	e.cleanup(id, "S09")
	noLeftovers(t, v.Migration.ShortID)
}

// TestVerificationChecksumMismatch: same row count, different contents.
func TestVerificationChecksumMismatch(t *testing.T) {
	name := uniq("it_sum")
	hook := func(ctx context.Context, d orch.Database) {
		exec1(t, dstConn, d.TargetName, "UPDATE orders SET amount = amount + 0.01 WHERE id = 7")
	}
	e := newEnv(t, "", orch.TestHooks{BeforeVerify: hook})
	seedDB(t, name, 3000)
	id := e.defineMigration("Checksum", []string{name}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, name, 3*time.Minute, orch.DInSync)
	e.startCutover(id)
	v := e.migration(id)
	if v.Migration.Flags.Verdict != "NO-GO" {
		t.Fatalf("verdict %q, want NO-GO", v.Migration.Flags.Verdict)
	}
	var ver struct {
		Results []orch.VerificationRow `json:"results"`
	}
	e.do("GET", "/api/v1/migrations/"+id+"/verification", nil, &ver, 200)
	ok := false
	for _, r := range ver.Results {
		if r.CheckID == "checksums" && r.Result == "blocker" && strings.Contains(string(r.Detail), "public.orders") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("expected a checksum blocker naming public.orders: %+v", ver.Results)
	}
	e.cleanup(id, "Checksum")
}
