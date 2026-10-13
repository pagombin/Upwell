package orch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/pgpack"
	"github.com/pagombin/upwell/internal/store"
)

// FreezeState is the write freeze on a migration's source databases
// (overnight item 1, spec "Write freeze at cutover").
type FreezeState struct {
	State      string     `json:"state"` // applying, applied, undone
	AppliedAt  int64      `json:"applied_at"`
	By         string     `json:"by"`
	Databases  []FreezeDB `json:"databases"`
	UndoneAt   int64      `json:"undone_at,omitempty"`
	UndoneBy   string     `json:"undone_by,omitempty"`
	UndoReason string     `json:"undo_reason,omitempty"`
	UndoError  string     `json:"undo_error,omitempty"`
}

// FreezeDB is the freeze on one source database.
type FreezeDB struct {
	Name          string   `json:"name"`
	Prior         string   `json:"prior,omitempty"`
	Applied       bool     `json:"applied"`
	Terminated    int      `json:"terminated"`
	NotTerminated []string `json:"not_terminated,omitempty"`
	Undone        bool     `json:"undone"`
}

// dbPrior returns a frozen database's setting from before the freeze.
func (f *FreezeState) dbPrior(name string) (string, bool) {
	if !f.Active() {
		return "", false
	}
	for _, d := range f.Databases {
		if d.Name == name && !d.Undone {
			return d.Prior, true
		}
	}
	return "", false
}

// Active reports whether any database may still be frozen.
func (f *FreezeState) Active() bool { return f != nil && f.State != "undone" }

// FreezeNote is what the preview and the console say about the freeze's limits.
const FreezeNote = "The freeze is a strong guard, not a lock: a session can still run SET default_transaction_read_only = off (or BEGIN READ WRITE) and write. Upwell still samples writes and refuses the cutover if any happen, and verification re-checks them. Sessions that connect while the freeze is applied stay read-only until they reconnect; undoing the freeze ends them so applications reconnect read-write."

// FreezeConfirmName is what the operator types to apply the freeze: the
// source cluster's name, which on DigitalOcean is the first label of its host.
func FreezeConfirmName(host string) string {
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return host
	}
	return strings.SplitN(host, ".", 2)[0]
}

// FreezePreview is exactly what applying the freeze would run and end.
type FreezePreview struct {
	Confirm   string                `json:"confirm"`
	Note      string                `json:"note"`
	Available bool                  `json:"available"`
	Databases []pgpack.FreezeTarget `json:"databases"`
	Active    *FreezeState          `json:"active,omitempty"`
}

// FreezePreviewFor reads, without changing anything, what the freeze would do.
func (o *Orchestrator) FreezePreviewFor(ctx context.Context, id string) (FreezePreview, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return FreezePreview{}, err
	}
	src, _, err := o.Conns(ctx, m)
	if err != nil {
		return FreezePreview{}, err
	}
	pv := FreezePreview{Confirm: FreezeConfirmName(src.Host), Note: FreezeNote, Available: true, Active: m.Flags.Freeze}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	for _, d := range included(dbs) {
		t, err := pgpack.InspectFreeze(ctx, src.WithDB(d.SourceName))
		if err != nil {
			return pv, fmt.Errorf("%s: could not inspect the database for the write freeze: %w", d.SourceName, err)
		}
		if !t.Available {
			pv.Available = false
		}
		pv.Databases = append(pv.Databases, t)
	}
	return pv, nil
}

// applyFreeze freezes every source database at the start of the writers
// step. The audit entry comes first (D58); a failure part-way undoes what
// was applied.
func (o *Orchestrator) applyFreeze(ctx context.Context, m Migration, a audit.Actor, dbs []Database, src pg.Conn, op *Operation) error {
	var targets []pgpack.FreezeTarget
	for _, d := range dbs {
		t, err := pgpack.InspectFreeze(ctx, src.WithDB(d.SourceName))
		if err != nil {
			return fmt.Errorf("%s: could not inspect the database for the write freeze: %w", d.SourceName, err)
		}
		if !t.Available {
			return fmt.Errorf("the write freeze is not available for %s: %s", d.SourceName, t.Reason)
		}
		targets = append(targets, t)
	}
	if _, err := o.audit.Append(ctx, a, "cutover.write_freeze", m.ID, nil, map[string]any{"databases": targets}); err != nil {
		return fmt.Errorf("the write freeze was not applied because it could not be recorded in the audit log: %w", err)
	}
	st := &FreezeState{State: "applying", AppliedAt: store.Now(), By: a.Username}
	for _, t := range targets {
		st.Databases = append(st.Databases, FreezeDB{Name: t.Database, Prior: t.Prior})
	}
	// Record the intent before changing anything: a crash part-way leaves a
	// state that the restart undoes.
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.Freeze = st })
	for i, t := range targets {
		res, err := pgpack.ApplyFreeze(ctx, src.WithDB(t.Database), t.Prior)
		st.Databases[i].Applied = err == nil || res.Terminated > 0
		st.Databases[i].Terminated, st.Databases[i].NotTerminated = res.Terminated, res.NotTerminated
		if err != nil {
			st.Databases[i].Applied = true // ALTER may have run: undo is idempotent
			o.updateFlags(ctx, m.ID, func(f *Flags) { f.Freeze = st })
			uerr := o.undoFreeze(ctx, m.ID, "Upwell", "applying the freeze failed: "+err.Error())
			return errors.Join(fmt.Errorf("applying the write freeze on %s: %w", t.Database, err), uerr)
		}
		op.Log("warn", "write freeze applied on %s: new sessions are read-only; %d client session(s) ended", t.Database, res.Terminated)
		if len(res.NotTerminated) > 0 {
			op.Log("warn", "%s: %d session(s) could not be ended: %s", t.Database, len(res.NotTerminated), strings.Join(limitStrings(res.NotTerminated, 3), "; "))
		}
	}
	st.State = "applied"
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.Freeze = st })
	o.Event(ctx, m.ID, "", "write_freeze", "warning", fmt.Sprintf("Write freeze applied by %s on %d database(s); it is undone automatically if the cutover does not end GO", a.Username, len(targets)), nil)
	return nil
}

// UndoFreeze is the always-available undo button.
func (o *Orchestrator) UndoFreeze(ctx context.Context, a audit.Actor, id string) (Migration, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return m, err
	}
	if !m.Flags.Freeze.Active() {
		return m, uerr("no_freeze", "No write freeze is applied.", "")
	}
	if c := m.Flags.Cutover; c != nil && c.EndedAt == 0 && (c.Phase == "heartbeat" || c.Phase == "await" || c.Phase == "endpos" || c.Phase == "drain") {
		o.Event(ctx, m.ID, "", "write_freeze", "warning", a.Username+" undid the write freeze during the cutover: writes from now on end NO-GO", nil)
	}
	if err := o.undoFreeze(ctx, m.ID, a.Username, "undone by "+a.Username); err != nil {
		return m, err
	}
	return o.GetMigration(ctx, m.ID)
}

// undoFreeze restores every frozen database. It is idempotent; a failure
// keeps the freeze marked as applied, raises a critical alert and is retried
// by supervision.
// freezeLocks serializes undoing a migration's freeze (migration id ->
// *sync.Mutex): an abort and the cutover it cancels both undo it, and two
// concurrent ALTER DATABASE ... SET on the same database fail with "tuple
// concurrently deleted", which left the freeze recorded as applied (D78).
var freezeLocks sync.Map

func (o *Orchestrator) undoFreeze(ctx context.Context, migID, by, reason string) error {
	mu, _ := freezeLocks.LoadOrStore(migID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	// Read after taking the lock: an undo that just finished is seen as done.
	m, err := o.GetMigration(ctx, migID)
	if err != nil {
		return err
	}
	st := m.Flags.Freeze
	if !st.Active() {
		return nil
	}
	src, _, err := o.Conns(ctx, m)
	var errs []error
	if err != nil {
		errs = append(errs, err)
	} else {
		since := time.UnixMilli(st.AppliedAt)
		for i := range st.Databases {
			d := &st.Databases[i]
			if d.Undone {
				continue
			}
			n, uerr := pgpack.UndoFreeze(ctx, src.WithDB(d.Name), d.Prior, since)
			if uerr != nil {
				errs = append(errs, uerr)
				continue
			}
			d.Undone = true
			o.logf("info", "orchestrator", m.ID, d.Name, "", "write freeze undone on %s; %d session(s) opened during the freeze ended", d.Name, n)
		}
	}
	if len(errs) > 0 {
		err := errors.Join(errs...)
		st.UndoError = err.Error()
		o.updateFlags(ctx, m.ID, func(f *Flags) { f.Freeze = st })
		o.alerts.fire(ctx, m.ID, "write_freeze_stuck", "source", "critical", "The write freeze could not be undone ("+trimErr(err)+"); the source databases may still be read-only for new sessions. Upwell retries; undo it from the cutover console or run the undo SQL from the freeze preview.")
		return fmt.Errorf("the write freeze could not be undone: %w", err)
	}
	st.State, st.UndoneAt, st.UndoneBy, st.UndoReason, st.UndoError = "undone", store.Now(), by, reason, ""
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.Freeze = st })
	o.alerts.resolve(ctx, m.ID, "write_freeze_stuck", "source")
	o.audit.Append(ctx, audit.Actor{Username: by}, "cutover.write_freeze_undo", m.ID, nil, map[string]any{"reason": reason})
	o.Event(ctx, m.ID, "", "write_freeze_undone", "info", "Write freeze undone ("+reason+"): the source databases accept writes again", nil)
	return nil
}

// freezeJanitor undoes a freeze that should no longer be applied: the
// cutover is not running and did not end GO (a failed undo, or one the
// restart could not finish). Called from supervision.
func (o *Orchestrator) freezeJanitor(ctx context.Context, m Migration) {
	if !m.Flags.Freeze.Active() || m.Flags.Verdict == "GO" || m.Flags.Verifying {
		return
	}
	if c := m.Flags.Cutover; c != nil && c.EndedAt == 0 {
		if _, running := cutoverCancels.Load(c.OpID); running {
			return
		}
	}
	if !o.metrics.due(m.ID+"|freeze-janitor", 30*time.Second) {
		return
	}
	_ = o.undoFreeze(ctx, m.ID, "Upwell", "the cutover is no longer running and did not end GO")
}

// MigrationSecrets returns the stored passwords of a migration's connections,
// so exports (the support bundle) can scrub any literal occurrence.
func (o *Orchestrator) MigrationSecrets(ctx context.Context, m Migration) []string {
	var out []string
	for _, cid := range []string{m.SourceConnID, m.TargetConnID} {
		if cid == "" {
			continue
		}
		c, err := o.GetConnection(ctx, cid)
		if err != nil || c.PasswordSecretID == "" {
			continue
		}
		if pw, err := o.vault.Get(ctx, c.PasswordSecretID); err == nil && pw != "" {
			out = append(out, pw)
		}
	}
	return out
}

// EngineLogPath is a database's engine log file.
func (o *Orchestrator) EngineLogPath(d Database) string { return o.runLog(d) }
