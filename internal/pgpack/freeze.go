package pgpack

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/pg"
)

// ClientSessionsWhere selects the client sessions in the current database
// other than Upwell's and the engine's (their application names start with
// "upwell" or "pgcopydb"). The writers check samples them and the write
// freeze ends them.
const ClientSessionsWhere = `datname=current_database() AND backend_type='client backend' AND pid<>pg_backend_pid() AND COALESCE(application_name,'') NOT LIKE 'upwell%' AND COALESCE(application_name,'') NOT LIKE 'pgcopydb%'`

// FreezeSession is a client session the write freeze would end.
type FreezeSession struct {
	PID     int    `json:"pid"`
	User    string `json:"user"`
	App     string `json:"application"`
	Client  string `json:"client"`
	State   string `json:"state"`
	Writing bool   `json:"open_write_transaction"`
}

// FreezeTarget is what the write freeze would do to one source database.
type FreezeTarget struct {
	Database      string          `json:"database"`
	Owner         bool            `json:"owner"`
	SignalBackend bool            `json:"pg_signal_backend"`
	Available     bool            `json:"available"`
	Reason        string          `json:"reason,omitempty"`
	Prior         string          `json:"prior,omitempty"` // the database's own default_transaction_read_only, if set
	Sessions      []FreezeSession `json:"sessions"`
	ApplySQL      []string        `json:"apply_sql"`
	UndoSQL       []string        `json:"undo_sql"`
}

// InspectFreeze reads, without changing anything, what freezing the
// database c names would do. Any failed query is an error.
func InspectFreeze(ctx context.Context, c pg.Conn) (FreezeTarget, error) {
	t := FreezeTarget{Database: c.DBName, Sessions: []FreezeSession{}}
	conn, err := pg.Connect(ctx, c, 15*time.Second)
	if err != nil {
		return t, err
	}
	defer conn.Close(ctx)
	if err := pg.QueryRow(ctx, conn, `SELECT pg_has_role((SELECT datdba FROM pg_database WHERE datname=current_database()),'MEMBER'), pg_has_role('pg_signal_backend','MEMBER')`).Scan(&t.Owner, &t.SignalBackend); err != nil {
		return t, err
	}
	prior, err := priorReadOnly(ctx, conn)
	if err != nil {
		return t, err
	}
	t.Prior = prior
	rows, err := pg.Query(ctx, conn, `SELECT pid, usename::text, COALESCE(application_name,''), COALESCE(client_addr::text,'local'), COALESCE(state,''), backend_xid IS NOT NULL
		FROM pg_stat_activity WHERE `+ClientSessionsWhere+` ORDER BY pid`)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var s FreezeSession
		if err := rows.Scan(&s.PID, &s.User, &s.App, &s.Client, &s.State, &s.Writing); err != nil {
			rows.Close()
			return t, err
		}
		t.Sessions = append(t.Sessions, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return t, err
	}
	switch {
	case !t.Owner:
		t.Reason = c.User + " does not own " + c.DBName + ", and ALTER DATABASE requires ownership"
	case !t.SignalBackend:
		t.Reason = c.User + " is not a member of pg_signal_backend, so it cannot end other roles' sessions"
	default:
		t.Available = true
	}
	db := pg.QuoteIdent(c.DBName)
	t.ApplySQL = []string{
		"ALTER DATABASE " + db + " SET default_transaction_read_only = on;",
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE " + ClientSessionsWhere + ";",
	}
	t.UndoSQL = []string{undoStatement(c.DBName, t.Prior), "-- and end the client sessions opened while the freeze was applied (they stay read-only until they reconnect)"}
	return t, nil
}

// priorReadOnly returns the database's own default_transaction_read_only
// setting (ALTER DATABASE ... SET), or "" when it has none.
func priorReadOnly(ctx context.Context, conn *pgx.Conn) (string, error) {
	var v *string
	err := pg.QueryRow(ctx, conn, `SELECT max(split_part(s, '=', 2)) FROM pg_db_role_setting r, unnest(r.setconfig) s
		WHERE r.setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database()) AND r.setrole = 0 AND s LIKE 'default_transaction_read_only=%'`).Scan(&v)
	if err != nil || v == nil {
		return "", err
	}
	return *v, nil
}

func undoStatement(db, prior string) string {
	if prior == "" {
		return "ALTER DATABASE " + pg.QuoteIdent(db) + " RESET default_transaction_read_only;"
	}
	return "ALTER DATABASE " + pg.QuoteIdent(db) + " SET default_transaction_read_only = " + pg.QuoteLiteral(prior) + ";"
}

// FreezeResult is what applying the freeze did to one database.
type FreezeResult struct {
	Database      string   `json:"database"`
	Prior         string   `json:"prior,omitempty"`
	Terminated    int      `json:"terminated"`
	NotTerminated []string `json:"not_terminated,omitempty"`
}

// ApplyFreeze makes new sessions in the database read-only by default and
// ends the client sessions other than Upwell's and the engine's. A session
// can still turn read-only off for itself: the writers check stays the
// proof that writes stopped.
func ApplyFreeze(ctx context.Context, c pg.Conn, prior string) (FreezeResult, error) {
	r := FreezeResult{Database: c.DBName, Prior: prior}
	conn, err := pg.Connect(ctx, c, 30*time.Second)
	if err != nil {
		return r, err
	}
	defer conn.Close(ctx)
	// A retry may find the freeze already in place for new sessions.
	if _, err := pg.Exec(ctx, conn, `SET default_transaction_read_only = off`); err != nil {
		return r, err
	}
	if _, err := pg.Exec(ctx, conn, `ALTER DATABASE `+pg.QuoteIdent(c.DBName)+` SET default_transaction_read_only = on`); err != nil {
		return r, fmt.Errorf("ALTER DATABASE %s: %w", c.DBName, err)
	}
	r.Terminated, r.NotTerminated, err = endSessions(ctx, conn, "")
	return r, err
}

// UndoFreeze restores the database's setting and ends the client sessions
// that connected while the freeze was applied, which stay read-only until
// they reconnect. since is when the freeze was applied.
func UndoFreeze(ctx context.Context, c pg.Conn, prior string, since time.Time) (int, error) {
	conn, err := pg.Connect(ctx, c, 30*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close(ctx)
	// This session is itself read-only by default while the freeze holds.
	if _, err := pg.Exec(ctx, conn, `SET default_transaction_read_only = off`); err != nil {
		return 0, err
	}
	if _, err := pg.Exec(ctx, conn, strings.TrimSuffix(undoStatement(c.DBName, prior), ";")); err != nil {
		return 0, fmt.Errorf("undoing the freeze on %s: %w", c.DBName, err)
	}
	n, _, err := endSessions(ctx, conn, since.UTC().Format(time.RFC3339Nano))
	return n, err
}

// endSessions ends the database's client sessions (only those that started
// at or after since, when given). A session the user may not end is listed,
// not fatal.
func endSessions(ctx context.Context, conn *pgx.Conn, since string) (int, []string, error) {
	q := `SELECT pid, usename::text||'@'||COALESCE(client_addr::text,'local')||' ('||COALESCE(application_name,'')||')' FROM pg_stat_activity WHERE ` + ClientSessionsWhere
	args := []any{}
	if since != "" {
		q += ` AND backend_start >= $1::timestamptz`
		args = append(args, since)
	}
	rows, err := pg.Query(ctx, conn, q, args...)
	if err != nil {
		return 0, nil, err
	}
	type sess struct {
		pid  int
		desc string
	}
	var all []sess
	for rows.Next() {
		var s sess
		if err := rows.Scan(&s.pid, &s.desc); err != nil {
			rows.Close()
			return 0, nil, err
		}
		all = append(all, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	n := 0
	var not []string
	for _, s := range all {
		var ok bool
		if err := pg.QueryRow(ctx, conn, `SELECT pg_terminate_backend($1)`, s.pid).Scan(&ok); err != nil {
			not = append(not, fmt.Sprintf("%s pid %d: %v", s.desc, s.pid, firstLine(err)))
			continue
		}
		if ok {
			n++
		}
	}
	return n, not, nil
}

// CopyDBProperties copies a source database's own ALTER DATABASE ... SET
// properties to the target database, except default_transaction_read_only
// (a write freeze sets it on the source). Role-specific settings (ALTER
// ROLE ... IN DATABASE) are listed as skipped.
func CopyDBProperties(ctx context.Context, src, dst pg.Conn) (set, skipped []string, err error) {
	sc, err := pg.Connect(ctx, src, 30*time.Second)
	if err != nil {
		return nil, nil, err
	}
	defer sc.Close(ctx)
	rows, err := pg.Query(ctx, sc, `SELECT COALESCE(r.rolname::text, ''), s FROM pg_db_role_setting d LEFT JOIN pg_roles r ON r.oid = d.setrole, unnest(d.setconfig) s
		WHERE d.setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database()) ORDER BY 1, 2`)
	if err != nil {
		return nil, nil, err
	}
	type kv struct{ role, k, v string }
	var all []kv
	for rows.Next() {
		var role, s string
		if err := rows.Scan(&role, &s); err != nil {
			rows.Close()
			return nil, nil, err
		}
		k, v, _ := strings.Cut(s, "=")
		all = append(all, kv{role, k, v})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	dc, err := pg.Connect(ctx, dst, 30*time.Second)
	if err != nil {
		return nil, nil, err
	}
	defer dc.Close(ctx)
	for _, x := range all {
		switch {
		case x.k == "default_transaction_read_only":
			continue
		case x.role != "":
			skipped = append(skipped, fmt.Sprintf("ALTER ROLE %s IN DATABASE ... SET %s = %s", x.role, x.k, x.v))
			continue
		}
		if _, err := pg.Exec(ctx, dc, `ALTER DATABASE `+pg.QuoteIdent(dst.DBName)+` SET `+pg.QuoteIdent(x.k)+` = `+pg.QuoteLiteral(x.v)); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s = %s (%s)", x.k, x.v, firstLine(err)))
			continue
		}
		set = append(set, x.k+" = "+x.v)
	}
	return set, skipped, nil
}
