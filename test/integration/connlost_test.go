//go:build integration

package integration

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
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
	e.waitFor(time.Minute, "a COPY into public.orders on the target", func() bool {
		return q(t, dstConn, db, `SELECT count(*) FROM pg_stat_progress_copy WHERE relid::regclass::text IN ('orders', 'public.orders')`) != "0"
	})
	cut(t, db)
	d := e.waitDBState(id, db, 3*time.Minute, orch.DRestartRequired, orch.DFailed)
	t.Logf("%s: %s", d.State, d.LastError)
	if strings.Contains(d.LastError, "Failed to close snapshot") && !strings.Contains(d.LastError, "connection to the target") {
		t.Fatalf("the reason is pgcopydb's last error, not the lost connection: %s", d.LastError)
	}
	for _, want := range []string{"connection to the target was closed", "public.orders"} {
		if !strings.Contains(d.LastError, want) {
			t.Fatalf("the reason lacks %q: %s", want, d.LastError)
		}
	}
	e.abortAndClean(id, name)
	return d
}

func TestBaseCopyTargetConnectionClosed(t *testing.T) {
	d := baseCopyWithLostTarget(t, "Target connection closed", func(t *testing.T, db string) {
		superSQL(t, dstPort, db, `SELECT pg_terminate_backend(pid) FROM pg_stat_progress_copy WHERE relid::regclass::text IN ('orders', 'public.orders')`)
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
