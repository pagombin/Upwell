package pgpack

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/check"
	"github.com/pagombin/upwell/internal/pg"
)

// VerifyInput describes one database's verification.
type VerifyInput struct {
	Source, Target     pg.Conn // pointed at the databases
	TargetMaint        pg.Conn
	Origin             string
	HeartbeatRequired  bool
	HeartbeatToken     string
	HBBeforeLSN        string
	OriginLSN          string // measured after the drain; empty to measure now
	ExactCountMaxBytes int64
	ChecksumBudget     time.Duration
	Database           string
}

// VerifyResult is one database's verification.
type VerifyResult struct {
	Results []check.Result `json:"results"`
	GO      bool           `json:"go"`
}

func vres(id, db, level, title, msg string, ev map[string]any) check.Result {
	r := check.Result{CheckID: id, Version: 1, Scope: check.ScopeDatabase, Database: db, Level: level, Hard: level == check.Blocker, Title: title, Message: msg, Evidence: ev}
	r.Finalize()
	return r
}

type tableInfo struct {
	Name    string
	Bytes   int64
	Est     int64
	Columns string
}

func listTables(ctx context.Context, c *pgx.Conn) (map[string]tableInfo, error) {
	// A partitioned parent has no storage of its own: its size is its
	// partitions', so the budget orders it correctly. The column signature
	// includes NOT NULL, identity and generated expressions and defaults.
	rows, err := c.Query(ctx, `SELECT format('%I.%I', n.nspname, c.relname),
		CASE WHEN c.relkind='p' THEN (SELECT COALESCE(sum(pg_total_relation_size(pt.relid)),0)::bigint FROM pg_partition_tree(c.oid) pt) ELSE pg_total_relation_size(c.oid) END,
		GREATEST(c.reltuples,0)::bigint,
		c.relkind::text || '|' || COALESCE((SELECT string_agg(a.attname||':'||format_type(a.atttypid,a.atttypmod)||CASE WHEN a.attnotnull THEN ' not null' ELSE '' END||
			CASE WHEN a.attidentity<>'' THEN ' identity '||a.attidentity::text ELSE '' END||CASE WHEN a.attgenerated<>'' THEN ' generated '||COALESCE(pg_get_expr(d.adbin,d.adrelid),'') ELSE '' END,
			',' ORDER BY a.attnum) FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped),'')
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind IN ('r','p') AND c.relpersistence<>'t' AND n.nspname NOT IN ('pg_catalog','information_schema','upwell') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%' AND NOT c.relispartition`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]tableInfo{}
	for rows.Next() {
		var t tableInfo
		if err := rows.Scan(&t.Name, &t.Bytes, &t.Est, &t.Columns); err != nil {
			return nil, err
		}
		out[t.Name] = t
	}
	return out, rows.Err()
}

// schemaObjects lists what pg_dump recreates besides table columns: indexes,
// constraints (validated or not), triggers, views, materialized views and
// functions, as comparable strings keyed by kind. NOT NULL constraints are in
// the column signature (PostgreSQL 18 also lists them in pg_constraint).
func schemaObjects(ctx context.Context, c *pgx.Conn) (map[string]map[string]bool, error) {
	const userNS = `n.nspname NOT IN ('pg_catalog','information_schema','upwell') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'`
	queries := map[string]string{
		"index": `SELECT pg_get_indexdef(i.indexrelid) FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE ` + userNS + ` AND c.relpersistence<>'t' AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid=i.indexrelid AND d.deptype='e')`,
		"constraint": `SELECT format('%I.%I', n.nspname, c.relname)||' '||co.conname||' '||pg_get_constraintdef(co.oid)||CASE WHEN co.convalidated THEN '' ELSE ' NOT VALID' END
			FROM pg_constraint co JOIN pg_class c ON c.oid=co.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE ` + userNS + ` AND c.relpersistence<>'t' AND co.contype IN ('p','u','f','c','x') AND co.conislocal`,
		"trigger": `SELECT format('%I.%I', n.nspname, c.relname)||' '||t.tgname||' '||t.tgenabled::text FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE ` + userNS + ` AND NOT t.tgisinternal`,
		"view": `SELECT c.relkind::text||' '||format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE ` + userNS + ` AND c.relkind IN ('v','m') AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid=c.oid AND d.deptype='e')`,
		"function": `SELECT format('%I.%I', n.nspname, p.proname)||'('||pg_get_function_identity_arguments(p.oid)||')' FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
			WHERE ` + userNS + ` AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid=p.oid AND d.deptype='e')`,
	}
	out := map[string]map[string]bool{}
	for kind, q := range queries {
		vals, err := pg.QueryStrings(ctx, c, q)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", kind, err)
		}
		m := map[string]bool{}
		for _, v := range vals {
			m[v] = true
		}
		out[kind] = m
	}
	return out, nil
}

// prepareVerifySession fixes everything that changes a row's text form or
// could cut or filter Upwell's own queries: the role's timeouts (pgcopydb
// clears them in its sessions, Upwell must too), row-level security (with
// row_security off a query errors instead of silently returning fewer rows),
// and the output settings, so the same row hashes the same on both servers.
func prepareVerifySession(ctx context.Context, c *pgx.Conn) error {
	stmts := []string{"SET statement_timeout = 0", "SET lock_timeout = 0", "SET idle_in_transaction_session_timeout = 0", "SET row_security = off",
		"SET TimeZone = 'UTC'", "SET DateStyle = 'ISO, YMD'", "SET IntervalStyle = 'postgres'", "SET extra_float_digits = 3", "SET bytea_output = 'hex'", "SET lc_monetary = 'C'"}
	if v, err := pg.ServerVersionNum(ctx, c); err == nil && v >= 140000 {
		stmts = append(stmts, "SET idle_session_timeout = 0")
	}
	for _, q := range stmts {
		if _, err := c.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}

// rowsHash returns count and the order-independent sum of 60-bit row hashes (D4).
func rowsHash(ctx context.Context, c *pgx.Conn, table string) (int64, string, error) {
	var n int64
	var sum *string
	err := c.QueryRow(ctx, fmt.Sprintf(`SELECT count(*), COALESCE(sum(('x'||substr(md5(ROW(t.*)::text),1,15))::bit(60)::bigint),0)::text FROM %s t`, table)).Scan(&n, &sum)
	if err != nil {
		return 0, "", err
	}
	return n, *sum, nil
}

func exactCount(ctx context.Context, c *pgx.Conn, table string) (int64, error) {
	var n int64
	err := c.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n)
	return n, err
}

// Verify compares source and target. It never relies on the engine's own
// success or positions (PC5, PC13).
func Verify(ctx context.Context, in VerifyInput) (VerifyResult, error) {
	db := in.Database
	var out []check.Result
	add := func(r check.Result) { out = append(out, r) }

	// 1. Final heartbeat present on the target.
	if in.HeartbeatRequired {
		if in.HeartbeatToken == "" {
			add(vres("final_heartbeat", db, check.Blocker, "Final heartbeat on the target", "No final heartbeat was written for this database, so nothing proves the last writes arrived.", map[string]any{}))
		} else {
			found, err := HeartbeatOnTarget(ctx, in.Target, in.HeartbeatToken)
			ev := map[string]any{"token": in.HeartbeatToken, "found": found}
			switch {
			case err != nil:
				add(vres("final_heartbeat", db, check.Blocker, "Final heartbeat on the target", "Could not read the heartbeat table on the target: "+err.Error(), ev))
			case !found:
				add(vres("final_heartbeat", db, check.Blocker, "Final heartbeat on the target", "The final heartbeat written after writes stopped never reached the target. Changes made before it may be missing too.", ev))
			default:
				add(vres("final_heartbeat", db, check.OK, "Final heartbeat on the target", "The final heartbeat, the last write before the end position, is on the target.", ev))
			}
		}
	} else {
		add(vres("final_heartbeat", db, check.Warning, "Final heartbeat on the target", "Heartbeat is off for this migration (accepted risk): no proof the last writes arrived beyond the data comparison.", map[string]any{"required": false}))
	}

	// 2. Origin reached the final heartbeat's position (D3).
	if in.HBBeforeLSN != "" {
		origin := in.OriginLSN
		if origin == "" {
			o, err := OriginProgress(ctx, in.TargetMaint, in.Origin)
			if err != nil {
				o = ""
				in.Origin += " (" + err.Error() + ")"
			}
			origin = o
		}
		ev := map[string]any{"origin": origin, "required_at_least": in.HBBeforeLSN, "origin_name": in.Origin}
		switch {
		case origin == "":
			add(vres("origin_progress", db, check.Blocker, "Target origin reached the final heartbeat", "The target has no progress recorded for origin "+in.Origin+".", ev))
		case !pg.LSNGreaterOrEqual(origin, in.HBBeforeLSN):
			add(vres("origin_progress", db, check.Blocker, "Target origin reached the final heartbeat", fmt.Sprintf("The target applied changes only up to %s, short of %s where the final heartbeat was written.", origin, in.HBBeforeLSN), ev))
		default:
			add(vres("origin_progress", db, check.OK, "Target origin reached the final heartbeat", fmt.Sprintf("The target applied changes through %s, past the final heartbeat at %s.", origin, in.HBBeforeLSN), ev))
		}
	}

	src, err := pg.Connect(ctx, in.Source, 0)
	if err != nil {
		add(vres("schema", db, check.Blocker, "Schema matches", "Cannot connect to the source: "+err.Error(), map[string]any{}))
		return finish(out), nil
	}
	defer src.Close(ctx)
	dst, err := pg.Connect(ctx, in.Target, 0)
	if err != nil {
		add(vres("schema", db, check.Blocker, "Schema matches", "Cannot connect to the target: "+err.Error(), map[string]any{}))
		return finish(out), nil
	}
	defer dst.Close(ctx)
	for side, c := range map[string]*pgx.Conn{"source": src, "target": dst} {
		if err := prepareVerifySession(ctx, c); err != nil {
			add(vres("schema", db, check.Blocker, "Schema matches", "Cannot prepare the "+side+" session for verification: "+err.Error(), map[string]any{}))
			return finish(out), nil
		}
	}

	// 3. Schema: tables and columns.
	st, err1 := listTables(ctx, src)
	dt, err2 := listTables(ctx, dst)
	if err1 != nil || err2 != nil {
		add(vres("schema", db, check.Blocker, "Schema matches", fmt.Sprintf("Could not list tables: %v %v", err1, err2), map[string]any{}))
		return finish(out), nil
	}
	var missing, colDiff, extra []string
	for name, s := range st {
		d, ok := dt[name]
		if !ok {
			missing = append(missing, name)
		} else if d.Columns != s.Columns {
			colDiff = append(colDiff, name)
		}
	}
	for name := range dt {
		if _, ok := st[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(colDiff)
	sort.Strings(extra)
	ev := map[string]any{"missing_on_target": missing, "column_differences": colDiff, "tables": len(st)}
	if len(missing) > 0 || len(colDiff) > 0 {
		msg := []string{}
		if len(missing) > 0 {
			msg = append(msg, fmt.Sprintf("%d table(s) missing on the target (%s)", len(missing), strings.Join(limitList(missing, 5), ", ")))
		}
		if len(colDiff) > 0 {
			msg = append(msg, fmt.Sprintf("%d table(s) with different columns (%s)", len(colDiff), strings.Join(limitList(colDiff, 5), ", ")))
		}
		add(vres("schema", db, check.Blocker, "Schema matches", strings.Join(msg, "; ")+".", ev))
	} else {
		add(vres("schema", db, check.OK, "Schema matches", fmt.Sprintf("All %d tables exist on the target with the same columns.", len(st)), ev))
	}
	// Indexes, constraints, triggers, views and functions.
	so, e1 := schemaObjects(ctx, src)
	do, e2 := schemaObjects(ctx, dst)
	if e1 != nil || e2 != nil {
		add(vres("schema_objects", db, check.Blocker, "Indexes, constraints and other objects match", fmt.Sprintf("Could not list schema objects: %v %v", e1, e2), map[string]any{}))
	} else {
		var objMissing []string
		counts := map[string]int{}
		for _, kind := range []string{"index", "constraint", "trigger", "view", "function"} {
			counts[kind] = len(so[kind])
			for v := range so[kind] {
				if !do[kind][v] {
					objMissing = append(objMissing, kind+" "+v)
				}
			}
		}
		sort.Strings(objMissing)
		oev := map[string]any{"missing_or_different_on_target": limitList(objMissing, 100), "counts": counts}
		if len(objMissing) > 0 {
			add(vres("schema_objects", db, check.Blocker, "Indexes, constraints and other objects match", fmt.Sprintf("%d object(s) are missing or different on the target: %s.", len(objMissing), strings.Join(limitList(objMissing, 3), "; ")), oev))
		} else {
			add(vres("schema_objects", db, check.OK, "Indexes, constraints and other objects match", fmt.Sprintf("%d indexes, %d constraints, %d triggers, %d views and %d functions match.", counts["index"], counts["constraint"], counts["trigger"], counts["view"], counts["function"]), oev))
		}
	}
	if len(extra) > 0 {
		add(vres("target_only_objects", db, check.Warning, "Objects only on the target", fmt.Sprintf("%d table(s) exist only on the target (for example %s); they were left untouched.", len(extra), extra[0]), map[string]any{"tables": limitList(extra, 50)}))
	}

	// 4. Counts and checksums, smallest tables first.
	tables := make([]tableInfo, 0, len(st))
	for name, t := range st {
		if _, ok := dt[name]; ok {
			tables = append(tables, t)
		}
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].Bytes < tables[j].Bytes })
	deadline := time.Now().Add(in.ChecksumBudget)
	var countFail, sumFail, notSummed, estOnly []string
	exactN, summedN := 0, 0
	type pair struct {
		n   int64
		sum string
		err error
	}
	for _, t := range tables {
		if ctx.Err() != nil {
			return finish(out), ctx.Err()
		}
		withinBudget := in.ChecksumBudget > 0 && time.Now().Before(deadline)
		if withinBudget {
			var a, b pair
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); a.n, a.sum, a.err = rowsHash(ctx, src, t.Name) }()
			go func() { defer wg.Done(); b.n, b.sum, b.err = rowsHash(ctx, dst, t.Name) }()
			wg.Wait()
			if a.err != nil || b.err != nil {
				countFail = append(countFail, fmt.Sprintf("%s (error: %v %v)", t.Name, a.err, b.err))
				continue
			}
			exactN++
			summedN++
			if a.n != b.n {
				countFail = append(countFail, fmt.Sprintf("%s (source %d, target %d)", t.Name, a.n, b.n))
			} else if a.sum != b.sum {
				sumFail = append(sumFail, t.Name)
			}
			continue
		}
		notSummed = append(notSummed, t.Name)
		if t.Bytes <= in.ExactCountMaxBytes {
			var a, b pair
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); a.n, a.err = exactCount(ctx, src, t.Name) }()
			go func() { defer wg.Done(); b.n, b.err = exactCount(ctx, dst, t.Name) }()
			wg.Wait()
			exactN++
			if a.err != nil || b.err != nil || a.n != b.n {
				countFail = append(countFail, fmt.Sprintf("%s (source %d, target %d)", t.Name, a.n, b.n))
			}
		} else {
			estOnly = append(estOnly, t.Name)
			// Not counted: at least catch a table that is empty or far
			// smaller on the target (its storage, since planner estimates
			// are not refreshed until ANALYZE).
			if dtb := dt[t.Name].Bytes; t.Bytes > 0 && dtb*10 < t.Bytes {
				countFail = append(countFail, fmt.Sprintf("%s (too large to count; %s on the source but only %s on the target)", t.Name, humanBytes(float64(t.Bytes)), humanBytes(float64(dtb))))
			}
		}
	}
	ev = map[string]any{"exact": exactN, "mismatches": countFail, "estimated_only": estOnly}
	if len(countFail) > 0 {
		add(vres("row_counts", db, check.Blocker, "Row counts match", fmt.Sprintf("%d table(s) have different row counts: %s.", len(countFail), strings.Join(limitList(countFail, 5), "; ")), ev))
	} else {
		add(vres("row_counts", db, check.OK, "Row counts match", fmt.Sprintf("Exact counts match for %d table(s).", exactN), ev))
	}
	if len(estOnly) > 0 {
		add(vres("row_estimates", db, check.Warning, "Large tables not counted", fmt.Sprintf("%d table(s) above the exact-count limit and outside the checksum window were not counted; only their size on disk was compared, which catches an empty or truncated table but not missing rows. Raise exact_count_max_mb or verify_checksum_budget_seconds to compare them fully.", len(estOnly)), map[string]any{"tables": limitList(estOnly, 50)}))
	}
	ev = map[string]any{"checksummed": summedN, "mismatches": sumFail, "not_checksummed": limitList(notSummed, 100)}
	switch {
	case len(sumFail) > 0:
		add(vres("checksums", db, check.Blocker, "Row checksums match", fmt.Sprintf("%d table(s) have the same row count but different contents: %s.", len(sumFail), strings.Join(limitList(sumFail, 5), ", ")), ev))
	case len(notSummed) > 0:
		add(vres("checksums", db, check.Warning, "Row checksums match", fmt.Sprintf("Checksums match for %d table(s); %d table(s) did not fit in the verification window and were not checksummed.", summedN, len(notSummed)), ev))
	default:
		add(vres("checksums", db, check.OK, "Row checksums match", fmt.Sprintf("Checksums match for all %d table(s).", summedN), ev))
	}

	// 5. Sequences.
	seqDiff, nseq, err := compareSequences(ctx, src, dst)
	ev = map[string]any{"sequences": nseq, "behind": seqDiff}
	switch {
	case err != nil:
		add(vres("sequences", db, check.Blocker, "Sequences synced", "Could not compare sequences: "+err.Error(), ev))
	case len(seqDiff) > 0:
		add(vres("sequences", db, check.Blocker, "Sequences synced", fmt.Sprintf("%d sequence(s) on the target are behind the source: %s.", len(seqDiff), strings.Join(limitList(seqDiff, 5), "; ")), ev))
	default:
		add(vres("sequences", db, check.OK, "Sequences synced", fmt.Sprintf("All %d sequence(s) are at or past the source's values.", nseq), ev))
	}
	return finish(out), nil
}

func finish(rs []check.Result) VerifyResult {
	g := true
	for _, r := range rs {
		if r.Level == check.Blocker {
			g = false
		}
	}
	return VerifyResult{Results: rs, GO: g}
}

type seqVal struct {
	v    int64
	desc bool
}

func sequenceValues(ctx context.Context, c *pgx.Conn) (map[string]seqVal, error) {
	rows, err := c.Query(ctx, `SELECT format('%I.%I', schemaname, sequencename), last_value, start_value, increment_by < 0 FROM pg_sequences WHERE schemaname <> 'upwell'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]seqVal{}
	for rows.Next() {
		var n string
		var last *int64
		var start int64
		var desc bool
		if err := rows.Scan(&n, &last, &start, &desc); err != nil {
			return nil, err
		}
		// Never called: the next value is the start, one step "before" it.
		var v int64
		switch {
		case last != nil:
			v = *last
		case desc:
			v = start + 1
		default:
			v = start - 1
		}
		out[n] = seqVal{v, desc}
	}
	return out, rows.Err()
}

// behind reports whether target t would hand out a value the source already
// used: for an ascending sequence t < s, for a descending one t > s.
func behind(s, t seqVal) bool {
	if s.desc {
		return t.v > s.v
	}
	return t.v < s.v
}

func compareSequences(ctx context.Context, src, dst *pgx.Conn) ([]string, int, error) {
	s, err := sequenceValues(ctx, src)
	if err != nil {
		return nil, 0, err
	}
	d, err := sequenceValues(ctx, dst)
	if err != nil {
		return nil, 0, err
	}
	var diff []string
	for n, v := range s {
		if dv, ok := d[n]; !ok || behind(v, dv) {
			diff = append(diff, fmt.Sprintf("%s (source %d, target %d)", n, v.v, d[n].v))
		}
	}
	sort.Strings(diff)
	return diff, len(s), nil
}

// SyncSequences raises every target sequence to the source's value when it is
// behind (a safety net: the engine syncs them at the end position too).
func SyncSequences(ctx context.Context, srcC, dstC pg.Conn) (int, error) {
	src, err := pg.Connect(ctx, srcC, time.Minute)
	if err != nil {
		return 0, err
	}
	defer src.Close(ctx)
	dst, err := pg.Connect(ctx, dstC, time.Minute)
	if err != nil {
		return 0, err
	}
	defer dst.Close(ctx)
	sv, err := sequenceValues(ctx, src)
	if err != nil {
		return 0, err
	}
	dv, err := sequenceValues(ctx, dst)
	if err != nil {
		return 0, err
	}
	n := 0
	for name, v := range sv {
		cur, ok := dv[name]
		if !ok || !behind(v, cur) {
			continue
		}
		if _, err := dst.Exec(ctx, `SELECT setval($1::regclass, $2)`, name, v.v); err != nil {
			return n, fmt.Errorf("sequence %s: %w", name, err)
		}
		n++
	}
	return n, nil
}

// WriteActivity samples total row changes in a database (for the manual
// stop-writes check).
func WriteActivity(ctx context.Context, c pg.Conn) (int64, []string, error) {
	conn, err := pg.Connect(ctx, c, 15*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close(ctx)
	var n int64
	conn.QueryRow(ctx, `SELECT COALESCE(sum(n_tup_ins+n_tup_upd+n_tup_del),0)::bigint FROM pg_stat_user_tables WHERE schemaname <> 'upwell'`).Scan(&n)
	sessions, _ := pg.QueryStrings(ctx, conn, `SELECT usename||'@'||COALESCE(client_addr::text,'local')||' ('||COALESCE(application_name,'')||', '||state||')' FROM pg_stat_activity
		WHERE datname=current_database() AND backend_type='client backend' AND pid<>pg_backend_pid() AND COALESCE(application_name,'') NOT LIKE 'upwell%' AND COALESCE(application_name,'') NOT LIKE 'pgcopydb%' ORDER BY 1 LIMIT 50`)
	return n, sessions, nil
}
