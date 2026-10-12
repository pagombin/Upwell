package orch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/pgpack"
	"github.com/pagombin/upwell/internal/settings"
	"github.com/pagombin/upwell/internal/store"
)

// Offline migrations (overnight item 2, D65): writes stop first (confirmed,
// or the write freeze), the engine copies once without streaming, and
// verification compares the data and re-checks that the source did not
// change. The whole run is one operation, kept in Flags.Cutover so the
// cutover console shows its steps and its verdict.

func offlineSteps() []Step {
	return []Step{{Key: "writers", Title: "Confirm writers stopped"}, {Key: "copy", Title: "Copy every database"},
		{Key: "sequences", Title: "Sync sequences"}, {Key: "verify", Title: "Verify data"}, {Key: "verdict", Title: "Verdict"}}
}

// IsOffline reports whether a started migration runs offline.
func (o *Orchestrator) IsOffline(ctx context.Context, m Migration) bool {
	return o.Settings(ctx, m).Str("mode") == "offline"
}

// offlineChecks: how writes stop must be settled before an offline start.
func (o *Orchestrator) offlineChecks(ctx context.Context, m Migration, opts StartOptions) error {
	if opts.WriteFreeze {
		if opts.Role != "admin" {
			return uerr("admin_only", "Only an Admin can apply the write freeze.", "Ask an Admin, or stop writers by hand and confirm it.")
		}
		src, _, err := o.Conns(ctx, m)
		if err != nil {
			return err
		}
		if want := FreezeConfirmName(src.Host); opts.FreezeConfirm != want {
			return uerr("freeze_confirm", "Type the source cluster's name ("+want+") to apply the write freeze.", "Review the freeze preview first.")
		}
	} else if !opts.ConfirmWritersStopped {
		return uerr("confirm_writers", "An offline migration copies once: confirm that every writer to the source is stopped, or apply the write freeze.", "Writes made after this point would be missing on the target, and verification would end NO-GO.")
	}
	return nil
}

func (o *Orchestrator) startOffline(ctx context.Context, a audit.Actor, m Migration, opts StartOptions, snap settings.Values) (Migration, error) {
	if m.Flags.Cutover != nil && m.Flags.Cutover.EndedAt == 0 {
		return m, uerr("running", "The offline migration is already running.", "")
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	inc := included(dbs)
	op, err := o.newOp(ctx, m.ID, "offline", "", a.Username, offlineSteps(), map[string]any{"write_freeze": opts.WriteFreeze, "confirm_writers_stopped": opts.ConfirmWritersStopped})
	if err != nil {
		return m, err
	}
	now := store.Now()
	o.st.DB.ExecContext(ctx, `UPDATE migrations SET settings_snapshot=?, started_at=?, frozen_at=?, updated_at=? WHERE id=?`, toJSON(snap), now, now, now, m.ID)
	o.updateFlags(ctx, m.ID, func(f *Flags) {
		f.Cutover = &CutoverState{StartedAt: now, OpID: op.ID, Phase: "writers"}
	})
	o.audit.Append(ctx, a, "migration.start", m.ID, nil, map[string]any{"settings_snapshot": snap, "mode": "offline", "warnings_reviewed": opts.WarningsReviewed, "write_freeze": opts.WriteFreeze, "operation": op.ID})
	o.Event(ctx, m.ID, "", "started", "info", fmt.Sprintf("Offline migration started by %s with %d database(s)", a.Username, len(inc)), nil)
	o.refreshState(ctx, m.ID)
	o.runOffline(m, inc, op, a, opts, false)
	return o.GetMigration(ctx, m.ID)
}

func (o *Orchestrator) runOffline(m Migration, inc []Database, op *Operation, a audit.Actor, opts StartOptions, resumed bool) {
	cctx, cancel := context.WithCancel(context.Background())
	cutoverCancels.Store(op.ID, cancel)
	go func() {
		defer cancel()
		defer cutoverCancels.Delete(op.ID)
		defer func() {
			if r := recover(); r != nil {
				o.logf("error", "orchestrator", m.ID, "", op.ID, "offline worker crashed: %v", r)
				op.Finish(fmt.Errorf("the offline migration stopped on an internal error: %v", r), nil)
			}
		}()
		o.offlineWorker(cctx, m, inc, op, a, opts, resumed)
	}()
}

func (o *Orchestrator) offlineWorker(ctx context.Context, m Migration, dbs []Database, op *Operation, a audit.Actor, opts StartOptions, resumed bool) {
	bg := context.Background()
	src, _, err := o.Conns(bg, m)
	// Before the copy starts, a failure simply ends the run: nothing was
	// copied, the migration can be started again.
	failEarly := func(step string, err error) {
		op.StepEnd(step, "failed", err.Error())
		o.updateFlags(bg, m.ID, func(f *Flags) { f.Cutover = nil })
		for _, d := range dbs {
			o.setDB(bg, d.ID, map[string]any{"writes_baseline": nil})
		}
		if uerr := o.undoFreeze(bg, m.ID, "Upwell", "the offline migration stopped before the copy: "+err.Error()); uerr != nil {
			op.Log("error", "%v", uerr)
		}
		o.refreshState(bg, m.ID)
		op.Finish(err, nil)
	}
	if resumed {
		op.StepStart("writers", "")
		op.StepEnd("writers", "skipped", "confirmed before Upwell restarted")
	} else {
		o.setPhase(bg, m.ID, "writers")
		op.StepStart("writers", "")
		if err != nil {
			failEarly("writers", err)
			return
		}
		if opts.WriteFreeze {
			op.StepDetail("writers", "applying the write freeze", 0.1)
			if err := o.applyFreeze(bg, m, a, dbs, src, op); err != nil {
				failEarly("writers", err)
				return
			}
		}
		secs := max(o.globalSettings(bg).Int("cutover_write_sample_seconds"), 1)
		first := map[string]pgpack.Activity{}
		for _, d := range dbs {
			act, err := pgpack.WriteActivity(bg, src.WithDB(d.SourceName))
			if err != nil {
				failEarly("writers", fmt.Errorf("%s: could not sample writes: %w", d.SourceName, err))
				return
			}
			first[d.SourceName] = act
		}
		op.StepDetail("writers", fmt.Sprintf("sampling row changes for %d s", secs), 0.3)
		select {
		case <-ctx.Done():
			failEarly("writers", errors.New("the offline migration was cancelled"))
			return
		case <-time.After(time.Duration(secs) * time.Second):
		}
		var changing []string
		for _, d := range dbs {
			act, err := pgpack.WriteActivity(bg, src.WithDB(d.SourceName))
			if err != nil {
				failEarly("writers", fmt.Errorf("%s: could not sample writes: %w", d.SourceName, err))
				return
			}
			if n := act.Changes - first[d.SourceName].Changes; n != 0 {
				changing = append(changing, fmt.Sprintf("%s (%d row changes in %d s)", d.SourceName, n, secs))
			}
			if len(act.OpenWriters) > 0 {
				changing = append(changing, fmt.Sprintf("%s (%d open write transaction(s): %s)", d.SourceName, len(act.OpenWriters), strings.Join(limitStrings(act.OpenWriters, 3), "; ")))
			}
			o.setDB(bg, d.ID, map[string]any{"writes_baseline": act.Changes})
		}
		if len(changing) > 0 {
			// No override offline: a write during the copy is simply missing.
			failEarly("writers", fmt.Errorf("writes have not stopped: %s. An offline migration copies once, so every writer must stop (and open transactions end) before it starts", strings.Join(changing, ", ")))
			return
		}
		now := store.Now()
		o.updateFlags(bg, m.ID, func(f *Flags) {
			if f.Cutover != nil {
				f.Cutover.WritesStoppedAt = now
			}
		})
		op.StepEnd("writers", "done", "no row changes detected")
		o.Event(bg, m.ID, "", "writes_stopped", "info", "Writers confirmed stopped; the offline copy starts now", nil)
	}

	// The copy: the scheduler launches each database as for an online
	// migration; supervision marks it copied when the engine exits cleanly.
	o.setPhase(bg, m.ID, "copy")
	op.StepStart("copy", "")
	o.updateFlags(bg, m.ID, func(f *Flags) { f.Started = true; f.Paused = false })
	o.refreshState(bg, m.ID)
	go o.Tick(context.Background())
	copyStart := time.Now()
	for {
		cur, err := o.GetMigration(bg, m.ID)
		if err == nil && cur.Flags.Aborted {
			op.StepEnd("copy", "failed", "the migration was aborted")
			o.finishCutover(bg, m, op, a, errors.New("the migration was aborted during the copy"))
			return
		}
		all, _ := o.ListDatabases(bg, m.ID)
		done, total := 0, 0
		var broken []string
		for _, d := range included(all) {
			total++
			switch d.State {
			case DCopied:
				done++
			case DFailed, DRestartRequired, DStopped, DDegraded:
				broken = append(broken, fmt.Sprintf("%s (%s: %s)", d.SourceName, Label(d.State), firstNonEmpty(d.LastError, "no detail")))
			}
		}
		if len(broken) > 0 {
			err := fmt.Errorf("the copy did not finish for %s; restart the database from zero to copy it again, then verify again (writes must still be stopped)", strings.Join(broken, "; "))
			op.StepEnd("copy", "failed", err.Error())
			o.finishCutover(bg, m, op, a, err)
			return
		}
		if pending := countState(included(all), DPending); pending == total && time.Since(copyStart) > 10*time.Minute {
			err := errors.New("no database started copying within 10 minutes; see the app log")
			op.StepEnd("copy", "failed", err.Error())
			o.finishCutover(bg, m, op, a, err)
			return
		}
		if total > 0 && done == total {
			op.StepEnd("copy", "done", fmt.Sprintf("%d database(s) copied", total))
			break
		}
		op.StepDetail("copy", fmt.Sprintf("%d of %d database(s) copied", done, total), float64(done)/float64(max(total, 1)))
		select {
		case <-ctx.Done():
			op.StepEnd("copy", "failed", "cancelled")
			o.finishCutover(bg, m, op, a, errors.New("the offline migration was cancelled during the copy"))
			return
		case <-time.After(2 * time.Second):
		}
	}
	o.verifyPhase(bg, m, op, a)
}

// resumeOffline continues an offline migration whose worker did not survive
// a restart: the engines kept copying (or finished), and the writers step was
// done. Called once from Reconcile.
func (o *Orchestrator) resumeOffline(ctx context.Context, m Migration) {
	c := m.Flags.Cutover
	if c == nil || c.EndedAt != 0 || c.Phase == "writers" || c.Phase == "" {
		return
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	op, err := o.newOp(ctx, m.ID, "offline", "", "Upwell", offlineSteps(), map[string]any{"resumed": true})
	if err != nil {
		return
	}
	o.updateFlags(ctx, m.ID, func(f *Flags) {
		if f.Cutover != nil {
			f.Cutover.OpID = op.ID
		}
	})
	o.Event(ctx, m.ID, "", "offline_resumed", "info", "Upwell restarted during the offline migration; it continues from the "+c.Phase+" step", nil)
	o.runOffline(m, included(dbs), op, audit.Actor{Username: "Upwell"}, StartOptions{}, true)
}

func countState(dbs []Database, st string) int {
	n := 0
	for _, d := range dbs {
		if d.State == st {
			n++
		}
	}
	return n
}
