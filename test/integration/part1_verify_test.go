//go:build integration

package integration

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/check"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/pgpack"
)

// twinDBs creates the same database (schema and rows) on the source and the
// target, independently of any migration, for tests of verification alone.
func twinDBs(t *testing.T, db, sql string) {
	t.Helper()
	dropDB(t, db)
	for _, c := range []pg.Conn{srcConn, dstConn} {
		exec1(t, c, "defaultdb", "CREATE DATABASE "+db)
		exec1(t, c, db, sql)
	}
	t.Cleanup(func() { dropDB(t, db) })
}

const twinSchema = `
	CREATE TABLE orders(id bigint PRIMARY KEY, customer int NOT NULL, note text);
	INSERT INTO orders SELECT g, g % 100, md5(g::text) FROM generate_series(1, 20000) g;
	CREATE TABLE nokey(a int, b text);
	INSERT INTO nokey SELECT g, 'x' FROM generate_series(1, 50) g;
	CREATE SEQUENCE up_seq; SELECT setval('up_seq', 500);
	CREATE SEQUENCE down_seq INCREMENT BY -1 MAXVALUE 1000 START WITH 1000; SELECT setval('down_seq', 400);
	CREATE MATERIALIZED VIEW per_customer AS SELECT customer, count(*) FROM orders GROUP BY customer;
	CREATE INDEX ON orders(customer);
	SELECT lo_from_bytea(424242, 'large object');
	ANALYZE;`

// 18. Every query verification makes, failing in turn: verification must
// never return GO when any one of them could not run. The queries are
// discovered by recording a clean run, so a query added later is covered
// without changing this test.
func TestP1EveryVerificationQueryFails(t *testing.T) {
	db := uniq("it_p1_vfail")
	twinDBs(t, db, twinSchema)
	ctx := context.Background()
	configs := map[string]pgpack.VerifyInput{
		"checksums": {ExactCountMaxBytes: 1 << 40, ChecksumBudget: time.Minute},
		// Nothing fits the checksum window: every table goes to the full,
		// chunked comparison.
		"chunked": {ExactCountMaxBytes: 1 << 40, ChecksumBudget: time.Nanosecond, FullLarge: true, ChunkBytes: 64 << 10, Workers: 2},
		// Nothing fits either window and no full comparison: counts and sizes.
		"estimates": {ExactCountMaxBytes: 1, ChecksumBudget: time.Nanosecond},
	}
	names := make([]string, 0, len(configs))
	for k := range configs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		in := configs[name]
		in.Source, in.Target, in.TargetMaint = srcConn.WithDB(db), dstConn.WithDB(db), dstConn
		in.Database = db
		t.Run(name, func(t *testing.T) {
			// A clean run records the queries and must be GO (the twins match).
			var mu sync.Mutex
			seen := map[string]bool{}
			restore := pg.SetFaultHook(func(sql string) error {
				mu.Lock()
				seen[sql] = true
				mu.Unlock()
				return nil
			})
			res, err := pgpack.Verify(ctx, in)
			restore()
			if err != nil || !res.GO {
				t.Fatalf("clean run: GO=%v err=%v\n%s", res.GO, err, blockers(res))
			}
			queries := make([]string, 0, len(seen))
			for q := range seen {
				queries = append(queries, q)
			}
			sort.Strings(queries)
			t.Logf("%d distinct queries", len(queries))
			for _, q := range queries {
				restore := pg.SetFaultHook(func(sql string) error {
					if sql == q {
						return errInjected
					}
					return nil
				})
				res, err := pgpack.Verify(ctx, in)
				restore()
				short := strings.Join(strings.Fields(q), " ")
				if len(short) > 110 {
					short = short[:110] + "…"
				}
				switch {
				case err != nil:
					t.Logf("fails closed (error): %s", short)
				case res.GO:
					t.Errorf("GO although this query failed: %s", short)
				default:
					t.Logf("fails closed (%s): %s", firstBlocker(res), short)
				}
			}
		})
	}
}

func blockers(r pgpack.VerifyResult) string {
	var b []string
	for _, x := range r.Results {
		if x.Level == check.Blocker {
			b = append(b, x.CheckID+": "+x.Message)
		}
	}
	return strings.Join(b, "\n")
}

func firstBlocker(r pgpack.VerifyResult) string {
	for _, x := range r.Results {
		if x.Level == check.Blocker {
			return x.CheckID
		}
	}
	return fmt.Sprint("no blocker, GO=", r.GO)
}

// 17a. Sequences that cannot be read, and descending sequences, checked by
// verification and the sequence sync directly (twin databases).
func TestP1SequencesUnreadableAndDescending(t *testing.T) {
	db := uniq("it_p1_seq")
	twinDBs(t, db, twinSchema)
	ctx := context.Background()
	in := pgpack.VerifyInput{Source: srcConn.WithDB(db), Target: dstConn.WithDB(db), TargetMaint: dstConn, Database: db, ExactCountMaxBytes: 1 << 40, ChecksumBudget: time.Minute}
	seqCheck := func() check.Result {
		t.Helper()
		res, err := pgpack.Verify(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Results {
			if r.CheckID == "sequences" {
				if r.Level == check.Blocker && res.GO {
					t.Fatal("GO with a blocking sequence result")
				}
				return r
			}
		}
		t.Fatal("no sequence result")
		return check.Result{}
	}
	if r := seqCheck(); r.Level != check.OK {
		t.Fatalf("twins: %s %s", r.Level, r.Message)
	}
	// Descending, target behind (a higher value: it would hand out 450..401,
	// which the source already used).
	exec1(t, dstConn, db, "SELECT setval('down_seq', 450)")
	if r := seqCheck(); r.Level != check.Blocker || !strings.Contains(r.Message, "down_seq") {
		t.Fatalf("descending sequence behind on the target: %s %s", r.Level, r.Message)
	}
	if n, err := pgpack.SyncSequences(ctx, srcConn.WithDB(db), dstConn.WithDB(db)); err != nil || n != 1 {
		t.Fatalf("sync: %d, %v", n, err)
	}
	if v := q(t, dstConn, db, "SELECT last_value::text FROM down_seq"); v != "400" {
		t.Fatalf("after the sync the target's down_seq is %s, want 400", v)
	}
	// Descending, target past the source (a lower value): fine, not lowered.
	exec1(t, dstConn, db, "SELECT setval('down_seq', 100)")
	if r := seqCheck(); r.Level != check.OK {
		t.Fatalf("descending sequence past the source: %s %s", r.Level, r.Message)
	}
	if n, err := pgpack.SyncSequences(ctx, srcConn.WithDB(db), dstConn.WithDB(db)); err != nil || n != 0 {
		t.Fatalf("sync lowered a sequence already past the source: %d, %v", n, err)
	}
	// A sequence the verifying role may not read: never "unused", never OK.
	role := uniq("it_seqro")
	for _, c := range []pg.Conn{srcConn, dstConn} {
		exec1(t, c, db, fmt.Sprintf(`CREATE ROLE %[1]s LOGIN PASSWORD '%[2]s'; GRANT USAGE ON SCHEMA public TO %[1]s;
			GRANT SELECT ON ALL TABLES IN SCHEMA public TO %[1]s; GRANT SELECT ON down_seq TO %[1]s; GRANT SELECT ON LARGE OBJECT 424242 TO %[1]s`, role, password))
	}
	t.Cleanup(func() {
		dropDB(t, db)
		for _, c := range []pg.Conn{srcConn, dstConn} {
			exec1(t, c, "defaultdb", "DROP ROLE IF EXISTS "+role)
		}
	})
	ro := func(c pg.Conn) pg.Conn { c.User = role; return c.WithDB(db) }
	in.Source, in.Target = ro(srcConn), ro(dstConn)
	r := seqCheck()
	if r.Level != check.Blocker || !strings.Contains(r.Message, "up_seq") {
		t.Fatalf("unreadable sequence: %s %s", r.Level, r.Message)
	}
	t.Logf("unreadable: %s", r.Message)
	if _, err := pgpack.SyncSequences(ctx, ro(srcConn), ro(dstConn)); err == nil {
		t.Fatal("the sync reported success with a sequence it could not read")
	}
}
