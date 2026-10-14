//go:build integration

package integration

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/orch"
)

// poolWriter is a sysbench-like load: n connections each writing once per
// pace (pgcopydb 0.18 applies only a few transactions a second, F11, so an
// unpaced load would leave a backlog the final heartbeat cannot cross),
// each reconnecting at once when its session is ended, recording when its last
// successful write happened.
type poolWriter struct {
	ok, refused atomic.Int64
	lastOK      atomic.Int64 // unix milliseconds of the latest successful write
	stop        chan struct{}
	wg          sync.WaitGroup
	mu          sync.Mutex
	lastErr     string
}

func startPoolWriter(t *testing.T, db, user, pass string, n int, pace time.Duration) *poolWriter {
	w := &poolWriter{stop: make(chan struct{})}
	for i := 0; i < n; i++ {
		w.wg.Add(1)
		go func(i int) {
			defer w.wg.Done()
			ctx := context.Background()
			var c *pgx.Conn
			defer func() {
				if c != nil {
					c.Close(ctx)
				}
			}()
			for {
				select {
				case <-w.stop:
					return
				default:
				}
				if c == nil {
					var err error
					c, err = pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable&application_name=tpcc-%d", user, pass, srcPort, db, i))
					if err != nil {
						time.Sleep(50 * time.Millisecond)
						continue
					}
				}
				_, err := c.Exec(ctx, `BEGIN; INSERT INTO orders(customer, amount, note) VALUES (9, 9, 'tpcc'); UPDATE orders SET amount = amount + 1 WHERE id = (SELECT min(id) FROM orders); COMMIT`)
				if err != nil {
					w.refused.Add(1)
					w.mu.Lock()
					w.lastErr = err.Error()
					w.mu.Unlock()
					c.Close(ctx)
					c = nil
					time.Sleep(20 * time.Millisecond)
					continue
				}
				w.ok.Add(1)
				w.lastOK.Store(time.Now().UnixMilli())
				select {
				case <-w.stop:
					return
				case <-time.After(pace):
				}
			}
		}(i)
	}
	return w
}

// logMissingRows prints the orders rows present on only one side, with
// their xmin, and the engine log's warnings and errors.
func logMissingRows(t *testing.T, e *Env, db string) {
	src := q(t, srcConn, db, `SELECT string_agg(id||':'||xmin::text||':'||note, ',' ORDER BY id) FROM orders`)
	dst := q(t, dstConn, db, `SELECT string_agg(id::text, ',' ORDER BY id) FROM orders`)
	have := map[string]bool{}
	for _, x := range strings.Split(dst, ",") {
		have[x] = true
	}
	var miss []string
	for _, x := range strings.Split(src, ",") {
		if !have[strings.SplitN(x, ":", 2)[0]] {
			miss = append(miss, x)
		}
	}
	t.Logf("rows on the source but not on the target (id:xmin:note): %v", miss)
	out, _ := exec.Command("bash", "-c", "grep -hE 'WARN|ERROR|FATAL|skip|Skipp' "+e.Cfg.RunsDir()+"/*/engine.log | tail -30").CombinedOutput()
	t.Logf("engine log:\n%s", out)
}

// logVerification prints a migration's verification rows.
func logVerification(t *testing.T, e *Env, id string) {
	var ver struct {
		Results []orch.VerificationRow `json:"results"`
	}
	e.do("GET", "/api/v1/migrations/"+id+"/verification", nil, &ver, 200)
	for _, r := range ver.Results {
		t.Logf("verify %s %-16s %-8s %.300s", r.Database, r.CheckID, r.Result, r.Detail)
	}
}

func (w *poolWriter) Stop() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	w.wg.Wait()
}

// A sysbench TPC-C style load on the source (16 connections, reconnecting
// at once when ended) while the cutover applies the write freeze: no write
// may succeed after the freeze is applied, and the verdict is GO with equal
// data on both sides.
func TestWriteFreezeStopsPooledLoad(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_frzpool")
	id := streamingMigration(t, e, "Freeze pooled load", db)
	w := startPoolWriter(t, db, "doadmin", password, 16, 8*time.Second)
	defer w.Stop()
	time.Sleep(3 * time.Second)
	if w.ok.Load() == 0 {
		t.Fatalf("the load did not write before the cutover: %s", w.lastErr)
	}
	op := e.freezeCutover(id)
	for _, s := range op.Steps {
		t.Logf("step %-10s %-8s %s", s.Key, s.State, s.Detail)
	}
	m := e.migration(id).Migration
	w.Stop()
	t.Logf("load: %d writes ok, %d refused (last error: %s)", w.ok.Load(), w.refused.Load(), w.lastErr)
	if op.State != "done" || m.Flags.Verdict != "GO" {
		logVerification(t, e, id)
		logMissingRows(t, e, db)
		t.Fatalf("expected GO with the freeze stopping the load: %s %q", m.Flags.Verdict, op.Error)
	}
	f := m.Flags.Freeze
	if f == nil || f.AppliedAt == 0 {
		t.Fatalf("no freeze recorded: %+v", f)
	}
	if last := w.lastOK.Load(); last > f.AppliedAt {
		t.Fatalf("a write succeeded %d ms after the freeze was applied", last-f.AppliedAt)
	}
	if !strings.Contains(w.lastErr, "read-only") {
		t.Fatalf("the load was not refused by the freeze: %s", w.lastErr)
	}
	if s, d := q(t, srcConn, db, `SELECT count(*)::text||'/'||sum(amount)::text FROM orders`), q(t, dstConn, db, `SELECT count(*)::text||'/'||sum(amount)::text FROM orders`); s != d {
		t.Fatalf("orders differ after GO: source %s, target %s", s, d)
	}
	e.do("POST", "/api/v1/migrations/"+id+"/cutover/unfreeze", nil, nil, 200)
	e.cleanup(id, "Freeze pooled load")
}

// The droplet's question (2026-10-14) led here: a role with its own
// default_transaction_read_only = off (ALTER ROLE ... SET, or ALTER ROLE ...
// IN DATABASE ... SET) overrides the database-level setting the freeze uses,
// so its sessions kept writing through the freeze. The freeze now sets the
// setting for such roles in the database too (and the undo restores theirs),
// and the preview lists them.
func TestWriteFreezeRoleOverride(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_frzrole")
	role := "it_rw_" + strings.TrimPrefix(db, "it_frzrole_")
	id := streamingMigration(t, e, "Freeze role override", db)
	superSQL(t, srcPort, "postgres", "CREATE ROLE "+role+" LOGIN PASSWORD 'x'")
	t.Cleanup(func() {
		for _, d := range []string{"postgres", db} {
			if superSQL(t, srcPort, "postgres", "SELECT count(*) FROM pg_database WHERE datname = '"+d+"'") == "1" {
				superSQL(t, srcPort, d, "DROP OWNED BY "+role+" CASCADE")
			}
		}
		superSQL(t, srcPort, "postgres", "DROP ROLE IF EXISTS "+role)
	})
	superSQL(t, srcPort, db, "GRANT ALL ON ALL TABLES IN SCHEMA public TO "+role+"; GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO "+role)
	superSQL(t, srcPort, "postgres", "ALTER ROLE "+role+" SET default_transaction_read_only = off")
	// doadmin manages this role (as on DigitalOcean, where doadmin creates
	// the application roles).
	superSQL(t, srcPort, "postgres", "GRANT "+role+" TO doadmin WITH ADMIN OPTION")

	var pv orch.FreezePreview
	e.do("GET", "/api/v1/migrations/"+id+"/cutover/freeze", nil, &pv, 200)
	if len(pv.Databases) != 1 || !strings.Contains(strings.Join(pv.Databases[0].ApplySQL, " "), `ALTER ROLE "`+role+`" IN DATABASE`) {
		t.Fatalf("the preview does not freeze the role with its own setting: %+v", pv.Databases)
	}
	w := startPoolWriter(t, db, role, "x", 4, 2*time.Second)
	defer w.Stop()
	time.Sleep(2 * time.Second)
	op := e.freezeCutover(id)
	for _, s := range op.Steps {
		t.Logf("step %-10s %-8s %s", s.Key, s.State, s.Detail)
	}
	m := e.migration(id).Migration
	w.Stop()
	t.Logf("role %s: %d writes ok, %d refused (last error: %s)", role, w.ok.Load(), w.refused.Load(), w.lastErr)
	if op.State != "done" || m.Flags.Verdict != "GO" {
		logVerification(t, e, id)
		t.Fatalf("expected the freeze to stop the role and the cutover to end GO: %s %q", m.Flags.Verdict, op.Error)
	}
	if last := w.lastOK.Load(); last > m.Flags.Freeze.AppliedAt {
		t.Fatalf("the role wrote %d ms after the freeze was applied", last-m.Flags.Freeze.AppliedAt)
	}
	e.do("POST", "/api/v1/migrations/"+id+"/cutover/unfreeze", nil, nil, 200)
	got := superSQL(t, srcPort, "postgres", "SELECT COALESCE(string_agg(CASE WHEN setdatabase = 0 THEN 'role' ELSE 'db' END||':'||array_to_string(setconfig, ','), ' ' ORDER BY setdatabase), '') FROM pg_db_role_setting WHERE setrole = '"+role+"'::regrole")
	if got != "role:default_transaction_read_only=off" {
		t.Fatalf("the undo did not restore the role's own settings: %q", got)
	}
	e.cleanup(id, "Freeze role override")
}

// A prepared transaction holds writes that any session can commit later,
// and it has no session, so the open-transaction check could not see it:
// the writers step and the writes_stopped resample must refuse while one is
// pending in the database.
func TestPreparedTransactionStopsCutover(t *testing.T) {
	if superSQL(t, srcPort, "postgres", "SHOW max_prepared_transactions") == "0" {
		t.Skip("max_prepared_transactions is 0 on the source (spikes/env/local-clusters.sh sets 10)")
	}
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_prepared")
	id := streamingMigration(t, e, "Prepared transaction", db)
	gid := "upwell_test_" + db
	superSQL(t, srcPort, db, "BEGIN; INSERT INTO orders(customer, amount, note) VALUES (5, 5, 'prepared'); PREPARE TRANSACTION '"+gid+"'")
	t.Cleanup(func() { superSQL(t, srcPort, db, "ROLLBACK PREPARED '"+gid+"'") })
	op := e.cutoverOp(id, map[string]any{"confirm_writers_stopped": true})
	for _, s := range op.Steps {
		t.Logf("step %-10s %-8s %s", s.Key, s.State, s.Detail)
	}
	if v := e.migration(id).Migration.Flags.Verdict; v == "GO" {
		t.Fatal("GO with a prepared transaction pending on the source")
	}
	if !strings.Contains(op.Error+fmt.Sprint(op.Steps), "prepared") {
		t.Fatalf("the cutover did not say why: %s %+v", op.Error, op.Steps)
	}
	e.abortAndClean(id, "Prepared transaction")
}

// D83: writes the row counter cannot see, made after a database was
// compared: a sequence moved by setval, and a TRUNCATE. Each must end NO-GO
// through the writes_stopped resample's fingerprint.
func TestInvisibleWritesAfterVerificationNoGo(t *testing.T) {
	for _, c := range []struct{ name, sql string }{
		{"setval", `SELECT setval('orders_id_seq', 5000000)`},
		{"truncate", `TRUNCATE items`},
	} {
		t.Run(c.name, func(t *testing.T) {
			hook := func(ctx context.Context, d orch.Database) { exec1(t, srcConn, d.SourceName, c.sql) }
			e := newEnv(t, "", orch.TestHooks{AfterVerify: hook})
			db := uniq("it_invis")
			id := streamingMigration(t, e, "Invisible writes "+c.name, db)
			time.Sleep(3 * time.Second)
			e.startCutover(id)
			if v := e.migration(id).Migration.Flags.Verdict; v != "NO-GO" {
				logVerification(t, e, id)
				t.Fatalf("verdict %s although the source changed (%s) after verification", v, c.name)
			}
			if !blockerWith(t, e, id, "writes_stopped", "truncated or rewritten, or a sequence") {
				logVerification(t, e, id)
				t.Fatal("no writes_stopped blocker naming the change")
			}
			e.abortAndClean(id, "Invisible writes "+c.name)
		})
	}
}

// F9, the droplet (2026-10-14): a second migration of a database an earlier
// migration had frozen (the freeze is kept after GO) failed in the base copy
// with "cannot execute CREATE PUBLICATION in a read-only transaction".
// Preflight now blocks on it, and a launch after a clean preflight refuses to
// start the engine.
func TestReadOnlySourceBlocked(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_readonly")
	seedDB(t, db, 1000)
	id := e.defineMigration("Read-only source", []string{db}, nil)
	defer superSQL(t, srcPort, "postgres", "ALTER DATABASE "+db+" RESET default_transaction_read_only")
	superSQL(t, srcPort, "postgres", "ALTER DATABASE "+db+" SET default_transaction_read_only = on")
	pv := e.preflight(id)
	found := false
	for _, r := range pv.Results {
		if r.CheckID == "source_writable" && r.Database == db {
			found = true
			if r.Level != "blocker" || !r.Hard || !strings.Contains(r.Message, "ALTER DATABASE") {
				t.Fatalf("source_writable: level %s hard %v: %s", r.Level, r.Hard, r.Message)
			}
		}
	}
	if !found {
		t.Fatal("no source_writable result for the read-only database")
	}
	// Writable at preflight, read-only at launch.
	superSQL(t, srcPort, "postgres", "ALTER DATABASE "+db+" RESET default_transaction_read_only")
	e.preflight(id)
	superSQL(t, srcPort, "postgres", "ALTER DATABASE "+db+" SET default_transaction_read_only = on")
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	d := e.waitDBState(id, db, 2*time.Minute, orch.DFailed, orch.DRestartRequired, orch.DInSync)
	if d.State == orch.DInSync || !strings.Contains(d.LastError, "refuses writes") {
		t.Fatalf("launch on a read-only source ended %s: %s", d.State, d.LastError)
	}
	n := q(t, srcConn, "defaultdb", `SELECT count(*) FROM pg_replication_slots WHERE database = '`+db+`'`)
	if n != "0" {
		t.Fatalf("%s slot(s) left on the source", n)
	}
	superSQL(t, srcPort, "postgres", "ALTER DATABASE "+db+" RESET default_transaction_read_only")
	e.abortAndClean(id, "Read-only source")
}
