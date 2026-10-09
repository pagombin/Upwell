// Package pgpack is the PostgreSQL check pack: discovery, preflight checks,
// heartbeat and verification. It and the pgcopydb engine are the only places
// that assume PostgreSQL.
package pgpack

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/pg"
)

// DiscoveredDB is one source database.
type DiscoveredDB struct {
	Name           string `json:"name"`
	SizeBytes      int64  `json:"size_bytes"`
	Tables         int    `json:"tables"`
	RowsEstimate   int64  `json:"rows_estimate"`
	Sequences      int    `json:"sequences"`
	Views          int    `json:"views"`
	Unlogged       int    `json:"unlogged"`
	LargeObjects   int64  `json:"large_objects"`
	Encoding       string `json:"encoding"`
	Collate        string `json:"collate"`
	Include        bool   `json:"include"`
	SkipReason     string `json:"skip_reason,omitempty"`
	ExistsOnTarget bool   `json:"exists_on_target"`
}

// systemDBs are never migrated.
var systemDBs = map[string]string{
	"template0": "template database",
	"template1": "template database",
	"_dodb":     "DigitalOcean internal database",
}

// Discover lists source databases with sizes and object counts.
func Discover(ctx context.Context, src, dst pg.Conn) ([]DiscoveredDB, error) {
	c, err := pg.Connect(ctx, src, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close(ctx)
	rows, err := c.Query(ctx, `SELECT datname, pg_database_size(oid), pg_encoding_to_char(encoding), datcollate
		FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname`)
	if err != nil {
		return nil, err
	}
	var out []DiscoveredDB
	for rows.Next() {
		var d DiscoveredDB
		if err := rows.Scan(&d.Name, &d.SizeBytes, &d.Encoding, &d.Collate); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
	}
	rows.Close()
	targets := map[string]bool{}
	if t, err := pg.Connect(ctx, dst, 30*time.Second); err == nil {
		names, _ := pg.QueryStrings(ctx, t, `SELECT datname FROM pg_database`)
		for _, n := range names {
			targets[n] = true
		}
		t.Close(ctx)
	}
	for i := range out {
		d := &out[i]
		d.ExistsOnTarget = targets[d.Name]
		if r, ok := systemDBs[d.Name]; ok {
			d.SkipReason = r
			continue
		}
		if !pg.ValidDBName(d.Name) {
			d.SkipReason = "name uses characters Upwell does not allow"
			continue
		}
		dc, err := pg.Connect(ctx, src.WithDB(d.Name), 30*time.Second)
		if err != nil {
			d.SkipReason = "cannot connect: " + err.Error()
			continue
		}
		dc.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind IN ('r','p') AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%' AND n.nspname <> 'upwell'),
			(SELECT COALESCE(sum(GREATEST(c.reltuples,0)),0)::bigint FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND n.nspname NOT IN ('pg_catalog','information_schema')),
			(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='S' AND n.nspname NOT IN ('pg_catalog','information_schema')),
			(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind IN ('v','m') AND n.nspname NOT IN ('pg_catalog','information_schema')),
			(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND c.relpersistence='u' AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_temp%'),
			(SELECT count(*) FROM pg_largeobject_metadata)`).Scan(&d.Tables, &d.RowsEstimate, &d.Sequences, &d.Views, &d.Unlogged, &d.LargeObjects)
		dc.Close(ctx)
		if d.Tables == 0 && d.Sequences == 0 && d.Views == 0 {
			d.SkipReason = "no user objects"
			continue
		}
		d.Include = true
	}
	return out, nil
}

// PluginChoice decides the decoding plugin for one database.
type PluginChoice struct {
	Plugin  string   `json:"plugin"`
	Reasons []string `json:"reasons,omitempty"`
	Forced  bool     `json:"forced"`
	OK      bool     `json:"ok"`
}

// ChoosePlugin checks whether pgoutput can create its publication (no
// unlogged tables, ownership of every table, CREATE on the database) and
// switches to test_decoding when it cannot, unless pgoutput is forced.
func ChoosePlugin(ctx context.Context, c *pgx.Conn, setting string) (PluginChoice, error) {
	if setting == "test_decoding" || setting == "wal2json" {
		return PluginChoice{Plugin: setting, Forced: true, OK: true}, nil
	}
	var unlogged, notOwned int
	var create bool
	err := c.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND c.relpersistence='u' AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_temp%'),
		(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind IN ('r','p') AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%' AND NOT pg_has_role(c.relowner,'USAGE')),
		has_database_privilege(current_database(),'CREATE')`).Scan(&unlogged, &notOwned, &create)
	if err != nil {
		return PluginChoice{}, err
	}
	var reasons []string
	if unlogged > 0 {
		reasons = append(reasons, fmt.Sprintf("%d unlogged table(s): a pgoutput publication cannot include them", unlogged))
	}
	if notOwned > 0 {
		reasons = append(reasons, fmt.Sprintf("%d table(s) are owned by another role: the publication needs ownership", notOwned))
	}
	if !create {
		reasons = append(reasons, "no CREATE privilege on the database: the publication cannot be created")
	}
	if len(reasons) == 0 {
		return PluginChoice{Plugin: "pgoutput", OK: true}, nil
	}
	if setting == "pgoutput" {
		return PluginChoice{Plugin: "pgoutput", Reasons: reasons, Forced: true, OK: false}, nil
	}
	return PluginChoice{Plugin: "test_decoding", Reasons: reasons, OK: true}, nil
}

// NormalizeLocale makes en_US.UTF-8, en_US.utf-8 and en_US.utf8 compare equal.
func NormalizeLocale(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	l = strings.ReplaceAll(l, "utf-8", "utf8")
	return l
}

// HeartbeatDDL creates the heartbeat schema and table (D5).
const HeartbeatDDL = `CREATE SCHEMA IF NOT EXISTS upwell;
CREATE TABLE IF NOT EXISTS upwell.heartbeat (id bigserial PRIMARY KEY, kind text NOT NULL, token text NOT NULL, written_at timestamptz NOT NULL DEFAULT now())`

// EnsureHeartbeat creates the heartbeat table on the source database.
func EnsureHeartbeat(ctx context.Context, c pg.Conn) error {
	conn, err := pg.Connect(ctx, c, 30*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, HeartbeatDDL)
	return err
}

// WriteHeartbeat writes one row and returns the source WAL position read just
// before the write (D3).
func WriteHeartbeat(ctx context.Context, c pg.Conn, kind, token string) (before string, err error) {
	conn, err := pg.Connect(ctx, c, 30*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close(ctx)
	if err := conn.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&before); err != nil {
		return "", err
	}
	_, err = conn.Exec(ctx, `INSERT INTO upwell.heartbeat(kind, token) VALUES ($1, $2)`, kind, token)
	return before, err
}

// HeartbeatLatency returns seconds between the newest periodic heartbeat on
// the source and the newest one applied on the target.
func HeartbeatLatency(ctx context.Context, src, dst *pgx.Conn) (float64, bool) {
	var s, d *time.Time
	if src.QueryRow(ctx, `SELECT max(written_at) FROM upwell.heartbeat`).Scan(&s) != nil || s == nil {
		return 0, false
	}
	if dst.QueryRow(ctx, `SELECT max(written_at) FROM upwell.heartbeat`).Scan(&d) != nil || d == nil {
		return 0, false
	}
	return s.Sub(*d).Seconds(), true
}

// HeartbeatOnTarget reports whether the token reached the target.
func HeartbeatOnTarget(ctx context.Context, c pg.Conn, token string) (bool, error) {
	conn, err := pg.Connect(ctx, c, 30*time.Second)
	if err != nil {
		return false, err
	}
	defer conn.Close(ctx)
	var n int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM upwell.heartbeat WHERE token=$1`, token).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// OriginProgress returns the target's replication origin progress.
func OriginProgress(ctx context.Context, c pg.Conn, origin string) (string, error) {
	conn, err := pg.Connect(ctx, c, 30*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close(ctx)
	var lsn *string
	err = conn.QueryRow(ctx, `SELECT pg_replication_origin_progress($1, false)::text`, origin).Scan(&lsn)
	if err != nil {
		return "", err
	}
	if lsn == nil {
		return "", nil
	}
	return *lsn, nil
}

// EnsureTargetDB creates the target database with the source's encoding and
// locale when it is missing.
func EnsureTargetDB(ctx context.Context, src, dst pg.Conn, srcDB, dstDB string) (bool, error) {
	t, err := pg.Connect(ctx, dst, 30*time.Second)
	if err != nil {
		return false, err
	}
	defer t.Close(ctx)
	var n int
	t.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname=$1`, dstDB).Scan(&n)
	if n > 0 {
		return false, nil
	}
	s, err := pg.Connect(ctx, src, 30*time.Second)
	if err != nil {
		return false, err
	}
	var enc, collate, ctype string
	err = s.QueryRow(ctx, `SELECT pg_encoding_to_char(encoding), datcollate, datctype FROM pg_database WHERE datname=$1`, srcDB).Scan(&enc, &collate, &ctype)
	s.Close(ctx)
	if err != nil {
		return false, err
	}
	// Prefer template1 so the new database inherits what the provider set up
	// there (grants included); template0 only when encoding or locale differ.
	var tEnc, tCollate, tCtype string
	t.QueryRow(ctx, `SELECT pg_encoding_to_char(encoding), datcollate, datctype FROM pg_database WHERE datname='template1'`).Scan(&tEnc, &tCollate, &tCtype)
	if tEnc == enc && NormalizeLocale(tCollate) == NormalizeLocale(collate) && NormalizeLocale(tCtype) == NormalizeLocale(ctype) {
		if _, err := t.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE template1`, pg.QuoteIdent(dstDB))); err == nil {
			return true, nil
		}
	}
	q := fmt.Sprintf(`CREATE DATABASE %s TEMPLATE template0 ENCODING %s LC_COLLATE %s LC_CTYPE %s`, pg.QuoteIdent(dstDB), pg.QuoteLiteral(enc), pg.QuoteLiteral(collate), pg.QuoteLiteral(ctype))
	if _, err := t.Exec(ctx, q); err != nil {
		// Locale names can differ between hosts; fall back to the target default.
		if _, err2 := t.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE template0 ENCODING %s`, pg.QuoteIdent(dstDB), pg.QuoteLiteral(enc))); err2 != nil {
			return false, fmt.Errorf("creating target database %s: %w", dstDB, err)
		}
	}
	return true, nil
}

// SortDiscovered orders included databases first, then by size descending.
func SortDiscovered(ds []DiscoveredDB) {
	sort.SliceStable(ds, func(i, j int) bool {
		if ds[i].Include != ds[j].Include {
			return ds[i].Include
		}
		return ds[i].SizeBytes > ds[j].SizeBytes
	})
}

// EnsureExtensions creates, in the target database, every extension installed
// in the source database (the engine runs with --skip-extensions, because a
// non-superuser cannot restore extension objects). Each is created in the same
// schema when that schema exists on the target; extensions whose schema does
// not exist yet are reported, not guessed. It returns what it created.
func EnsureExtensions(ctx context.Context, src, dst pg.Conn) (created, skipped []string, err error) {
	sc, err := pg.Connect(ctx, src, 30*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("source: %w", err)
	}
	defer sc.Close(ctx)
	rows, err := sc.Query(ctx, `SELECT e.extname, n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname <> 'plpgsql' ORDER BY e.oid`)
	if err != nil {
		return nil, nil, err
	}
	type ext struct{ name, schema string }
	var exts []ext
	for rows.Next() {
		var x ext
		if rows.Scan(&x.name, &x.schema) == nil {
			exts = append(exts, x)
		}
	}
	rows.Close()
	if len(exts) == 0 {
		return nil, nil, nil
	}
	dc, err := pg.Connect(ctx, dst, 30*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("target: %w", err)
	}
	defer dc.Close(ctx)
	for _, x := range exts {
		var present, schemaOK bool
		_ = dc.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1), EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $2)`, x.name, x.schema).Scan(&present, &schemaOK)
		if present {
			continue
		}
		if !schemaOK {
			skipped = append(skipped, fmt.Sprintf("%s (schema %s does not exist on the target yet)", x.name, x.schema))
			continue
		}
		if _, err := dc.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+pg.QuoteIdent(x.name)+" SCHEMA "+pg.QuoteIdent(x.schema)); err != nil {
			return created, skipped, fmt.Errorf("creating extension %s on the target: %w", x.name, err)
		}
		created = append(created, x.name)
	}
	return created, skipped, nil
}
