// Package pg holds PostgreSQL connection helpers shared by the PostgreSQL
// check pack, the pgcopydb engine and verification. Nothing outside those
// may assume PostgreSQL.
package pg

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Conn describes one endpoint. Password is never logged or put in a URI that
// leaves the process; pgcopydb receives it through a pgpass file.
type Conn struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"-"`
	DBName   string `json:"dbname"`
	SSLMode  string `json:"sslmode"`
	CACert   string `json:"-"`
	CAFile   string `json:"-"`
}

// WithDB returns a copy pointed at db.
func (c Conn) WithDB(db string) Conn { c.DBName = db; return c }

// URI returns a libpq URI without the password.
func (c Conn) URI() string {
	u := url.URL{Scheme: "postgres", User: url.User(c.User), Host: fmt.Sprintf("%s:%d", c.Host, c.Port), Path: "/" + c.DBName}
	q := url.Values{}
	q.Set("sslmode", c.SSLMode)
	if c.CAFile != "" {
		q.Set("sslrootcert", c.CAFile)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// connString includes the password, for pgx only.
func (c Conn) connString() string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(c.User, c.Password), Host: fmt.Sprintf("%s:%d", c.Host, c.Port), Path: "/" + c.DBName}
	q := url.Values{}
	q.Set("sslmode", c.SSLMode)
	q.Set("connect_timeout", "10")
	q.Set("application_name", "upwell")
	if c.CAFile != "" {
		q.Set("sslrootcert", c.CAFile)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Connect opens a connection with a statement timeout. The timeout is a
// startup parameter for a direct connection. A pooler such as PgBouncer
// refuses unknown startup parameters; then Upwell reconnects without it and
// sets the timeout only if the session is really its own, because in
// transaction mode a SET would stay on a server connection that the
// customer's application uses next.
func Connect(ctx context.Context, c Conn, stmtTimeout time.Duration) (*pgx.Conn, error) {
	key := fmt.Sprintf("%s:%d", c.Host, c.Port)
	if _, pooled := refusesStartupParams.Load(key); !pooled || stmtTimeout <= 0 {
		conn, err := connect(ctx, c, stmtTimeout)
		if err == nil || stmtTimeout <= 0 || !strings.Contains(err.Error(), "unsupported startup parameter") {
			return conn, Explain(err)
		}
		refusesStartupParams.Store(key, true)
	}
	conn, err := connect(ctx, c, 0)
	if err != nil {
		return nil, Explain(err)
	}
	pinned, _, perr := sessionPinned(ctx, conn, c)
	if perr != nil {
		conn.Close(ctx)
		return nil, Explain(perr)
	}
	if pinned {
		if _, err := conn.Exec(ctx, "SET statement_timeout = "+strconv.Itoa(int(stmtTimeout/time.Millisecond)), pgx.QueryExecModeSimpleProtocol); err != nil {
			conn.Close(ctx)
			return nil, Explain(err)
		}
	}
	return conn, nil
}

// refusesStartupParams remembers endpoints (host:port) that refused a
// startup parameter, so each connection does not first get refused again.
var refusesStartupParams sync.Map

func connect(ctx context.Context, c Conn, stmtTimeout time.Duration) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(c.connString())
	if err != nil {
		return nil, fmt.Errorf("connection details for %s: %w", c.Host, err)
	}
	if stmtTimeout > 0 {
		cfg.RuntimeParams["statement_timeout"] = strconv.Itoa(int(stmtTimeout / time.Millisecond))
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return pgx.ConnectConfig(cctx, cfg)
}

// Session describes whether session state survives between transactions on
// a connection, which the engine depends on (session_replication_role, the
// replication origin, the exported snapshot).
type Session struct {
	ClientAddr  string `json:"client_addr"`
	ProxyLikely bool   `json:"proxy_likely"`
	Pinned      bool   `json:"pinned"`
	PIDs        []int  `json:"backend_pids"`
}

// ProbeSession checks that consecutive transactions on c reach the same
// server session. A client address that is not one of this host's own
// suggests a pooler or proxy in between.
func ProbeSession(ctx context.Context, c *pgx.Conn, at Conn) (Session, error) {
	var s Session
	var addr *string
	if err := c.QueryRow(ctx, `SELECT host(inet_client_addr())`, pgx.QueryExecModeSimpleProtocol).Scan(&addr); err != nil {
		return s, err
	}
	if addr != nil {
		s.ClientAddr = *addr
	}
	s.ProxyLikely = s.ClientAddr == "" || !localAddr(s.ClientAddr)
	var err error
	s.Pinned, s.PIDs, err = sessionPinned(ctx, c, at)
	return s, err
}

// sessionPinned compares c's backend PID across transactions. A quiet pooler
// in transaction mode would usually hand back the same server connection, so
// a second client connection to the same address holds a transaction open in
// between: in transaction mode it takes over c's server connection and c
// moves to another. It changes no setting (the simple protocol, because
// prepared statements do not survive transaction pooling).
func sessionPinned(ctx context.Context, c *pgx.Conn, at Conn) (bool, []int, error) {
	var pids []int
	pinned := true
	probe := func() error {
		var pid int
		if err := c.QueryRow(ctx, `SELECT pg_backend_pid()`, pgx.QueryExecModeSimpleProtocol).Scan(&pid); err != nil {
			return err
		}
		pids = append(pids, pid)
		if pid != pids[0] {
			pinned = false
		}
		return nil
	}
	if err := probe(); err != nil {
		return false, pids, err
	}
	if other, err := connect(ctx, at, 0); err == nil {
		if tx, err := other.Begin(ctx); err == nil {
			var pid int
			if tx.QueryRow(ctx, `SELECT pg_backend_pid()`, pgx.QueryExecModeSimpleProtocol).Scan(&pid) == nil && pid == pids[0] {
				pinned = false // another client got this connection's server session
			}
			err = probe()
			_ = tx.Rollback(ctx)
			if err != nil {
				other.Close(ctx)
				return false, pids, err
			}
		}
		other.Close(ctx)
	}
	for i := 0; i < 3; i++ {
		if err := probe(); err != nil {
			return false, pids, err
		}
	}
	return pinned, pids, nil
}

func localAddr(a string) bool {
	ip := net.ParseIP(a)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true
	}
	for _, ia := range addrs {
		if n, ok := ia.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// PassLine returns a pgpass line for c.
func (c Conn) PassLine() string {
	esc := func(s string) string { return strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(s) }
	return fmt.Sprintf("%s:%d:*:%s:%s\n", esc(c.Host), c.Port, esc(c.User), esc(c.Password))
}

// WritePassFile writes a 0600 pgpass file covering conns.
func WritePassFile(path string, conns ...Conn) error {
	var b strings.Builder
	for _, c := range conns {
		b.WriteString(c.PassLine())
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

var dbNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$-]{0,62}$`)

// ValidDBName reports whether name is allowed as a database name.
func ValidDBName(name string) bool { return dbNameRe.MatchString(name) }

// QuoteIdent quotes an identifier.
func QuoteIdent(s string) string { return pgx.Identifier{s}.Sanitize() }

// QuoteLiteral quotes a string literal.
func QuoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ParseLSN converts X/Y to a number.
func ParseLSN(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return 0, fmt.Errorf("bad LSN %q", s)
	}
	hi, err := strconv.ParseUint(parts[0], 16, 32)
	if err != nil {
		return 0, fmt.Errorf("bad LSN %q", s)
	}
	lo, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return 0, fmt.Errorf("bad LSN %q", s)
	}
	return hi<<32 | lo, nil
}

// FormatLSN formats a number as X/Y.
func FormatLSN(v uint64) string { return fmt.Sprintf("%X/%X", v>>32, v&0xFFFFFFFF) }

// LSNDiff returns a-b in bytes (0 when b > a or either is invalid).
func LSNDiff(a, b string) int64 {
	x, err1 := ParseLSN(a)
	y, err2 := ParseLSN(b)
	if err1 != nil || err2 != nil || y > x {
		return 0
	}
	return int64(x - y)
}

// LSNGreaterOrEqual reports a >= b.
func LSNGreaterOrEqual(a, b string) bool {
	x, err1 := ParseLSN(a)
	y, err2 := ParseLSN(b)
	return err1 == nil && err2 == nil && x >= y
}

// Explain turns common connection errors into plain English.
func Explain(err error) error {
	if err == nil {
		return nil
	}
	m := err.Error()
	switch {
	case strings.Contains(m, "password authentication failed"):
		return fmt.Errorf("the server rejected the username or password (%v)", trim(m))
	case strings.Contains(m, "no pg_hba.conf entry") || strings.Contains(m, "not allowed to connect"):
		return fmt.Errorf("the server does not allow connections from this droplet: add it to the cluster's trusted sources (%v)", trim(m))
	case strings.Contains(m, "connection refused"):
		return fmt.Errorf("nothing is listening at that host and port (%v)", trim(m))
	case strings.Contains(m, "no such host"):
		return fmt.Errorf("the hostname does not resolve (%v)", trim(m))
	case strings.Contains(m, "unsupported startup parameter"):
		return fmt.Errorf("a connection pooler or proxy at this address refused a connection setting (%v)", trim(m))
	case strings.Contains(m, "i/o timeout") || strings.Contains(m, "deadline exceeded") || strings.Contains(m, "timeout expired") || strings.Contains(m, "dial timeout"):
		return fmt.Errorf("the server did not answer in time: check the host, port and trusted sources (%v)", trim(m))
	case strings.Contains(m, "does not exist") && strings.Contains(m, "database"):
		return fmt.Errorf("the database does not exist (%v)", trim(m))
	}
	return err
}

func trim(s string) string {
	if len(s) > 240 {
		return s[:240] + "…"
	}
	return s
}

// ServerVersionNum returns server_version_num.
func ServerVersionNum(ctx context.Context, c *pgx.Conn) (int, error) {
	var s string
	if err := c.QueryRow(ctx, `SHOW server_version_num`).Scan(&s); err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}

// QueryStrings runs q and returns the first column as strings.
func QueryStrings(ctx context.Context, c *pgx.Conn, q string, args ...any) ([]string, error) {
	rows, err := c.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
