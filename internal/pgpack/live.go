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
	// Read in the background (D80): a busy target can take seconds to accept
	// a connection, and a failed read is reported, never shown as idle.
	conn, err := pg.Connect(ctx, c, 20*time.Second)
	if err != nil {
		out["error"] = "connecting to the target: " + err.Error()
		return out
	}
	defer conn.Close(ctx)
	copies := []map[string]any{}
	var names []string
	// One row per table: a split table (D78) is copied by several COPYs at
	// once, each a part of it, so their rows and bytes are summed; parts that
	// already finished are no longer listed, so a split table's fraction comes
	// from its size on the target against its size on the source instead.
	// Never wait on a lock: pgcopydb truncates a table and copies it in one
	// transaction, holding an AccessExclusiveLock on it until the COPY ends
	// (hours for a large table), so anything that opens the table, such as
	// pg_table_size, waits that long (D80). Reading the progress view and
	// casting an oid to regclass take no lock on the table.
	if _, err := pg.Exec(ctx, conn, `SET lock_timeout = '1s'`); err != nil {
		out["error"] = "configuring the progress read on the target: " + err.Error()
		return out
	}
	rows, err := pg.Query(ctx, conn, `SELECT COALESCE(p.relid::regclass::text, '?'), sum(p.bytes_processed)::bigint, sum(p.bytes_total)::bigint, sum(p.tuples_processed)::bigint, count(*), min(p.relid::bigint)
		FROM pg_stat_progress_copy p WHERE p.datname=current_database() GROUP BY 1 ORDER BY 1`)
	split := map[string]int64{}
	if err == nil {
		for rows.Next() {
			var rel string
			var done, total, tuples, parts, relid int64
			_ = rows.Scan(&rel, &done, &total, &tuples, &parts, &relid)
			copies = append(copies, map[string]any{"table": rel, "bytes": done, "bytes_total": total, "tuples": tuples, "parts": parts})
			names = append(names, rel)
			if parts > 1 {
				split[rel] = relid
			}
		}
		rows.Close()
		err = rows.Err()
	}
	if err != nil {
		out["error"] = "reading pg_stat_progress_copy on the target: " + err.Error()
		return out
	}
	// A table copied in parts is not truncated in the COPYs' transactions, so
	// its size can be read; lock_timeout still bounds the wait.
	for _, cp := range copies {
		if relid, ok := split[cp["table"].(string)]; ok {
			var size int64
			if pg.QueryRow(ctx, conn, `SELECT pg_table_size($1::oid)`, relid).Scan(&size) == nil {
				cp["target_bytes"] = size
			}
		}
	}
	if len(names) > 0 {
		if sc, err := pg.Connect(ctx, src, 20*time.Second); err == nil {
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
				if cp["parts"].(int64) > 1 {
					if tb, ok := cp["target_bytes"].(int64); ok && e[1] > 0 {
						cp["fraction"] = min(float64(tb)/float64(e[1]), 0.99)
					}
					if e[0] > 0 {
						cp["rows_estimate"] = e[0]
					}
					continue
				}
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
		err = rows.Err()
	}
	if err != nil {
		out["error"] = "reading pg_stat_progress_create_index on the target: " + err.Error()
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
