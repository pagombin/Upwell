package orch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/store"
)

// Watched tables (overnight item 9): exact row counts on both sides, for
// tables the operator picks, taken while streaming. Counting is exact and
// therefore costs a scan of each table, so only chosen tables are counted,
// once a minute, with a time limit.

// WatchedTable is one watched table and its latest counts.
type WatchedTable struct {
	ID         string `json:"id"`
	Database   string `json:"database"`
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	AddedBy    string `json:"added_by"`
	AddedAt    int64  `json:"added_at"`
	SourceRows *int64 `json:"source_rows,omitempty"`
	TargetRows *int64 `json:"target_rows,omitempty"`
	CountedAt  *int64 `json:"counted_at,omitempty"`
	CountError string `json:"count_error,omitempty"`
}

// QualifiedName is the quoted schema.table.
func (w WatchedTable) QualifiedName() string { return pgx.Identifier{w.Schema, w.Table}.Sanitize() }

// ListWatched returns a migration's watched tables (for one database when db is set).
func (o *Orchestrator) ListWatched(ctx context.Context, migID, db string) ([]WatchedTable, error) {
	q := `SELECT id, database, schema_name, table_name, added_by, added_at, source_rows, target_rows, counted_at, COALESCE(count_error,'') FROM watched_tables WHERE migration_id=?`
	args := []any{migID}
	if db != "" {
		q += ` AND database=?`
		args = append(args, db)
	}
	rows, err := o.st.DB.QueryContext(ctx, q+` ORDER BY database, schema_name, table_name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WatchedTable{}
	for rows.Next() {
		var w WatchedTable
		var s, t, c sql.NullInt64
		if err := rows.Scan(&w.ID, &w.Database, &w.Schema, &w.Table, &w.AddedBy, &w.AddedAt, &s, &t, &c, &w.CountError); err != nil {
			return nil, err
		}
		if s.Valid {
			w.SourceRows = &s.Int64
		}
		if t.Valid {
			w.TargetRows = &t.Int64
		}
		if c.Valid {
			w.CountedAt = &c.Int64
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SourceTables lists a source database's tables for the watch picker.
func (o *Orchestrator) SourceTables(ctx context.Context, migID, db string) ([]map[string]any, error) {
	m, err := o.GetMigration(ctx, migID)
	if err != nil {
		return nil, err
	}
	d, err := o.dbByName(ctx, m.ID, db)
	if err != nil {
		return nil, err
	}
	src, _, err := o.Conns(ctx, m)
	if err != nil {
		return nil, err
	}
	c, err := pg.Connect(ctx, src.WithDB(d.SourceName), 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close(ctx)
	rows, err := pg.Query(ctx, c, `SELECT n.nspname::text, c.relname::text, GREATEST(c.reltuples, 0)::bigint, pg_total_relation_size(c.oid)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r','p') AND n.nspname NOT IN ('pg_catalog','information_schema','upwell') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'
		ORDER BY 1, 2 LIMIT 2000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var s, t string
		var est, size int64
		if err := rows.Scan(&s, &t, &est, &size); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"schema": s, "table": t, "estimated_rows": est, "size_bytes": size})
	}
	return out, rows.Err()
}

func (o *Orchestrator) dbByName(ctx context.Context, migID, name string) (Database, error) {
	dbs, err := o.ListDatabases(ctx, migID)
	if err != nil {
		return Database{}, err
	}
	for _, d := range dbs {
		if d.SourceName == name && d.Include {
			return d, nil
		}
	}
	return Database{}, store.ErrNotFound
}

// AddWatched starts watching a source table (it must exist on the source).
func (o *Orchestrator) AddWatched(ctx context.Context, a audit.Actor, migID, db, schema, table string) (WatchedTable, error) {
	tables, err := o.SourceTables(ctx, migID, db)
	if err != nil {
		return WatchedTable{}, err
	}
	found := false
	for _, t := range tables {
		if t["schema"] == schema && t["table"] == table {
			found = true
		}
	}
	if !found {
		return WatchedTable{}, uerr("no_table", fmt.Sprintf("%s has no table %s.", db, pgx.Identifier{schema, table}.Sanitize()), "")
	}
	w := WatchedTable{ID: store.NewID(), Database: db, Schema: schema, Table: table, AddedBy: a.Username, AddedAt: store.Now()}
	if _, err := o.st.DB.ExecContext(ctx, `INSERT INTO watched_tables(id, migration_id, database, schema_name, table_name, added_by, added_at) VALUES (?,?,?,?,?,?,?)`,
		w.ID, migID, db, schema, table, w.AddedBy, w.AddedAt); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return w, uerr("watched", "That table is already watched.", "")
		}
		return w, err
	}
	o.audit.Append(ctx, a, "watch.add", migID, nil, map[string]any{"database": db, "table": w.QualifiedName()})
	return w, nil
}

// RemoveWatched stops watching a table.
func (o *Orchestrator) RemoveWatched(ctx context.Context, a audit.Actor, migID, id string) error {
	res, err := o.st.DB.ExecContext(ctx, `DELETE FROM watched_tables WHERE id=? AND migration_id=?`, id, migID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	o.audit.Append(ctx, a, "watch.remove", migID, nil, map[string]any{"id": id})
	return nil
}

// countWatched counts each watched table of a database on both sides; a
// failed count is stored as such, never as zero. Called from the collector.
// watchBusy holds the databases whose watched tables are being counted (O6:
// a count slower than the collector's interval would otherwise pile up
// full-table scans on both clusters).
var watchBusy sync.Map

func (o *Orchestrator) countWatched(ctx context.Context, m Migration, d Database, src, dst pg.Conn) {
	if _, busy := watchBusy.LoadOrStore(d.ID, true); busy {
		return
	}
	defer watchBusy.Delete(d.ID)
	ws, err := o.ListWatched(ctx, m.ID, d.SourceName)
	if err != nil || len(ws) == 0 {
		return
	}
	count := func(c pg.Conn, q string) (int64, error) {
		conn, err := pg.Connect(ctx, c, 60*time.Second)
		if err != nil {
			return 0, err
		}
		defer conn.Close(ctx)
		var n int64
		err = pg.QueryRow(ctx, conn, `SELECT count(*) FROM `+q).Scan(&n)
		return n, err
	}
	for _, w := range ws {
		s, serr := count(src.WithDB(d.SourceName), w.QualifiedName())
		t, terr := count(dst.WithDB(d.TargetName), w.QualifiedName())
		var msg string
		if e := errors.Join(prefixErr("source", serr), prefixErr("target", terr)); e != nil {
			msg = trimErr(e)
		}
		var sv, tv any
		if serr == nil {
			sv = s
		}
		if terr == nil {
			tv = t
		}
		o.st.DB.ExecContext(ctx, `UPDATE watched_tables SET source_rows=?, target_rows=?, counted_at=?, count_error=? WHERE id=?`, sv, tv, store.Now(), store.NullString(msg), w.ID)
		if serr == nil && terr == nil {
			ts := time.Now().UnixMilli()
			o.metrics.put(ctx, m.ID, d.SourceName, "watch_diff:"+w.Schema+"."+w.Table, float64(s-t), ts)
		}
	}
}

func prefixErr(side string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", side, err)
}
