//go:build integration

package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"

	_ "modernc.org/sqlite"
)

// D108, the droplet (2026-10-17): pgcopydb 0.18 applies in libpq pipeline
// mode and never reads the target's answers, so it published as applied
// (sentinel replay_lsn, the source slot's flush) what it had only queued:
// 2.3 GB past the target's replication origin when a large transaction
// overflowed the send buffer. Here the target refuses one change (a
// duplicate key). The engine must stop with that error, and the source slot
// must never be confirmed past a change the target did not commit.
func TestApplyErrorStopsProgress(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_applyerr")
	id := streamingMigration(t, e, "Apply error", db)
	slot := e.db(id, db).SlotName

	exec1(t, dstConn, db, `INSERT INTO orders(id, customer, amount, note) VALUES (999999777, 1, 1, 'target only')`)
	exec1(t, srcConn, db, `INSERT INTO orders(id, customer, amount, note) VALUES (999999777, 2, 2, 'conflicts')`)
	after := q(t, srcConn, db, `SELECT pg_current_wal_lsn()::text`)
	for i := 0; i < 50; i++ {
		exec1(t, srcConn, db, `INSERT INTO orders(customer, amount, note) VALUES (3, 3, 'after the conflict')`)
	}

	deadline := time.Now().Add(2 * time.Minute)
	var d orch.Database
	for time.Now().Before(deadline) {
		if past := q(t, srcConn, db, `SELECT (confirmed_flush_lsn >= '`+after+`'::pg_lsn)::text FROM pg_replication_slots WHERE slot_name = '`+slot+`'`); past == "true" {
			t.Fatalf("the source slot was confirmed past %s although the target refused a change before it (target has %s 'after the conflict' rows)",
				after, q(t, dstConn, db, `SELECT count(*)::text FROM orders WHERE note = 'after the conflict'`))
		}
		d = e.db(id, db)
		if strings.Contains(d.LastError, "duplicate key") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !strings.Contains(d.LastError, "duplicate key") {
		t.Fatalf("the target's refusal never surfaced: state %s, error %q", d.State, d.LastError)
	}
	t.Logf("surfaced: %s: %s", d.State, d.LastError)

	// Once the conflict is resolved, the resumed engine applies everything.
	exec1(t, dstConn, db, `DELETE FROM orders WHERE id = 999999777`)
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/resume", nil, nil, 200)
	e.waitFor(3*time.Minute, "the changes after the conflict to reach the target", func() bool {
		return q(t, dstConn, db, `SELECT count(*)::text FROM orders WHERE note = 'after the conflict'`) == "50" &&
			q(t, dstConn, db, `SELECT note FROM orders WHERE id = 999999777`) == "conflicts"
	})
	e.abortAndClean(id, "Apply error")
}

// engineRSS sums the resident memory, in MiB, of the pgcopydb processes
// for db: those whose command line names it, and their children (which
// rename themselves).
func engineRSS(t *testing.T, db string) int {
	t.Helper()
	ents, _ := os.ReadDir("/proc")
	parent := map[int]int{}
	rss := map[int]int{}
	var roots []int
	for _, ent := range ents {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		st, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(st), "\n") {
			f := strings.Fields(l)
			if len(f) >= 2 && f[0] == "PPid:" {
				parent[pid], _ = strconv.Atoi(f[1])
			}
			if len(f) >= 2 && f[0] == "VmRSS:" {
				kb, _ := strconv.Atoi(f[1])
				rss[pid] = kb / 1024
			}
		}
		if cmd, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil && strings.Contains(string(cmd), "pgcopydb") && strings.Contains(string(cmd), db) {
			roots = append(roots, pid)
		}
	}
	in := map[int]bool{}
	for _, r := range roots {
		in[r] = true
	}
	for changed := true; changed; {
		changed = false
		for pid, pp := range parent {
			if !in[pid] && in[pp] {
				in[pid], changed = true, true
			}
		}
	}
	total := 0
	for pid := range in {
		total += rss[pid]
	}
	return total
}

// D108, the droplet (2026-10-17): with the target slower than the engine,
// pgcopydb queued statements in memory without bound
// until libpq failed ("cannot allocate memory for output buffer"). Here the
// target is held by a lock while 380 MB of transactions arrive; the engine's
// memory must stay bounded, and the transaction must then apply in full.
func TestApplyQueueBoundedMemory(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_applyq")
	id := streamingMigration(t, e, "Apply queue", db)
	base := engineRSS(t, db)

	ctx := context.Background()
	lock, err := pg.Connect(ctx, dstConn.WithDB(db), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close(ctx)
	if _, err := lock.Exec(ctx, `BEGIN; LOCK TABLE orders IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	// About 380 MB of changes in 30 transactions of 200 statements: orders
	// of 128 kB (compressed in the WAL, decoded in full), each followed by
	// an item so that the engine cannot merge them. The engine holds one
	// transaction at a time in memory; what it sends must not pile up.
	for i := 0; i < 30; i++ {
		exec1(t, srcConn, db, `DO $$ DECLARE o bigint; BEGIN
			FOR g IN 1..100 LOOP
				INSERT INTO orders(customer, amount, note) VALUES (9, 9, 'big '||repeat(md5(g::text), 4000)) RETURNING id INTO o;
				INSERT INTO items VALUES (o, 1, 'big', 1);
			END LOOP; END $$`)
	}
	// Three minutes for the engine to receive the transaction and queue
	// what it can while the target is held.
	peak := 0
	for i := 0; i < 90; i++ {
		if m := engineRSS(t, db); m > peak {
			peak = m
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("received up to %s; source at %s", e.db(id, db).WriteLSN, q(t, srcConn, db, `SELECT pg_current_wal_lsn()::text`))
	t.Logf("engine memory: %d MiB before, %d MiB at peak while the target was held", base, peak)
	if _, err := lock.Exec(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	e.waitFor(10*time.Minute, "every transaction on the target", func() bool {
		return q(t, dstConn, db, `SELECT count(*)::text FROM orders WHERE note LIKE 'big %'`) == "3000"
	})
	if peak-base > 120 {
		t.Fatalf("the engine grew by %d MiB while the target held 380 MB of transactions", peak-base)
	}
	e.abortAndClean(id, "Apply queue")
}

// D108: pgcopydb opened its SQLite files with no busy timeout, so a file
// another process was writing failed the open at once ("database is
// locked"), for example the index the engine adds to an existing CDC file
// (D106) while the receive process writes to it. Here the test holds a write
// lock on the current replay file while the database resumes; the engine
// must wait for it and stream again.
func TestEngineWaitsForCDCFileLock(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_cdclock")
	id := streamingMigration(t, e, "CDC file lock", db)
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/stop", nil, nil, 200)
	e.waitDBState(id, db, 2*time.Minute, orch.DStopped)
	files, _ := filepath.Glob(filepath.Join(e.Cfg.RunsDir(), "*"+db+"*", "clone", "cdc", "*-replay.db"))
	if len(files) == 0 {
		t.Fatal("no CDC replay file")
	}
	sort.Strings(files)
	lockdb, err := sql.Open("sqlite", files[len(files)-1])
	if err != nil {
		t.Fatal(err)
	}
	defer lockdb.Close()
	conn, err := lockdb.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	exec1(t, srcConn, db, `INSERT INTO orders(customer, amount, note) VALUES (7, 7, 'after the lock')`)
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/resume", nil, nil, 200)
	time.Sleep(8 * time.Second)
	conn.ExecContext(context.Background(), `ROLLBACK`)
	conn.Close()
	if d := e.waitDBState(id, db, 3*time.Minute, orch.DInSync, orch.DFailed, orch.DRestartRequired, orch.DStopped); d.State != orch.DInSync {
		t.Fatalf("resume while a CDC file was locked ended %s: %s", d.State, d.LastError)
	} else if d.RetryCount > 0 {
		t.Fatalf("the engine failed while a CDC file was locked and was resumed %d time(s) automatically", d.RetryCount)
	}
	e.waitFor(2*time.Minute, "the row written while stopped to reach the target", func() bool {
		return q(t, dstConn, db, `SELECT count(*) FROM orders WHERE note = 'after the lock'`) == "1"
	})
	e.abortAndClean(id, "CDC file lock")
}

// D108: after "Follow mode is now done, reached endpos", pgcopydb's main
// process still copies the sequences through its own catalog and can wait
// there for minutes ("database is locked"). The drain waited for the unit to
// exit and timed out: a NO-GO although every change had been applied. Here
// the test holds the catalog the moment the engine reports its end
// position; the cutover must take the engine's word, stop the unit and end
// GO (Upwell syncs the sequences itself).
func TestDrainDoesNotWaitForEngineSequenceStep(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	if _, _, err := e.Orch.SettingsService().Set(context.Background(), "cutover_drain_timeout_seconds", 60, "test"); err != nil {
		t.Fatal(err)
	}
	db := uniq("it_drainseq")
	id := streamingMigration(t, e, "Drain sequence step", db)
	runs, _ := filepath.Glob(filepath.Join(e.Cfg.RunsDir(), "*"+db+"*"))
	if len(runs) == 0 {
		t.Fatal("no run directory")
	}
	logf := filepath.Join(runs[0], "engine.log")
	start, _ := os.Stat(logf)
	held := make(chan *sql.Conn, 1)
	go func() {
		defer close(held)
		for i := 0; i < 6000; i++ {
			b, _ := os.ReadFile(logf)
			if int64(len(b)) > start.Size() && strings.Contains(string(b[start.Size():]), "Follow mode is now done") {
				cat, err := sql.Open("sqlite", filepath.Join(runs[0], "clone", "schema", "source.db"))
				if err != nil {
					return
				}
				c, err := cat.Conn(context.Background())
				if err != nil {
					return
				}
				if _, err := c.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err == nil {
					held <- c
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	op := e.startCutover(id)
	c, ok := <-held
	if !ok || c == nil {
		t.Fatal("the test never held the engine's catalog")
	}
	c.ExecContext(context.Background(), `ROLLBACK`)
	c.Close()
	if v := e.migration(id).Migration.Flags.Verdict; v != "GO" {
		t.Fatalf("verdict %s (%s) with the engine's sequence step held after it reached its end position", v, op.Error)
	}
	e.cleanup(id, "Drain sequence step")
}
