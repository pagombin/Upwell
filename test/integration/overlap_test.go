//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"
)

// D84: pgcopydb 0.18 took transactions in BEGIN order, so a transaction that
// began before another one committed, and committed after it, was never
// applied (the droplet's sysbench TPC-C load lost committed rows). Each round
// here is that interleaving, made deterministic: T1 begins and writes, T2
// writes and commits, then T1 commits. Every row must reach the target and
// the cutover end GO.
func TestOverlappingTransactionsReachTarget(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_overlap")
	seedDB(t, db, 1000)
	id := e.defineMigration("Overlapping transactions", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	if d := e.waitDBState(id, db, 3*time.Minute, orch.DInSync, orch.DRestartRequired, orch.DFailed); d.State != orch.DInSync {
		t.Fatalf("copy ended %s: %s", d.State, d.LastError)
	}
	ctx := context.Background()
	c1, err := pg.Connect(ctx, srcConn.WithDB(db), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close(ctx)
	c2, err := pg.Connect(ctx, srcConn.WithDB(db), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close(ctx)
	const rounds = 10
	for i := 0; i < rounds; i++ {
		must := func(c *pgx.Conn, sql string) {
			if _, err := c.Exec(ctx, sql); err != nil {
				t.Fatalf("round %d: %s: %v", i, sql, err)
			}
		}
		must(c1, "BEGIN")
		must(c1, "INSERT INTO orders(customer, amount, note) VALUES (1, 1, 'overlap-outer')")
		must(c2, "BEGIN")
		must(c2, "INSERT INTO orders(customer, amount, note) VALUES (2, 2, 'overlap-inner')")
		must(c2, "UPDATE orders SET amount = amount + 1 WHERE id = (SELECT min(id) FROM orders)")
		must(c2, "COMMIT")
		must(c1, "COMMIT")
	}
	// The writers step reads the source's statistics, which lag commits by
	// up to a second or so: let them settle (and the target catch up) first.
	time.Sleep(10 * time.Second)
	e.startCutover(id)
	if v := e.migration(id).Migration.Flags.Verdict; v != "GO" {
		logVerification(t, e, id)
		t.Errorf("verdict %s", v)
	}
	src := q(t, srcConn, db, "SELECT count(*) FILTER (WHERE note LIKE 'overlap-%') || '/' || sum(amount) FROM orders")
	dst := q(t, dstConn, db, "SELECT count(*) FILTER (WHERE note LIKE 'overlap-%') || '/' || sum(amount) FROM orders")
	t.Logf("overlap rows/amount: source %s, target %s", src, dst)
	if src != dst {
		logMissingRows(t, e, db)
		t.Fatalf("the target lost overlapping transactions: source %s, target %s", src, dst)
	}
	e.cleanup(id, "Overlapping transactions")
}

// D89, the droplet (2026-10-14): migration_poc's apply connection stalled;
// when the engine was started again the target still had the old session,
// which held the replication origin, and every resume failed with
// "replication origin with ID 2 is already active for PID ...". Here a
// session named like pgcopydb's holds the origin while the database is
// stopped; resuming must end it and stream again.
func TestResumeEndsStaleApplySession(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_staleapply")
	id := streamingMigration(t, e, "Stale apply session", db)
	d := e.db(id, db)
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/stop", nil, nil, 200)
	e.waitDBState(id, db, 2*time.Minute, orch.DStopped)
	ctx := context.Background()
	stale, err := pg.Connect(ctx, dstConn.WithDB(db), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close(ctx)
	if _, err := stale.Exec(ctx, "SET application_name = 'pgcopydb[999999]'"); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Exec(ctx, "SELECT pg_replication_origin_session_setup($1)", d.OriginName); err != nil {
		t.Fatalf("holding origin %s: %v", d.OriginName, err)
	}
	exec1(t, srcConn, db, `INSERT INTO orders(customer, amount, note) VALUES (7, 7, 'after stale')`)
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/resume", nil, nil, 200)
	if d := e.waitDBState(id, db, 3*time.Minute, orch.DInSync, orch.DFailed, orch.DRestartRequired, orch.DStopped); d.State != orch.DInSync {
		t.Fatalf("resume with a stale apply session on the target ended %s: %s", d.State, d.LastError)
	}
	e.waitFor(2*time.Minute, "the row written while stopped to reach the target", func() bool {
		return q(t, dstConn, db, `SELECT count(*) FROM orders WHERE note = 'after stale'`) == "1"
	})
	e.abortAndClean(id, "Stale apply session")
}
