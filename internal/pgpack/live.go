package pgpack

import (
	"context"
	"time"

	"github.com/pagombin/upwell/internal/pg"
)

// CopyProgress lists active COPY and CREATE INDEX work on a target database.
// pgcopydb copies with COPY FROM STDIN, for which pg_stat_progress_copy
// reports bytes_total = 0, so each copy's fraction comes from the source
// table's estimated row count (pg_class.reltuples, kept by ANALYZE and
// autovacuum): rows_estimate and fraction are absent when the source has no
// estimate. The fraction is capped below 1 while the COPY runs, because an
// estimate can be low.
func CopyProgress(ctx context.Context, src, c pg.Conn) map[string]any {
	out := map[string]any{"copies": []any{}, "indexes": []any{}}
	conn, err := pg.Connect(ctx, c, 3*time.Second)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	defer conn.Close(ctx)
	copies := []map[string]any{}
	var names []string
	rows, err := pg.Query(ctx, conn, `SELECT COALESCE(p.relid::regclass::text, '?'), p.bytes_processed, p.bytes_total, p.tuples_processed FROM pg_stat_progress_copy p WHERE p.datname=current_database()`)
	if err == nil {
		for rows.Next() {
			var rel string
			var done, total, tuples int64
			_ = rows.Scan(&rel, &done, &total, &tuples)
			copies = append(copies, map[string]any{"table": rel, "bytes": done, "bytes_total": total, "tuples": tuples})
			names = append(names, rel)
		}
		rows.Close()
	}
	if len(names) > 0 {
		if sc, err := pg.Connect(ctx, src, 3*time.Second); err == nil {
			est := map[string][2]int64{}
			if r, err := pg.Query(ctx, sc, `SELECT c.oid::regclass::text, c.reltuples::bigint, pg_table_size(c.oid) FROM pg_class c WHERE c.oid::regclass::text = ANY($1)`, names); err == nil {
				for r.Next() {
					var n string
					var tup, size int64
					if r.Scan(&n, &tup, &size) == nil {
						est[n] = [2]int64{tup, size}
					}
				}
				r.Close()
			}
			sc.Close(ctx)
			for _, cp := range copies {
				e, ok := est[cp["table"].(string)]
				if !ok {
					continue
				}
				cp["source_bytes"] = e[1]
				if e[0] > 0 { // -1: never analyzed
					cp["rows_estimate"] = e[0]
					cp["fraction"] = min(float64(cp["tuples"].(int64))/float64(e[0]), 0.99)
				}
			}
		}
	}
	idx := []map[string]any{}
	rows, err = pg.Query(ctx, conn, `SELECT COALESCE(p.index_relid::regclass::text, p.relid::regclass::text), p.phase, p.blocks_done, p.blocks_total, p.tuples_done, p.tuples_total FROM pg_stat_progress_create_index p WHERE p.datname=current_database()`)
	if err == nil {
		for rows.Next() {
			var rel, phase string
			var bd, bt, td, tt int64
			_ = rows.Scan(&rel, &phase, &bd, &bt, &td, &tt)
			pct := 0.0
			if bt > 0 {
				pct = float64(bd) / float64(bt) * 100
			} else if tt > 0 {
				pct = float64(td) / float64(tt) * 100
			}
			idx = append(idx, map[string]any{"index": rel, "phase": phase, "percent": pct})
		}
		rows.Close()
	}
	out["copies"], out["indexes"] = copies, idx
	return out
}

// SourceHealth reports the migration's impact on the source cluster.
func SourceHealth(ctx context.Context, src pg.Conn, dbs []string) map[string]any {
	out := map[string]any{"available": true}
	conn, err := pg.Connect(ctx, src, 5*time.Second)
	if err != nil {
		return map[string]any{"available": false, "reason": err.Error()}
	}
	defer conn.Close(ctx)
	var oldestXact, oldestSnapshot float64
	var xminAge int64
	var conns, maxConns int
	pg.QueryRow(ctx, conn, `SELECT COALESCE(max(extract(epoch FROM now()-xact_start)),0), COALESCE(max(extract(epoch FROM now()-backend_xact_start)) FILTER (WHERE backend_xmin IS NOT NULL),0),
		COALESCE(max(age(backend_xmin)),0), count(*), current_setting('max_connections')::int
		FROM (SELECT xact_start, xact_start AS backend_xact_start, backend_xmin FROM pg_stat_activity) a`).Scan(&oldestXact, &oldestSnapshot, &xminAge, &conns, &maxConns)
	out["oldest_xact_seconds"] = oldestXact
	out["oldest_snapshot_seconds"] = oldestSnapshot
	out["xmin_age"] = xminAge
	out["connections"] = conns
	out["max_connections"] = maxConns
	long := []map[string]any{}
	rows, err := pg.Query(ctx, conn, `SELECT pid, usename, COALESCE(datname,''), COALESCE(application_name,''), COALESCE(state,''), extract(epoch FROM now()-xact_start)::float8, left(COALESCE(query,''), 200)
		FROM pg_stat_activity WHERE xact_start IS NOT NULL AND backend_type='client backend' AND pid <> pg_backend_pid() ORDER BY xact_start LIMIT 20`)
	if err == nil {
		for rows.Next() {
			var pid int32
			var user, db, app, state, q string
			var secs float64
			_ = rows.Scan(&pid, &user, &db, &app, &state, &secs, &q)
			long = append(long, map[string]any{"pid": pid, "user": user, "database": db, "application": app, "state": state, "seconds": secs, "query": q})
		}
		rows.Close()
	}
	out["transactions"] = long
	churn := []map[string]any{}
	for _, db := range dbs {
		dc, err := pg.Connect(ctx, src.WithDB(db), 3*time.Second)
		if err != nil {
			continue
		}
		rows, err := pg.Query(ctx, dc, `SELECT format('%I.%I', schemaname, relname), n_dead_tup, n_live_tup, n_tup_upd+n_tup_del, COALESCE(extract(epoch FROM now()-GREATEST(last_autovacuum,last_vacuum))::float8, -1)
			FROM pg_stat_user_tables WHERE schemaname <> 'upwell' ORDER BY n_dead_tup DESC LIMIT 10`)
		if err == nil {
			for rows.Next() {
				var t string
				var dead, live, churnN int64
				var since float64
				_ = rows.Scan(&t, &dead, &live, &churnN, &since)
				churn = append(churn, map[string]any{"database": db, "table": t, "dead_tuples": dead, "live_tuples": live, "changes": churnN, "since_vacuum_seconds": since})
			}
			rows.Close()
		}
		dc.Close(ctx)
	}
	out["top_dead_tuples"] = churn
	return out
}
