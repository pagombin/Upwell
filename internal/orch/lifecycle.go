package orch

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/pgpack"
	"github.com/pagombin/upwell/internal/settings"
	"github.com/pagombin/upwell/internal/store"
)

// UserError is shown to the operator as is (HTTP 400/409).
type UserError struct {
	Code        string
	Msg         string
	Remediation string
}

func (e *UserError) Error() string { return e.Msg }

func uerr(code, msg, fix string) error { return &UserError{Code: code, Msg: msg, Remediation: fix} }

func newShortID() string {
	const alpha = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 5)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alpha))))
		b[i] = alpha[n.Int64()]
	}
	return "m" + string(b)
}

// CreateMigration makes a draft.
func (o *Orchestrator) CreateMigration(ctx context.Context, a audit.Actor, name string) (Migration, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 {
		return Migration{}, uerr("invalid_name", "Give the migration a name of 1 to 120 characters.", "")
	}
	m := Migration{ID: store.NewID(), ShortID: newShortID(), Name: name, State: MDraft, Mode: "online", Engine: o.eng.Name(), Settings: map[string]any{}, CreatedBy: a.Username, CreatedAt: store.Now()}
	m.UpdatedAt = m.CreatedAt
	_, err := o.st.DB.ExecContext(ctx, `INSERT INTO migrations(id,short_id,name,state,mode,engine,settings,flags,created_by,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.ShortID, m.Name, m.State, m.Mode, m.Engine, "{}", "{}", m.CreatedBy, m.CreatedAt, m.UpdatedAt)
	if err != nil {
		return m, err
	}
	o.audit.Append(ctx, a, "migration.create", m.ID, nil, map[string]any{"name": name, "short_id": m.ShortID})
	o.Event(ctx, m.ID, "", "created", "info", fmt.Sprintf("Migration %q created by %s", name, a.Username), nil)
	return m, nil
}

func (o *Orchestrator) requireEditable(m Migration) error {
	if m.Flags.Started {
		return uerr("frozen", "This migration has started; its settings and databases are frozen.", "Abort and clean up, then create a new migration to change them.")
	}
	return nil
}

// UpdateMigration changes the name and per-migration setting overrides.
// Editing sends a Ready migration back to Draft until preflight runs again.
func (o *Orchestrator) UpdateMigration(ctx context.Context, a audit.Actor, id string, name *string, overrides map[string]any) (Migration, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return m, err
	}
	if err := o.requireEditable(m); err != nil {
		return m, err
	}
	before := map[string]any{"name": m.Name, "settings": m.Settings}
	if name != nil {
		n := strings.TrimSpace(*name)
		if n == "" || len(n) > 120 {
			return m, uerr("invalid_name", "Give the migration a name of 1 to 120 characters.", "")
		}
		m.Name = n
	}
	if overrides != nil {
		g, _ := o.settings.Global(ctx)
		if _, err := settings.ForMigration(g, overrides); err != nil {
			return m, uerr("invalid_setting", err.Error(), "")
		}
		m.Settings = overrides
	}
	o.st.DB.ExecContext(ctx, `UPDATE migrations SET name=?, settings=?, updated_at=? WHERE id=?`, m.Name, toJSON(m.Settings), store.Now(), m.ID)
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.PreflightOK = false })
	o.audit.Append(ctx, a, "migration.update", m.ID, before, map[string]any{"name": m.Name, "settings": m.Settings})
	o.refreshState(ctx, m.ID)
	return o.GetMigration(ctx, m.ID)
}

// ConnInput is a manual connection.
type ConnInput struct {
	Host      string  `json:"host"`
	Port      int     `json:"port"`
	User      string  `json:"user"`
	Password  string  `json:"password"`
	DBName    string  `json:"dbname"`
	SSLMode   string  `json:"sslmode"`
	CACert    string  `json:"ca_cert"`
	StorageGB float64 `json:"storage_gb"`
}

// Validate checks a connection input.
func (c *ConnInput) Validate() error {
	c.Host = strings.TrimSpace(c.Host)
	if c.Host == "" || len(c.Host) > 253 || strings.ContainsAny(c.Host, " /@") {
		return uerr("invalid_host", "Enter a hostname or IP address.", "")
	}
	if c.Port == 0 {
		c.Port = 25060
	}
	if c.Port < 1 || c.Port > 65535 {
		return uerr("invalid_port", "The port must be between 1 and 65535.", "")
	}
	if c.User == "" || len(c.User) > 63 {
		return uerr("invalid_user", "Enter the database user, usually doadmin.", "")
	}
	if c.DBName == "" {
		c.DBName = "defaultdb"
	}
	if c.SSLMode == "" {
		c.SSLMode = "require"
	}
	switch c.SSLMode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		return uerr("invalid_sslmode", "SSL mode must be one of disable, allow, prefer, require, verify-ca or verify-full.", "")
	}
	if c.StorageGB < 0 || c.StorageGB > 1e6 {
		return uerr("invalid_storage", "Storage size must be between 0 and 1,000,000 GB.", "")
	}
	return nil
}

// PG returns the input as pg.Conn.
func (c ConnInput) PG() pg.Conn {
	return pg.Conn{Host: c.Host, Port: c.Port, User: c.User, Password: c.Password, DBName: c.DBName, SSLMode: c.SSLMode}
}

// SetConnection stores the source or target connection. The permission
// record must exist before any connection is attempted (spec: security).
func (o *Orchestrator) SetConnection(ctx context.Context, a audit.Actor, id, kind string, in ConnInput) (Connection, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return Connection{}, err
	}
	if err := o.requireEditable(m); err != nil {
		return Connection{}, err
	}
	if err := in.Validate(); err != nil {
		return Connection{}, err
	}
	existingID := m.SourceConnID
	if kind == "target" {
		existingID = m.TargetConnID
	}
	var secretID string
	if in.Password != "" {
		secretID, err = o.vault.Put(ctx, kind+" password for "+m.ShortID, in.Password)
		if err != nil {
			return Connection{}, err
		}
	} else if existingID != "" {
		if old, err := o.GetConnection(ctx, existingID); err == nil {
			secretID = old.PasswordSecretID
		}
	}
	cid := store.NewID()
	_, err = o.st.DB.ExecContext(ctx, `INSERT INTO connections(id,kind,discovery,host,port,username,dbname,sslmode,ca_cert,password_secret_id,storage_gb,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		cid, kind, "manual", in.Host, in.Port, in.User, in.DBName, in.SSLMode, store.NullString(in.CACert), store.NullString(secretID), in.StorageGB, store.Now())
	if err != nil {
		return Connection{}, err
	}
	col := "source_conn_id"
	if kind == "target" {
		col = "target_conn_id"
	}
	o.st.DB.ExecContext(ctx, `UPDATE migrations SET `+col+`=?, updated_at=? WHERE id=?`, cid, store.Now(), m.ID)
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.PreflightOK = false })
	o.audit.Append(ctx, a, "migration.connection."+kind, m.ID, nil, map[string]any{"host": in.Host, "port": in.Port, "user": in.User, "dbname": in.DBName, "sslmode": in.SSLMode, "password_changed": in.Password != ""})
	o.refreshState(ctx, m.ID)
	return o.GetConnection(ctx, cid)
}

// ProbeResult is one connection test line.
type ProbeResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// TestConnection runs connectivity, version and privilege probes.
func TestConnection(ctx context.Context, c pg.Conn, kind string) []ProbeResult {
	var out []ProbeResult
	conn, err := pg.Connect(ctx, c, 15*time.Second)
	if err != nil {
		return append(out, ProbeResult{"connect", false, err.Error()})
	}
	defer conn.Close(ctx)
	var ver string
	conn.QueryRow(ctx, `SHOW server_version`).Scan(&ver)
	out = append(out, ProbeResult{"connect", true, "Connected as " + c.User + "."}, ProbeResult{"version", true, "PostgreSQL " + ver + "."})
	var ssl bool
	conn.QueryRow(ctx, `SELECT COALESCE((SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()), false)`).Scan(&ssl)
	if ssl {
		out = append(out, ProbeResult{"tls", true, "The connection is encrypted."})
	} else {
		out = append(out, ProbeResult{"tls", c.SSLMode == "disable", "The connection is not encrypted."})
	}
	var repl, super, createdb bool
	conn.QueryRow(ctx, `SELECT rolreplication, rolsuper, rolcreatedb FROM pg_roles WHERE rolname=current_user`).Scan(&repl, &super, &createdb)
	if kind == "source" {
		var wl string
		conn.QueryRow(ctx, `SHOW wal_level`).Scan(&wl)
		out = append(out, ProbeResult{"replication", repl || super, map[bool]string{true: "The user has the REPLICATION privilege.", false: "The user lacks REPLICATION."}[repl || super]},
			ProbeResult{"wal_level", wl == "logical", "wal_level is " + wl + "."})
	} else {
		ver, _ := pg.ServerVersionNum(ctx, conn)
		ap := pgpack.TargetApplyPrivileges(ctx, conn, ver)
		msg := "The user can apply replicated changes."
		if !ap.OK {
			msg = "The user cannot " + strings.Join(ap.Missing, " or ") + "."
		}
		out = append(out, ProbeResult{"apply_privileges", ap.OK, msg}, ProbeResult{"createdb", createdb || super, map[bool]string{true: "The user can create databases.", false: "The user cannot create databases."}[createdb || super]})
	}
	return out
}

// SetPermission stores the customer permission record. It is immutable once
// a connection to a customer cluster has been made (locked).
func (o *Orchestrator) SetPermission(ctx context.Context, a audit.Actor, id string, p Permission) (*Permission, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, err
	}
	old, _ := o.GetPermission(ctx, m.ID)
	if old != nil && old.LockedAt != nil {
		return nil, uerr("permission_locked", "The permission record is locked because Upwell has already connected to the customer's clusters.", "Create a new migration if the permission changed.")
	}
	for _, f := range []*string{&p.Customer, &p.AccountID, &p.Ticket, &p.GrantedBy, &p.GrantedAt, &p.Scope, &p.Notes} {
		*f = strings.TrimSpace(*f)
		if len(*f) > 2000 {
			return nil, uerr("too_long", "Permission fields are limited to 2,000 characters.", "")
		}
	}
	now := store.Now()
	_, err = o.st.DB.ExecContext(ctx, `INSERT INTO permission_records(id,migration_id,customer,account_id,ticket,granted_by,granted_at,scope,notes,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(migration_id) DO UPDATE SET customer=excluded.customer, account_id=excluded.account_id, ticket=excluded.ticket, granted_by=excluded.granted_by, granted_at=excluded.granted_at, scope=excluded.scope, notes=excluded.notes, updated_at=excluded.updated_at`,
		store.NewID(), m.ID, p.Customer, p.AccountID, p.Ticket, p.GrantedBy, p.GrantedAt, p.Scope, store.NullString(p.Notes), now, now)
	if err != nil {
		return nil, err
	}
	o.audit.Append(ctx, a, "migration.permission", m.ID, old, p)
	o.refreshState(ctx, m.ID)
	return o.GetPermission(ctx, m.ID)
}

// requirePermission refuses cluster access without a complete record, and
// locks the record on first access.
func (o *Orchestrator) requirePermission(ctx context.Context, m Migration) error {
	p, err := o.GetPermission(ctx, m.ID)
	if err != nil {
		return err
	}
	if p == nil || len(p.Missing()) > 0 {
		return uerr("permission_required", "Record the customer's permission before Upwell connects to their clusters.", "Fill in step 1 of the migration: customer, account, ticket, who granted permission, when, and the scope.")
	}
	if p.LockedAt == nil {
		o.st.DB.ExecContext(ctx, `UPDATE permission_records SET locked_at=? WHERE migration_id=? AND locked_at IS NULL`, store.Now(), m.ID)
	}
	return nil
}

// TestMigrationConnection probes the stored source or target.
func (o *Orchestrator) TestMigrationConnection(ctx context.Context, id, kind string) ([]ProbeResult, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := o.requirePermission(ctx, m); err != nil {
		return nil, err
	}
	cid := m.SourceConnID
	if kind == "target" {
		cid = m.TargetConnID
	}
	if cid == "" {
		return nil, uerr("no_connection", "Enter the "+kind+" connection details first.", "")
	}
	c, _, err := o.PGConn(ctx, cid)
	if err != nil {
		return nil, err
	}
	return TestConnection(ctx, c, kind), nil
}

// Discover lists source databases and stores the selection candidates.
func (o *Orchestrator) Discover(ctx context.Context, a audit.Actor, id string) ([]Database, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := o.requireEditable(m); err != nil {
		return nil, err
	}
	if err := o.requirePermission(ctx, m); err != nil {
		return nil, err
	}
	src, dst, err := o.Conns(ctx, m)
	if err != nil {
		return nil, uerr("no_connection", err.Error(), "Enter both connections first.")
	}
	found, err := pgpack.Discover(ctx, src, dst)
	if err != nil {
		return nil, err
	}
	existing, _ := o.ListDatabases(ctx, m.ID)
	prev := map[string]Database{}
	for _, d := range existing {
		prev[d.SourceName] = d
	}
	seen := map[string]bool{}
	now := store.Now()
	for _, f := range found {
		seen[f.Name] = true
		inc := f.Include
		target := f.Name
		if p, ok := prev[f.Name]; ok {
			inc = p.Include && f.SkipReason == ""
			o.st.DB.ExecContext(ctx, `UPDATE migration_databases SET skip_reason=?, size_bytes=?, rows_estimate=?, table_count=?, include=?, updated_at=? WHERE id=?`,
				store.NullString(f.SkipReason), f.SizeBytes, f.RowsEstimate, f.Tables, inc, now, p.ID)
			continue
		}
		o.st.DB.ExecContext(ctx, `INSERT INTO migration_databases(id,migration_id,source_name,target_name,include,skip_reason,state,slot_name,origin_name,instance,size_bytes,rows_estimate,table_count,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, store.NewID(), m.ID, f.Name, target, inc, store.NullString(f.SkipReason), DPending,
			SlotNameFor(m.ShortID, f.Name), SlotNameFor(m.ShortID, f.Name), InstanceFor(m.ShortID, f.Name), f.SizeBytes, f.RowsEstimate, f.Tables, now, now)
	}
	for name, d := range prev {
		if !seen[name] {
			o.st.DB.ExecContext(ctx, `UPDATE migration_databases SET include=0, skip_reason='no longer on the source', updated_at=? WHERE id=?`, now, d.ID)
		}
	}
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.PreflightOK = false })
	o.audit.Append(ctx, a, "migration.discover", m.ID, nil, map[string]any{"found": len(found)})
	o.refreshState(ctx, m.ID)
	return o.ListDatabases(ctx, m.ID)
}

// DBSelection chooses databases and target names.
type DBSelection struct {
	SourceName string `json:"source_name"`
	TargetName string `json:"target_name"`
	Include    bool   `json:"include"`
}

// SetDatabases updates the selection.
func (o *Orchestrator) SetDatabases(ctx context.Context, a audit.Actor, id string, sel []DBSelection) ([]Database, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := o.requireEditable(m); err != nil {
		return nil, err
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	byName := map[string]Database{}
	for _, d := range dbs {
		byName[d.SourceName] = d
	}
	targets := map[string]bool{}
	for _, s := range sel {
		d, ok := byName[s.SourceName]
		if !ok {
			return nil, uerr("unknown_database", "Database "+s.SourceName+" was not discovered on the source.", "Run discovery again.")
		}
		if s.TargetName == "" {
			s.TargetName = s.SourceName
		}
		if !pg.ValidDBName(s.TargetName) {
			return nil, uerr("invalid_target_name", "Target name "+s.TargetName+" is not allowed: use letters, digits, _ $ and -, at most 63 characters, not starting with a digit.", "")
		}
		if s.Include && d.SkipReason != "" {
			return nil, uerr("skipped_database", s.SourceName+" cannot be migrated: "+d.SkipReason+".", "")
		}
		if s.Include {
			if targets[s.TargetName] {
				return nil, uerr("duplicate_target", "Two databases would be copied into "+s.TargetName+".", "Give each database its own target name.")
			}
			targets[s.TargetName] = true
		}
		o.st.DB.ExecContext(ctx, `UPDATE migration_databases SET include=?, target_name=?, updated_at=? WHERE id=?`, s.Include, s.TargetName, store.Now(), d.ID)
	}
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.PreflightOK = false })
	o.audit.Append(ctx, a, "migration.databases", m.ID, nil, sel)
	o.refreshState(ctx, m.ID)
	return o.ListDatabases(ctx, m.ID)
}

// DeleteMigration purges a migration that is not running.
func (o *Orchestrator) DeleteMigration(ctx context.Context, a audit.Actor, id string) error {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return err
	}
	if m.Flags.Started && !m.Flags.CleanedUp {
		return uerr("not_cleaned", "Clean up this migration before deleting it, so no slot or origin is left on the customer's clusters.", "Abort, then Clean up.")
	}
	for _, cid := range []string{m.SourceConnID, m.TargetConnID} {
		if cid != "" {
			if c, err := o.GetConnection(ctx, cid); err == nil && c.PasswordSecretID != "" {
				_ = o.vault.Delete(ctx, c.PasswordSecretID)
			}
		}
	}
	o.st.DB.ExecContext(ctx, `DELETE FROM migrations WHERE id=?`, m.ID)
	o.st.DB.ExecContext(ctx, `DELETE FROM logs WHERE migration_id=?`, m.ID)
	o.st.DB.ExecContext(ctx, `DELETE FROM events WHERE migration_id=?`, m.ID)
	o.st.DB.ExecContext(ctx, `DELETE FROM metric_samples WHERE series_id IN (SELECT id FROM metric_series WHERE migration_id=?)`, m.ID)
	o.st.DB.ExecContext(ctx, `DELETE FROM metric_rollups_1m WHERE series_id IN (SELECT id FROM metric_series WHERE migration_id=?)`, m.ID)
	o.st.DB.ExecContext(ctx, `DELETE FROM metric_series WHERE migration_id=?`, m.ID)
	o.st.DB.ExecContext(ctx, `DELETE FROM alerts WHERE migration_id=?`, m.ID)
	os.RemoveAll(filepath.Join(o.cfg.RunsDir(), m.ShortID))
	o.audit.Append(ctx, a, "migration.delete", m.ID, map[string]any{"name": m.Name}, nil)
	return nil
}

// ---------- start, stop, resume

// StartOptions are the operator's confirmations at start.
type StartOptions struct {
	WarningsReviewed bool `json:"warnings_reviewed"`
}

// Start freezes the settings and begins the migration. The scheduler launches
// databases up to max_concurrent_base_copies.
func (o *Orchestrator) Start(ctx context.Context, a audit.Actor, id string, opts StartOptions) (Migration, error) {
	unlock := o.lock(id)
	defer unlock()
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return m, err
	}
	if m.Flags.Started {
		return m, uerr("already_started", "This migration has already started.", "")
	}
	if m.Fixture {
		return m, uerr("fixture", "This is a demonstration fixture and cannot be started.", "")
	}
	if err := o.requirePermission(ctx, m); err != nil {
		return m, err
	}
	if !o.testHooks.SkipPreflightGate {
		run, results, err := o.LatestPreflight(ctx, m.ID)
		if err != nil || run == nil {
			return m, uerr("preflight_required", "Run preflight before starting.", "")
		}
		if run.State != "done" {
			return m, uerr("preflight_running", "Preflight is still running.", "Wait for it to finish.")
		}
		if !o.preflightStillValid(ctx, m) {
			return m, uerr("preflight_stale", "The migration changed after the last preflight.", "Run preflight again.")
		}
		var blocking, warnings int
		for _, r := range results {
			if r.Blocking() {
				blocking++
			}
			if r.Level == "warning" {
				warnings++
			}
		}
		if blocking > 0 {
			return m, uerr("preflight_blocked", fmt.Sprintf("%d preflight blocker(s) are unresolved.", blocking), "Fix them, or accept the acceptable ones with a reason, and run preflight again.")
		}
		if warnings > 0 && !opts.WarningsReviewed {
			return m, uerr("warnings_not_reviewed", fmt.Sprintf("Preflight raised %d warning(s).", warnings), "Tick \"I have reviewed the warnings\" to start.")
		}
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	if len(included(dbs)) == 0 {
		return m, uerr("no_databases", "No databases are selected.", "")
	}
	snap := o.Settings(ctx, m)
	now := store.Now()
	o.st.DB.ExecContext(ctx, `UPDATE migrations SET settings_snapshot=?, started_at=?, frozen_at=?, updated_at=? WHERE id=?`, toJSON(snap), now, now, now, m.ID)
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.Started = true; f.Paused = false })
	o.audit.Append(ctx, a, "migration.start", m.ID, nil, map[string]any{"settings_snapshot": snap, "warnings_reviewed": opts.WarningsReviewed})
	o.Event(ctx, m.ID, "", "started", "info", fmt.Sprintf("Migration started by %s with %d database(s)", a.Username, len(included(dbs))), nil)
	o.refreshState(ctx, m.ID)
	go o.Tick(context.Background())
	return o.GetMigration(ctx, m.ID)
}

// dbSpec builds the engine spec for one database.
func (o *Orchestrator) dbSpec(ctx context.Context, m Migration, d Database) (engine.DatabaseSpec, error) {
	src, dst, err := o.Conns(ctx, m)
	if err != nil {
		return engine.DatabaseSpec{}, err
	}
	v := o.Settings(ctx, m)
	conc := max(1, min(len(o.mustIncluded(ctx, m.ID)), v.Int("max_concurrent_base_copies")))
	tj, ij := settings.Jobs(v, conc)
	spec := engine.DatabaseSpec{MigrationID: m.ID, Source: d.SourceName, Target: d.TargetName, SourceConn: src, TargetConn: dst,
		Instance: d.Instance, SlotName: d.SlotName, OriginName: d.OriginName, Plugin: d.Plugin,
		RunDir: filepath.Join(o.cfg.RunsDir(), d.Instance), TableJobs: tj, IndexJobs: ij, SplitLarger: v.Str("split_tables_larger_than"),
		SyncCommitOff: v.Bool("target_synchronous_commit_off"), MaintWorkMem: v.Str("target_maintenance_work_mem")}
	if o.testHooks.ExtraEngineArgs != nil {
		spec.ExtraArgs = o.testHooks.ExtraEngineArgs(d.SourceName)
	}
	return spec, nil
}

func (o *Orchestrator) mustIncluded(ctx context.Context, migID string) []Database {
	dbs, _ := o.ListDatabases(ctx, migID)
	return included(dbs)
}

// launch prepares and starts a database's first attempt (or a restart from zero).
func (o *Orchestrator) launch(ctx context.Context, m Migration, d Database) error {
	done := o.intent(ctx, "", "launch", d.ID, map[string]any{"database": d.SourceName})
	o.setDB(ctx, d.ID, map[string]any{"state": DPreparing, "last_error": nil, "error_class": nil, "next_retry_at": nil, "base_copy_done": 0})
	o.Event(ctx, m.ID, d.SourceName, "db_state", "info", "Preparing "+d.SourceName, nil)
	spec, err := o.dbSpec(ctx, m, d)
	if err != nil {
		done("failed: " + err.Error())
		return o.dbFailed(ctx, m, d, engine.FailPermanent, err.Error())
	}
	v := o.Settings(ctx, m)
	src, dst := spec.SourceConn, spec.TargetConn
	// Roles once per migration, best effort.
	if v.Bool("copy_roles") && !m.Flags.RolesCopied {
		if msg, err := o.eng.CopyRoles(ctx, src, dst, filepath.Join(o.cfg.RunsDir(), m.ShortID+"-roles")); err != nil {
			o.Event(ctx, m.ID, "", "roles", "warning", "Roles were not copied: "+trimErr(err)+". Create the application roles on the target before switching applications.", nil)
		} else {
			o.Event(ctx, m.ID, "", "roles", "info", msg, nil)
		}
		o.updateFlags(ctx, m.ID, func(f *Flags) { f.RolesCopied = true })
	}
	if v.Bool("create_target_databases") {
		created, err := pgpack.EnsureTargetDB(ctx, src, dst, d.SourceName, d.TargetName)
		if err != nil {
			done("failed: " + err.Error())
			return o.dbFailed(ctx, m, d, engine.FailPermanent, err.Error())
		}
		if created {
			o.Event(ctx, m.ID, d.SourceName, "target_db", "info", "Created target database "+d.TargetName, nil)
		}
	}
	// Extensions first: tables may use their types, functions and operators.
	if made, skipped, err := pgpack.EnsureExtensions(ctx, src.WithDB(d.SourceName), dst.WithDB(d.TargetName)); err != nil {
		done("failed: " + err.Error())
		return o.dbFailed(ctx, m, d, engine.FailPermanent, err.Error())
	} else {
		if len(made) > 0 {
			o.Event(ctx, m.ID, d.SourceName, "extensions", "info", "Created extensions on the target: "+strings.Join(made, ", "), nil)
		}
		if len(skipped) > 0 {
			o.Event(ctx, m.ID, d.SourceName, "extensions", "warning", "Extensions not created on the target: "+strings.Join(skipped, "; "), nil)
		}
	}
	// The data-safety gate starts here: re-check the apply privileges inside the
	// database the engine will apply into (function grants are per database).
	if tc, err := pg.Connect(ctx, dst.WithDB(d.TargetName), 30*time.Second); err == nil {
		ver, _ := pg.ServerVersionNum(ctx, tc)
		ap := pgpack.TargetApplyPrivileges(ctx, tc, ver)
		tc.Close(ctx)
		if !ap.OK {
			done("failed: apply privileges")
			return o.dbFailed(ctx, m, d, engine.FailPermanent, "On target database "+d.TargetName+" the user cannot "+strings.Join(ap.Missing, " or ")+
				". pgcopydb would apply no changes and still report success (finding F2), so Upwell will not start it. Grant the privileges in that database and restart it from zero.")
		}
	}
	if v.Bool("heartbeat") {
		if err := pgpack.EnsureHeartbeat(ctx, src.WithDB(d.SourceName)); err != nil {
			done("failed: " + err.Error())
			return o.dbFailed(ctx, m, d, engine.FailPermanent, "creating the heartbeat table: "+err.Error())
		}
	}
	// Decoding plugin.
	if conn, err := pg.Connect(ctx, src.WithDB(d.SourceName), 30*time.Second); err == nil {
		choice, err := pgpack.ChoosePlugin(ctx, conn, v.Str("decoding_plugin"))
		conn.Close(ctx)
		if err == nil {
			if !choice.OK {
				done("failed: plugin")
				return o.dbFailed(ctx, m, d, engine.FailPermanent, "pgoutput is forced but cannot work: "+strings.Join(choice.Reasons, "; "))
			}
			spec.Plugin = choice.Plugin
			if choice.Plugin == "pgoutput" && v.Str("decoding_plugin") == "auto" {
				spec.Plugin = "" // engine default
			}
			o.setDB(ctx, d.ID, map[string]any{"plugin": choice.Plugin})
			if len(choice.Reasons) > 0 {
				o.Event(ctx, m.ID, d.SourceName, "plugin", "warning", d.SourceName+" uses test_decoding: "+strings.Join(choice.Reasons, "; "), nil)
			}
		}
	}
	os.RemoveAll(spec.RunDir)
	plan, err := o.eng.Plan(spec, false)
	if err != nil {
		done("failed: " + err.Error())
		return o.dbFailed(ctx, m, d, engine.FailPermanent, err.Error())
	}
	if err := o.startAttempt(ctx, m, d, spec, plan, "start"); err != nil {
		done("failed: " + err.Error())
		return o.dbFailed(ctx, m, d, engine.FailTransient, err.Error())
	}
	o.setDB(ctx, d.ID, map[string]any{"state": DBaseCopy, "in_sync_since": nil})
	o.Event(ctx, m.ID, d.SourceName, "db_state", "info", "Base copy started for "+d.SourceName, map[string]any{"unit": plan.Unit})
	done("started " + plan.Unit)
	return nil
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func (o *Orchestrator) startAttempt(ctx context.Context, m Migration, d Database, spec engine.DatabaseSpec, plan engine.RunPlan, kind string) error {
	if st, _ := o.run.Status(ctx, d.Instance); st.Active {
		return fmt.Errorf("unit %s is still running", plan.Unit)
	}
	if err := os.MkdirAll(plan.RunDir, 0o750); err != nil {
		return err
	}
	var n int
	o.st.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(number),0)+1 FROM attempts WHERE database_id=?`, d.ID).Scan(&n)
	var off int64
	if fi, err := os.Stat(plan.LogFile); err == nil {
		off = fi.Size()
	}
	attemptID := store.NewID()
	o.st.DB.ExecContext(ctx, `INSERT INTO attempts(id,database_id,number,kind,unit_name,command,started_at,log_offset) VALUES (?,?,?,?,?,?,?,?)`,
		attemptID, d.ID, n, kind, plan.Unit, plan.Display, store.Now(), off)
	o.logf("debug", "orchestrator", m.ID, d.SourceName, "", "launching attempt %d: %s", n, plan.Display)
	if err := o.eng.Start(ctx, plan, spec); err != nil {
		o.st.DB.ExecContext(ctx, `UPDATE attempts SET ended_at=?, end_reason=? WHERE id=?`, store.Now(), "failed to start: "+err.Error(), attemptID)
		return err
	}
	o.tails.attach(m.ID, d.SourceName, plan.LogFile, n)
	return nil
}

func (o *Orchestrator) endAttempt(ctx context.Context, d Database, exit *int, reason string) {
	var code any
	if exit != nil {
		code = *exit
	}
	o.st.DB.ExecContext(ctx, `UPDATE attempts SET ended_at=?, exit_code=?, end_reason=? WHERE database_id=? AND ended_at IS NULL`, store.Now(), code, reason, d.ID)
}

// resumeDB relaunches a database whose base copy finished, after confirming
// the old unit is gone and the slot is released.
func (o *Orchestrator) resumeDB(ctx context.Context, m Migration, d Database, why string) error {
	if !d.BaseCopyDone {
		return uerr("resume_unsafe", "Resuming during the base copy cannot guarantee a consistent copy.", "Restart this database from zero instead.")
	}
	done := o.intent(ctx, "", "resume", d.ID, map[string]any{"database": d.SourceName, "why": why})
	spec, err := o.dbSpec(ctx, m, d)
	if err != nil {
		done("failed: " + err.Error())
		return err
	}
	plan, err := o.eng.Plan(spec, true)
	if err != nil {
		done("failed: " + err.Error())
		return err
	}
	if st, _ := o.run.Status(ctx, d.Instance); st.Active {
		if _, err := o.eng.Stop(ctx, plan, spec); err != nil {
			done("failed: " + err.Error())
			return err
		}
	}
	if _, _, err := o.eng.ReleaseSlot(ctx, spec); err != nil {
		done("failed: " + err.Error())
		return err
	}
	if err := o.diskOK(ctx); err != nil {
		done("failed: " + err.Error())
		return err
	}
	if err := o.startAttempt(ctx, m, d, spec, plan, "resume"); err != nil {
		done("failed: " + err.Error())
		return err
	}
	state := DCatchUp
	if d.State == DDraining {
		state = DDraining
	}
	o.setDB(ctx, d.ID, map[string]any{"state": state, "next_retry_at": nil, "last_error": nil, "error_class": nil})
	o.Event(ctx, m.ID, d.SourceName, "resume", "info", fmt.Sprintf("Resumed %s (%s)", d.SourceName, why), nil)
	done("resumed")
	return nil
}

// diskOK checks the work volume before a relaunch.
func (o *Orchestrator) diskOK(ctx context.Context) error {
	pct, _, err := diskUsage(o.cfg.DataDir)
	if err != nil {
		return nil
	}
	limit := float64(o.globalSettings(ctx).Int("disk_guard_stop_pct"))
	if limit == 0 {
		limit = 95
	}
	if pct >= limit {
		return fmt.Errorf("the work volume is %.0f%% full, at or above the %.0f%% guard", pct, limit)
	}
	return nil
}

// stopUnit stops one database's unit and waits for its slot.
func (o *Orchestrator) stopUnit(ctx context.Context, m Migration, d Database) (engine.StopResult, error) {
	spec, err := o.dbSpec(ctx, m, d)
	if err != nil {
		return engine.StopResult{}, err
	}
	plan, _ := o.eng.Plan(spec, d.BaseCopyDone)
	res, err := o.eng.Stop(ctx, plan, spec)
	if err == nil {
		o.logf("info", "orchestrator", m.ID, d.SourceName, "", "unit stopped in %s; slot released after %s (walsender terminated: %v)", res.Duration.Round(time.Millisecond), res.SlotReleaseWaited.Round(time.Millisecond), res.WalsenderKilled)
	}
	return res, err
}

// mustStop stops a unit the supervisor decided to stop and logs a failure:
// a unit that keeps running after such a decision needs an operator.
func (o *Orchestrator) mustStop(ctx context.Context, m Migration, d Database, why string) {
	if _, err := o.stopUnit(ctx, m, d); err != nil {
		o.logf("error", "orchestrator", m.ID, d.SourceName, "", "stopping %s (%s) failed: %v", d.SourceName, why, err)
		o.Event(ctx, m.ID, d.SourceName, "stop_failed", "critical", fmt.Sprintf("Upwell could not stop the engine of %s (%s): %v", d.SourceName, why, err), nil)
	}
}

// StopDatabase stops one database; slot and state are kept.
func (o *Orchestrator) StopDatabase(ctx context.Context, a audit.Actor, id, db string) (Database, error) {
	unlock := o.lock(id)
	defer unlock()
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return Database{}, err
	}
	d, err := o.GetDatabase(ctx, m.ID, db)
	if err != nil {
		return d, err
	}
	if !activeDB[d.State] && !(d.State == DStopped && d.NextRetryAt != nil) {
		return d, uerr("not_running", d.SourceName+" is not running.", "")
	}
	if m.Flags.Cutover != nil && m.Flags.Cutover.EndedAt == 0 {
		return d, uerr("cutover_running", "A cutover is in progress.", "Abort the cutover first.")
	}
	done := o.intent(ctx, "", "stop", d.ID, nil)
	if _, err := o.stopUnit(ctx, m, d); err != nil {
		done("failed: " + err.Error())
		return d, err
	}
	o.endAttempt(ctx, d, nil, "stopped by "+a.Username)
	o.setDB(ctx, d.ID, map[string]any{"state": DStopped, "next_retry_at": nil})
	done("stopped")
	o.audit.Append(ctx, a, "database.stop", m.ID, map[string]any{"database": d.SourceName, "state": d.State}, map[string]any{"state": DStopped})
	o.Event(ctx, m.ID, d.SourceName, "db_state", "warning", d.SourceName+" stopped by "+a.Username, nil)
	o.refreshState(ctx, m.ID)
	return o.GetDatabase(ctx, m.ID, d.ID)
}

// ResumeDatabase resumes one stopped or failed database (CDC phase only).
func (o *Orchestrator) ResumeDatabase(ctx context.Context, a audit.Actor, id, db string) (Database, error) {
	unlock := o.lock(id)
	defer unlock()
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return Database{}, err
	}
	d, err := o.GetDatabase(ctx, m.ID, db)
	if err != nil {
		return d, err
	}
	if activeDB[d.State] {
		return d, uerr("running", d.SourceName+" is already running.", "")
	}
	if d.State == DRestartRequired || !d.BaseCopyDone {
		return d, uerr("resume_unsafe", d.SourceName+" cannot resume: its base copy did not finish, or its slot was lost.", "Restart it from zero.")
	}
	if err := o.resumeDB(ctx, m, d, "resumed by "+a.Username); err != nil {
		return d, err
	}
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.Paused = false })
	o.audit.Append(ctx, a, "database.resume", m.ID, map[string]any{"database": d.SourceName, "state": d.State}, nil)
	o.refreshState(ctx, m.ID)
	return o.GetDatabase(ctx, m.ID, d.ID)
}

// RestartDatabase restarts one database from zero: stop, drop the slot,
// publication and origin, reset the target database, wipe the work
// directory, and start a fresh attempt. The operator types the name.
func (o *Orchestrator) RestartDatabase(ctx context.Context, a audit.Actor, id, db, confirm string) (Database, error) {
	unlock := o.lock(id)
	defer unlock()
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return Database{}, err
	}
	d, err := o.GetDatabase(ctx, m.ID, db)
	if err != nil {
		return d, err
	}
	if confirm != d.SourceName {
		return d, uerr("confirm", "Type the database name to confirm restarting it from zero.", "")
	}
	if m.Flags.Cutover != nil && m.Flags.Cutover.EndedAt == 0 {
		return d, uerr("cutover_running", "A cutover is in progress.", "")
	}
	if !m.Flags.Started || m.Flags.Aborted || m.Flags.CleanedUp {
		return d, uerr("not_running", "The migration is not running.", "")
	}
	done := o.intent(ctx, "", "restart", d.ID, nil)
	spec, err := o.dbSpec(ctx, m, d)
	if err != nil {
		done("failed")
		return d, err
	}
	if _, err := o.eng.Cleanup(ctx, spec, engine.CleanupOptions{RemoveWorkDir: true}); err != nil {
		done("failed: " + err.Error())
		return d, err
	}
	o.endAttempt(ctx, d, nil, "restart from zero by "+a.Username)
	if err := resetTargetDB(ctx, spec); err != nil {
		done("failed: " + err.Error())
		return d, err
	}
	o.setDB(ctx, d.ID, map[string]any{"state": DPending, "base_copy_done": 0, "last_error": nil, "error_class": nil, "next_retry_at": nil, "retry_count": 0,
		"hb_before_lsn": nil, "hb_token": nil, "endpos": nil, "origin_lsn": nil, "verdict": nil, "in_sync_since": nil, "replay_lsn": nil, "write_lsn": nil})
	done("reset")
	o.audit.Append(ctx, a, "database.restart", m.ID, map[string]any{"database": d.SourceName, "state": d.State}, map[string]any{"state": DPending})
	o.Event(ctx, m.ID, d.SourceName, "restart", "warning", d.SourceName+" restarted from zero by "+a.Username, nil)
	o.updateFlags(ctx, m.ID, func(f *Flags) {
		if f.Verdict == "NO-GO" {
			f.Verdict = ""
			f.Cutover = nil
		}
	})
	o.refreshState(ctx, m.ID)
	go o.Tick(context.Background())
	return o.GetDatabase(ctx, m.ID, d.ID)
}

// resetTargetDB drops the migrated objects: the whole database when it holds
// only what came from the source, else only the source's relations.
func resetTargetDB(ctx context.Context, spec engine.DatabaseSpec) error {
	src, err := pg.Connect(ctx, spec.SourceConn.WithDB(spec.Source), time.Minute)
	if err != nil {
		return err
	}
	srcRels, _ := pg.QueryStrings(ctx, src, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind IN ('r','p','v','m','S','f') AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'`)
	src.Close(ctx)
	dst, err := pg.Connect(ctx, spec.TargetConn.WithDB(spec.Target), time.Minute)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return nil
		}
		return err
	}
	dstRels, _ := pg.QueryStrings(ctx, dst, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind IN ('r','p','v','m','S','f') AND n.nspname NOT IN ('pg_catalog','information_schema','upwell') AND n.nspname NOT LIKE 'pg_toast%'`)
	srcSet := map[string]bool{}
	for _, r := range srcRels {
		srcSet[r] = true
	}
	onlyOurs := true
	for _, r := range dstRels {
		if !srcSet[r] {
			onlyOurs = false
		}
	}
	if !onlyOurs {
		for _, r := range dstRels {
			if srcSet[r] {
				dst.Exec(ctx, `DROP TABLE IF EXISTS `+r+` CASCADE`)
				dst.Exec(ctx, `DROP VIEW IF EXISTS `+r+` CASCADE`)
				dst.Exec(ctx, `DROP MATERIALIZED VIEW IF EXISTS `+r+` CASCADE`)
				dst.Exec(ctx, `DROP SEQUENCE IF EXISTS `+r+` CASCADE`)
			}
		}
		dst.Exec(ctx, `DROP SCHEMA IF EXISTS upwell CASCADE`)
		dst.Close(ctx)
		return nil
	}
	dst.Close(ctx)
	maint, err := pg.Connect(ctx, spec.TargetConn, time.Minute)
	if err != nil {
		return err
	}
	defer maint.Close(ctx)
	if _, err := maint.Exec(ctx, `DROP DATABASE IF EXISTS `+pg.QuoteIdent(spec.Target)+` WITH (FORCE)`); err != nil {
		return fmt.Errorf("dropping target database %s: %w", spec.Target, err)
	}
	return nil
}

// Pause stops every running database, keeping slots and state.
func (o *Orchestrator) Pause(ctx context.Context, a audit.Actor, id string) (Migration, error) {
	unlock := o.lock(id)
	defer unlock()
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return m, err
	}
	if !m.Flags.Started || m.Flags.Aborted || m.Flags.CleanedUp {
		return m, uerr("not_running", "The migration is not running.", "")
	}
	if m.Flags.Cutover != nil && m.Flags.Cutover.EndedAt == 0 {
		return m, uerr("cutover_running", "A cutover is in progress.", "")
	}
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.Paused = true })
	dbs, _ := o.ListDatabases(ctx, m.ID)
	for _, d := range included(dbs) {
		if activeDB[d.State] || (d.State == DStopped && d.NextRetryAt != nil) {
			if _, err := o.stopUnit(ctx, m, d); err != nil {
				o.Event(ctx, m.ID, d.SourceName, "stop", "critical", "Could not stop "+d.SourceName+": "+err.Error(), nil)
				continue
			}
			o.endAttempt(ctx, d, nil, "paused by "+a.Username)
			st := DStopped
			if !d.BaseCopyDone && d.State != DPending {
				st = DRestartRequired
			}
			o.setDB(ctx, d.ID, map[string]any{"state": st, "next_retry_at": nil})
		}
	}
	o.audit.Append(ctx, a, "migration.pause", m.ID, map[string]any{"state": m.State}, nil)
	o.Event(ctx, m.ID, "", "paused", "warning", "Migration paused by "+a.Username, nil)
	o.refreshState(ctx, m.ID)
	return o.GetMigration(ctx, m.ID)
}

// Resume resumes every stopped database whose base copy finished.
func (o *Orchestrator) Resume(ctx context.Context, a audit.Actor, id string) (Migration, error) {
	unlock := o.lock(id)
	defer unlock()
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return m, err
	}
	if !m.Flags.Started || m.Flags.Aborted || m.Flags.CleanedUp {
		return m, uerr("not_running", "The migration is not running.", "")
	}
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.Paused = false })
	dbs, _ := o.ListDatabases(ctx, m.ID)
	var errs []string
	for _, d := range included(dbs) {
		if d.State == DStopped && d.BaseCopyDone {
			if err := o.resumeDB(ctx, m, d, "migration resumed by "+a.Username); err != nil {
				errs = append(errs, d.SourceName+": "+err.Error())
			}
		}
	}
	o.audit.Append(ctx, a, "migration.resume", m.ID, map[string]any{"state": m.State}, nil)
	o.Event(ctx, m.ID, "", "resumed", "info", "Migration resumed by "+a.Username, nil)
	o.refreshState(ctx, m.ID)
	if len(errs) > 0 {
		return m, errors.New(strings.Join(errs, "; "))
	}
	go o.Tick(context.Background())
	return o.GetMigration(ctx, m.ID)
}

// Abort stops everything for good; cleanup follows.
func (o *Orchestrator) Abort(ctx context.Context, a audit.Actor, id, confirm string) (Migration, error) {
	unlock := o.lock(id)
	defer unlock()
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return m, err
	}
	if confirm != m.Name {
		return m, uerr("confirm", "Type the migration name to confirm aborting it.", "")
	}
	if m.Flags.Aborted || m.Flags.CleanedUp {
		return m, uerr("already_aborted", "The migration is already aborted.", "")
	}
	if m.Flags.Cutover != nil && m.Flags.Cutover.EndedAt == 0 && m.Flags.Cutover.Phase != "readiness" && m.Flags.Cutover.Phase != "writers" && m.Flags.Cutover.Phase != "heartbeat" && m.Flags.Cutover.Phase != "await" {
		return m, uerr("cutover_running", "End positions are already set; the cutover cannot be aborted now.", "Wait for the verdict.")
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	for _, d := range included(dbs) {
		if activeDB[d.State] || d.NextRetryAt != nil {
			if _, err := o.stopUnit(ctx, m, d); err != nil {
				o.Event(ctx, m.ID, d.SourceName, "stop", "critical", "Could not stop "+d.SourceName+": "+err.Error(), nil)
			}
			o.endAttempt(ctx, d, nil, "aborted by "+a.Username)
			o.setDB(ctx, d.ID, map[string]any{"state": DStopped, "next_retry_at": nil})
		}
	}
	o.updateFlags(ctx, m.ID, func(f *Flags) {
		f.Aborted = true
		if f.Cutover != nil && f.Cutover.EndedAt == 0 {
			f.Cutover.EndedAt = store.Now()
			f.Cutover.Verdict = "aborted"
		}
	})
	o.audit.Append(ctx, a, "migration.abort", m.ID, map[string]any{"state": m.State}, nil)
	o.Event(ctx, m.ID, "", "aborted", "warning", "Migration aborted by "+a.Username+"; clean up removes its slots, publications and origins", nil)
	o.refreshState(ctx, m.ID)
	return o.GetMigration(ctx, m.ID)
}

// MarkSwitched records that applications now use the target. Rollback is no
// longer offered after this.
func (o *Orchestrator) MarkSwitched(ctx context.Context, a audit.Actor, id, confirm string) (Migration, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return m, err
	}
	if m.Flags.Verdict != "GO" {
		return m, uerr("not_go", "Applications can be marked switched only after a GO verdict.", "")
	}
	if confirm != m.Name {
		return m, uerr("confirm", "Type the migration name to confirm.", "")
	}
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.AppsSwitched = true })
	o.audit.Append(ctx, a, "migration.apps_switched", m.ID, nil, map[string]any{"apps_switched": true})
	o.Event(ctx, m.ID, "", "switched", "info", "Applications marked as switched to the target by "+a.Username, nil)
	return o.GetMigration(ctx, m.ID)
}

// Cleanup removes slots, publications, origins, heartbeat objects, work
// directories and stored credentials. Runs as an operation.
func (o *Orchestrator) Cleanup(ctx context.Context, a audit.Actor, id, key, confirm string) (*Operation, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, err
	}
	if confirm != m.Name {
		return nil, uerr("confirm", "Type the migration name to confirm cleanup.", "")
	}
	if m.Flags.CleanedUp {
		return nil, uerr("cleaned", "This migration is already cleaned up.", "")
	}
	ok := m.Flags.Aborted || m.Flags.Verdict != "" || !m.Flags.Started
	if !ok {
		return nil, uerr("running", "Clean up is available after the verdict or after aborting.", "Abort the migration first, or finish the cutover.")
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	steps := []Step{}
	for _, d := range included(dbs) {
		steps = append(steps, Step{Key: "db:" + d.SourceName, Title: "Clean up " + d.SourceName})
	}
	steps = append(steps, Step{Key: "credentials", Title: "Delete stored credentials"})
	op, err := o.newOp(ctx, m.ID, "cleanup", key, a.Username, steps, nil)
	if err != nil {
		return op, err
	}
	o.audit.Append(ctx, a, "migration.cleanup", m.ID, nil, map[string]any{"operation": op.ID})
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.CleanupRunning = true })
	go func() {
		bg := context.Background()
		var failed []string
		for _, d := range included(dbs) {
			op.StepStart("db:"+d.SourceName, "")
			spec, err := o.dbSpec(bg, m, d)
			if err != nil {
				op.StepEnd("db:"+d.SourceName, "failed", err.Error())
				failed = append(failed, d.SourceName)
				continue
			}
			did, err := o.eng.Cleanup(bg, spec, engine.CleanupOptions{DropHeartbeat: true, RemoveWorkDir: true})
			if err != nil {
				op.StepEnd("db:"+d.SourceName, "failed", err.Error())
				failed = append(failed, d.SourceName)
				continue
			}
			o.endAttempt(bg, d, nil, "cleaned up")
			if d.State != DVerified {
				o.setDB(bg, d.ID, map[string]any{"state": DStopped, "next_retry_at": nil})
			}
			op.StepEnd("db:"+d.SourceName, "done", strings.Join(did, "; "))
		}
		if len(failed) > 0 {
			o.updateFlags(bg, m.ID, func(f *Flags) { f.CleanupRunning = false })
			op.Finish(fmt.Errorf("cleanup failed for %s; nothing was marked cleaned up", strings.Join(failed, ", ")), nil)
			return
		}
		op.StepStart("credentials", "")
		for _, cid := range []string{m.SourceConnID, m.TargetConnID} {
			if c, err := o.GetConnection(bg, cid); err == nil && c.PasswordSecretID != "" {
				_ = o.vault.Delete(bg, c.PasswordSecretID)
				o.st.DB.ExecContext(bg, `UPDATE connections SET password_secret_id=NULL WHERE id=?`, cid)
			}
		}
		os.RemoveAll(filepath.Join(o.cfg.DataDir, "ca"))
		op.StepEnd("credentials", "done", "")
		o.updateFlags(bg, m.ID, func(f *Flags) { f.CleanupRunning = false; f.CleanedUp = true; f.Paused = false })
		o.Event(bg, m.ID, "", "cleaned_up", "info", "Cleanup finished: slots, publications, origins, heartbeat tables and credentials removed", nil)
		o.refreshState(bg, m.ID)
		op.Finish(nil, map[string]any{"databases": len(included(dbs))})
	}()
	return op, nil
}
