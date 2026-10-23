//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/pg"
)

// D112, workload shapes far from TPC-C. Each scenario changes the source
// while the database streams and must end GO with every table identical
// (all schemas, partitions on their own), under both plugins.

// A table of 250 columns: the engine merges consecutive INSERTs into one
// statement with up to 65,535 parameters (262 rows here), and UPDATEs
// carry every column.
func TestWorkloadWideTable(t *testing.T) {
	var cols, vals []string
	for i := 1; i <= 249; i++ {
		cols = append(cols, fmt.Sprintf("c%d text", i))
		vals = append(vals, fmt.Sprintf("md5((g * %d)::text)", i))
	}
	forPlugins(t, scenario{name: "wide", wantGO: true,
		schema: `CREATE TABLE wide(id int PRIMARY KEY, ` + strings.Join(cols, ", ") + `);
			INSERT INTO wide SELECT g, ` + strings.Join(vals, ", ") + ` FROM generate_series(1, 500) g;`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `INSERT INTO wide SELECT g, `+strings.Join(vals, ", ")+` FROM generate_series(501, 3000) g;
				UPDATE wide SET c1 = 'u', c125 = NULL, c249 = repeat('z', 500) WHERE id % 7 = 0;
				DELETE FROM wide WHERE id % 11 = 0;`)
			return nil
		}})
}

// Single values of tens of megabytes: one statement far larger than the
// engine's pipeline batch, through the change files and the target.
func TestWorkloadHugeValues(t *testing.T) {
	forPlugins(t, scenario{name: "huge", wantGO: true,
		schema: `CREATE TABLE blobs(id int PRIMARY KEY, t text, b bytea, j jsonb);
			INSERT INTO blobs VALUES (1, 'small', '\x00', '{}');`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `INSERT INTO blobs SELECT 2, string_agg(md5(g::text), '') , NULL, NULL FROM generate_series(1, 1200000) g`)              // about 38 MB of text
			exec1(t, srcConn, db, `INSERT INTO blobs SELECT 3, NULL, decode(string_agg(md5(g::text), ''), 'hex'), NULL FROM generate_series(1, 600000) g`) // about 10 MB of bytea
			exec1(t, srcConn, db, `INSERT INTO blobs SELECT 4, NULL, NULL, jsonb_agg(jsonb_build_object('k', g, 'v', md5(g::text))) FROM generate_series(1, 100000) g`)
			exec1(t, srcConn, db, `UPDATE blobs SET t = t || 'tail' WHERE id = 2; UPDATE blobs SET b = NULL WHERE id = 3; DELETE FROM blobs WHERE id = 1`)
			return nil
		}})
}

// Keys that change: primary key updates (simple and composite), a table
// whose only columns are its key, a deferred unique constraint swapped
// inside a transaction, the same key deleted and inserted again, one row
// updated a thousand times in one transaction, and savepoints rolled back.
func TestWorkloadChangingKeys(t *testing.T) {
	forPlugins(t, scenario{name: "keys", wantGO: true,
		schema: `CREATE TABLE k(id int PRIMARY KEY, v text); INSERT INTO k SELECT g, 'v' || g FROM generate_series(1, 1000) g;
			CREATE TABLE ck(a int, b text, v int, PRIMARY KEY (a, b)); INSERT INTO ck SELECT g, 'x', g FROM generate_series(1, 500) g;
			CREATE TABLE onlykey(a int, b int, PRIMARY KEY (a, b)); INSERT INTO onlykey SELECT g, g FROM generate_series(1, 300) g;
			CREATE TABLE sw(id int PRIMARY KEY, u int NOT NULL UNIQUE DEFERRABLE INITIALLY DEFERRED); INSERT INTO sw VALUES (1, 10), (2, 20);
			CREATE TABLE hot(id int PRIMARY KEY, n bigint); INSERT INTO hot VALUES (1, 0);`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `UPDATE k SET id = id + 1000000 WHERE id <= 300;
				UPDATE ck SET b = 'y', a = a + 5000 WHERE a % 3 = 0;
				UPDATE onlykey SET b = b + 1000 WHERE a <= 100;
				DELETE FROM onlykey WHERE a > 250;`)
			exec1(t, srcConn, db, `BEGIN; UPDATE sw SET u = 20 WHERE id = 1; UPDATE sw SET u = 10 WHERE id = 2; COMMIT;`)
			exec1(t, srcConn, db, `BEGIN; DELETE FROM k WHERE id = 500; INSERT INTO k VALUES (500, 'again'); DELETE FROM k WHERE id = 500; INSERT INTO k VALUES (500, 'third'); COMMIT;`)
			exec1(t, srcConn, db, `DO $$ BEGIN FOR i IN 1..1000 LOOP UPDATE hot SET n = n + i WHERE id = 1; END LOOP; END $$`)
			exec1(t, srcConn, db, `BEGIN;
				INSERT INTO k VALUES (2000001, 'kept');
				SAVEPOINT a; INSERT INTO k VALUES (2000002, 'rolled back'); UPDATE k SET v = 'rolled back' WHERE id = 600; ROLLBACK TO SAVEPOINT a;
				SAVEPOINT b; UPDATE k SET v = 'kept too' WHERE id = 601; RELEASE SAVEPOINT b;
				COMMIT;`)
			return nil
		}})
}

// Names that need quoting everywhere: spaces, capitals, a double quote,
// reserved words, non-Latin letters, and a second schema.
func TestWorkloadAwkwardNames(t *testing.T) {
	forPlugins(t, scenario{name: "names", wantGO: true,
		schema: `CREATE SCHEMA "Sales Data";
			CREATE TABLE "Sales Data"."Order" ("ID" int PRIMARY KEY, "select" text, "Weird ""Quote""" int, "ÜnïCödé" text, "from" date);
			INSERT INTO "Sales Data"."Order" SELECT g, 'sel' || g, g, 'ü' || g, '2026-01-01'::date + g FROM generate_series(1, 300) g;
			CREATE TABLE "MixedCase"(id int PRIMARY KEY, "Value" text); INSERT INTO "MixedCase" SELECT g, 'v' FROM generate_series(1, 100) g;
			CREATE TABLE "表"(主键 int PRIMARY KEY, 值 text); INSERT INTO "表" VALUES (1, '一');`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `INSERT INTO "Sales Data"."Order" SELECT g, 'new', g, 'ñ', NULL FROM generate_series(301, 600) g;
				UPDATE "Sales Data"."Order" SET "select" = 'it''s "quoted"', "Weird ""Quote""" = -1 WHERE "ID" % 5 = 0;
				DELETE FROM "Sales Data"."Order" WHERE "ID" % 13 = 0;
				UPDATE "MixedCase" SET "Value" = E'line1\nline2\ttab\\backslash' WHERE id <= 10;
				INSERT INTO "表" VALUES (2, '二'), (3, '三'); UPDATE "表" SET 值 = '更新' WHERE 主键 = 1;`)
			return nil
		}})
}

// Replica identity other than a primary key: FULL on rows with NULLs in
// several columns (an UPDATE or DELETE must still find them), and USING
// INDEX on a unique index, including updates of that index's key.
func TestWorkloadReplicaIdentities(t *testing.T) {
	forPlugins(t, scenario{name: "identity", wantGO: true,
		schema: `CREATE TABLE full_nulls(a int, b text, c numeric, d timestamptz, UNIQUE (a));
			ALTER TABLE full_nulls REPLICA IDENTITY FULL;
			INSERT INTO full_nulls SELECT g, CASE WHEN g % 2 = 0 THEN NULL ELSE 'b' || g END, CASE WHEN g % 3 = 0 THEN NULL ELSE g * 1.5 END, NULL FROM generate_series(1, 300) g;
			CREATE TABLE by_index(id int NOT NULL, code text NOT NULL, v int);
			CREATE UNIQUE INDEX by_index_code ON by_index(code);
			ALTER TABLE by_index REPLICA IDENTITY USING INDEX by_index_code;
			INSERT INTO by_index SELECT g, 'code-' || g, g FROM generate_series(1, 300) g;`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `UPDATE full_nulls SET d = '2026-10-20 12:00:00+00' WHERE a % 4 = 0;
				UPDATE full_nulls SET b = 'set' WHERE b IS NULL AND a <= 50;
				DELETE FROM full_nulls WHERE c IS NULL AND a > 250;
				UPDATE by_index SET v = -v WHERE id % 2 = 0;
				UPDATE by_index SET code = code || '-moved' WHERE id <= 40;
				DELETE FROM by_index WHERE id > 280;`)
			return nil
		}})
}

// Two hundred small tables written in interleaved transactions, and a
// parent with ON DELETE / ON UPDATE CASCADE children.
func TestWorkloadManyTablesAndCascades(t *testing.T) {
	var ddl, fill, writes strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&ddl, "CREATE TABLE t%03d(id int PRIMARY KEY, v int);", i)
		fmt.Fprintf(&fill, "INSERT INTO t%03d SELECT g, g FROM generate_series(1, 20) g;", i)
	}
	for r := 0; r < 20; r++ {
		writes.WriteString("BEGIN;")
		for i := 1 + r; i <= 200; i += 20 {
			fmt.Fprintf(&writes, "UPDATE t%03d SET v = v + %d WHERE id = %d; INSERT INTO t%03d VALUES (%d, 0); DELETE FROM t%03d WHERE id = %d;", i, r, 1+r%20, i, 100+r, i, 20-r%10)
		}
		writes.WriteString("COMMIT;")
	}
	forPlugins(t, scenario{name: "many", wantGO: true,
		schema: ddl.String() + fill.String() + `
			CREATE TABLE parent(id int PRIMARY KEY, name text);
			CREATE TABLE child(id int PRIMARY KEY, parent_id int REFERENCES parent(id) ON DELETE CASCADE ON UPDATE CASCADE, v int);
			INSERT INTO parent SELECT g, 'p' || g FROM generate_series(1, 100) g;
			INSERT INTO child SELECT g, 1 + g % 100, g FROM generate_series(1, 1000) g;`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, writes.String())
			exec1(t, srcConn, db, `DELETE FROM parent WHERE id <= 10; UPDATE parent SET id = id + 1000 WHERE id BETWEEN 11 AND 20;`)
			return nil
		}})
}

// Two-phase commit: a prepared transaction committed and another rolled
// back while streaming (the cutover refuses to start while one is open).
func TestWorkloadPreparedTransactions(t *testing.T) {
	forPlugins(t, scenario{name: "twophase", wantGO: true,
		schema: `CREATE TABLE tp(id int PRIMARY KEY, v text); INSERT INTO tp SELECT g, 'v' FROM generate_series(1, 100) g;`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `BEGIN; INSERT INTO tp VALUES (1001, 'prepared, committed'); UPDATE tp SET v = 'by 2pc' WHERE id = 1; PREPARE TRANSACTION 'upwell_it_commit_`+db+`';`)
			exec1(t, srcConn, db, `BEGIN; INSERT INTO tp VALUES (1002, 'prepared, rolled back'); DELETE FROM tp WHERE id = 2; PREPARE TRANSACTION 'upwell_it_rollback_`+db+`';`)
			exec1(t, srcConn, db, `INSERT INTO tp VALUES (1003, 'plain, in between')`)
			exec1(t, srcConn, db, `COMMIT PREPARED 'upwell_it_commit_`+db+`'`)
			exec1(t, srcConn, db, `ROLLBACK PREPARED 'upwell_it_rollback_`+db+`'`)
			return nil
		}})
}

// Types beyond the common ones, changed while streaming.
func TestWorkloadMoreTypes(t *testing.T) {
	forPlugins(t, scenario{name: "types2", wantGO: true,
		schema: `CREATE TYPE pair AS (x int, y text);
			CREATE DOMAIN positive AS numeric CHECK (VALUE > 0);
			CREATE TABLE t2(id int PRIMARY KEY, iv interval, m money, bv bit varying(16), u uuid, ip inet, net cidr, mac macaddr,
				r int4range, tr tstzrange, mr int4multirange, p pair, pa pair[], dm positive, ts tsvector, pt point, x xml, js json,
				big numeric(40, 10), sm smallint, ch char(5), vc varchar(10));
			INSERT INTO t2 VALUES (1, '1 day 2 hours', '12.34', B'1010', '6f1c8e3e-2b8a-4c0a-9d1e-0123456789ab', '10.0.0.1/24', '10.0.0.0/8', '08:00:2b:01:02:03',
				'[1,10)', '[2026-01-01,2026-02-01)', '{[1,3),[5,9)}', ROW(1, 'a'), ARRAY[ROW(2, 'b')::pair, ROW(3, NULL)::pair], 1.5, 'a fat cat', '(1,2)', '<a>1</a>', '{"k": [1, 2]}',
				123456789012345678901234567890.0123456789, -32768, 'ab', 'short');`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `INSERT INTO t2 VALUES (2, '-3 months 4 seconds', '-0.01', B'', '00000000-0000-0000-0000-000000000000', '::1', '2001:db8::/32', 'ff:ff:ff:ff:ff:ff',
				'empty', '(,)', '{}', ROW(NULL, NULL), '{}', 0.0000001, '', '(-1.5,2e10)', '<x/>', 'null',
				-0.0000000001, 32767, '', 'x'),
				(3, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);
				UPDATE t2 SET iv = iv * 2, m = m + '1', bv = bv || B'1', r = '[5,6]', mr = mr + '{[20,30)}', p = ROW(9, 'z'), ts = 'rat sat', js = '[]' WHERE id = 1;
				DELETE FROM t2 WHERE id = 3;`)
			return nil
		}})
}

// Bulk loads with COPY while streaming. PostgreSQL writes the rows of one
// COPY into a single WAL record per page of rows, so every row decoded
// from it carries the same LSN: rows that share an LSN must all arrive.
// A table with a key, one without (replica identity FULL), and a
// partitioned one; then the loaded rows change.
func TestWorkloadBulkCopy(t *testing.T) {
	forPlugins(t, scenario{name: "copy", wantGO: true,
		schema: `CREATE TABLE loaded(id int PRIMARY KEY, v text, n bigint);
			CREATE TABLE nokey(a int, b text); ALTER TABLE nokey REPLICA IDENTITY FULL;
			CREATE TABLE parts(id int, d date, v text, PRIMARY KEY (id, d)) PARTITION BY RANGE (d);
			CREATE TABLE parts_a PARTITION OF parts FOR VALUES FROM ('2026-01-01') TO ('2026-07-01');
			CREATE TABLE parts_b PARTITION OF parts FOR VALUES FROM ('2026-07-01') TO ('2027-01-01');
			INSERT INTO loaded VALUES (0, 'seed', 0);`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			ctx := context.Background()
			c, err := pg.Connect(ctx, srcConn.WithDB(db), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close(ctx)
			copyRows := func(table string, cols []string, n int, row func(i int) []any) {
				rows := make([][]any, n)
				for i := range rows {
					rows[i] = row(i)
				}
				if got, err := c.CopyFrom(ctx, pgx.Identifier{table}, cols, pgx.CopyFromRows(rows)); err != nil || got != int64(n) {
					t.Fatalf("COPY %s: %d rows, %v", table, got, err)
				}
			}
			copyRows("loaded", []string{"id", "v", "n"}, 20000, func(i int) []any { return []any{i + 1, fmt.Sprintf("row %d", i+1), int64(i) * 7} })
			copyRows("nokey", []string{"a", "b"}, 5000, func(i int) []any { return []any{i % 50, fmt.Sprintf("dup %d", i%50)} })
			day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			copyRows("parts", []string{"id", "d", "v"}, 8000, func(i int) []any { return []any{i, day.AddDate(0, 0, i%365), "p"} })
			// Several COPYs in one transaction, and one that is rolled back.
			tx, err := c.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for k := 0; k < 3; k++ {
				rows := make([][]any, 3000)
				for i := range rows {
					rows[i] = []any{100000 + k*3000 + i, "tx", int64(k)}
				}
				if _, err := tx.CopyFrom(ctx, pgx.Identifier{"loaded"}, []string{"id", "v", "n"}, pgx.CopyFromRows(rows)); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			tx, err = c.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{"loaded"}, []string{"id", "v", "n"}, pgx.CopyFromRows([][]any{{900001, "gone", int64(1)}, {900002, "gone", int64(2)}})); err != nil {
				t.Fatal(err)
			}
			_ = tx.Rollback(ctx)
			exec1(t, srcConn, db, `UPDATE loaded SET n = -n WHERE id % 3 = 0;
				DELETE FROM loaded WHERE id % 10 = 0;
				UPDATE nokey SET b = b || '!' WHERE a = 7;
				DELETE FROM nokey WHERE a = 8;
				UPDATE parts SET d = d + 200 WHERE id % 13 = 0 AND d < '2026-06-01';`)
			return nil
		}})
}

// Shapes older databases accumulate, and the ETL reload: a table with
// classic inheritance (the parent's own rows and its children's must each
// arrive once), dropped columns and a column added later with a default (the
// source's physical layout differs from the target's fresh table), and
// reloads that truncate and COPY in one transaction, truncate a partitioned
// table, restart an identity, cascade to a referencing table, truncate two
// tables at once (one named with ", "), and truncate only an inheritance
// parent, whose children must keep their rows. D115: a TRUNCATE of several
// relations kept only the first.
func TestWorkloadLegacyShapesAndReloads(t *testing.T) {
	forPlugins(t, scenario{name: "legacy", wantGO: true,
		schema: `CREATE TABLE animal(id int PRIMARY KEY, name text);
			CREATE TABLE dog(breed text) INHERITS (animal); ALTER TABLE dog ADD PRIMARY KEY (id);
			CREATE TABLE cat(indoor bool) INHERITS (animal); ALTER TABLE cat ADD PRIMARY KEY (id);
			INSERT INTO animal SELECT g, 'animal ' || g FROM generate_series(1, 100) g;
			INSERT INTO dog SELECT g, 'dog ' || g, 'breed ' || (g % 7) FROM generate_series(1001, 1200) g;
			INSERT INTO cat SELECT g, 'cat ' || g, g % 2 = 0 FROM generate_series(2001, 2150) g;
			CREATE TABLE layout(id int PRIMARY KEY, a text, gone1 text, b int, gone2 bytea);
			INSERT INTO layout SELECT g, 'a' || g, 'x', g, '\x00' FROM generate_series(1, 500) g;
			ALTER TABLE layout DROP COLUMN gone1;
			ALTER TABLE layout ADD COLUMN c text NOT NULL DEFAULT 'added later';
			ALTER TABLE layout DROP COLUMN gone2;
			CREATE TABLE staging(id int PRIMARY KEY, v text);
			INSERT INTO staging SELECT g, 'old' FROM generate_series(1, 3000) g;
			CREATE TABLE events(id int, d date, v text, PRIMARY KEY (id, d)) PARTITION BY RANGE (d);
			CREATE TABLE events_a PARTITION OF events FOR VALUES FROM ('2026-01-01') TO ('2026-07-01');
			CREATE TABLE events_b PARTITION OF events FOR VALUES FROM ('2026-07-01') TO ('2027-01-01');
			INSERT INTO events SELECT g, DATE '2026-01-01' + (g % 360), 'e' FROM generate_series(1, 2000) g;
			CREATE TABLE ticket(id int GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, v text);
			INSERT INTO ticket(v) SELECT 't' FROM generate_series(1, 50);
			CREATE TABLE fk_parent(id int PRIMARY KEY);
			CREATE TABLE fk_child(id int PRIMARY KEY, parent int REFERENCES fk_parent);
			INSERT INTO fk_parent SELECT generate_series(1, 100);
			INSERT INTO fk_child SELECT g, g FROM generate_series(1, 100) g;
			CREATE TABLE "we, ird"(i int PRIMARY KEY); CREATE TABLE plain(i int PRIMARY KEY);
			INSERT INTO "we, ird" SELECT generate_series(1, 30); INSERT INTO plain SELECT generate_series(1, 30);`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `INSERT INTO dog VALUES (1500, 'new dog', 'mixed');
				INSERT INTO animal VALUES (500, 'new animal');
				UPDATE animal SET name = name || ' (updated through the parent)' WHERE id % 10 = 0;
				DELETE FROM animal WHERE id IN (1003, 2005, 7);
				UPDATE cat SET indoor = NOT indoor WHERE id % 3 = 0;
				INSERT INTO layout(id, a, b) SELECT g, 'new', g FROM generate_series(501, 800) g;
				UPDATE layout SET a = 'changed', c = 'changed too' WHERE id % 5 = 0;
				DELETE FROM layout WHERE id % 9 = 0;`)
			ctx := context.Background()
			c, err := pg.Connect(ctx, srcConn.WithDB(db), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close(ctx)
			for round := 0; round < 3; round++ {
				tx, err := c.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `TRUNCATE staging`); err != nil {
					t.Fatal(err)
				}
				rows := make([][]any, 4000)
				for i := range rows {
					rows[i] = []any{i + 1, fmt.Sprintf("reload %d", round)}
				}
				if _, err := tx.CopyFrom(ctx, pgx.Identifier{"staging"}, []string{"id", "v"}, pgx.CopyFromRows(rows)); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			exec1(t, srcConn, db, `TRUNCATE events;
				INSERT INTO events SELECT g, DATE '2026-01-01' + (g % 360), 'after truncate' FROM generate_series(1, 700) g;
				TRUNCATE ticket RESTART IDENTITY;
				INSERT INTO ticket(v) SELECT 'after restart' FROM generate_series(1, 20);
				TRUNCATE fk_parent CASCADE;
				INSERT INTO fk_parent VALUES (1), (2); INSERT INTO fk_child VALUES (1, 1);
				TRUNCATE "we, ird", plain;
				INSERT INTO plain VALUES (7);
				TRUNCATE ONLY animal;`)
			return nil
		}})
}

// Settings on the source database change how values are written as text,
// and every value crosses as text: the base copy's COPY and both plugins
// format it in a session that takes the database's settings. Floats with
// fewer digits, day-first dates, SQL-standard intervals, escaped bytea and
// another time zone must all arrive unchanged. (lc_monetary is not varied:
// the local clusters have only the C locales.)
func TestWorkloadSourceDatabaseSettings(t *testing.T) {
	forPlugins(t, scenario{name: "settings", wantGO: true,
		schema: `CREATE TABLE v(id int PRIMARY KEY, f8 float8, f4 real, d date, ts timestamp, tz timestamptz, iv interval, b bytea, m money, n numeric);
			INSERT INTO v SELECT g, g / 3.0, g / 7.0, DATE '2026-01-02' + g, TIMESTAMP '2026-03-04 05:06:07.891234' + g * interval '1 day',
				TIMESTAMPTZ '2026-03-04 05:06:07.5+00' + g * interval '1 hour', make_interval(0, g % 13, 0, g, g % 24, g % 60, 1.25), decode(md5(g::text), 'hex'),
				(g * 1234.56)::numeric::money, g * 1.0000001 FROM generate_series(1, 500) g;
			DO $$ BEGIN
				EXECUTE format('ALTER DATABASE %I SET extra_float_digits = -10', current_database());
				EXECUTE format('ALTER DATABASE %I SET DateStyle = %L', current_database(), 'SQL, DMY');
				EXECUTE format('ALTER DATABASE %I SET IntervalStyle = %L', current_database(), 'sql_standard');
				EXECUTE format('ALTER DATABASE %I SET bytea_output = %L', current_database(), 'escape');
				EXECUTE format('ALTER DATABASE %I SET TimeZone = %L', current_database(), 'Asia/Kolkata');
			END $$;`,
		streaming: func(t *testing.T, e *Env, id, db string) func() {
			exec1(t, srcConn, db, `INSERT INTO v SELECT g, g / 3.0, g / 7.0, DATE '2026-01-02' + g, TIMESTAMP '2026-03-04 05:06:07.891234' + g * interval '1 day',
					TIMESTAMPTZ '2026-03-04 05:06:07.5+00' + g * interval '1 hour', make_interval(0, g % 13, 0, g, g % 24, g % 60, 1.25), decode(md5(g::text), 'hex'),
					(g * 1234.56)::numeric::money, g * 1.0000001 FROM generate_series(501, 1000) g;
				UPDATE v SET f8 = f8 * pi(), d = d + 40, iv = -iv, m = m * 2 WHERE id % 4 = 0;`)
			return nil
		}})
}
