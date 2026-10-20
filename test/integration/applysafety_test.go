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
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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

// D109, the droplet (2026-10-19): WAL is the whole cluster's, so a database
// with no writes of its own streams every other database's changes too and
// reads as megabytes behind between the engine's position reports. Its
// state flipped between In sync and Catching up every few seconds while
// TPC-C ran on its neighbour. A database the heartbeat shows current stays
// In sync.
func TestIdleDatabaseStaysInSyncBesideBusyOne(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	busy, idle := uniq("it_busy"), uniq("it_idle")
	seedDB(t, busy, 3000)
	seedDB(t, idle, 3000)
	id := e.defineMigration("Busy and idle", []string{busy, idle}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, busy, 3*time.Minute, orch.DInSync)
	e.waitDBState(id, idle, 3*time.Minute, orch.DInSync)
	e.waitFor(time.Minute, "a heartbeat lag for the idle database", func() bool {
		c := e.condition(id, "lag", idle)
		return c != nil && c.OK
	})

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { // a few MB of WAL a second on the busy database (the local slots keep 1 GB)
		defer close(done)
		ctx := context.Background()
		c, err := pg.Connect(ctx, srcConn.WithDB(busy), time.Minute)
		if err != nil {
			return
		}
		defer c.Close(ctx)
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.Exec(ctx, `INSERT INTO orders(customer, amount, note) SELECT g, g, repeat(md5(g::text), 8) FROM generate_series(1, 5000) g`)
			time.Sleep(time.Second)
		}
	}()
	defer func() { close(stop); <-done }()
	flips, maxBacklog := 0, int64(0)
	for i := 0; i < 60; i++ {
		d := e.db(id, idle)
		if d.BacklogBytes != nil && *d.BacklogBytes > maxBacklog {
			maxBacklog = *d.BacklogBytes
		}
		if d.State != orch.DInSync {
			flips++
		}
		if i%5 == 0 {
			lag := e.condition(id, "lag", idle)
			t.Logf("t=%2ds idle %s, backlog %d MiB, %s", i, d.State, maxBacklog>>20, lag.Detail)
		}
		time.Sleep(time.Second)
	}
	t.Logf("idle database: %d of 60 samples not in sync; largest byte backlog %d MiB", flips, maxBacklog>>20)
	if maxBacklog < 16<<20 {
		t.Fatalf("the busy database wrote too little WAL (largest backlog %d MiB) for the test to show anything", maxBacklog>>20)
	}
	if flips > 0 {
		t.Fatalf("the idle database left In sync in %d of 60 samples although nothing changed in it", flips)
	}
}

// D111: the receive process writes the change files in batches, the
// transform writes replay.db in one transaction per pass, and the files
// checkpoint less often. None of these files is the durable record, so an
// engine killed with a batch open must lose nothing. Here three writers mix
// inserts, updates and deletes in small transactions, plus transactions
// larger than one batch (half of them COPYs, D114), while the change files
// rotate every 4 MB and the
// whole engine is killed four times. The cutover must end GO with every row
// identical, under both plugins.
func TestEngineKilledUnderLoadLosesNothing(t *testing.T) {
	t.Setenv("PGCOPYDB_MAX_REPLAYDB_SIZE", "4MB")
	forPlugins(t, scenario{name: "killload", wantGO: true,
		schema: `CREATE TABLE w(id bigint PRIMARY KEY, v int NOT NULL, note text);
			INSERT INTO w SELECT g, 0, md5(g::text) FROM generate_series(1, 20000) g;`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			var stops []func()
			for k := 0; k < 3; k++ {
				k := k
				stops = append(stops, background(t, db, 20*time.Millisecond, func(ctx context.Context, c *pgx.Conn, i int) error {
					base := int64(1000000*(k+1) + i*10)
					if i%100 == 49 { // larger than one batch of the change files
						_, err := c.Exec(ctx, `INSERT INTO w SELECT g, 1, repeat('x', 200) FROM generate_series($1::bigint, $1::bigint + 2999) g`, base*10)
						return err
					}
					if i%100 == 99 { // the same as a COPY: rows that share an LSN, sent again after a kill
						rows := make([][]any, 3000)
						for j := range rows {
							rows[j] = []any{base*10 + int64(j), 2, "copied"}
						}
						_, err := c.CopyFrom(ctx, pgx.Identifier{"w"}, []string{"id", "v", "note"}, pgx.CopyFromRows(rows))
						return err
					}
					_, err := c.Exec(ctx, `BEGIN;
						INSERT INTO w VALUES ($1, 1, 'new'), ($1 + 1, 1, 'new'), ($1 + 2, 1, 'new');
						UPDATE w SET v = v + 1, note = md5(random()::text) WHERE id = (1 + ($1 % 20000));
						DELETE FROM w WHERE id = (1 + (($1 * 7) % 20000));
						COMMIT`, base)
					return err
				}))
			}
			for kill := 1; kill <= 4; kill++ {
				time.Sleep(8 * time.Second)
				pid := e.mainPID(id, db)
				before := e.db(id, db).RetryCount
				if pgid, _ := syscall.Getpgid(pid); pgid > 0 {
					syscall.Kill(-pgid, syscall.SIGKILL)
				} else {
					syscall.Kill(pid, syscall.SIGKILL)
				}
				t.Logf("kill %d: engine pid %d", kill, pid)
				e.waitFor(3*time.Minute, "an automatic resume", func() bool { return e.db(id, db).RetryCount > before })
				e.waitDBState(id, db, 5*time.Minute, orch.DInSync, orch.DCatchUp)
			}
			files, _ := filepath.Glob(filepath.Join(e.Cfg.RunsDir(), "*"+db+"*", "clone", "cdc", "*-output.db"))
			t.Logf("%d change file(s) after the kills", len(files))
			return func() {
				for _, s := range stops {
					s()
				}
				e.waitDBState(id, db, 10*time.Minute, orch.DInSync)
			}
		}})
}

// D114: when the receive rotates the change file, the transaction whose
// COMMIT ended the file was skipped if the apply's last pass over the file
// ran just before that COMMIT was stored: the apply saw the file closed,
// moved to the next one, and the next file's positions passed the commit.
// The receive stores up to 1,000 messages at a time, so the window is a
// whole batch wide. Here the files rotate every megabyte (the default is
// 1 GB) under three writers, with no failures at all, and every row must
// arrive. PGCOPYDB_TEST_ADVANCE_DELAY_MS widens the window so the race is
// not left to chance: with it, upwell.10 lost whole transactions here.
func TestRotationKeepsEveryTransaction(t *testing.T) {
	t.Setenv("PGCOPYDB_MAX_REPLAYDB_SIZE", "1MB")
	t.Setenv("PGCOPYDB_TEST_ADVANCE_DELAY_MS", "50")
	forPlugins(t, scenario{name: "rotate", wantGO: true,
		schema: `CREATE TABLE w(id bigint PRIMARY KEY, v int NOT NULL, note text);
			INSERT INTO w SELECT g, 0, md5(g::text) FROM generate_series(1, 20000) g;`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			var stops []func()
			for k := 0; k < 3; k++ {
				k := k
				stops = append(stops, background(t, db, 10*time.Millisecond, func(ctx context.Context, c *pgx.Conn, i int) error {
					base := int64(1000000*(k+1) + i*10)
					if i%5 == 4 {
						_, err := c.Exec(ctx, `INSERT INTO w SELECT g, 1, repeat('x', 200) FROM generate_series($1::bigint, $1::bigint + 1499) g`, base*10)
						return err
					}
					_, err := c.Exec(ctx, `BEGIN;
						INSERT INTO w VALUES ($1, 1, 'new'), ($1 + 1, 1, 'new'), ($1 + 2, 1, 'new');
						UPDATE w SET v = v + 1, note = md5(random()::text) WHERE id = (1 + ($1 % 20000));
						DELETE FROM w WHERE id = (1 + (($1 * 7) % 20000));
						COMMIT`, base)
					return err
				}))
			}
			time.Sleep(40 * time.Second)
			return func() {
				for _, s := range stops {
					s()
				}
				e.waitDBState(id, db, 10*time.Minute, orch.DInSync)
			}
		}})
}

// D113: Upwell recorded the slot's WAL headroom (safe_wal_size) but never
// warned before it ran out, although an invalidated slot means restarting the
// database from zero. Here the database is stopped, so its slot does not
// advance, while the source writes a few MB of WAL a second against the local
// 1 GB max_slot_wal_keep_size. The critical headroom alert must fire well
// before the slot is lost, and clear once the database streams again.
func TestSlotHeadroomAlert(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_headroom")
	id := streamingMigration(t, e, "Slot headroom", db)
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/stop", nil, nil, 200)
	e.waitDBState(id, db, 2*time.Minute, orch.DStopped)
	stop := background(t, db, 200*time.Millisecond, func(ctx context.Context, c *pgx.Conn, i int) error {
		_, err := c.Exec(ctx, `INSERT INTO orders(customer, amount, note) SELECT g, g, repeat(md5(g::text), 8) FROM generate_series(1, 2000) g`)
		return err
	})
	var a *orch.Alert
	e.waitFor(6*time.Minute, "the slot headroom alert", func() bool {
		a = e.firing("slot_headroom_critical", db)
		return a != nil
	})
	stop()
	t.Logf("%s: %s", a.Severity, a.Message)
	if st := q(t, srcConn, db, `SELECT wal_status FROM pg_replication_slots WHERE slot_name = '`+e.db(id, db).SlotName+`'`); st == "lost" {
		t.Fatal("the slot was lost before the alert fired")
	}
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/resume", nil, nil, 200)
	e.waitDBState(id, db, 5*time.Minute, orch.DInSync)
	e.waitFor(5*time.Minute, "the alert to resolve", func() bool { return e.firing("slot_headroom_critical", db) == nil })
	e.abortAndClean(id, "Slot headroom")
}
