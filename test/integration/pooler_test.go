//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"
)

// startPgBouncer runs PgBouncer in front of the target in the given pool
// mode and returns its port. It refuses unknown startup parameters, as the
// pooler in front of a DigitalOcean Advanced cluster did on the droplet.
func startPgBouncer(t *testing.T, mode string, port int) pg.Conn {
	t.Helper()
	c, _ := startPgBouncerTo(t, mode, port, dstPort)
	return c
}

// bouncer is a running PgBouncer a test can kill, start again, and point at
// another server (a failover behind a stable address).
type bouncer struct {
	t       *testing.T
	bin     string
	dir     string
	mode    string
	port    int
	backend int
}

func (b *bouncer) writeIni() {
	ini := fmt.Sprintf(`[databases]
* = host=127.0.0.1 port=%d
[pgbouncer]
listen_addr = 127.0.0.1
listen_port = %d
auth_type = plain
auth_file = %s/users.txt
pool_mode = %s
unix_socket_dir =
logfile = %s/pgbouncer.log
pidfile = %s/pgbouncer.pid
default_pool_size = 5
`, b.backend, b.port, b.dir, b.mode, b.dir, b.dir)
	os.WriteFile(filepath.Join(b.dir, "pgbouncer.ini"), []byte(ini), 0o644)
	exec.Command("chown", "-R", "postgres", b.dir).Run()
}

func (b *bouncer) start() {
	b.t.Helper()
	b.writeIni()
	if out, err := exec.Command("runuser", "-u", "postgres", "--", b.bin, "-d", filepath.Join(b.dir, "pgbouncer.ini")).CombinedOutput(); err != nil {
		b.t.Fatalf("pgbouncer: %v %s", err, out)
	}
	time.Sleep(500 * time.Millisecond)
}

// kill ends PgBouncer with SIGKILL: every client connection drops at once.
func (b *bouncer) kill() {
	if raw, err := os.ReadFile(filepath.Join(b.dir, "pgbouncer.pid")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
			}
		}
	}
	time.Sleep(300 * time.Millisecond)
	os.Remove(filepath.Join(b.dir, "pgbouncer.pid"))
}

func startPgBouncerTo(t *testing.T, mode string, port, backend int) (pg.Conn, *bouncer) {
	t.Helper()
	bin, err := exec.LookPath("pgbouncer")
	if err != nil {
		t.Skip("pgbouncer is not installed")
	}
	dir, err := os.MkdirTemp("", "upwell-pgbouncer-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o755)
	t.Cleanup(func() {
		if t.Failed() {
			if b, err := os.ReadFile(filepath.Join(dir, "pgbouncer.log")); err == nil {
				lines := strings.Split(strings.TrimSpace(string(b)), "\n")
				var bad []string
				for _, l := range lines {
					if strings.Contains(l, "statement_timeout") {
						continue
					}
					if strings.Contains(l, "WARNING") || strings.Contains(l, "ERROR") || strings.Contains(l, "FATAL") || strings.Contains(l, "closing because") && !strings.Contains(l, "client close request") {
						bad = append(bad, l)
					}
				}
				if len(bad) > 25 {
					bad = bad[:25]
				}
				t.Logf("pgbouncer log:\n%s", strings.Join(bad, "\n"))
			}
		}
		os.RemoveAll(dir)
	})
	os.WriteFile(filepath.Join(dir, "users.txt"), []byte(`"doadmin" "`+password+`"`+"\n"), 0o644)
	b := &bouncer{t: t, bin: bin, dir: dir, mode: mode, port: port, backend: backend}
	b.start()
	t.Cleanup(b.kill)
	c := dstConn
	c.Port = port
	return c, b
}

func TestPoolerInFront(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		mode   string
		port   int
		pinned bool
	}{{"session", 56533, true}, {"transaction", 56534, false}} {
		t.Run(tc.mode, func(t *testing.T) {
			c := startPgBouncer(t, tc.mode, tc.port)
			// The droplet failure: a statement timeout sent as a startup
			// parameter was refused. Connect must work through the pooler.
			conn, err := pg.Connect(ctx, c, 30*time.Second)
			if err != nil {
				t.Fatalf("connect through PgBouncer (%s): %v", tc.mode, err)
			}
			defer conn.Close(ctx)
			var st string
			conn.QueryRow(ctx, `SHOW statement_timeout`).Scan(&st)
			t.Logf("%s: connected, statement_timeout %s", tc.mode, st)
			if want := map[bool]string{true: "30s", false: "0"}[tc.pinned]; st != want {
				t.Fatalf("statement_timeout %s through a %s pooler, want %s (a SET must never leak into a transaction pool)", st, tc.mode, want)
			}
			ss, err := pg.ProbeSession(ctx, conn, c)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: pinned %v, backend PIDs %v", tc.mode, ss.Pinned, ss.PIDs)
			if ss.Pinned != tc.pinned {
				t.Fatalf("pinned %v, want %v", ss.Pinned, tc.pinned)
			}
			// The connection test shows the outcome in plain words.
			for _, r := range orch.TestConnection(ctx, c, "target") {
				t.Logf("connection test %-16s ok=%v %s", r.Name, r.OK, r.Message)
				if r.Name == "session" && r.OK != tc.pinned {
					t.Fatalf("connection test session ok=%v, want %v", r.OK, tc.pinned)
				}
			}
		})
	}
	if err := pg.Explain(fmt.Errorf("server error: FATAL: unsupported startup parameter: statement_timeout (SQLSTATE 08P01)")); strings.Contains(err.Error(), "did not answer in time") {
		t.Fatalf("a refused startup parameter is explained as a timeout: %v", err)
	}
}

// TestMigrationThroughSessionPooler: the whole migration with the target
// behind PgBouncer in session mode, ending GO with identical data.
func TestMigrationThroughSessionPooler(t *testing.T) {
	c := startPgBouncer(t, "session", 56535)
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_pooler")
	seedDB(t, db, 5000)
	id := e.defineMigration("Through PgBouncer", []string{db}, map[string]any{"_target_port": c.Port})
	pv := e.preflight(id)
	for _, r := range pv.Results {
		if r.CheckID == "session_pinning" || r.CheckID == "connect" {
			t.Logf("preflight %s: %s %s", r.CheckID, r.Level, r.Message)
		}
	}
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	w := startWriter(t, db)
	time.Sleep(8 * time.Second)
	w.Stop()
	if v := outcome(t, e, id, db); v != "GO" {
		t.Fatalf("verdict %s through a session pooler", v)
	}
	e.cleanup(id, "Through PgBouncer")
}
