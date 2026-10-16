package pgcopydb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"

	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/pg"
)

// Retarget points a stopped database's pgcopydb catalogs at new addresses of
// the same source and target clusters (D100). pgcopydb records the bare
// source and target URIs (user@host:port/dbname, no password or options) in
// its catalog and refuses to resume with others; the caller has proved the
// clusters are the same and that no engine runs. Only the user, host and port
// are replaced, so the recorded form is otherwise pgcopydb's own. A database
// that has no catalog yet needs nothing. Returns whether it changed anything.
func (e *Engine) Retarget(ctx context.Context, d engine.DatabaseSpec) (bool, error) {
	file := filepath.Join(d.RunDir, "clone", "schema", "source.db") // Plan's work directory
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	db, err := sql.Open("sqlite", "file:"+file+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return false, err
	}
	defer db.Close()
	var src, dst sql.NullString
	switch err := db.QueryRowContext(ctx, `SELECT source_pg_uri, target_pg_uri FROM setup WHERE id = 1`).Scan(&src, &dst); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading the engine's catalog setup: %w", err)
	}
	ns, err := rebase(src, d.SourceConn)
	if err != nil {
		return false, fmt.Errorf("the engine's recorded source %q: %w", src.String, err)
	}
	nt, err := rebase(dst, d.TargetConn)
	if err != nil {
		return false, fmt.Errorf("the engine's recorded target %q: %w", dst.String, err)
	}
	if ns == src && nt == dst {
		return false, nil
	}
	if _, err := db.ExecContext(ctx, `UPDATE setup SET source_pg_uri = ?, target_pg_uri = ? WHERE id = 1`, ns, nt); err != nil {
		return false, fmt.Errorf("updating the engine's catalog setup: %w", err)
	}
	return true, nil
}

// rebase replaces the user, host and port of a recorded bare URI.
func rebase(v sql.NullString, c pg.Conn) (sql.NullString, error) {
	if !v.Valid || v.String == "" {
		return v, nil
	}
	u, err := url.Parse(v.String)
	if err != nil {
		return v, err
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return v, fmt.Errorf("not a postgres URI")
	}
	u.User = url.User(c.User)
	u.Host = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	return sql.NullString{String: u.String(), Valid: true}, nil
}
