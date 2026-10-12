package orch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/store"
)

// The run lock and the queue (overnight item 7, spec "One runner"): one
// active migration per install, held in a store row and mirrored in a lock
// file in the data directory; other migrations wait in a queue the operator
// orders. A queued migration starts only when an operator starts it, unless
// its entry has auto-start.

var runLockMu sync.Mutex

// holdsRunLock: a started migration holds the lock until it ends GO, is
// aborted or is cleaned up (a NO-GO keeps its slots, so it still holds it).
func holdsRunLock(m Migration) bool {
	f := m.Flags
	running := f.Started || (f.Cutover != nil && f.Cutover.EndedAt == 0) // an offline run before its copy
	return !m.Fixture && running && !f.Aborted && !f.CleanedUp && f.Verdict != "GO"
}

func (o *Orchestrator) lockFile() string { return filepath.Join(o.cfg.DataDir, "run.lock") }

// RunLockHolder returns the migration holding the run lock, if any.
func (o *Orchestrator) RunLockHolder(ctx context.Context) (*Migration, error) {
	var id sql.NullString
	if err := o.st.DB.QueryRowContext(ctx, `SELECT migration_id FROM run_lock WHERE id=1`).Scan(&id); err != nil {
		return nil, err
	}
	if !id.Valid || id.String == "" {
		return nil, nil
	}
	m, err := o.GetMigration(ctx, id.String)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !holdsRunLock(m) {
		return nil, nil
	}
	return &m, nil
}

// acquireRunLock takes the run lock for m, or says who holds it.
func (o *Orchestrator) acquireRunLock(ctx context.Context, m Migration) error {
	runLockMu.Lock()
	defer runLockMu.Unlock()
	h, err := o.RunLockHolder(ctx)
	if err != nil {
		return fmt.Errorf("the run lock could not be read: %w", err)
	}
	if h != nil && h.ID != m.ID {
		return uerr("run_locked", fmt.Sprintf("Another migration is running (%s, %s): one runs at a time.", h.Name, Label(h.State)), "Queue this one; it waits until the running migration ends.")
	}
	// Migrations started before the run lock existed hold it too.
	if h == nil {
		ms, _ := o.ListMigrations(ctx)
		for _, x := range ms {
			if x.ID != m.ID && holdsRunLock(x) {
				return uerr("run_locked", fmt.Sprintf("Another migration is running (%s, %s): one runs at a time.", x.Name, Label(x.State)), "Queue this one; it waits until the running migration ends.")
			}
		}
	}
	if _, err := o.st.DB.ExecContext(ctx, `UPDATE run_lock SET migration_id=?, acquired_at=? WHERE id=1`, m.ID, store.Now()); err != nil {
		return err
	}
	_ = os.WriteFile(o.lockFile(), []byte(m.ID+" "+m.Name+"\n"), 0o640)
	o.st.DB.ExecContext(ctx, `DELETE FROM queue_entries WHERE migration_id=?`, m.ID)
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.Queued = false })
	return nil
}

// runLockJanitor releases a lock whose holder ended and offers the queue's
// next entry: started automatically only with auto-start. Called each tick.
func (o *Orchestrator) runLockJanitor(ctx context.Context) {
	runLockMu.Lock()
	var id sql.NullString
	o.st.DB.QueryRowContext(ctx, `SELECT migration_id FROM run_lock WHERE id=1`).Scan(&id)
	h, err := o.RunLockHolder(ctx)
	if err != nil || h != nil {
		runLockMu.Unlock()
		return
	}
	if id.Valid && id.String != "" {
		o.st.DB.ExecContext(ctx, `UPDATE run_lock SET migration_id=NULL, acquired_at=NULL WHERE id=1`)
		os.Remove(o.lockFile())
		o.Event(ctx, id.String, "", "run_lock", "info", "The run lock is free: the next queued migration may start", nil)
	}
	runLockMu.Unlock()
	q, _ := o.ListQueue(ctx)
	if len(q) == 0 || !q[0].AutoStart || !o.metrics.due("queue-autostart|"+q[0].MigrationID, time.Minute) {
		return
	}
	head := q[0]
	_, err = o.Start(ctx, audit.Actor{Username: "Upwell (auto-start, queued by " + head.QueuedBy + ")"}, head.MigrationID, StartOptions{WarningsReviewed: head.WarningsReviewed})
	if err != nil {
		o.Event(ctx, head.MigrationID, "", "queue", "warning", "Auto-start did not start this queued migration: "+err.Error()+". It stays queued.", nil)
		return
	}
	o.Event(ctx, head.MigrationID, "", "queue", "info", "Started from the queue (auto-start, set by "+head.QueuedBy+")", nil)
}

// QueueEntry is one queued migration.
type QueueEntry struct {
	MigrationID      string `json:"migration_id"`
	ShortID          string `json:"short_id"`
	Name             string `json:"name"`
	Position         int    `json:"position"`
	AutoStart        bool   `json:"auto_start"`
	WarningsReviewed bool   `json:"warnings_reviewed"`
	QueuedBy         string `json:"queued_by"`
	QueuedAt         int64  `json:"queued_at"`
}

// ListQueue returns the queue in order.
func (o *Orchestrator) ListQueue(ctx context.Context) ([]QueueEntry, error) {
	rows, err := o.st.DB.QueryContext(ctx, `SELECT q.migration_id, m.short_id, m.name, q.position, q.auto_start, q.warnings_reviewed, q.queued_by, q.queued_at
		FROM queue_entries q JOIN migrations m ON m.id = q.migration_id ORDER BY q.position, q.queued_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QueueEntry{}
	for rows.Next() {
		var e QueueEntry
		if err := rows.Scan(&e.MigrationID, &e.ShortID, &e.Name, &e.Position, &e.AutoStart, &e.WarningsReviewed, &e.QueuedBy, &e.QueuedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// QueueMigration puts a ready migration at the end of the queue.
func (o *Orchestrator) QueueMigration(ctx context.Context, a audit.Actor, id string, autoStart, warningsReviewed bool) (Migration, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return m, err
	}
	if m.Flags.Started {
		return m, uerr("already_started", "This migration has already started.", "")
	}
	if m.Flags.Queued {
		return m, uerr("queued", "This migration is already queued.", "")
	}
	if err := o.startChecks(ctx, m, StartOptions{WarningsReviewed: warningsReviewed}); err != nil {
		return m, err
	}
	if autoStart && o.IsOffline(ctx, m) {
		return m, uerr("offline_autostart", "An offline migration cannot start by itself: its writers must be stopped (or frozen) when it starts.", "Queue it without auto-start and start it when the time comes.")
	}
	var pos int
	o.st.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) + 1 FROM queue_entries`).Scan(&pos)
	if _, err := o.st.DB.ExecContext(ctx, `INSERT INTO queue_entries(migration_id, position, auto_start, warnings_reviewed, queued_by, queued_at) VALUES (?,?,?,?,?,?)`,
		m.ID, pos, autoStart, warningsReviewed, a.Username, store.Now()); err != nil {
		return m, err
	}
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.Queued = true })
	o.audit.Append(ctx, a, "queue.add", m.ID, nil, map[string]any{"position": pos, "auto_start": autoStart})
	o.Event(ctx, m.ID, "", "queue", "info", fmt.Sprintf("Queued by %s at position %d%s", a.Username, pos, map[bool]string{true: " (auto-start)", false: ""}[autoStart]), nil)
	o.refreshState(ctx, m.ID)
	return o.GetMigration(ctx, m.ID)
}

// Unqueue takes a migration out of the queue.
func (o *Orchestrator) Unqueue(ctx context.Context, a audit.Actor, id string) (Migration, error) {
	res, err := o.st.DB.ExecContext(ctx, `DELETE FROM queue_entries WHERE migration_id=?`, id)
	if err != nil {
		return Migration{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Migration{}, uerr("not_queued", "This migration is not queued.", "")
	}
	o.updateFlags(ctx, id, func(f *Flags) { f.Queued = false })
	o.audit.Append(ctx, a, "queue.remove", id, nil, nil)
	o.refreshState(ctx, id)
	return o.GetMigration(ctx, id)
}

// ReorderQueue sets the queue's order; ids must be exactly the queued migrations.
func (o *Orchestrator) ReorderQueue(ctx context.Context, a audit.Actor, ids []string) ([]QueueEntry, error) {
	cur, err := o.ListQueue(ctx)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, e := range cur {
		have[e.MigrationID] = true
	}
	if len(ids) != len(cur) {
		return nil, uerr("queue_changed", "The queue changed; reload it and try again.", "")
	}
	for _, id := range ids {
		if !have[id] {
			return nil, uerr("queue_changed", "The queue changed; reload it and try again.", "")
		}
		delete(have, id)
	}
	tx, err := o.st.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE queue_entries SET position=? WHERE migration_id=?`, i+1, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	o.audit.Append(ctx, a, "queue.reorder", "", nil, map[string]any{"order": ids})
	return o.ListQueue(ctx)
}

// SetQueueAutoStart turns auto-start on or off for a queued migration.
func (o *Orchestrator) SetQueueAutoStart(ctx context.Context, a audit.Actor, id string, on bool) error {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return err
	}
	if on && o.IsOffline(ctx, m) {
		return uerr("offline_autostart", "An offline migration cannot start by itself.", "")
	}
	res, err := o.st.DB.ExecContext(ctx, `UPDATE queue_entries SET auto_start=? WHERE migration_id=?`, on, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return uerr("not_queued", "This migration is not queued.", "")
	}
	o.audit.Append(ctx, a, "queue.auto_start", id, nil, map[string]any{"auto_start": on})
	return nil
}
