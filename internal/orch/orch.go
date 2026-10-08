// Package orch is the orchestrator: the single writer of migration and
// database state. It runs preflight, launches and supervises engine units,
// drives cutover, applies the retry policy and reconciles after restarts.
package orch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/bus"
	"github.com/pagombin/upwell/internal/config"
	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/logx"
	"github.com/pagombin/upwell/internal/runner"
	"github.com/pagombin/upwell/internal/secrets"
	"github.com/pagombin/upwell/internal/settings"
	"github.com/pagombin/upwell/internal/store"
)

// Orchestrator owns migration state.
type Orchestrator struct {
	cfg      config.Config
	st       *store.Store
	vault    *secrets.Vault
	eng      engine.Engine
	run      runner.Runner
	log      *logx.Logger
	bus      *bus.Bus
	audit    *audit.Log
	settings *settings.Service

	flagMu  sync.Mutex
	migMu   sync.Map // migration id -> *sync.Mutex
	tails   *tailers
	metrics *collector
	alerts  *alerter

	Heartbeat   func(name string) // watchdog heartbeat hook
	lastLoop    time.Time
	loopMu      sync.Mutex
	Reconciled  bool
	StartedAt   time.Time
	Rebooted    bool
	testHooks   TestHooks
}

// TestHooks let tests inject faults. Never set from the API.
type TestHooks struct {
	ExtraEngineArgs  func(db string) []string
	SkipPreflightGate bool
}

// Deps bundles constructor arguments.
type Deps struct {
	Config   config.Config
	Store    *store.Store
	Vault    *secrets.Vault
	Engine   engine.Engine
	Runner   runner.Runner
	Logger   *logx.Logger
	Bus      *bus.Bus
	Audit    *audit.Log
	Settings *settings.Service
}

// New builds an orchestrator.
func New(d Deps) *Orchestrator {
	o := &Orchestrator{cfg: d.Config, st: d.Store, vault: d.Vault, eng: d.Engine, run: d.Runner, log: d.Logger, bus: d.Bus, audit: d.Audit, settings: d.Settings, StartedAt: time.Now()}
	o.tails = newTailers(o)
	o.metrics = newCollector(o)
	o.alerts = newAlerter(o)
	return o
}

// SetTestHooks installs fault-injection hooks (tests only).
func (o *Orchestrator) SetTestHooks(h TestHooks) { o.testHooks = h }

// Engine returns the engine.
func (o *Orchestrator) Engine() engine.Engine { return o.eng }

// Runner returns the runner.
func (o *Orchestrator) Runner() runner.Runner { return o.run }

func (o *Orchestrator) lock(id string) func() {
	v, _ := o.migMu.LoadOrStore(id, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

func (o *Orchestrator) logf(level, component, migration, db, op, format string, args ...any) {
	o.log.Write(logx.Record{Level: level, Component: component, Migration: migration, Database: db, Op: op, Msg: fmt.Sprintf(format, args...)})
}

// Event records a timeline event and publishes it.
func (o *Orchestrator) Event(ctx context.Context, migration, db, typ, severity, msg string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	id := store.NewID()
	ts := store.Now()
	var seq int64
	o.st.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM events`).Scan(&seq)
	o.st.DB.ExecContext(ctx, `INSERT INTO events(id,seq,migration_id,database,type,severity,message,data,ts) VALUES (?,?,?,?,?,?,?,?,?)`,
		id, seq, store.NullString(migration), store.NullString(db), typ, severity, msg, toJSON(data), ts)
	ev := map[string]any{"id": id, "migration_id": migration, "database": db, "type": typ, "severity": severity, "message": msg, "data": data, "ts": ts}
	o.bus.Publish("migration", "event", migration, ev)
	lvl := "info"
	if severity == "warning" {
		lvl = "warn"
	} else if severity == "critical" {
		lvl = "error"
	}
	o.logf(lvl, "orchestrator", migration, db, "", "%s", msg)
}

// ---------- operations, steps and intents

// Step is one numbered step of an operation.
type Step struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	State     string `json:"state"` // waiting, running, done, failed, skipped
	StartedAt int64  `json:"started_at,omitempty"`
	EndedAt   int64  `json:"ended_at,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Progress  float64 `json:"progress,omitempty"`
}

// Operation is a long-running command.
type Operation struct {
	ID          string         `json:"id"`
	MigrationID string         `json:"migration_id,omitempty"`
	Kind        string         `json:"kind"`
	Key         string         `json:"operation_key,omitempty"`
	State       string         `json:"state"` // running, done, failed, interrupted
	Steps       []Step         `json:"steps"`
	Params      map[string]any `json:"params"`
	StartedBy   string         `json:"started_by,omitempty"`
	StartedAt   int64          `json:"started_at"`
	EndedAt     *int64         `json:"ended_at,omitempty"`
	Result      map[string]any `json:"result,omitempty"`
	Error       string         `json:"error,omitempty"`

	o  *Orchestrator
	mu *sync.Mutex
}

// ErrDuplicate is returned with the original operation when an idempotency
// key is reused.
var ErrDuplicate = errors.New("operation already submitted")

func (o *Orchestrator) newOp(ctx context.Context, migrationID, kind, key, by string, steps []Step, params map[string]any) (*Operation, error) {
	if key != "" {
		var id string
		err := o.st.DB.QueryRowContext(ctx, `SELECT id FROM operations WHERE operation_key=?`, key).Scan(&id)
		if err == nil {
			op, err := o.GetOperation(ctx, id)
			if err != nil {
				return nil, err
			}
			return op, ErrDuplicate
		}
	}
	for i := range steps {
		steps[i].State = "waiting"
	}
	if params == nil {
		params = map[string]any{}
	}
	op := &Operation{ID: store.NewID(), MigrationID: migrationID, Kind: kind, Key: key, State: "running", Steps: steps, Params: params, StartedBy: by, StartedAt: store.Now(), o: o, mu: &sync.Mutex{}}
	_, err := o.st.DB.ExecContext(ctx, `INSERT INTO operations(id,migration_id,kind,operation_key,state,steps,params,started_by,started_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		op.ID, store.NullString(migrationID), kind, store.NullString(key), op.State, toJSON(op.Steps), toJSON(params), by, op.StartedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") && key != "" {
			var id string
			o.st.DB.QueryRowContext(ctx, `SELECT id FROM operations WHERE operation_key=?`, key).Scan(&id)
			existing, gerr := o.GetOperation(ctx, id)
			if gerr == nil {
				return existing, ErrDuplicate
			}
		}
		return nil, err
	}
	o.publishOp(op)
	return op, nil
}

func (o *Orchestrator) publishOp(op *Operation) {
	o.bus.Publish("migration", "operation", op.MigrationID, op.snapshot())
}

func (op *Operation) snapshot() Operation {
	op.mu.Lock()
	defer op.mu.Unlock()
	cp := Operation{ID: op.ID, MigrationID: op.MigrationID, Kind: op.Kind, Key: op.Key, State: op.State, Params: op.Params, StartedBy: op.StartedBy, StartedAt: op.StartedAt, EndedAt: op.EndedAt, Result: op.Result, Error: op.Error}
	cp.Steps = append([]Step(nil), op.Steps...)
	return cp
}

func (op *Operation) save() {
	s := op.snapshot()
	var ended any
	if s.EndedAt != nil {
		ended = *s.EndedAt
	}
	op.o.st.DB.Exec(`UPDATE operations SET state=?, steps=?, result=?, error=?, ended_at=? WHERE id=?`, s.State, toJSON(s.Steps), toJSON(s.Result), store.NullString(s.Error), ended, s.ID)
	op.o.publishOp(op)
}

// StepStart marks a step running.
func (op *Operation) StepStart(key, detail string) {
	op.mu.Lock()
	for i := range op.Steps {
		if op.Steps[i].Key == key {
			op.Steps[i].State = "running"
			op.Steps[i].StartedAt = store.Now()
			op.Steps[i].Detail = detail
		}
	}
	op.mu.Unlock()
	op.save()
	op.Log("info", "%s", stepTitle(op, key)+": started")
}

func stepTitle(op *Operation, key string) string {
	op.mu.Lock()
	defer op.mu.Unlock()
	for _, s := range op.Steps {
		if s.Key == key {
			return s.Title
		}
	}
	return key
}

// StepDetail updates a running step's detail and progress.
func (op *Operation) StepDetail(key, detail string, progress float64) {
	op.mu.Lock()
	for i := range op.Steps {
		if op.Steps[i].Key == key {
			op.Steps[i].Detail = detail
			op.Steps[i].Progress = progress
		}
	}
	op.mu.Unlock()
	op.save()
}

// StepEnd finishes a step.
func (op *Operation) StepEnd(key, state, detail string) {
	op.mu.Lock()
	for i := range op.Steps {
		if op.Steps[i].Key == key {
			op.Steps[i].State = state
			op.Steps[i].EndedAt = store.Now()
			if op.Steps[i].StartedAt == 0 {
				op.Steps[i].StartedAt = op.Steps[i].EndedAt
			}
			if detail != "" {
				op.Steps[i].Detail = detail
			}
		}
	}
	op.mu.Unlock()
	op.save()
	lvl := "info"
	if state == "failed" {
		lvl = "error"
	}
	op.Log(lvl, "%s: %s%s", stepTitle(op, key), state, func() string {
		if detail != "" {
			return " (" + detail + ")"
		}
		return ""
	}())
}

// Finish ends the operation.
func (op *Operation) Finish(err error, result map[string]any) {
	now := store.Now()
	op.mu.Lock()
	op.EndedAt = &now
	op.Result = result
	if err != nil {
		op.State = "failed"
		op.Error = err.Error()
		for i := range op.Steps {
			if op.Steps[i].State == "running" {
				op.Steps[i].State = "failed"
				op.Steps[i].EndedAt = now
			} else if op.Steps[i].State == "waiting" {
				op.Steps[i].State = "skipped"
			}
		}
	} else {
		op.State = "done"
		for i := range op.Steps {
			if op.Steps[i].State == "waiting" {
				op.Steps[i].State = "skipped"
			}
		}
	}
	op.mu.Unlock()
	op.save()
	if err != nil {
		op.Log("error", "%s failed: %v", op.Kind, err)
	} else {
		op.Log("info", "%s finished", op.Kind)
	}
}

// Log writes a record tagged with the operation ID.
func (op *Operation) Log(level, format string, args ...any) {
	op.o.log.Write(logx.Record{Level: level, Component: componentFor(op.Kind), Migration: op.MigrationID, Op: op.ID, Msg: fmt.Sprintf(format, args...)})
}

// LogDB writes a record for one database.
func (op *Operation) LogDB(level, db, format string, args ...any) {
	op.o.log.Write(logx.Record{Level: level, Component: componentFor(op.Kind), Migration: op.MigrationID, Database: db, Op: op.ID, Msg: fmt.Sprintf(format, args...)})
}

func componentFor(kind string) string {
	switch kind {
	case "preflight":
		return "preflight"
	case "cutover":
		return "cutover"
	case "verify":
		return "verify"
	}
	return "orchestrator"
}

// GetOperation loads an operation.
func (o *Orchestrator) GetOperation(ctx context.Context, id string) (*Operation, error) {
	op := &Operation{o: o, mu: &sync.Mutex{}}
	var steps, params string
	var result, errS, key, mig sql.NullString
	var ended sql.NullInt64
	err := o.st.DB.QueryRowContext(ctx, `SELECT id, migration_id, kind, operation_key, state, steps, params, COALESCE(started_by,''), started_at, ended_at, result, error FROM operations WHERE id=?`, id).
		Scan(&op.ID, &mig, &op.Kind, &key, &op.State, &steps, &params, &op.StartedBy, &op.StartedAt, &ended, &result, &errS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	op.MigrationID, op.Key, op.Error = mig.String, key.String, errS.String
	scanJSON(steps, &op.Steps)
	scanJSON(params, &op.Params)
	if result.Valid {
		scanJSON(result.String, &op.Result)
	}
	if ended.Valid {
		op.EndedAt = &ended.Int64
	}
	return op, nil
}

// ListOperations returns a migration's operations, newest first.
func (o *Orchestrator) ListOperations(ctx context.Context, migrationID string, limit int) ([]Operation, error) {
	rows, err := o.st.DB.QueryContext(ctx, `SELECT id FROM operations WHERE migration_id=? ORDER BY started_at DESC LIMIT ?`, migrationID, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := []Operation{}
	for _, id := range ids {
		if op, err := o.GetOperation(ctx, id); err == nil {
			out = append(out, op.snapshot())
		}
	}
	return out, nil
}

// intent records an action before its side effect; done records the outcome.
func (o *Orchestrator) intent(ctx context.Context, opID, action, target string, payload map[string]any) func(outcome string) {
	id := store.NewID()
	o.st.DB.ExecContext(ctx, `INSERT INTO intents(id,operation_id,action,target,payload,recorded_at) VALUES (?,?,?,?,?,?)`, id, store.NullString(opID), action, target, toJSON(payload), store.Now())
	return func(outcome string) {
		o.st.DB.Exec(`UPDATE intents SET outcome=?, outcome_at=? WHERE id=?`, outcome, store.Now(), id)
	}
}

// ---------- state derivation

// DeriveState computes the migration state from its databases and flags.
func DeriveState(m Migration, dbs []Database, preflightOK, permissionOK bool) string {
	f := m.Flags
	switch {
	case f.CleanedUp:
		return MCleanedUp
	case f.Aborted:
		return MAborted
	}
	inc := included(dbs)
	if !f.Started {
		if preflightOK && permissionOK && len(inc) > 0 {
			return MReady
		}
		return MDraft
	}
	if f.Verdict == "GO" && !f.Verifying {
		return MCompleted
	}
	if f.Cutover != nil && f.Cutover.EndedAt == 0 {
		if f.Cutover.Phase == "verify" {
			return MVerifying
		}
		return MCutover
	}
	if f.Verifying {
		return MVerifying
	}
	if f.Verdict == "NO-GO" {
		return MNeedsAttention
	}
	attention := false
	allStreaming := len(inc) > 0
	anyActive := false
	allStopped := len(inc) > 0
	for _, d := range inc {
		switch d.State {
		case DFailed, DDegraded, DRestartRequired:
			if d.NextRetryAt == nil {
				attention = true
			}
		}
		if d.State == DStopped && d.NextRetryAt != nil {
			// waiting for an automatic resume: still running from the operator's view
		} else if d.State != DStopped {
			allStopped = false
		}
		if d.State != DCatchUp && d.State != DInSync {
			allStreaming = false
		}
		if activeDB[d.State] {
			anyActive = true
		}
	}
	switch {
	case attention:
		return MNeedsAttention
	case f.Paused && !anyActive:
		return MPaused
	case allStopped && !f.Paused:
		return MNeedsAttention
	case allStreaming:
		return MStreaming
	}
	return MRunning
}

// refreshState recomputes and stores the migration state.
func (o *Orchestrator) refreshState(ctx context.Context, id string) (string, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return "", err
	}
	dbs, err := o.ListDatabases(ctx, m.ID)
	if err != nil {
		return "", err
	}
	perm, _ := o.GetPermission(ctx, m.ID)
	permOK := perm != nil && len(perm.Missing()) == 0
	pfOK := m.Flags.PreflightOK && o.preflightStillValid(ctx, m)
	s := DeriveState(m, dbs, pfOK, permOK)
	if s != m.State {
		o.st.DB.ExecContext(ctx, `UPDATE migrations SET state=?, updated_at=? WHERE id=?`, s, store.Now(), m.ID)
		o.Event(ctx, m.ID, "", "state", severityFor(s), fmt.Sprintf("Migration is now %s", Label(s)), map[string]any{"from": m.State, "to": s})
	}
	o.bus.Publish("migration", "state", m.ID, map[string]any{"state": s})
	return s, nil
}

func severityFor(s string) string {
	switch s {
	case MNeedsAttention:
		return "critical"
	case MPaused, MAborted:
		return "warning"
	}
	return "info"
}

// Label returns plain-English state names.
func Label(s string) string {
	return map[string]string{
		MDraft: "Draft", MReady: "Ready", MQueued: "Queued", MRunning: "Running", MStreaming: "Streaming", MCutover: "Cutover",
		MVerifying: "Verifying", MCompleted: "Completed", MNeedsAttention: "Needs attention", MPaused: "Paused", MAborted: "Aborted", MCleanedUp: "Cleaned up",
		DPending: "Pending", DPreparing: "Preparing", DBaseCopy: "Base copy", DCatchUp: "Catch-up", DInSync: "In sync", DDraining: "Draining",
		DDrained: "Drained", DVerified: "Verified", DFailed: "Failed", DDegraded: "Degraded", DStopped: "Stopped", DRestartRequired: "Restart required",
	}[s]
}

// Settings returns the effective settings for a migration: the frozen
// snapshot once started, else global defaults plus overrides.
func (o *Orchestrator) Settings(ctx context.Context, m Migration) settings.Values {
	if m.Snapshot != nil {
		v := settings.Values{}
		for k, x := range m.Snapshot {
			v[k] = x
		}
		// numbers come back from JSON as float64
		return v
	}
	g, _ := o.settings.Global(ctx)
	v, err := settings.ForMigration(g, m.Settings)
	if err != nil {
		return g
	}
	return v
}

// globalSettings returns global settings (for install-wide values).
func (o *Orchestrator) globalSettings(ctx context.Context) settings.Values {
	g, _ := o.settings.Global(ctx)
	return g
}

func bootID() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}
