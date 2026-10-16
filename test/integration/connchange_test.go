//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

// TestConnectionChangeRules (D100): a started migration's connections can
// change only when that cannot strand it.
//   - While an engine streams: an endpoint change is refused; a password
//     alone is accepted if it works and refused if it does not.
//   - Stopped: the target can move to another address of the same cluster
//     (PgBouncer here), and the resumed engine uses it; another cluster is
//     refused.
//   - After an abort and clean up: nothing changes.
func TestConnectionChangeRules(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_connchg")
	seedDB(t, db, 20000)
	id := e.defineMigration("Connection change", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)

	type apiErr struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	conn := func(kind, host string, port int, pw string) map[string]any {
		c := map[string]any{"host": host, "port": port, "user": "doadmin", "sslmode": "disable"}
		if pw != "" {
			c["password"] = pw
		}
		if kind == "target" {
			c["storage_gb"] = 100
		}
		return c
	}
	put := func(kind string, body map[string]any, want int, code string) {
		t.Helper()
		var r apiErr
		e.do("PUT", "/api/v1/migrations/"+id+"/connections/"+kind, body, &r, want)
		if code != "" && r.Code != code {
			t.Fatalf("%s change: code %q, want %q: %s", kind, r.Code, code, r.Message)
		}
		if r.Message != "" {
			t.Logf("%s: %s", kind, r.Message)
		}
	}

	// Streaming: no endpoint change, even to the same cluster.
	put("target", conn("target", "localhost", dstPort, ""), 409, "conn_busy")
	put("source", conn("source", "localhost", srcPort, ""), 409, "conn_busy")
	// A password alone: only one that works.
	put("source", conn("source", "127.0.0.1", srcPort, "not-the-password"), 409, "conn_test_failed")
	put("source", conn("source", "127.0.0.1", srcPort, password), 200, "")
	put("target", map[string]any{"host": "127.0.0.1", "port": dstPort, "user": "doadmin", "sslmode": "disable", "storage_gb": 250}, 200, "")
	if d := e.db(id, db); d.State != orch.DInSync {
		t.Fatalf("a password change disturbed the stream: %s", d.State)
	}

	// Stopped: another address of the same cluster is accepted.
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/stop", nil, nil, 200)
	_, b := startPgBouncerTo(t, "session", 56570, dstPort)
	put("target", conn("target", "127.0.0.1", 56570, ""), 200, "")
	put("source", conn("source", "localhost", srcPort, ""), 200, "")
	// Another cluster is not.
	requirePG18(t)
	put("target", conn("target", "127.0.0.1", pg18Port, ""), 409, "conn_other_cluster")

	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/resume", nil, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	exec1(t, srcConn, db, "INSERT INTO orders(id, customer, note) VALUES (999999001, 1, 'after the change')")
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("bash", "-c", "grep -nE 'apply|ERROR|WARN|origin|transform|catchup|Catchup' "+e.Cfg.RunsDir()+"/*/engine.log | tail -40").CombinedOutput()
			bl, _ := os.ReadFile(filepath.Join(b.dir, "pgbouncer.log"))
			t.Logf("engine log:\n%s\ntarget sessions: %s\npgbouncer:\n%s", out,
				q(t, dstConn, "defaultdb", "SELECT string_agg(application_name||' '||state||' '||left(query,60), E'\\n') FROM pg_stat_activity WHERE datname='"+db+"'"), bl)
		}
	})
	e.waitFor(3*time.Minute, "the row through the new target address", func() bool {
		return q(t, dstConn, db, "SELECT count(*)::text FROM orders WHERE id = 999999001") == "1"
	})
	logb, _ := os.ReadFile(filepath.Join(b.dir, "pgbouncer.log"))
	if !strings.Contains(string(logb), "db="+db) && !strings.Contains(string(logb), db+"/doadmin") {
		t.Fatalf("the resumed engine did not connect through the new target address:\n%s", logb)
	}
	n := 0
	for _, ev := range eventsOf(t, e, id) {
		if ev["type"] == "connection_changed" {
			n++
		}
	}
	if n < 4 { // source password, target storage, target address, source address
		t.Fatalf("%d connection_changed events", n)
	}
	if a := e.auditActions(id); !strings.Contains(strings.Join(a, ","), "migration.connection.target") {
		t.Fatalf("audit: %v", a)
	}

	e.abortAndClean(id, "Connection change")
	put("source", conn("source", "127.0.0.1", srcPort, password), 409, "conn_frozen")
}
