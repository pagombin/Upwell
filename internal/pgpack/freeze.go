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
	Database      string `json:"database"`
	Owner         bool   `json:"owner"`
	SignalBackend bool   `json:"pg_signal_backend"`
	Available     bool   `json:"available"`
	Reason        string `json:"reason,omitempty"`
	Prior         string `json:"prior,omitempty"` // the database's own default_transaction_read_only, if set
	// Roles whose own default_transaction_read_only (ALTER ROLE ... SET, or
	// ALTER ROLE ... IN DATABASE ... SET) would override the database's: the
	// freeze sets it for them in this database too (D83).
	Roles    []RoleFreeze    `json:"roles"`
	Sessions []FreezeSession `json:"sessions"`
	ApplySQL []string        `json:"apply_sql"`
	UndoSQL  []string        `json:"undo_sql"`
}

// RoleFreeze is a login role whose own setting would let its sessions write
// through the database-level freeze.
type RoleFreeze struct {
	Role string `json:"role"`
	// Effective is the setting its sessions would get; Prior is its
	// role-in-database setting before the freeze ("" when it had none), which
	// the undo restores.
	Effective string `json:"effective"`
	Prior     string `json:"prior,omitempty"`
	CanAlter  bool   `json:"can_alter"`
	Applied   bool   `json:"applied,omitempty"`
}

// readOnlyOn reports whether a default_transaction_read_only value means on.
func readOnlyOn(v string) bool {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'`)) {
	case "on", "true", "yes", "1", "t", "y":
		return true
	}
	return false
}

// roleOverrides lists the login roles whose role-wide or role-in-database
// default_transaction_read_only is not on, and whether this user may change
// their setting in this database (superuser; before PostgreSQL 16, CREATEROLE
// for a non-superuser role; from 16, CREATEROLE with ADMIN OPTION on it).
func roleOverrides(ctx context.Context, conn *pgx.Conn) ([]RoleFreeze, error) {
	rows, err := pg.Query(ctx, conn, `WITH s AS (
			SELECT r.rolname::text AS role, r.rolsuper, s.setdatabase <> 0 AS indb, split_part(c, '=', 2) AS v
			FROM pg_db_role_setting s JOIN pg_roles r ON r.oid = s.setrole, unnest(s.setconfig) c
			WHERE s.setrole <> 0 AND r.rolcanlogin AND c LIKE 'default_transaction_read_only=%'
			  AND s.setdatabase IN (0, (SELECT oid FROM pg_database WHERE datname = current_database())))
		SELECT role, bool_or(rolsuper),
			COALESCE(max(v) FILTER (WHERE indb), max(v) FILTER (WHERE NOT indb)),
			COALESCE(max(v) FILTER (WHERE indb), ''),
			(SELECT rolsuper FROM pg_roles WHERE rolname = current_user)
			  OR (NOT bool_or(rolsuper) AND (SELECT rolcreaterole FROM pg_roles WHERE rolname = current_user)
			      AND (current_setting('server_version_num')::int < 160000 OR pg_has_role(role, 'MEMBER WITH ADMIN OPTION')))
		FROM s GROUP BY role ORDER BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoleFreeze
	for rows.Next() {
		var r RoleFreeze
		var super bool
		if err := rows.Scan(&r.Role, &super, &r.Effective, &r.Prior, &r.CanAlter); err != nil {
			return nil, err
		}
		if !readOnlyOn(r.Effective) {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

func roleFreezeSQL(role, db string) string {
	return "ALTER ROLE " + pg.QuoteIdent(role) + " IN DATABASE " + pg.QuoteIdent(db) + " SET default_transaction_read_only = on;"
}

func roleUndoSQL(role, db, prior string) string {
	if prior == "" {
		return "ALTER ROLE " + pg.QuoteIdent(role) + " IN DATABASE " + pg.QuoteIdent(db) + " RESET default_transaction_read_only;"
	}
	return "ALTER ROLE " + pg.QuoteIdent(role) + " IN DATABASE " + pg.QuoteIdent(db) + " SET default_transaction_read_only = " + pg.QuoteLiteral(prior) + ";"
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
	if t.Roles, err = roleOverrides(ctx, conn); err != nil {
		return t, err
	}
	if t.Roles == nil {
		t.Roles = []RoleFreeze{}
	}
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
	var locked []string
	for _, r := range t.Roles {
		if !r.CanAlter {
			locked = append(locked, r.Role)
		}
	}
	switch {
	case !t.Owner:
		t.Reason = c.User + " does not own " + c.DBName + ", and ALTER DATABASE requires ownership"
	case !t.SignalBackend:
		t.Reason = c.User + " is not a member of pg_signal_backend, so it cannot end other roles' sessions"
	case len(locked) > 0:
		// Freezing the database but not these roles would leave them writing.
		t.Reason = "these roles have their own default_transaction_read_only = off, which overrides the freeze, and " + c.User + " may not change it for them: " + strings.Join(locked, ", ") + " (it needs CREATEROLE with ADMIN OPTION on each)"
	default:
		t.Available = true
	}
	db := pg.QuoteIdent(c.DBName)
	t.ApplySQL = []string{"ALTER DATABASE " + db + " SET default_transaction_read_only = on;"}
	for _, r := range t.Roles {
		t.ApplySQL = append(t.ApplySQL, roleFreezeSQL(r.Role, c.DBName))
	}
	t.ApplySQL = append(t.ApplySQL, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE "+ClientSessionsWhere+";")
	t.UndoSQL = []string{undoStatement(c.DBName, t.Prior)}
	for _, r := range t.Roles {
		t.UndoSQL = append(t.UndoSQL, roleUndoSQL(r.Role, c.DBName, r.Prior))
	}
	t.UndoSQL = append(t.UndoSQL, "-- and end the client sessions opened while the freeze was applied (they stay read-only until they reconnect)")
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
	Database      string       `json:"database"`
	Prior         string       `json:"prior,omitempty"`
	Roles         []RoleFreeze `json:"roles,omitempty"`
	Terminated    int          `json:"terminated"`
	NotTerminated []string     `json:"not_terminated,omitempty"`
}

// ApplyFreeze makes new sessions in the database read-only by default and
// ends the client sessions other than Upwell's and the engine's. A session
// can still turn read-only off for itself: the writers check stays the
// proof that writes stopped.
func ApplyFreeze(ctx context.Context, c pg.Conn, prior string, roles []RoleFreeze) (FreezeResult, error) {
	r := FreezeResult{Database: c.DBName, Prior: prior, Roles: roles}
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
	// Roles whose own setting would override the database's (D83), before the
	// sessions are ended so that they reconnect read-only.
	for i := range r.Roles {
		if _, err := pg.Exec(ctx, conn, strings.TrimSuffix(roleFreezeSQL(r.Roles[i].Role, c.DBName), ";")); err != nil {
			return r, fmt.Errorf("freezing role %s in %s: %w", r.Roles[i].Role, c.DBName, err)
		}
		r.Roles[i].Applied = true
	}
	r.Terminated, r.NotTerminated, err = endSessions(ctx, conn, "")
	return r, err
}

// UndoFreeze restores the database's setting and ends the client sessions
// that connected while the freeze was applied, which stay read-only until
// they reconnect. since is when the freeze was applied.
func UndoFreeze(ctx context.Context, c pg.Conn, prior string, roles []RoleFreeze, since time.Time) (int, error) {
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
	for _, r := range roles {
		if !r.Applied {
			continue
		}
		if _, err := pg.Exec(ctx, conn, strings.TrimSuffix(roleUndoSQL(r.Role, c.DBName, r.Prior), ";")); err != nil {
			return 0, fmt.Errorf("restoring role %s's setting in %s: %w", r.Role, c.DBName, err)
		}
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

// sourceReadOnlySQL reads whether new sessions in the current database are
// read-only, and the database-level setting (as a write freeze leaves it).
const sourceReadOnlySQL = `SELECT current_setting('default_transaction_read_only'), COALESCE((SELECT max(split_part(s, '=', 2)) FROM pg_db_role_setting r, unnest(r.setconfig) s
	WHERE r.setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database()) AND r.setrole = 0 AND s LIKE 'default_transaction_read_only=%'), '')`

// SourceReadOnly reports why new sessions of this user in the connected
// database refuse writes ("" when they accept them).
func SourceReadOnly(ctx context.Context, c *pgx.Conn) (string, error) {
	var ro, dbSetting string
	if err := c.QueryRow(ctx, sourceReadOnlySQL).Scan(&ro, &dbSetting); err != nil {
		return "", err
	}
	switch {
	case !readOnlyOn(ro):
		return "", nil
	case readOnlyOn(dbSetting):
		return "the database is set read-only for new sessions (ALTER DATABASE ... SET default_transaction_read_only = on), as a write freeze leaves it", nil
	}
	return "its new sessions are read-only (default_transaction_read_only = " + ro + ")", nil
}
