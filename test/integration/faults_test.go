//go:build integration

package integration

import (
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

type dbDetail struct {
	Database orch.Database `json:"database"`
	Unit     *struct {
		Active   bool  `json:"active"`
		MainPID  int   `json:"main_pid"`
		MainLive bool  `json:"main_live"`
		Procs    []int `json:"procs"`
	} `json:"unit"`
}

func (e *Env) mainPID(id, db string) int {
	e.t.Helper()
	var d dbDetail
	e.waitFor(30*time.Second, "the engine main process", func() bool {
		e.do("GET", "/api/v1/migrations/"+id+"/databases/"+db, nil, &d, 200)
		return d.Unit != nil && d.Unit.MainPID > 0 && d.Unit.MainLive
	})
	return d.Unit.MainPID
}

func (e *Env) mustGO(id string) {
	e.t.Helper()
	op := e.startCutover(id)
	v := e.migration(id)
	if op.State != "done" || v.Migration.Flags.Verdict != "GO" {
		for _, st := range op.Steps {
			e.t.Logf("step %-10s %-8s %s", st.Key, st.State, st.Detail)
		}
		e.t.Fatalf("expected GO, got %s (%s)", v.Migration.Flags.Verdict, op.Error)
	}
}

func (e *Env) mustNOGO(id string) orch.Operation {
	e.t.Helper()
	op := e.startCutover(id)
	v := e.migration(id)
	if v.Migration.Flags.Verdict != "NO-GO" {
		e.t.Fatalf("expected NO-GO, got %q", v.Migration.Flags.Verdict)
	}
	return *op
}

// TestFaultSIGKILLMainDuringCDC: the engine's main process dies while
// streaming. The supervisor stops what is left of the unit, resumes from the
// slot, and the migration still reaches GO with every row.
func TestFaultSIGKILLMainDuringCDC(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	a := uniq("it_kill_cdc")
	seedDB(t, a, 30000)
	id := e.defineMigration("Kill main during CDC", []string{a}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, a, 3*time.Minute, orch.DInSync)
	w := startWriter(t, a)
	time.Sleep(2 * time.Second)
	pid := e.mainPID(id, a)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill %d: %v", pid, err)
	}
	e.waitFor(2*time.Minute, "an automatic resume", func() bool { return e.db(id, a).RetryCount >= 1 })
	e.waitDBState(id, a, 3*time.Minute, orch.DInSync)
	if p2 := e.mainPID(id, a); p2 == pid {
		t.Fatalf("main process %d did not change", pid)
	}
	time.Sleep(2 * time.Second)
	w.Stop()
	e.mustGO(id)
	if got, want := q(t, dstConn, a, "SELECT count(*)::text FROM orders"), q(t, srcConn, a, "SELECT count(*)::text FROM orders"); got != want {
		t.Fatalf("orders: target %s source %s", got, want)
	}
	e.cleanup(id, "Kill main during CDC")
}

// TestFaultSIGKILLDuringBaseCopy: a base copy cannot be resumed safely, so the
// database goes to Restart required; restarting from zero then succeeds.
func TestFaultSIGKILLDuringBaseCopy(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	a := uniq("it_kill_copy")
	seedDB(t, a, 1500000)
	id := e.defineMigration("Kill during base copy", []string{a}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, a, time.Minute, orch.DBaseCopy)
	time.Sleep(time.Second)
	pid := e.mainPID(id, a)
	syscall.Kill(pid, syscall.SIGKILL)
	d := e.waitDBState(id, a, 2*time.Minute, orch.DRestartRequired, orch.DFailed)
	t.Logf("after kill: %s (%s) %s", d.State, d.ErrorClass, d.LastError)
	if d.State != orch.DRestartRequired {
		t.Fatalf("state %s, want restart_required", d.State)
	}
	// Resume must refuse: the copy would not be consistent.
	if code := e.do("POST", "/api/v1/migrations/"+id+"/databases/"+a+"/resume", nil, nil); code < 400 {
		t.Fatalf("resume during base copy was accepted (%d)", code)
	}
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+a+"/restart", map[string]any{"confirm": a}, nil, 200)
	e.waitDBState(id, a, 5*time.Minute, orch.DInSync)
	e.mustGO(id)
	e.cleanup(id, "Kill during base copy")
}

// TestFaultAppKilledDuringCDC: the app goes away while engines stream; a new
// process on the same data directory reattaches without restarting them.
func TestFaultAppKilledDuringCDC(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	a := uniq("it_app_kill")
	seedDB(t, a, 20000)
	id := e.defineMigration("App killed during CDC", []string{a}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, a, 3*time.Minute, orch.DInSync)
	pid := e.mainPID(id, a)
	dir := e.Dir
	e.Close()
	w := startWriter(t, a)
	time.Sleep(5 * time.Second)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("engine %d died with the app: %v", pid, err)
	}
	e2 := newEnv(t, dir, orch.TestHooks{})
	defer func() {
		if e2.migration(id).Migration.Flags.Verdict == "" {
			e2.abortAndClean(id, "App killed during CDC")
		}
	}()
	e2.waitDBState(id, a, 2*time.Minute, orch.DInSync)
	if p2 := e2.mainPID(id, a); p2 != pid {
		t.Fatalf("engine was restarted (%d -> %d); it should have been reattached", pid, p2)
	}
	time.Sleep(2 * time.Second)
	w.Stop()
	e2.mustGO(id)
	e2.cleanup(id, "App killed during CDC")
}

// TestFaultStaleWalsender: something else holds the slot when the database
// resumes; Upwell terminates the stale walsender and resumes.
func TestFaultStaleWalsender(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	a := uniq("it_stale_ws")
	seedDB(t, a, 10000)
	id := e.defineMigration("Stale walsender", []string{a}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	d := e.waitDBState(id, a, 3*time.Minute, orch.DInSync)
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+a+"/stop", nil, nil, 200)
	// Hold the slot with a foreign walsender.
	cmd := exec.Command("runuser", "-u", "postgres", "--", "pg_recvlogical", "-h", sockDir, "-p", strconv.Itoa(srcPort), "-d", a, "--slot", d.SlotName, "--start", "-f", "/dev/null", "-o", "proto_version=1", "-o", "publication_names="+d.SlotName)
	if d.Plugin == "test_decoding" {
		cmd = exec.Command("runuser", "-u", "postgres", "--", "pg_recvlogical", "-h", sockDir, "-p", strconv.Itoa(srcPort), "-d", a, "--slot", d.SlotName, "--start", "-f", "/dev/null")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	e.waitFor(20*time.Second, "the foreign walsender to hold the slot", func() bool {
		return q(t, srcConn, "defaultdb", "SELECT active::text FROM pg_replication_slots WHERE slot_name='"+d.SlotName+"'") == "true"
	})
	start := time.Now()
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+a+"/resume", nil, nil, 200)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the stale walsender was not terminated")
	}
	t.Logf("slot released after %s", time.Since(start).Round(time.Second))
	e.waitDBState(id, a, 2*time.Minute, orch.DInSync)
	e.mustGO(id)
	e.cleanup(id, "Stale walsender")
}

// TestFaultDoubleSubmit: the same command sent twice with one key acts once;
// a second cutover with a new key is refused while the first runs.
func TestFaultDoubleSubmit(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	a := uniq("it_double")
	seedDB(t, a, 10000)
	id := e.defineMigration("Double submit", []string{a}, nil)
	e.preflight(id)
	key := fmt.Sprintf("start-%d", time.Now().UnixNano())
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = e.doKey("POST", "/api/v1/migrations/"+id+"/start", key, map[string]any{"warnings_reviewed": true}, nil)
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("submission %d: status %d (the replay should return the first response)", i, c)
		}
	}
	var n int
	e.Store.DB.QueryRow(`SELECT count(*) FROM audit_log WHERE action='migration.start' AND target=?`, id).Scan(&n)
	if n != 1 {
		t.Fatalf("start recorded %d times", n)
	}
	e.waitDBState(id, a, 3*time.Minute, orch.DInSync)
	// Two cutovers with different keys: the second is refused.
	e.waitFor(3*time.Minute, "readiness", func() bool {
		var rd struct {
			Ready bool `json:"ready"`
		}
		e.do("GET", "/api/v1/migrations/"+id+"/cutover/readiness", nil, &rd, 200)
		return rd.Ready
	})
	var first struct {
		OperationID string `json:"operation_id"`
	}
	e.do("POST", "/api/v1/migrations/"+id+"/cutover", map[string]any{"confirm_writers_stopped": true}, &first, 202)
	if code := e.do("POST", "/api/v1/migrations/"+id+"/cutover", map[string]any{"confirm_writers_stopped": true}, nil); code < 400 {
		t.Fatalf("a second concurrent cutover was accepted (%d)", code)
	}
	op := e.waitOp(first.OperationID, 5*time.Minute)
	if op.State != "done" {
		t.Fatalf("cutover %s: %s", op.State, op.Error)
	}
	e.cleanup(id, "Double submit")
}

// TestFaultUnloggedTable: changes to an unlogged table are not decoded by
// pgoutput (or any plugin). Preflight warns, and if the warning is reviewed
// anyway, verification must catch the lost rows: NO-GO.
func TestFaultUnloggedTable(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	a := uniq("it_unlogged")
	seedDB(t, a, 5000)
	exec1(t, srcConn, a, `CREATE UNLOGGED TABLE scratch(id int PRIMARY KEY, v text); INSERT INTO scratch SELECT g, 'x' FROM generate_series(1,100) g;`)
	id := e.defineMigration("Unlogged table", []string{a}, nil)
	pv := e.preflight(id)
	found := false
	for _, r := range pv.Results {
		if r.CheckID == "unlogged_tables" && r.Level == "warning" {
			found = true
		}
	}
	if !found {
		t.Fatal("preflight did not warn about the unlogged table")
	}
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, a, 3*time.Minute, orch.DInSync)
	exec1(t, srcConn, a, `INSERT INTO scratch SELECT g, 'late' FROM generate_series(101,150) g;`)
	time.Sleep(3 * time.Second)
	op := e.mustNOGO(id)
	t.Logf("cutover %s: %s", op.State, op.Error)
	e.cleanup(id, "Unlogged table")
}

// TestFaultNameConflictOnTarget: a table that already exists in the target
// database is a hard blocker; Start is refused.
func TestFaultNameConflictOnTarget(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	a := uniq("it_conflict")
	seedDB(t, a, 1000)
	superSQL(t, dstPort, "postgres", "CREATE DATABASE "+a+" OWNER doadmin")
	t.Cleanup(func() { superSQL(t, dstPort, "postgres", "DROP DATABASE IF EXISTS "+a+" WITH (FORCE)") })
	exec1(t, dstConn, a, `CREATE TABLE orders(id int)`)
	id := e.defineMigration("Name conflict", []string{a}, nil)
	pv := e.preflight(id)
	hard := false
	for _, r := range pv.Results {
		if r.CheckID == "dst_conflicts" && r.Level == "blocker" && r.Hard {
			hard = true
			// Hard blockers cannot be accepted.
			if code := e.do("POST", "/api/v1/migrations/"+id+"/acceptances", map[string]any{"result_id": r.ID, "reason": "testing that this is refused"}, nil); code < 400 {
				t.Fatalf("accepting a hard blocker returned %d", code)
			}
		}
	}
	if !hard {
		t.Fatal("no hard dst_conflicts blocker")
	}
	if code := e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil); code < 400 {
		t.Fatalf("start with a hard blocker returned %d", code)
	}
	if e.migration(id).Migration.Flags.Started {
		t.Fatal("migration started despite the hard blocker")
	}
}

// TestRBAC: every mutating endpoint refuses viewers; operators cannot manage
// users or settings; unauthenticated calls get 401.
func TestRBAC(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	var m orch.Migration
	e.do("POST", "/api/v1/migrations", map[string]any{"name": "RBAC"}, &m, 201)
	e.login("viewer")
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/migrations"}, {"PATCH", "/api/v1/migrations/" + m.ID}, {"PUT", "/api/v1/migrations/" + m.ID + "/permission"},
		{"POST", "/api/v1/migrations/" + m.ID + "/preflight"}, {"POST", "/api/v1/migrations/" + m.ID + "/start"}, {"POST", "/api/v1/migrations/" + m.ID + "/abort"},
		{"POST", "/api/v1/migrations/" + m.ID + "/cutover"}, {"POST", "/api/v1/migrations/" + m.ID + "/cleanup"}, {"POST", "/api/v1/users"}, {"PATCH", "/api/v1/settings"},
		{"POST", "/api/v1/system/restart"}, {"POST", "/api/v1/dev/fixtures"},
	} {
		if code := e.do(c.method, c.path, map[string]any{}, nil); code != 403 {
			t.Errorf("viewer %s %s: %d, want 403", c.method, c.path, code)
		}
	}
	e.do("GET", "/api/v1/migrations/"+m.ID, nil, nil, 200)
	e.login("operator")
	for _, c := range []struct{ method, path string }{{"POST", "/api/v1/users"}, {"GET", "/api/v1/users"}, {"PATCH", "/api/v1/settings"}, {"POST", "/api/v1/system/restart"}} {
		if code := e.do(c.method, c.path, map[string]any{}, nil); code != 403 {
			t.Errorf("operator %s %s: %d, want 403", c.method, c.path, code)
		}
	}
	e.do("PATCH", "/api/v1/migrations/"+m.ID, map[string]any{"name": "RBAC renamed"}, nil, 200)
	e.do("POST", "/api/v1/auth/logout", nil, nil, 200)
	if code, _ := e.raw("GET", "/api/v1/migrations"); code != 401 {
		t.Errorf("signed out GET: %d, want 401", code)
	}
	// Missing CSRF token and missing idempotency key.
	e.login("admin")
	csrf := e.csrf
	e.csrf = "wrong"
	if code := e.do("POST", "/api/v1/migrations", map[string]any{"name": "x"}, nil); code != 403 {
		t.Errorf("bad CSRF: %d, want 403", code)
	}
	e.csrf = csrf
	if code := e.doKey("POST", "/api/v1/migrations", "", map[string]any{"name": "x"}, nil); code != 400 {
		t.Errorf("missing idempotency key: %d, want 400", code)
	}
}
