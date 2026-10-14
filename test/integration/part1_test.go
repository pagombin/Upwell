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
	// dstPort, when set, is another target cluster (scenario 16).
	dstPort int
}

func runScenario(t *testing.T, plugin string, sc scenario) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_p1_" + sc.name[:min(len(sc.name), 10)])
	dst := dstConn
	if sc.dstPort != 0 {
		dst.Port = sc.dstPort
		if c, err := pg.Connect(context.Background(), dst, 5*time.Second); err != nil {
			t.Skipf("no target cluster on port %d: %v", sc.dstPort, err)
		} else {
			c.Close(context.Background())
		}
		t.Cleanup(func() { exec1(t, dst, "defaultdb", "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)") })
	}
	dropDB(t, db)
	exec1(t, srcConn, "defaultdb", "CREATE DATABASE "+db)
	t.Cleanup(func() { dropDB(t, db) })
	exec1(t, srcConn, db, sc.schema)
	if _, _, err := e.Orch.SettingsService().Set(context.Background(), "cutover_drain_timeout_seconds", 3600, "test"); err != nil {
		t.Fatal(err)
	}
	set := map[string]any{"decoding_plugin": plugin}
	if sc.dstPort != 0 {
		set["_target_port"] = sc.dstPort
	}
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
	n, diffs := compareDatabaseOn(t, srcConn, dst, db)
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

// waitSlotReady waits until db's logical slot exists with a consistent start
// point (confirmed_flush_lsn is set once creation has finished).
func waitSlotReady(t *testing.T, e *Env, db string) {
	t.Helper()
	e.waitFor(10*time.Minute, "the slot of "+db, func() bool {
		return q(t, srcConn, "defaultdb", fmt.Sprintf(`SELECT count(*)::text FROM pg_replication_slots WHERE database = '%s' AND slot_type = 'logical' AND confirmed_flush_lsn IS NOT NULL`, db)) == "1"
	})
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
			CREATE TABLE pad(id bigint PRIMARY KEY, v text);
			INSERT INTO big SELECT g, md5(g::text) FROM generate_series(1, 1000) g;
			INSERT INTO pad SELECT g, md5(g::text) FROM generate_series(1, 1500000) g;`,
		// Opened during the base copy (after the slot exists: creating a
		// logical slot waits for every running transaction), committed once
		// the database is in sync.
		during: func(t *testing.T, e *Env, id, db string) func() {
			// The state reads base_copy from the engine's launch, before its
			// slot exists: wait for the slot (opened earlier, a writing
			// transaction holds slot creation until it commits).
			waitSlotReady(t, e, db)
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

// 13. Every cutover step interrupted by an Upwell restart (SIGKILL of the
// process), then the cutover continued: GO with identical data.
func TestP1CutoverRestartAtEveryStep(t *testing.T) {
	steps := []string{"readiness", "writers", "heartbeat", "await", "endpos", "drain", "origin", "sequences", "verify"}
	for _, plugin := range plugins {
		for _, step := range steps {
			plugin, step := plugin, step
			t.Run(plugin+"/"+step, func(t *testing.T) {
				e := newProcEnv(t)
				db := uniq("it_p1_rs_" + step[:min(4, len(step))])
				seedDB(t, db, 20000)
				name := "Restart at " + step + " " + plugin
				id := e.defineMigration(name, []string{db}, map[string]any{"decoding_plugin": plugin})
				e.preflight(id)
				e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
				e.waitDBState(id, db, 5*time.Minute, orch.DInSync)
				exec1(t, srcConn, db, `INSERT INTO orders(customer, amount, note) SELECT g, g, 'streamed' FROM generate_series(1, 2000) g; UPDATE orders SET amount = amount + 1 WHERE id % 97 = 0`)
				time.Sleep(10 * time.Second)
				var rd struct {
					Ready bool `json:"ready"`
				}
				e.waitFor(10*time.Minute, "readiness", func() bool {
					e.do("GET", "/api/v1/migrations/"+id+"/cutover/readiness", nil, &rd, 200)
					return rd.Ready
				})
				var r struct {
					OperationID string `json:"operation_id"`
				}
				e.do("POST", "/api/v1/migrations/"+id+"/cutover", map[string]any{"confirm_writers_stopped": true}, &r, 202)
				// Kill Upwell while the step runs (or as soon as it has passed).
				killedAt := ""
				order := map[string]int{}
				for i, s := range []string{"readiness", "writers", "heartbeat", "await", "endpos", "drain", "origin", "sequences", "verify", "verdict"} {
					order[s] = i
				}
				e.waitFor(10*time.Minute, "step "+step, func() bool {
					var op orch.Operation
					e.do("GET", "/api/v1/operations/"+r.OperationID, nil, &op, 200)
					for _, st := range op.Steps {
						if st.State == "running" && order[st.Key] >= order[step] {
							killedAt = st.Key
							return true
						}
					}
					return op.State != "running"
				})
				if killedAt == "" {
					t.Skipf("the cutover finished before step %s could be interrupted", step)
				}
				e.restartProc()
				t.Logf("killed Upwell during step %s", killedAt)
				// Continue: the console offers it; the API is the same call.
				m := e.migration(id).Migration
				if m.Flags.Verdict == "" {
					var r2 struct {
						OperationID string `json:"operation_id"`
					}
					code := e.do("POST", "/api/v1/migrations/"+id+"/cutover", map[string]any{"confirm_writers_stopped": true}, &r2)
					if code == 202 {
						e.waitOp(r2.OperationID, 25*time.Minute)
					} else {
						t.Logf("continue answered %d", code)
					}
				}
				v := e.migration(id).Migration.Flags.Verdict
				_, diffs := compareDatabase(t, db)
				if v == "GO" && len(diffs) > 0 {
					t.Fatalf("CRITICAL: GO with different data after a restart at %s:\n%s", killedAt, strings.Join(diffs, "\n"))
				}
				if v != "GO" {
					logVerification(t, e, id)
					t.Fatalf("restart at %s: verdict %q, want GO (writes were stopped throughout)", killedAt, v)
				}
				e.cleanup(id, name)
			})
		}
	}
}

// 17b. Descending and ascending sequences used while streaming: after the
// cutover neither may be behind on the target (for a descending sequence,
// behind is a higher value). The comparison is compareDatabase's own.
func TestP1SequencesStreaming(t *testing.T) {
	forPlugins(t, scenario{name: "sequences", wantGO: true,
		schema: `CREATE SEQUENCE down_seq INCREMENT BY -3 MAXVALUE 100000 START WITH 100000;
			CREATE SEQUENCE up_seq INCREMENT BY 7;
			CREATE TABLE t(id bigint PRIMARY KEY DEFAULT nextval('down_seq'), u bigint DEFAULT nextval('up_seq'));
			INSERT INTO t SELECT FROM generate_series(1, 1000);`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `INSERT INTO t SELECT FROM generate_series(1, 500); SELECT nextval('down_seq') FROM generate_series(1, 50)`)
			return nil
		},
		check: func(t *testing.T, e *Env, id, db string) {
			if s, d := q(t, srcConn, db, "SELECT last_value::text FROM down_seq"), q(t, dstConn, db, "SELECT last_value::text FROM down_seq"); s != d {
				t.Fatalf("down_seq: source %s, target %s", s, d)
			}
		}})
}

// 16. PostgreSQL 16 to 18: the row hashes verification compares are computed
// on each server from the row's text form, so a type that prints differently
// on 18 would be a false NO-GO, and identical text from different data a
// false GO. Types whose text form depends on settings or version are
// streamed; the verdict must be GO with every row compared, and then one
// changed byte on the target must turn a full re-verification into NO-GO.
// PostGIS is not installed locally, and the local PostgreSQL 18 has no XML
// support (preflight now blocks xml columns there: TestPreflightXMLSupport).
func TestP1CrossVersionHashing(t *testing.T) {
	forPlugins(t, scenario{name: "xver", wantGO: true, dstPort: pg18Port,
		settings: map[string]any{"verify_large_tables": "full"},
		schema: `CREATE TABLE x(id int PRIMARY KEY, f4 real, f8 float8, n numeric, m money, iv interval, tz timetz, ts timestamptz,
				j jsonb, js json, tv tsvector, r int4range, mr int4multirange, tr tstzrange, p point, bx box, pl polygon, c circle,
				ip inet, cd cidr, mac macaddr8, u uuid, bt bit varying(16), ba bytea, a float8[], o oid, ch char(5), b boolean);
			INSERT INTO x SELECT g, g / 3.0, g / 7.0, g / 11.0, g * 1.25, make_interval(days => g, secs => g / 9.0), '12:34:56.789+05:30',
				'2026-03-29 01:30:00+01'::timestamptz + g * interval '17 minutes', jsonb_build_object('k', g, 'f', g / 3.0, 'a', jsonb_build_array(1.0, 'x')),
				'{"b": 1, "a": [1.10, 2]}', to_tsvector('simple', 'the quick ' || g), int4range(g, g + 10), int4multirange(int4range(1, g + 2), int4range(g + 5, g + 9)),
				tstzrange('2026-01-01', '2026-01-01'::timestamptz + g * interval '1 hour'), point(g / 3.0, -g), box(point(0, 0), point(g, g / 7.0)), polygon(box(point(0, 0), point(g, 1))),
				circle(point(g, g), g / 3.0), ('10.0.' || (g % 255) || '.1/24')::inet, '192.168.0.0/16', '08:00:2b:01:02:03:04:05', md5(g::text)::uuid, (g % 65536)::bit(16)::varbit,
				decode(md5(g::text), 'hex'), ARRAY[g / 3.0, 1e-300, 'NaN', '-Infinity']::float8[], g, 'ab', g % 2 = 0
			FROM generate_series(1, 3000) g;`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `UPDATE x SET f4 = f4 * 1.1, f8 = 'Infinity', iv = iv * 1.5, m = m + 0.01::money WHERE id % 10 = 0;
				INSERT INTO x(id, f4, f8, n, j, tv) VALUES (100001, 'NaN', '-0', 'NaN', '{"deep": {"x": [null, true, 1e400]}}', '');
				DELETE FROM x WHERE id % 97 = 0`)
			return nil
		},
		check: func(t *testing.T, e *Env, id, db string) {
			if f := e.migration(id).Migration.Flags; f.UnverifiedCount != 0 {
				t.Fatalf("GO not fully verified: %v", f.Unverified)
			}
			// One changed byte on the PostgreSQL 18 target: a full re-verification must be NO-GO.
			dst := dstConn
			dst.Port = pg18Port
			exec1(t, dst, db, `UPDATE x SET js = '{"b": 1, "a": [1.10, 3]}' WHERE id = 1500`)
			var op struct {
				ID string `json:"operation_id"`
			}
			e.do("POST", "/api/v1/migrations/"+id+"/verify", map[string]any{"full": true}, &op, 202)
			e.waitOp(op.ID, 10*time.Minute)
			if v := e.migration(id).Migration.Flags.Verdict; v != "NO-GO" {
				logVerification(t, e, id)
				t.Fatalf("one changed byte on the 18 target: verdict %s, want NO-GO", v)
			}
		}})
}

// 10-12. A writer with an open transaction through the writers step, a writer
// resuming after verification, and an Admin override with writes running,
// under both plugins: never GO, and the independent comparison shows what
// would have been lost.
func TestP1WritersUnderBothPlugins(t *testing.T) {
	cases := []struct {
		name string
		f    func(*testing.T)
	}{
		{"open_txn", TestWritersOpenTransactionRefused},
		{"after_verify", TestWritesAfterVerificationNoGo},
		{"override", TestOverrideWithWritesRunningNoGo},
	}
	for _, p := range plugins {
		for _, c := range cases {
			p, c := p, c
			t.Run(p+"/"+c.name, func(t *testing.T) {
				itPlugin = p
				defer func() { itPlugin = "" }()
				c.f(t)
			})
		}
	}
}

// 14. The final heartbeat never reaches the target (its apply is blocked):
// NO-GO, streaming continues; once the apply is unblocked the heartbeat
// arrives after nudges, and a new cutover ends GO with identical data.
func TestP1HeartbeatNeverThenLate(t *testing.T) {
	for _, p := range plugins {
		p := p
		t.Run(p, func(t *testing.T) {
			itPlugin = p
			defer func() { itPlugin = "" }()
			e := newEnv(t, "", orch.TestHooks{})
			if _, _, err := e.Orch.SettingsService().Set(context.Background(), "cutover_drain_timeout_seconds", 60, "test"); err != nil {
				t.Fatal(err)
			}
			db := uniq("it_p1_hb")
			name := "Heartbeat never " + p
			id := streamingMigration(t, e, name, db)
			exec1(t, srcConn, db, `INSERT INTO orders(customer, amount, note) SELECT g, g, 'before' FROM generate_series(1, 500) g`)
			time.Sleep(10 * time.Second)
			// Block the apply: a session on the target holds the heartbeat table.
			ctx := context.Background()
			lk, err := pg.Connect(ctx, dstConn.WithDB(db), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := lk.Exec(ctx, `BEGIN; LOCK TABLE upwell.heartbeat IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
			op := e.cutoverOp(id, map[string]any{"confirm_writers_stopped": true})
			v := e.migration(id).Migration.Flags.Verdict
			t.Logf("blocked: operation %s, verdict %q: %.200s", op.State, v, op.Error)
			if v == "GO" {
				t.Fatal("GO although the final heartbeat never reached the target")
			}
			if !strings.Contains(op.Error, "final heartbeat did not reach the target") {
				t.Fatalf("the failure does not name the missing heartbeat: %q", op.Error)
			}
			lk.Exec(ctx, "ROLLBACK")
			lk.Close(ctx)
			// Streaming continued: once unblocked, a new cutover ends GO.
			e.waitDBState(id, db, 5*time.Minute, orch.DInSync)
			exec1(t, srcConn, db, `INSERT INTO orders(customer, amount, note) SELECT g, g, 'after' FROM generate_series(1, 500) g`)
			time.Sleep(10 * time.Second)
			e.startCutover(id)
			v = e.migration(id).Migration.Flags.Verdict
			_, diffs := compareDatabase(t, db)
			if v == "GO" && len(diffs) > 0 {
				t.Fatalf("CRITICAL: GO with different data:\n%s", strings.Join(diffs, "\n"))
			}
			if v != "GO" {
				logVerification(t, e, id)
				t.Fatalf("second cutover: verdict %q, want GO", v)
			}
			e.cleanup(id, name)
		})
	}
}

// TestSnapshotWaitShown (Part 2, D94): a transaction left open on the source
// (in another database) holds the slot creation, so the base copy cannot
// start. Upwell names the session on the migration view and in an event, and
// the copy starts by itself once it ends.
func TestSnapshotWaitShown(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_snapwait")
	seedDB(t, db, 1000)
	other := uniq("it_snapwait_other")
	dropDB(t, other)
	exec1(t, srcConn, "defaultdb", "CREATE DATABASE "+other)
	t.Cleanup(func() { dropDB(t, other) })
	exec1(t, srcConn, other, "CREATE TABLE t(i int)")
	ctx := context.Background()
	w, err := pgx.Connect(ctx, fmt.Sprintf("postgres://doadmin:%s@127.0.0.1:%d/%s?sslmode=disable&application_name=batch-report", password, srcPort, other))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(ctx)
	if _, err := w.Exec(ctx, "BEGIN; INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	var pid int
	w.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid)
	id := e.defineMigration("Snapshot wait", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	var view struct {
		Waits map[string]struct {
			Since    int64 `json:"since"`
			Blockers []struct {
				PID int    `json:"pid"`
				App string `json:"application"`
			} `json:"blockers"`
		} `json:"snapshot_waits"`
	}
	e.waitFor(2*time.Minute, "the snapshot wait on the migration view", func() bool {
		view.Waits = nil
		e.do("GET", "/api/v1/migrations/"+id, nil, &view, 200)
		sw, ok := view.Waits[db]
		return ok && len(sw.Blockers) > 0 && sw.Blockers[0].PID == pid && sw.Blockers[0].App == "batch-report"
	})
	if !hasEvent(t, e, id, "snapshot_wait", fmt.Sprintf("PID %d (batch-report)", pid)) {
		t.Fatal("no event naming the session the slot creation waits for")
	}
	if _, err := w.Exec(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	e.waitDBState(id, db, 5*time.Minute, orch.DInSync)
	view.Waits = nil // decoding into a map merges
	e.do("GET", "/api/v1/migrations/"+id, nil, &view, 200)
	if _, ok := view.Waits[db]; ok {
		t.Fatal("the wait is still shown after the copy started")
	}
	if !hasEvent(t, e, id, "snapshot_wait", "the base copy can start") {
		t.Fatal("no event saying the wait ended")
	}
	e.abortAndClean(id, "Snapshot wait")
}

func hasEvent(t *testing.T, e *Env, id, typ, contains string) bool {
	t.Helper()
	var ev []struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	e.do("GET", "/api/v1/migrations/"+id+"/events?limit=500", nil, &ev, 200)
	for _, x := range ev {
		if x.Type == typ && strings.Contains(x.Message, contains) {
			return true
		}
	}
	return false
}
