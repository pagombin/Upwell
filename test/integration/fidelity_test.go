//go:build integration

package integration

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"
)

// Data fidelity: a realistic schema with awkward data types, and a mixed
// transactional workload that runs through the base copy, CDC, a database
// stop and resume, an engine crash, and a large final transaction. After the
// cutover, every table is compared row by row with a checksum computed here,
// independently of Upwell's own verification. Nothing may be missing, extra
// or different.

const fidelitySchema = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy', 'ecstatic');
CREATE TABLE customers(
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  email text UNIQUE NOT NULL, name text, mood mood, tags text[], prefs jsonb,
  created timestamptz DEFAULT now(), balance numeric(16,4), ratio float8, active boolean,
  ip inet, uid uuid DEFAULT gen_random_uuid(), avatar bytea, notes text,
  token text DEFAULT encode(gen_random_bytes(8), 'hex'));
CREATE TABLE orders(
  id bigserial PRIMARY KEY, customer_id bigint REFERENCES customers(id), total numeric(12,2),
  status text NOT NULL DEFAULT 'new', placed_at timestamptz DEFAULT now(), ship interval, payload jsonb);
CREATE TABLE order_lines(
  order_id bigint REFERENCES orders(id) ON DELETE CASCADE, line int, sku text, qty int, price numeric(10,2),
  PRIMARY KEY(order_id, line));
CREATE TABLE events_log(ts timestamptz DEFAULT clock_timestamp(), kind text, body text);
ALTER TABLE events_log REPLICA IDENTITY FULL;
CREATE TABLE big_docs(id int PRIMARY KEY, title text, version int DEFAULT 1, body text, meta jsonb);
CREATE TABLE measurements(id bigserial, sensor int, ts timestamptz NOT NULL, value float8, PRIMARY KEY(id, ts)) PARTITION BY RANGE (ts);
CREATE TABLE measurements_2025 PARTITION OF measurements FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
CREATE TABLE measurements_2026 PARTITION OF measurements FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE TABLE counters(name text PRIMARY KEY, n bigint NOT NULL);
CREATE TABLE gen_cols(id int PRIMARY KEY, a int, b int GENERATED ALWAYS AS (a * 2) STORED);
CREATE TABLE unicode_text(id int PRIMARY KEY, s text);
CREATE TABLE ghosts(id serial PRIMARY KEY, note text);
CREATE SEQUENCE invoice_seq;

INSERT INTO customers(email, name, mood, tags, prefs, balance, ratio, active, ip, avatar, notes)
SELECT 'c' || g || '@example.com',
       CASE WHEN g % 50 = 0 THEN NULL ELSE 'Customer ' || g || CASE WHEN g % 7 = 0 THEN ' O''Brien "Q" \ back' ELSE '' END END,
       (ARRAY['sad','ok','happy','ecstatic'])[1 + g % 4]::mood,
       CASE WHEN g % 9 = 0 THEN ARRAY['a', NULL, 'c,d'] ELSE ARRAY['vip', 'tier' || (g % 3)] END,
       jsonb_build_object('lang', 'en', 'n', g, 'nested', jsonb_build_object('x', g % 10, 'list', jsonb_build_array(1, 'two', NULL, true))),
       (g * 12345.6789) % 999999, CASE WHEN g % 97 = 0 THEN 'NaN'::float8 WHEN g % 89 = 0 THEN 'Infinity'::float8 ELSE g / 7.0 END,
       g % 2 = 0, ('10.' || (g % 255) || '.0.' || (g % 200))::inet, gen_random_bytes(16 + g % 64),
       CASE WHEN g % 11 = 0 THEN E'line1\nline2\ttab' WHEN g % 13 = 0 THEN '' ELSE NULL END
FROM generate_series(1, 20000) g;
INSERT INTO orders(customer_id, total, status, ship, payload)
SELECT 1 + (g % 20000), (g % 100000) / 100.0, (ARRAY['new','paid','shipped'])[1 + g % 3], (g % 72) * interval '1 hour',
       jsonb_build_object('items', g % 5, 'note', repeat('x', g % 40))
FROM generate_series(1, 60000) g;
INSERT INTO order_lines SELECT id, l, 'SKU-' || (id * l) % 5000, l, (id % 900) / 10.0 FROM orders, generate_series(1, 3) l;
INSERT INTO events_log(kind, body) SELECT 'boot', 'event ' || g FROM generate_series(1, 3000) g;
INSERT INTO events_log(kind, body) SELECT 'dup', 'same row' FROM generate_series(1, 20);
INSERT INTO big_docs SELECT g, 'Doc ' || g, 1, repeat(md5(g::text), 3000 + g % 500), jsonb_build_object('pages', g) FROM generate_series(1, 300) g;
INSERT INTO measurements(sensor, ts, value) SELECT g % 50, timestamptz '2025-06-01' + g * interval '7 minutes', sin(g) FROM generate_series(1, 40000) g;
INSERT INTO counters SELECT 'k' || g, 0 FROM generate_series(1, 20) g;
INSERT INTO gen_cols SELECT g, g FROM generate_series(1, 1000) g;
INSERT INTO unicode_text VALUES (1, 'héllo wörld'), (2, '日本語のテキスト'), (3, 'emoji 🎉🚀👩‍💻'), (4, 'RTL مرحبا'), (5, 'zero' || chr(8203) || 'width'), (6, E'quote '' and backslash \\'), (7, repeat('ü', 5000));
SELECT setval('invoice_seq', 1000);
ANALYZE;
`

// fidelityWorkload runs a mix of real transactions until stop is closed.
type fidelityWorkload struct {
	stop      chan struct{}
	wg        sync.WaitGroup
	committed atomic.Int64
	rolled    atomic.Int64
	errs      atomic.Int64
	lastErr   atomic.Value
}

func startFidelityWorkload(t *testing.T, db string, workers int) *fidelityWorkload {
	w := &fidelityWorkload{stop: make(chan struct{})}
	for i := 0; i < workers; i++ {
		w.wg.Add(1)
		go func(seed int64) {
			defer w.wg.Done()
			ctx := context.Background()
			rng := rand.New(rand.NewSource(seed))
			var conn *pgx.Conn
			for {
				select {
				case <-w.stop:
					if conn != nil {
						conn.Close(ctx)
					}
					return
				default:
				}
				if conn == nil {
					c, err := pg.Connect(ctx, srcConn.WithDB(db), time.Minute)
					if err != nil {
						time.Sleep(200 * time.Millisecond)
						continue
					}
					conn = c
				}
				committed, err := fidelityTxn(ctx, conn, rng)
				switch {
				case err != nil:
					w.errs.Add(1)
					w.lastErr.Store(err.Error())
					conn.Close(ctx)
					conn = nil
				case committed:
					w.committed.Add(1)
				default:
					w.rolled.Add(1)
				}
				time.Sleep(time.Duration(150+rng.Intn(150)) * time.Millisecond) // within pgcopydb's apply rate (F11)
			}
		}(int64(i + 1))
	}
	return w
}

func (w *fidelityWorkload) Stop() { close(w.stop); w.wg.Wait() }

// fidelityTxn runs one random transaction; false means it was rolled back on purpose.
func fidelityTxn(ctx context.Context, c *pgx.Conn, rng *rand.Rand) (bool, error) {
	tx, err := c.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	ex := func(sql string, args ...any) error { _, err := tx.Exec(ctx, sql, args...); return err }
	n := rng.Intn(100)
	switch {
	case n < 20: // new customer with an order and lines, all in one transaction
		var cid, oid int64
		if err := tx.QueryRow(ctx, `INSERT INTO customers(email, name, mood, tags, prefs, balance, ratio, active, ip, avatar, notes)
			VALUES ($1, $2, 'happy', ARRAY['new', NULL], jsonb_build_object('src', 'live', 'r', $3::int), $4, $5, true, '192.168.1.1', gen_random_bytes(32), $6) RETURNING id`,
			fmt.Sprintf("live-%d-%d@example.com", time.Now().UnixNano(), rng.Int63()), "Live “Customer” ✓", rng.Intn(1000), rng.Float64()*10000, rng.NormFloat64(), "κείμενο\nmulti-line").Scan(&cid); err != nil {
			return false, err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO orders(customer_id, total, payload) VALUES ($1, $2, '{"live": true}') RETURNING id`, cid, rng.Intn(100000)/100).Scan(&oid); err != nil {
			return false, err
		}
		for l := 1; l <= 1+rng.Intn(5); l++ {
			if err := ex(`INSERT INTO order_lines VALUES ($1, $2, $3, $4, $5)`, oid, l, fmt.Sprintf("SKU-%d", rng.Intn(5000)), 1+rng.Intn(9), float64(rng.Intn(10000))/100); err != nil {
				return false, err
			}
		}
	case n < 35: // updates of several kinds
		if err := ex(`UPDATE orders SET status = (ARRAY['paid','shipped','refunded'])[1 + floor(random() * 3)::int], payload = payload || jsonb_build_object('u', now()::text) WHERE id = $1`, 1+rng.Int63n(60000)); err != nil {
			return false, err
		}
		if err := ex(`UPDATE customers SET balance = balance + $1, prefs = jsonb_set(coalesce(prefs, '{}'), '{nested,x}', to_jsonb($2::int)), mood = 'ecstatic' WHERE id = $3`, rng.Float64()*10, rng.Intn(100), 1+rng.Int63n(20000)); err != nil {
			return false, err
		}
	case n < 45: // TOASTed rows: change a small column only (unchanged TOAST datum), sometimes the body too
		id := 1 + rng.Intn(300)
		if rng.Intn(3) == 0 {
			if err := ex(`UPDATE big_docs SET body = repeat(md5(random()::text), 400), version = version + 1 WHERE id = $1`, id); err != nil {
				return false, err
			}
		} else if err := ex(`UPDATE big_docs SET title = title || '.', version = version + 1 WHERE id = $1`, id); err != nil {
			return false, err
		}
	case n < 55: // deletes, cascading and on a table without a primary key
		if err := ex(`DELETE FROM orders WHERE id = $1`, 1+rng.Int63n(60000)); err != nil {
			return false, err
		}
		if err := ex(`DELETE FROM events_log WHERE ctid = (SELECT ctid FROM events_log WHERE kind = 'boot' ORDER BY random() LIMIT 1)`); err != nil {
			return false, err
		}
	case n < 62: // update a primary key, and an update on the no-PK table
		id := 1000 + rng.Intn(1_000_000)
		if err := ex(`INSERT INTO unicode_text VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, id, "πρώτο 🙂"); err != nil {
			return false, err
		}
		if err := ex(`UPDATE unicode_text SET id = id + 2000000, s = s || ' moved' WHERE id = $1`, id); err != nil {
			return false, err
		}
		if err := ex(`UPDATE events_log SET body = body || '!' WHERE ctid = (SELECT ctid FROM events_log WHERE kind = 'boot' ORDER BY random() LIMIT 1)`); err != nil {
			return false, err
		}
	case n < 72: // hot rows
		if err := ex(`UPDATE counters SET n = n + 1 WHERE name = $1`, fmt.Sprintf("k%d", 1+rng.Intn(20))); err != nil {
			return false, err
		}
		if err := ex(`UPDATE gen_cols SET a = a + 1 WHERE id = $1`, 1+rng.Intn(1000)); err != nil {
			return false, err
		}
	case n < 80: // savepoints: the inner part is rolled back, the rest commits
		if err := ex(`INSERT INTO events_log(kind, body) VALUES ('kept', 'before savepoint')`); err != nil {
			return false, err
		}
		if err := ex(`SAVEPOINT sp1`); err != nil {
			return false, err
		}
		if err := ex(`INSERT INTO ghosts(note) VALUES ('rolled back to savepoint: must never appear')`); err != nil {
			return false, err
		}
		if err := ex(`ROLLBACK TO SAVEPOINT sp1`); err != nil {
			return false, err
		}
		if err := ex(`INSERT INTO events_log(kind, body) VALUES ('kept', 'after savepoint')`); err != nil {
			return false, err
		}
	case n < 86: // a whole transaction rolled back
		if err := ex(`INSERT INTO ghosts(note) VALUES ('rolled back: must never appear')`); err != nil {
			return false, err
		}
		if err := ex(`UPDATE customers SET name = 'GHOST' WHERE id = $1`, 1+rng.Int63n(20000)); err != nil {
			return false, err
		}
		return false, tx.Rollback(ctx)
	case n < 93: // partitioned table, both partitions
		if err := ex(`INSERT INTO measurements(sensor, ts, value) SELECT $1, timestamptz '2025-12-31 23:00' + g * interval '1 minute', random() FROM generate_series(1, 120) g`, rng.Intn(50)); err != nil {
			return false, err
		}
	default: // sequences and duplicate rows in the no-PK table
		if err := ex(`SELECT nextval('invoice_seq')`); err != nil {
			return false, err
		}
		if err := ex(`INSERT INTO events_log(kind, body) VALUES ('dup', 'same row')`); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// tableFingerprints returns count and an order-independent content hash for
// every table and partition in the public schema, computed with plain SQL.
func tableFingerprints(t *testing.T, c pg.Conn, db string) map[string]string {
	t.Helper()
	ctx := context.Background()
	conn, err := pg.Connect(ctx, c.WithDB(db), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	out := map[string]string{}
	for _, n := range names {
		var cnt int64
		var h string
		if err := conn.QueryRow(ctx, fmt.Sprintf(`SELECT count(*), coalesce(md5(string_agg(h, '' ORDER BY h)), '') FROM (SELECT md5(t::text) h FROM public.%s t) s`, pg.QuoteIdent(n))).Scan(&cnt, &h); err != nil {
			t.Fatalf("%s.%s: %v", db, n, err)
		}
		out[n] = fmt.Sprintf("%d rows, %s", cnt, h)
	}
	return out
}

func sequenceValues(t *testing.T, c pg.Conn, db string) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := pg.Connect(ctx, c.WithDB(db), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `SELECT sequencename, coalesce(last_value, 0) FROM pg_sequences WHERE schemaname = 'public'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var n string
		var v int64
		rows.Scan(&n, &v)
		out[n] = v
	}
	return out
}

func compareDatabase(t *testing.T, db string) (tables int, mismatches []string) {
	src, dst := tableFingerprints(t, srcConn, db), tableFingerprints(t, dstConn, db)
	names := make([]string, 0, len(src))
	for n := range src {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if dst[n] != src[n] {
			mismatches = append(mismatches, fmt.Sprintf("%s.%s: source %s, target %s", db, n, src[n], dst[n]))
		} else {
			t.Logf("  %-22s %-18s identical: %s", db, n, src[n][:strings.Index(src[n], ",")])
		}
	}
	for n := range dst {
		if _, ok := src[n]; !ok {
			mismatches = append(mismatches, fmt.Sprintf("%s.%s exists only on the target", db, n))
		}
	}
	ss, ds := sequenceValues(t, srcConn, db), sequenceValues(t, dstConn, db)
	for n, v := range ss {
		if ds[n] < v {
			mismatches = append(mismatches, fmt.Sprintf("%s sequence %s: source %d, target %d", db, n, v, ds[n]))
		}
	}
	if g := q(t, dstConn, db, "SELECT count(*)::text FROM ghosts"); g != "0" {
		mismatches = append(mismatches, fmt.Sprintf("%s: %s rolled-back row(s) reached the target", db, g))
	}
	if g := q(t, dstConn, db, "SELECT count(*)::text FROM customers WHERE name = 'GHOST'"); g != "0" {
		mismatches = append(mismatches, fmt.Sprintf("%s: a rolled-back update reached the target", db))
	}
	return len(src), mismatches
}

func runFidelity(t *testing.T, plugin string) {
	e := newEnv(t, "", orch.TestHooks{})
	a, b := uniq("it_fid_a"), uniq("it_fid_b")
	for _, db := range []string{a, b} {
		dropDB(t, db)
		exec1(t, srcConn, "defaultdb", "CREATE DATABASE "+db)
		exec1(t, srcConn, db, fidelitySchema)
		db := db
		t.Cleanup(func() { dropDB(t, db) })
	}
	// pgcopydb applies about 200 rows a second here (TestApplyThroughput), so
	// give the drain room.
	if _, _, err := e.Orch.SettingsService().Set(context.Background(), "cutover_drain_timeout_seconds", 1200, "test"); err != nil {
		t.Fatal(err)
	}
	name := "Fidelity " + plugin
	id := e.defineMigration(name, []string{a, b}, map[string]any{"decoding_plugin": plugin})
	pv := e.preflight(id)
	for _, r := range pv.Results {
		if r.Level == "blocker" {
			t.Fatalf("unexpected blocker %s %s: %s", r.CheckID, r.Database, r.Message)
		}
	}
	// Writes start before the migration, so they run through the base copy.
	walStart, tStart := q(t, srcConn, "defaultdb", "SELECT pg_current_wal_lsn()::text"), time.Now()
	wa, wb := startFidelityWorkload(t, a, 1), startFidelityWorkload(t, b, 1)
	time.Sleep(2 * time.Second)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, a, 5*time.Minute, orch.DInSync)
	e.waitDBState(id, b, 5*time.Minute, orch.DInSync)
	time.Sleep(30 * time.Second)

	// Disruption 1: stop and resume database A under load.
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+a+"/stop", nil, nil, 200)
	time.Sleep(5 * time.Second)
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+a+"/resume", nil, nil, 200)
	// Disruption 2: the engine of database B crashes under load.
	pid := e.mainPID(id, b)
	syscall.Kill(pid, syscall.SIGKILL)
	e.waitFor(2*time.Minute, "an automatic resume of "+b, func() bool { return e.db(id, b).RetryCount >= 1 })
	e.waitDBState(id, a, 10*time.Minute, orch.DInSync)
	e.waitDBState(id, b, 10*time.Minute, orch.DInSync)
	time.Sleep(45 * time.Second)

	// One large transaction just before writers stop (F9 territory), in both databases.
	wa.Stop()
	wb.Stop()
	for _, db := range []string{a, b} {
		exec1(t, srcConn, db, `INSERT INTO measurements(sensor, ts, value) SELECT g % 50, timestamptz '2026-03-01' + g * interval '1 second', g FROM generate_series(1, 10000) g;
			UPDATE big_docs SET version = version + 100;`)
	}
	walEnd := q(t, srcConn, "defaultdb", "SELECT pg_current_wal_lsn()::text")
	t.Logf("source WAL written during the workload: %.1f MB in %s (%.2f MB/s)", float64(pg.LSNDiff(walEnd, walStart))/1e6, time.Since(tStart).Round(time.Second), float64(pg.LSNDiff(walEnd, walStart))/1e6/time.Since(tStart).Seconds())
	t.Logf("workload: %s committed %d, rolled back %d, errors %d; %s committed %d, rolled back %d, errors %d",
		a, wa.committed.Load(), wa.rolled.Load(), wa.errs.Load(), b, wb.committed.Load(), wb.rolled.Load(), wb.errs.Load())
	for _, w := range []*fidelityWorkload{wa, wb} {
		if v := w.lastErr.Load(); v != nil {
			t.Logf("last workload error (retried): %v", v)
		}
	}

	op := e.startCutover(id)
	v := e.migration(id)
	var ver struct {
		Results []orch.VerificationRow `json:"results"`
	}
	e.do("GET", "/api/v1/migrations/"+id+"/verification", nil, &ver, 200)
	for _, r := range ver.Results {
		t.Logf("upwell verify %-14s %-16s %-8s %s", r.Database, r.CheckID, r.Result, r.Detail)
	}
	if op.State != "done" || v.Migration.Flags.Verdict != "GO" {
		for _, st := range op.Steps {
			t.Logf("step %-10s %-8s %s", st.Key, st.State, st.Detail)
		}
		t.Errorf("verdict %q (%s)", v.Migration.Flags.Verdict, op.Error)
	}

	// Independent comparison of every row.
	total := 0
	var all []string
	for _, db := range []string{a, b} {
		n, mm := compareDatabase(t, db)
		total += n
		all = append(all, mm...)
	}
	if len(all) > 0 {
		t.Fatalf("source and target differ:\n%s", strings.Join(all, "\n"))
	}
	t.Logf("independent comparison: %d tables in 2 databases identical row for row; sequences at or past the source; no rolled-back data on the target", total)
	e.cleanup(id, name)
}

func TestFidelityPgoutput(t *testing.T)     { runFidelity(t, "pgoutput") }
func TestFidelityTestDecoding(t *testing.T) { runFidelity(t, "test_decoding") }
