package orch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/store"
)

// Migration states.
const (
	MDraft          = "draft"
	MReady          = "ready"
	MQueued         = "queued"
	MRunning        = "running"
	MStreaming      = "streaming"
	MCutover        = "cutover"
	MVerifying      = "verifying"
	MCompleted      = "completed"
	MNeedsAttention = "needs_attention"
	MPaused         = "paused"
	MAborted        = "aborted"
	MCleanedUp      = "cleaned_up"
)

// Database states.
const (
	DPending         = "pending"
	DPreparing       = "preparing"
	DBaseCopy        = "base_copy"
	DCatchUp         = "catch_up"
	DInSync          = "in_sync"
	DDraining        = "draining"
	DDrained         = "drained"
	DVerified        = "verified"
	DFailed          = "failed"
	DDegraded        = "degraded"
	DStopped         = "stopped"
	DRestartRequired = "restart_required"
)

// activeDB states have (or should have) a running engine unit.
var activeDB = map[string]bool{DPreparing: true, DBaseCopy: true, DCatchUp: true, DInSync: true, DDraining: true, DDegraded: true}

// CutoverState tracks an in-progress or finished cutover.
type CutoverState struct {
	OpID            string `json:"op_id"`
	Phase           string `json:"phase"`
	StartedAt       int64  `json:"started_at"`
	WritesStoppedAt int64  `json:"writes_stopped_at,omitempty"`
	EndedAt         int64  `json:"ended_at,omitempty"`
	WritePauseMS    int64  `json:"write_pause_ms,omitempty"`
	Override        bool   `json:"override,omitempty"`
	Verdict         string `json:"verdict,omitempty"`
}

// Flags are operator and lifecycle flags; the migration state is derived
// from them and the database states.
type Flags struct {
	Started          bool          `json:"started,omitempty"`
	Paused           bool          `json:"paused,omitempty"`
	Aborted          bool          `json:"aborted,omitempty"`
	CleanedUp        bool          `json:"cleaned_up,omitempty"`
	AppsSwitched     bool          `json:"apps_switched,omitempty"`
	RolesCopied      bool          `json:"roles_copied,omitempty"`
	PreflightRunID   string        `json:"preflight_run_id,omitempty"`
	PreflightRunning bool          `json:"preflight_running,omitempty"`
	PreflightOK      bool          `json:"preflight_ok,omitempty"`
	Cutover          *CutoverState `json:"cutover,omitempty"`
	Verdict          string        `json:"verdict,omitempty"`
	VerdictAt        int64         `json:"verdict_at,omitempty"`
	VerifyRunID      string        `json:"verify_run_id,omitempty"`
	Verifying        bool          `json:"verifying,omitempty"`
	CleanupRunning   bool          `json:"cleanup_running,omitempty"`
}

// Migration is one migration definition.
type Migration struct {
	ID           string         `json:"id"`
	ShortID      string         `json:"short_id"`
	Name         string         `json:"name"`
	State        string         `json:"state"`
	Mode         string         `json:"mode"`
	Engine       string         `json:"engine"`
	SourceConnID string         `json:"source_conn_id,omitempty"`
	TargetConnID string         `json:"target_conn_id,omitempty"`
	Settings     map[string]any `json:"settings"`
	Snapshot     map[string]any `json:"settings_snapshot,omitempty"`
	Flags        Flags          `json:"flags"`
	Fixture      bool           `json:"fixture,omitempty"`
	CreatedBy    string         `json:"created_by,omitempty"`
	CreatedAt    int64          `json:"created_at"`
	UpdatedAt    int64          `json:"updated_at"`
	StartedAt    *int64         `json:"started_at,omitempty"`
}

// Connection is a stored endpoint (password in the vault).
type Connection struct {
	ID               string  `json:"id"`
	Kind             string  `json:"kind"`
	Discovery        string  `json:"discovery"`
	Host             string  `json:"host"`
	Port             int     `json:"port"`
	User             string  `json:"user"`
	DBName           string  `json:"dbname"`
	SSLMode          string  `json:"sslmode"`
	CACert           string  `json:"ca_cert,omitempty"`
	PasswordSecretID string  `json:"-"`
	HasPassword      bool    `json:"has_password"`
	StorageGB        float64 `json:"storage_gb,omitempty"`
}

// Permission is the customer permission record.
type Permission struct {
	Customer  string `json:"customer"`
	AccountID string `json:"account_id"`
	Ticket    string `json:"ticket"`
	GrantedBy string `json:"granted_by"`
	GrantedAt string `json:"granted_at"`
	Scope     string `json:"scope"`
	Notes     string `json:"notes,omitempty"`
	LockedAt  *int64 `json:"locked_at,omitempty"`
}

// Missing lists empty required fields.
func (p *Permission) Missing() []string {
	if p == nil {
		return []string{"customer", "account_id", "ticket", "granted_by", "granted_at", "scope"}
	}
	var m []string
	for k, v := range map[string]string{"customer": p.Customer, "account_id": p.AccountID, "ticket": p.Ticket, "granted_by": p.GrantedBy, "granted_at": p.GrantedAt, "scope": p.Scope} {
		if strings.TrimSpace(v) == "" {
			m = append(m, k)
		}
	}
	return m
}

// Database is one database inside a migration.
type Database struct {
	ID           string  `json:"id"`
	MigrationID  string  `json:"migration_id"`
	SourceName   string  `json:"source_name"`
	TargetName   string  `json:"target_name"`
	Include      bool    `json:"include"`
	SkipReason   string  `json:"skip_reason,omitempty"`
	State        string  `json:"state"`
	SlotName     string  `json:"slot_name"`
	OriginName   string  `json:"origin_name"`
	Plugin       string  `json:"plugin,omitempty"`
	Instance     string  `json:"instance"`
	SizeBytes    int64   `json:"size_bytes"`
	RowsEstimate int64   `json:"rows_estimate"`
	TableCount   int     `json:"table_count"`
	LastError    string  `json:"last_error,omitempty"`
	ErrorClass   string  `json:"error_class,omitempty"`
	InSyncSince  *int64  `json:"in_sync_since,omitempty"`
	BacklogBytes *int64  `json:"backlog_bytes,omitempty"`
	ReplayLSN    string  `json:"replay_lsn,omitempty"`
	WriteLSN     string  `json:"write_lsn,omitempty"`
	HBBeforeLSN  string  `json:"hb_before_lsn,omitempty"`
	HBToken      string  `json:"hb_token,omitempty"`
	EndPos       string  `json:"endpos,omitempty"`
	OriginLSN    string  `json:"origin_lsn,omitempty"`
	Verdict      string  `json:"verdict,omitempty"`
	RetryCount   int     `json:"retry_count"`
	RetryWindow  *int64  `json:"-"`
	NextRetryAt  *int64  `json:"next_retry_at,omitempty"`
	BaseCopyDone bool    `json:"base_copy_done"`
	CreatedAt    int64   `json:"created_at"`
	UpdatedAt    int64   `json:"updated_at"`
	Progress     float64 `json:"progress,omitempty"`
}

var shortSafe = regexp.MustCompile(`[^a-z0-9_]`)

// SlotNameFor builds upwell_<migration>_<db>: lowercase, at most 63 bytes.
func SlotNameFor(short, db string) string {
	base := "upwell_" + short + "_"
	part := shortSafe.ReplaceAllString(strings.ToLower(db), "_")
	name := base + part
	if len(name) > 63 || part != strings.ToLower(db) {
		h := sha256.Sum256([]byte(db))
		suffix := "_" + hex.EncodeToString(h[:])[:6]
		room := 63 - len(base) - len(suffix)
		if len(part) > room {
			part = part[:room]
		}
		name = base + part + suffix
	}
	return name
}

// InstanceFor builds the systemd instance name <migration>-<db>.
func InstanceFor(short, db string) string {
	part := regexp.MustCompile(`[^a-zA-Z0-9_.-]`).ReplaceAllString(db, "_")
	inst := short + "-" + part
	if part != db || len(inst) > 80 {
		h := sha256.Sum256([]byte(db))
		if len(part) > 60 {
			part = part[:60]
		}
		inst = short + "-" + part + "-" + hex.EncodeToString(h[:])[:6]
	}
	return inst
}

func scanJSON(raw string, v any) {
	if raw != "" {
		json.Unmarshal([]byte(raw), v)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

const migCols = `id, short_id, name, state, mode, engine, COALESCE(source_conn_id,''), COALESCE(target_conn_id,''), settings, COALESCE(settings_snapshot,''), flags, fixture, COALESCE(created_by,''), created_at, updated_at, started_at`

func scanMigration(row interface{ Scan(...any) error }) (Migration, error) {
	var m Migration
	var settingsRaw, snapRaw, flagsRaw string
	var started sql.NullInt64
	err := row.Scan(&m.ID, &m.ShortID, &m.Name, &m.State, &m.Mode, &m.Engine, &m.SourceConnID, &m.TargetConnID, &settingsRaw, &snapRaw, &flagsRaw, &m.Fixture, &m.CreatedBy, &m.CreatedAt, &m.UpdatedAt, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return m, store.ErrNotFound
	}
	if err != nil {
		return m, err
	}
	m.Settings = map[string]any{}
	scanJSON(settingsRaw, &m.Settings)
	if snapRaw != "" {
		scanJSON(snapRaw, &m.Snapshot)
	}
	scanJSON(flagsRaw, &m.Flags)
	if started.Valid {
		m.StartedAt = &started.Int64
	}
	return m, nil
}

// GetMigration loads one migration.
func (o *Orchestrator) GetMigration(ctx context.Context, id string) (Migration, error) {
	return scanMigration(o.st.DB.QueryRowContext(ctx, `SELECT `+migCols+` FROM migrations WHERE id=? OR short_id=?`, id, id))
}

// ListMigrations returns all migrations, newest first.
func (o *Orchestrator) ListMigrations(ctx context.Context) ([]Migration, error) {
	rows, err := o.st.DB.QueryContext(ctx, `SELECT `+migCols+` FROM migrations ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Migration
	for rows.Next() {
		m, err := scanMigration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (o *Orchestrator) saveFlags(ctx context.Context, id string, f Flags) error {
	_, err := o.st.DB.ExecContext(ctx, `UPDATE migrations SET flags=?, updated_at=? WHERE id=?`, toJSON(f), store.Now(), id)
	if err != nil {
		o.logf("error", "orchestrator", id, "", "", "saving migration flags failed: %v", err)
	}
	return err
}

// updateFlags applies fn to the stored flags under the migration lock.
func (o *Orchestrator) updateFlags(ctx context.Context, id string, fn func(*Flags)) (Flags, error) {
	o.flagMu.Lock()
	defer o.flagMu.Unlock()
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return Flags{}, err
	}
	fn(&m.Flags)
	return m.Flags, o.saveFlags(ctx, m.ID, m.Flags)
}

const dbCols = `id, migration_id, source_name, target_name, include, COALESCE(skip_reason,''), state, slot_name, origin_name, COALESCE(plugin,''), instance,
	COALESCE(size_bytes,0), COALESCE(rows_estimate,0), COALESCE(table_count,0), COALESCE(last_error,''), COALESCE(error_class,''), in_sync_since, backlog_bytes,
	COALESCE(replay_lsn,''), COALESCE(write_lsn,''), COALESCE(hb_before_lsn,''), COALESCE(hb_token,''), COALESCE(endpos,''), COALESCE(origin_lsn,''), COALESCE(verdict,''),
	retry_count, retry_window_start, next_retry_at, base_copy_done, created_at, updated_at`

func scanDB(row interface{ Scan(...any) error }) (Database, error) {
	var d Database
	var inSync, backlog, rw, next sql.NullInt64
	err := row.Scan(&d.ID, &d.MigrationID, &d.SourceName, &d.TargetName, &d.Include, &d.SkipReason, &d.State, &d.SlotName, &d.OriginName, &d.Plugin, &d.Instance,
		&d.SizeBytes, &d.RowsEstimate, &d.TableCount, &d.LastError, &d.ErrorClass, &inSync, &backlog,
		&d.ReplayLSN, &d.WriteLSN, &d.HBBeforeLSN, &d.HBToken, &d.EndPos, &d.OriginLSN, &d.Verdict,
		&d.RetryCount, &rw, &next, &d.BaseCopyDone, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, store.ErrNotFound
	}
	if inSync.Valid {
		d.InSyncSince = &inSync.Int64
	}
	if backlog.Valid {
		d.BacklogBytes = &backlog.Int64
	}
	if rw.Valid {
		d.RetryWindow = &rw.Int64
	}
	if next.Valid {
		d.NextRetryAt = &next.Int64
	}
	return d, err
}

// ListDatabases returns a migration's databases (included first).
func (o *Orchestrator) ListDatabases(ctx context.Context, migrationID string) ([]Database, error) {
	rows, err := o.st.DB.QueryContext(ctx, `SELECT `+dbCols+` FROM migration_databases WHERE migration_id=? ORDER BY include DESC, source_name`, migrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Database
	for rows.Next() {
		d, err := scanDB(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDatabase loads one database by id or source name.
func (o *Orchestrator) GetDatabase(ctx context.Context, migrationID, idOrName string) (Database, error) {
	return scanDB(o.st.DB.QueryRowContext(ctx, `SELECT `+dbCols+` FROM migration_databases WHERE migration_id=? AND (id=? OR source_name=?)`, migrationID, idOrName, idOrName))
}

// included returns only included databases.
func included(dbs []Database) []Database {
	var out []Database
	for _, d := range dbs {
		if d.Include {
			out = append(out, d)
		}
	}
	return out
}

// setDB updates columns of one database.
func (o *Orchestrator) setDB(ctx context.Context, id string, cols map[string]any) error {
	if len(cols) == 0 {
		return nil
	}
	var sets []string
	var args []any
	for k, v := range cols {
		sets = append(sets, k+"=?")
		args = append(args, v)
	}
	sets = append(sets, "updated_at=?")
	args = append(args, store.Now(), id)
	_, err := o.st.DB.ExecContext(ctx, `UPDATE migration_databases SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	if err != nil {
		o.logf("error", "orchestrator", "", "", "", "saving database %s state failed: %v", id, err)
	}
	return err
}

// GetConnection loads a connection.
func (o *Orchestrator) GetConnection(ctx context.Context, id string) (Connection, error) {
	var c Connection
	var ca, sec sql.NullString
	var gb sql.NullFloat64
	err := o.st.DB.QueryRowContext(ctx, `SELECT id, kind, discovery, host, port, username, dbname, sslmode, ca_cert, password_secret_id, storage_gb FROM connections WHERE id=?`, id).
		Scan(&c.ID, &c.Kind, &c.Discovery, &c.Host, &c.Port, &c.User, &c.DBName, &c.SSLMode, &ca, &sec, &gb)
	if errors.Is(err, sql.ErrNoRows) {
		return c, store.ErrNotFound
	}
	c.CACert, c.PasswordSecretID, c.StorageGB = ca.String, sec.String, gb.Float64
	c.HasPassword = sec.Valid
	return c, err
}

// PGConn resolves a connection to pg.Conn with its password.
func (o *Orchestrator) PGConn(ctx context.Context, id string) (pg.Conn, Connection, error) {
	c, err := o.GetConnection(ctx, id)
	if err != nil {
		return pg.Conn{}, c, err
	}
	pc := pg.Conn{Host: c.Host, Port: c.Port, User: c.User, DBName: c.DBName, SSLMode: c.SSLMode}
	if c.PasswordSecretID != "" {
		pw, err := o.vault.Get(ctx, c.PasswordSecretID)
		if err != nil {
			return pc, c, fmt.Errorf("the %s password cannot be read: %w", c.Kind, err)
		}
		pc.Password = pw
	}
	if c.CACert != "" {
		dir := filepath.Join(o.cfg.DataDir, "ca")
		os.MkdirAll(dir, 0o750)
		p := filepath.Join(dir, c.ID+".pem")
		if err := os.WriteFile(p, []byte(c.CACert), 0o640); err == nil {
			pc.CAFile = p
		}
	}
	return pc, c, nil
}

// Conns resolves both endpoints of a migration.
func (o *Orchestrator) Conns(ctx context.Context, m Migration) (pg.Conn, pg.Conn, error) {
	if m.SourceConnID == "" || m.TargetConnID == "" {
		return pg.Conn{}, pg.Conn{}, errors.New("source and target connections are not set yet")
	}
	s, _, err := o.PGConn(ctx, m.SourceConnID)
	if err != nil {
		return s, pg.Conn{}, err
	}
	t, _, err := o.PGConn(ctx, m.TargetConnID)
	return s, t, err
}

// GetPermission loads the permission record (nil when absent).
func (o *Orchestrator) GetPermission(ctx context.Context, migrationID string) (*Permission, error) {
	var p Permission
	var notes sql.NullString
	var locked sql.NullInt64
	err := o.st.DB.QueryRowContext(ctx, `SELECT customer, account_id, ticket, granted_by, granted_at, scope, notes, locked_at FROM permission_records WHERE migration_id=?`, migrationID).
		Scan(&p.Customer, &p.AccountID, &p.Ticket, &p.GrantedBy, &p.GrantedAt, &p.Scope, &notes, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Notes = notes.String
	if locked.Valid {
		p.LockedAt = &locked.Int64
	}
	return &p, nil
}
