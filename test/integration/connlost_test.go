//go:build integration

package integration

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"

	_ "modernc.org/sqlite"
)

// The droplet (2026-10-11 and 2026-10-12): the base copy failed twice when
// the target closed the connection copying the same table ("[TARGET 0] SSL
// error: unexpected eof while reading"), 16 and 90 minutes in. pgcopydb kept
// streaming, so Upwell detected the failure from the log while the unit was
// still alive, and that path showed pgcopydb's last error ("Failed to close
// snapshot ... on (null)") instead of the root cause, named the table by oid,
// and could not say whether the target had restarted.
func baseCopyWithLostTarget(t *testing.T, name string, cut func(t *testing.T, db string)) orch.Database {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_connlost")
	seedDB(t, db, 1500000)
	id := e.defineMigration(name, []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, time.Minute, orch.DBaseCopy)
	// Whichever table is being copied (under load the orders COPY can start
	// or end between two samples).
	var table string
	e.waitFor(3*time.Minute, "a COPY on the target", func() bool {
		table = q(t, dstConn, db, `SELECT COALESCE((SELECT format('%I.%I', n.nspname, c.relname) FROM pg_stat_progress_copy p JOIN pg_class c ON c.oid = p.relid JOIN pg_namespace n ON n.oid = c.relnamespace ORDER BY p.tuples_processed LIMIT 1), '')`)
		return table != ""
	})
	cut(t, db)
	d := e.waitDBState(id, db, 3*time.Minute, orch.DRestartRequired, orch.DFailed)
	t.Logf("%s (copying %s when cut): %s", d.State, table, d.LastError)
	if strings.Contains(d.LastError, "Failed to close snapshot") && !strings.Contains(d.LastError, "connection to the target") {
		t.Fatalf("the reason is pgcopydb's last error, not the lost connection: %s", d.LastError)
	}
	for _, want := range []string{"connection to the target was closed", "while copying public."} {
		if !strings.Contains(d.LastError, want) {
			t.Fatalf("the reason lacks %q: %s", want, d.LastError)
		}
	}
	// D81: the failed copy's slot is useless (a restart from zero creates a
	// new one at a new snapshot) and would keep source WAL until then.
	e.waitFor(2*time.Minute, "the failed copy's slot to be dropped", func() bool {
		return q(t, srcConn, "defaultdb", `SELECT count(*) FROM pg_replication_slots WHERE database = '`+db+`'`) == "0"
	})
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/restart", map[string]any{"confirm": db}, nil, 200)
	if d := e.waitDBState(id, db, 3*time.Minute, orch.DInSync, orch.DRestartRequired, orch.DFailed); d.State != orch.DInSync {
		t.Fatalf("restart from zero after the slot was dropped ended %s: %s", d.State, d.LastError)
	}
	e.abortAndClean(id, name)
	return d
}

func TestBaseCopyTargetConnectionClosed(t *testing.T) {
	d := baseCopyWithLostTarget(t, "Target connection closed", func(t *testing.T, db string) {
		superSQL(t, dstPort, db, `SELECT pg_terminate_backend(pid) FROM pg_stat_progress_copy`)
	})
	if !strings.Contains(d.LastError, "did not restart") {
		t.Fatalf("the reason does not say the target kept running: %s", d.LastError)
	}
}

func TestBaseCopyTargetRestarted(t *testing.T) {
	data := envStr("UPWELL_IT_DST_DATA", "/var/tmp/upwell-spike/target")
	ctl := envStr("UPWELL_IT_PG_CTL", "/usr/lib/postgresql/16/bin/pg_ctl")
	if _, err := os.Stat(data + "/PG_VERSION"); err != nil {
		t.Skipf("no local target data directory at %s", data)
	}
	d := baseCopyWithLostTarget(t, "Target restarted", func(t *testing.T, db string) {
		time.Sleep(2 * time.Second)
		// A log file and no pipe: the restarted server would otherwise keep
		// this process's output open.
		cmd := exec.Command("runuser", "-u", "postgres", "--", ctl, "-D", data, "-l", data+".log", "restart", "-m", "fast", "-w")
		if err := cmd.Run(); err != nil {
			t.Fatalf("restarting the target: %v", err)
		}
	})
	if !strings.Contains(d.LastError, "restarted") || strings.Contains(d.LastError, "did not restart") {
		t.Fatalf("the reason does not say the target restarted: %s", d.LastError)
	}
}

// D78: same-table parallel copy end to end (the size is small so the local
// orders table, about 130 MB, is split): the copy, a stop and resume while
// streaming, and a GO cutover.
func TestSplitTablesGO(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_split")
	seedDB(t, db, 1500000)
	id := e.defineMigration("Split tables", []string{db}, map[string]any{"split_tables_larger_than": "40MB", "table_jobs": 4})
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	if d := e.waitDBState(id, db, 5*time.Minute, orch.DInSync, orch.DRestartRequired, orch.DFailed); d.State != orch.DInSync {
		t.Fatalf("split copy ended %s: %s", d.State, d.LastError)
	}
	out, _ := exec.Command("bash", "-c", "grep -h 'tables split in' "+e.Cfg.RunsDir()+"/*/engine.log | head -2").CombinedOutput()
	t.Logf("engine log: %s", out)
	if !strings.Contains(string(out), "tables split in") || strings.Contains(string(out), "including 0 tables split") {
		t.Fatal("the engine did not split the table")
	}
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/stop", nil, nil, 200)
	e.waitDBState(id, db, 2*time.Minute, orch.DStopped)
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/resume", nil, nil, 200)
	if d := e.waitDBState(id, db, 3*time.Minute, orch.DInSync, orch.DRestartRequired, orch.DFailed); d.State != orch.DInSync {
		t.Fatalf("resume after a split copy ended %s: %s", d.State, d.LastError)
	}
	exec1(t, srcConn, db, `INSERT INTO orders(customer, amount, note) VALUES (1, 1, 'after resume')`)
	e.startCutover(id)
	if v := e.migration(id).Migration.Flags.Verdict; v != "GO" {
		t.Fatalf("verdict %s after a split copy", v)
	}
	e.cleanup(id, "Split tables")
}

// D81: one server process killed (as the out-of-memory killer does) makes
// PostgreSQL end every connection and reinitialize, without changing
// pg_postmaster_start_time(); the checkpointer's start shows it.
func TestBaseCopyTargetBackendCrashed(t *testing.T) {
	d := baseCopyWithLostTarget(t, "Target backend crashed", func(t *testing.T, db string) {
		pid := superSQL(t, dstPort, db, `SELECT pid FROM pg_stat_progress_copy LIMIT 1`)
		if out, err := exec.Command("kill", "-9", pid).CombinedOutput(); err != nil {
			t.Fatalf("killing the COPY backend %s: %v: %s", pid, err, out)
		}
	})
	if !strings.Contains(d.LastError, "crashed") {
		t.Fatalf("the reason does not say a server process crashed: %s", d.LastError)
	}
}

// D87, the droplet (2026-10-14): the base copy of migration_db failed with
// "the engine's internal catalog was locked": an index worker could not
// record the index it had built ("update summary set done_time_epoch ...")
// because pgcopydb retried its SQLite catalog for only 5 s. Here a writer
// holds that catalog for 15 s in the middle of the copy; with Upwell's build
// (0.18+upwell.2) the workers wait and the copy reaches In sync.
func TestBaseCopyCatalogLockHeld(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_catlock")
	seedDB(t, db, 1500000)
	id := e.defineMigration("Catalog lock", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	d := e.waitDBState(id, db, time.Minute, orch.DBaseCopy)
	catalog := filepath.Join(e.Cfg.RunsDir(), d.Instance, "clone", "schema", "source.db")
	e.waitFor(2*time.Minute, "the engine's catalog and a COPY on the target", func() bool {
		if _, err := os.Stat(catalog); err != nil {
			matches, _ := filepath.Glob(filepath.Join(e.Cfg.RunsDir(), d.Instance, "*", "schema", "source.db"))
			if len(matches) == 0 {
				return false
			}
			catalog = matches[0]
		}
		return q(t, dstConn, db, `SELECT count(*) FROM pg_stat_progress_copy`) != "0"
	})
	lock, err := sql.Open("sqlite", "file:"+catalog+"?_pragma=busy_timeout(60000)")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	lock.SetMaxOpenConns(1)
	if _, err := lock.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("locking the engine's catalog %s: %v", catalog, err)
	}
	t.Logf("holding the write lock on %s for 15 s", catalog)
	time.Sleep(15 * time.Second)
	if _, err := lock.Exec("COMMIT"); err != nil {
		t.Fatal(err)
	}
	if d := e.waitDBState(id, db, 5*time.Minute, orch.DInSync, orch.DRestartRequired, orch.DFailed); d.State != orch.DInSync || d.AutoRestarts > 0 {
		t.Fatalf("with the catalog locked for 15 s the copy ended %s (automatic restarts %d): %s", d.State, d.AutoRestarts, d.LastError)
	}
	e.abortAndClean(id, "Catalog lock")
}
