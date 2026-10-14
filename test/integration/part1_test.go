//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"
)

// Part 1 (2026-10-14): streaming, cutover and verification scenarios, each
// run under both decoding plugins and judged by compareDatabase (a count
// and an md5 over every row of every table and partition, computed here,
// independently of Upwell's verification). A GO with any difference fails
// the test as a critical bug.

var plugins = []string{"pgoutput", "test_decoding"}

type scenario struct {
	name   string
	schema string // run on the source database before the migration
	// before runs after the schema, before the start; during, once the
	// database is in the base copy; streaming, once it is in sync. Each may
	// return a cleanup to run before the cutover (to stop a writer).
	before, during, streaming func(t *testing.T, e *Env, id, db string) func()
	settings                  map[string]any
	// wantGO: the cutover must end GO with identical data. Otherwise check
	// is called with the database's final state and must accept it.
	wantGO bool
	check  func(t *testing.T, e *Env, id, db string)
}

func runScenario(t *testing.T, plugin string, sc scenario) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_p1_" + sc.name[:min(len(sc.name), 10)])
	dropDB(t, db)
	exec1(t, srcConn, "defaultdb", "CREATE DATABASE "+db)
	t.Cleanup(func() { dropDB(t, db) })
	exec1(t, srcConn, db, sc.schema)
	if _, _, err := e.Orch.SettingsService().Set(context.Background(), "cutover_drain_timeout_seconds", 3600, "test"); err != nil {
		t.Fatal(err)
	}
	set := map[string]any{"decoding_plugin": plugin}
	for k, v := range sc.settings {
		set[k] = v
	}
	name := "P1 " + sc.name + " " + plugin
	id := e.defineMigration(name, []string{db}, set)
	e.preflight(id)
	var cleanups []func()
	run := func(f func(t *testing.T, e *Env, id, db string) func()) {
		if f != nil {
			if c := f(t, e, id, db); c != nil {
				cleanups = append(cleanups, c)
			}
		}
	}
	run(sc.before)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	if sc.during != nil {
		e.waitDBState(id, db, 3*time.Minute, orch.DBaseCopy, orch.DInSync)
		run(sc.during)
	}
	if d := e.waitDBState(id, db, 20*time.Minute, orch.DInSync, orch.DRestartRequired, orch.DFailed); d.State != orch.DInSync {
		if sc.check != nil && !sc.wantGO {
			sc.check(t, e, id, db)
			for _, c := range cleanups {
				c()
			}
			e.abortAndClean(id, name)
			return
		}
		t.Fatalf("%s ended %s before the cutover: %s", db, d.State, d.LastError)
	}
	run(sc.streaming)
	for _, c := range cleanups {
		c()
	}
	if sc.check != nil && !sc.wantGO {
		sc.check(t, e, id, db)
		e.abortAndClean(id, name)
		return
	}
	time.Sleep(10 * time.Second) // the source's statistics settle before the writers step
	op := e.startCutover(id)
	v := e.migration(id).Migration.Flags.Verdict
	n, diffs := compareDatabase(t, db)
	switch {
	case v == "GO" && len(diffs) > 0:
		logVerification(t, e, id)
		t.Fatalf("CRITICAL: GO with different data (%s, %s):\n%s", sc.name, plugin, strings.Join(diffs, "\n"))
	case sc.wantGO && v != "GO":
		logVerification(t, e, id)
		for _, st := range op.Steps {
			t.Logf("step %-10s %-8s %s", st.Key, st.State, st.Detail)
		}
		t.Fatalf("verdict %s, want GO (data differences: %d)\n%s", v, len(diffs), strings.Join(diffs, "\n"))
	case sc.wantGO && len(diffs) > 0:
		t.Fatalf("data differs:\n%s", strings.Join(diffs, "\n"))
	}
	if sc.check != nil {
		sc.check(t, e, id, db)
	}
	t.Logf("%s / %s: verdict %s, %d tables identical", sc.name, plugin, v, n)
	e.cleanup(id, name)
}

func forPlugins(t *testing.T, sc scenario) {
	for _, p := range plugins {
		sc := sc
		t.Run(p, func(t *testing.T) { runScenario(t, p, sc) })
	}
}

// background runs f every pace until the returned stop is called.
func background(t *testing.T, db string, pace time.Duration, f func(ctx context.Context, c *pgx.Conn, i int) error) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := context.Background()
		var c *pgx.Conn
		for i := 0; ; i++ {
			select {
			case <-stop:
				if c != nil {
					c.Close(ctx)
				}
				return
			case <-time.After(pace):
			}
			if c == nil {
				var err error
				if c, err = pg.Connect(ctx, srcConn.WithDB(db), time.Minute); err != nil {
					continue
				}
			}
			if err := f(ctx, c, i); err != nil {
				c.Close(ctx)
				c = nil
			}
		}
	}()
	return func() { close(stop); <-done }
}

// holdTx opens a session on the source and runs sql inside a transaction
// left open; the returned function commits it.
func holdTx(t *testing.T, db, sql string) func() {
	ctx := context.Background()
	c, err := pg.Connect(ctx, srcConn.WithDB(db), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "BEGIN; "+sql); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := c.Exec(ctx, "COMMIT"); err != nil {
			t.Errorf("commit: %v", err)
		}
		c.Close(ctx)
	}
}

// 1. UPDATE of a small column on rows whose other columns are large TOASTed
// values (logical decoding does not resend unchanged TOAST values), with a
// key and with REPLICA IDENTITY FULL.
func TestP1ToastUnchanged(t *testing.T) {
	forPlugins(t, scenario{name: "toast", wantGO: true,
		schema: `CREATE TABLE docs(id int PRIMARY KEY, flag int, body text, blob bytea, meta jsonb);
			CREATE TABLE docs_full(id int, flag int, body text);
			ALTER TABLE docs_full REPLICA IDENTITY FULL;
			INSERT INTO docs SELECT g, 0, (SELECT string_agg(md5(random()::text || g || s), '') FROM generate_series(1, 4000) s),
				(SELECT string_agg(decode(md5(random()::text), 'hex'), ''::bytea) FROM generate_series(1, 4000)),
				jsonb_build_object('pages', (SELECT jsonb_agg(md5(s::text)) FROM generate_series(1, 2000) s)) FROM generate_series(1, 200) g;
			INSERT INTO docs_full SELECT g, 0, (SELECT string_agg(md5(random()::text || g || s), '') FROM generate_series(1, 4000) s) FROM generate_series(1, 50) g;`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `UPDATE docs SET flag = flag + 1; UPDATE docs SET flag = flag + 1 WHERE id % 3 = 0;
				UPDATE docs_full SET flag = flag + 1 WHERE id <= 10;
				UPDATE docs SET body = body || 'x' WHERE id = 7;`)
			return nil
		},
		check: func(t *testing.T, e *Env, id, db string) {
			if n := q(t, dstConn, db, `SELECT count(*) FROM docs WHERE body IS NULL OR blob IS NULL OR meta IS NULL`); n != "0" {
				t.Fatalf("%s TOASTed value(s) are NULL on the target", n)
			}
		}})
}

// 2. Writes during slot creation and the base copy snapshot, including a
// transaction that starts before the snapshot and commits after it.
func TestP1WritesAcrossSnapshot(t *testing.T) {
	forPlugins(t, scenario{name: "snapshot", wantGO: true,
		schema: `CREATE TABLE t(id bigserial PRIMARY KEY, v int, note text);
			INSERT INTO t(v, note) SELECT g, md5(g::text) FROM generate_series(1, 300000) g;`,
		before: func(t *testing.T, e *Env, id, db string) func() {
			commit := holdTx(t, db, `INSERT INTO t(v, note) VALUES (-1, 'opened before the snapshot'); UPDATE t SET v = -v WHERE id <= 100`)
			stop := background(t, db, 20*time.Millisecond, func(ctx context.Context, c *pgx.Conn, i int) error {
				_, err := c.Exec(ctx, `INSERT INTO t(v, note) VALUES ($1, 'during'); UPDATE t SET v = v + 1 WHERE id = $2`, i, 1000+i%5000)
				return err
			})
			holdCommit = commit
			return stop
		},
		during: func(t *testing.T, e *Env, id, db string) func() {
			time.Sleep(2 * time.Second)
			holdCommit()
			return nil
		},
		check: func(t *testing.T, e *Env, id, db string) {
			if n := q(t, dstConn, db, `SELECT count(*) FROM t WHERE note = 'opened before the snapshot'`); n != "1" {
				t.Fatalf("the transaction spanning the snapshot is missing on the target (%s)", n)
			}
		}})
}

var holdCommit func()

// 3. A single transaction of a million rows, a transaction with thousands of
// savepoints, and a long-running transaction open across the end of the base copy.
func TestP1LargeTransactions(t *testing.T) {
	forPlugins(t, scenario{name: "bigtx", wantGO: true,
		schema: `CREATE TABLE big(id bigint PRIMARY KEY, v text); CREATE TABLE sp(id int PRIMARY KEY, v int); CREATE TABLE lng(id int PRIMARY KEY, v text);
			INSERT INTO big SELECT g, md5(g::text) FROM generate_series(1, 1000) g;`,
		before: func(t *testing.T, e *Env, id, db string) func() {
			// Opened before the start, committed once the database is in sync.
			longCommit = holdTx(t, db, `INSERT INTO lng SELECT g, 'long ' || g FROM generate_series(1, 5000) g`)
			return nil
		},
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			longCommit()
			exec1(t, srcConn, db, `INSERT INTO big SELECT g, md5(g::text) FROM generate_series(1001, 1001000) g`)
			var b strings.Builder
			b.WriteString("BEGIN;")
			for i := 1; i <= 3000; i++ {
				fmt.Fprintf(&b, "SAVEPOINT s%d; INSERT INTO sp VALUES (%d, %d);", i, i, i)
				if i%3 == 0 {
					fmt.Fprintf(&b, "ROLLBACK TO SAVEPOINT s%d;", i)
				} else if i%5 == 0 {
					fmt.Fprintf(&b, "RELEASE SAVEPOINT s%d;", i)
				}
			}
			b.WriteString("COMMIT;")
			exec1(t, srcConn, db, b.String())
			return nil
		},
		check: func(t *testing.T, e *Env, id, db string) {
			if n := q(t, dstConn, db, `SELECT count(*) FROM sp`); n != "2000" {
				t.Fatalf("savepoint table has %s rows on the target, want 2000", n)
			}
		}})
}

var longCommit func()

// 4. The engine killed with SIGKILL in the middle of applying a large
// transaction, then resumed: no row applied twice, none missing.
func TestP1KillDuringLargeApply(t *testing.T) {
	forPlugins(t, scenario{name: "killapply", wantGO: true,
		schema: `CREATE TABLE k(id bigint PRIMARY KEY, v text); INSERT INTO k SELECT g, 'seed' FROM generate_series(1, 1000) g;`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `INSERT INTO k SELECT g, md5(g::text) FROM generate_series(1001, 301000) g`)
			// Kill while the target is inside the apply's transaction.
			e.waitFor(10*time.Minute, "the large transaction being applied on the target", func() bool {
				return q(t, dstConn, db, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND application_name LIKE 'pgcopydb[%' AND backend_xid IS NOT NULL`) != "0"
			})
			time.Sleep(3 * time.Second)
			pid := e.mainPID(id, db)
			pgid, _ := syscall.Getpgid(pid)
			if pgid > 0 {
				syscall.Kill(-pgid, syscall.SIGKILL)
			} else {
				syscall.Kill(pid, syscall.SIGKILL)
			}
			t.Logf("killed the engine (pid %d, group %d) mid-apply", pid, pgid)
			e.waitFor(3*time.Minute, "an automatic resume", func() bool { return e.db(id, db).RetryCount >= 1 })
			e.waitDBState(id, db, 30*time.Minute, orch.DInSync)
			return nil
		}})
}

// 6. A source failover: the new primary has no logical slot (before
// PostgreSQL 17 slots do not survive it). Upwell must require a restart
// from zero, never resume past the gap.
func TestP1SourceSlotGone(t *testing.T) {
	forPlugins(t, scenario{name: "slotgone",
		schema: `CREATE TABLE s(id bigint PRIMARY KEY, v int); INSERT INTO s SELECT g, g FROM generate_series(1, 1000) g;`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			d := e.db(id, db)
			e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/stop", nil, nil, 200)
			e.waitDBState(id, db, 2*time.Minute, orch.DStopped)
			exec1(t, srcConn, db, `INSERT INTO s SELECT g, g FROM generate_series(1001, 2000) g`)
			superSQL(t, srcPort, "postgres", "SELECT pg_drop_replication_slot('"+d.SlotName+"')")
			exec1(t, srcConn, db, `INSERT INTO s SELECT g, g FROM generate_series(2001, 3000) g`)
			e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/resume", nil, nil)
			return nil
		},
		check: func(t *testing.T, e *Env, id, db string) {
			d := e.waitDBState(id, db, 3*time.Minute, orch.DRestartRequired, orch.DFailed, orch.DInSync)
			if d.State == orch.DInSync {
				t.Fatalf("resumed past a lost slot: %s", d.LastError)
			}
			t.Logf("after the slot was lost: %s (%s): %s", d.State, d.ErrorClass, d.LastError)
			if d.State != orch.DRestartRequired {
				t.Fatalf("state %s, want restart_required", d.State)
			}
		}})
}

// 7. Slot WAL retention exceeded (wal_status lost): restart required, never a
// silent continue.
func TestP1SlotWALLost(t *testing.T) {
	forPlugins(t, scenario{name: "wallost",
		schema: `CREATE TABLE w(id bigint PRIMARY KEY, v text); INSERT INTO w SELECT g, 'x' FROM generate_series(1, 1000) g;`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			d := e.db(id, db)
			e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/stop", nil, nil, 200)
			e.waitDBState(id, db, 2*time.Minute, orch.DStopped)
			superSQL(t, srcPort, "postgres", "ALTER SYSTEM SET max_slot_wal_keep_size = '64MB'; SELECT pg_reload_conf()")
			defer superSQL(t, srcPort, "postgres", "ALTER SYSTEM RESET max_slot_wal_keep_size; SELECT pg_reload_conf()")
			for i := 0; i < 6 && superSQL(t, srcPort, "postgres", "SELECT wal_status FROM pg_replication_slots WHERE slot_name = '"+d.SlotName+"'") != "lost"; i++ {
				exec1(t, srcConn, db, `INSERT INTO w SELECT g, repeat(md5(g::text), 20) FROM generate_series(100000 * `+fmt.Sprint(i+1)+`, 100000 * `+fmt.Sprint(i+1)+` + 60000) g`)
				superSQL(t, srcPort, "postgres", "SELECT pg_switch_wal(); CHECKPOINT")
			}
			st := superSQL(t, srcPort, "postgres", "SELECT wal_status FROM pg_replication_slots WHERE slot_name = '"+d.SlotName+"'")
			t.Logf("slot %s wal_status %s", d.SlotName, st)
			if st != "lost" {
				t.Skipf("could not make the slot lose its WAL here (wal_status %s)", st)
			}
			e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/resume", nil, nil)
			return nil
		},
		check: func(t *testing.T, e *Env, id, db string) {
			d := e.waitDBState(id, db, 3*time.Minute, orch.DRestartRequired, orch.DFailed, orch.DInSync)
			t.Logf("after the slot lost its WAL: %s (%s): %s", d.State, d.ErrorClass, d.LastError)
			if d.State != orch.DRestartRequired {
				t.Fatalf("state %s, want restart_required", d.State)
			}
		}})
}

// 8. Data shapes: TRUNCATE, rows moving between partitions, REPLICA IDENTITY
// FULL with duplicate rows, generated and identity columns, enum, jsonb,
// arrays, bytea, numeric NaN and infinity, timestamps around DST changes and
// Unicode in keys, all changed while streaming.
func TestP1DataShapes(t *testing.T) {
	forPlugins(t, scenario{name: "shapes", wantGO: true,
		schema: `CREATE TYPE color AS ENUM ('red', 'green', 'blue');
			CREATE TABLE tr(id int PRIMARY KEY, v text); INSERT INTO tr SELECT g, 'before' FROM generate_series(1, 500) g;
			CREATE TABLE parts(id int, region text, v int, PRIMARY KEY(id, region)) PARTITION BY LIST (region);
			CREATE TABLE parts_eu PARTITION OF parts FOR VALUES IN ('eu'); CREATE TABLE parts_us PARTITION OF parts FOR VALUES IN ('us');
			INSERT INTO parts SELECT g, CASE WHEN g % 2 = 0 THEN 'eu' ELSE 'us' END, g FROM generate_series(1, 400) g;
			CREATE TABLE dup(a int, b text); ALTER TABLE dup REPLICA IDENTITY FULL; INSERT INTO dup SELECT 1, 'same' FROM generate_series(1, 5); INSERT INTO dup VALUES (2, 'x');
			CREATE TABLE gi(id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY, a numeric, b numeric GENERATED ALWAYS AS (a * 2) STORED);
			INSERT INTO gi(a) SELECT g FROM generate_series(1, 100) g;
			CREATE TABLE types(id text PRIMARY KEY, c color, j jsonb, arr int[], tarr text[], b bytea, n numeric, f float8, ts timestamptz, t timestamp, d date);
			INSERT INTO types VALUES ('ascii', 'red', '{"a": [1, null, "x"]}', '{1,2,NULL}', '{"a,b",NULL}', '\x00ff10', 'NaN', 'Infinity', '2026-03-08 01:59:59-05', '2026-03-08 02:30:00', '2026-03-08');`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `TRUNCATE tr; INSERT INTO tr SELECT g, 'after' FROM generate_series(1, 300) g;
				UPDATE parts SET region = 'us' WHERE region = 'eu' AND id <= 100;
				INSERT INTO dup VALUES (1, 'same'), (3, 'new'); UPDATE dup SET b = 'changed' WHERE a = 2; DELETE FROM dup WHERE a = 3;
				INSERT INTO gi(a) SELECT g FROM generate_series(101, 150) g; UPDATE gi SET a = a + 0.5 WHERE id <= 10; DELETE FROM gi WHERE id = 20;
				INSERT INTO types VALUES
				  ('日本語キー', 'blue', '{"ü": "ß", "n": 1.5e300}', '{}', '{"",""}', '\x', '-Infinity', '-Infinity', '2026-11-01 01:30:00-04', '2026-11-01 01:30:00', 'infinity'),
				  ('emoji 🎉', 'green', 'null', '{-1}', '{"\""}', '\xdeadbeef', '0.000000000000000000001', 'NaN', '2026-11-01 01:30:00-05', '1999-12-31 23:59:59.999999', '2000-02-29'),
				  (E'tab\tkey', NULL, '[]', NULL, NULL, NULL, '1e-30', '-0', 'infinity', '-infinity', '-infinity');
				UPDATE types SET n = 'NaN', f = 'Infinity' WHERE id = 'ascii';`)
			return nil
		}})
}

// 8b. REPLICA IDENTITY FULL without a unique key, and a DELETE of one of
// several identical rows: the engine applies it to every identical row. The
// expected result is NO-GO naming that cause (preflight warns of it).
func TestP1IdentityFullDuplicates(t *testing.T) {
	forPlugins(t, scenario{name: "fulldup",
		schema: `CREATE TABLE dup(a int, b text); ALTER TABLE dup REPLICA IDENTITY FULL;
			INSERT INTO dup SELECT 1, 'same' FROM generate_series(1, 5); INSERT INTO dup VALUES (2, 'x');`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `DELETE FROM dup WHERE ctid = (SELECT min(ctid) FROM dup WHERE a = 1)`)
			return nil
		},
		check: func(t *testing.T, e *Env, id, db string) {
			time.Sleep(10 * time.Second)
			e.startCutover(id)
			v := e.migration(id).Migration.Flags.Verdict
			rc := findCheck(verificationRows(e, id), db, "row_counts")
			if v != "NO-GO" || rc == nil || !strings.Contains(string(rc.Detail), "REPLICA IDENTITY FULL without a unique key") {
				logVerification(t, e, id)
				t.Fatalf("verdict %s; want NO-GO naming the duplicate rows (%+v)", v, rc)
			}
			t.Logf("NO-GO as expected: %.300s", rc.Detail)
		}})
}
