package orch

import (
	"context"
	"fmt"
	"strings"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/engine"
)

// D105: run a migration again with the same definition: connections,
// permission record, settings and databases. Whatever state it is in (running,
// paused, aborted, after a verdict), its engines stop, its slots,
// publications, origins, heartbeat objects and work directories are removed,
// and its target databases are reset, then it is a definition again:
// databases and options can change, and preflight and Start run it anew.
// The finished run is kept in the run history first.
//
// Safeguards:
//   - never once applications were marked as switched to the target: the
//     target is then the customer's live database;
//   - never during a cutover;
//   - a write freeze is undone first, and nothing happens if it cannot be;
//   - a target database is dropped only when everything in it came from the
//     source (resetTargetDB); otherwise only the source's tables are dropped.

// RerunOptions selects what a re-run does.
type RerunOptions struct {
	Confirm    string `json:"confirm"`
	KeepTarget bool   `json:"keep_target"` // leave the target databases as they are; the next launch overwrites what came from the source
}

// Rerun resets a started migration to its definition. Runs as an operation.
func (o *Orchestrator) Rerun(ctx context.Context, a audit.Actor, id, key string, opts RerunOptions) (*Operation, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, err
	}
	if opts.Confirm != m.Name {
		return nil, uerr("confirm", "Type the migration name to confirm running it again.", "")
	}
	switch {
	case m.Fixture:
		return nil, uerr("fixture", "Demonstration data cannot run.", "")
	case !m.Flags.Started:
		return nil, uerr("not_started", "This migration has not started; start it instead.", "")
	case m.Flags.AppsSwitched:
		return nil, uerr("apps_switched", "Applications were marked as switched to the target, so its databases are live; Upwell will not reset them.", "Create a new migration to migrate again.")
	case m.Flags.Cutover != nil && m.Flags.Cutover.EndedAt == 0:
		return nil, uerr("cutover_running", "A cutover is in progress.", "Wait for its verdict, or abort it first.")
	case m.Flags.CleanupRunning:
		return nil, uerr("cleanup_running", "Clean up is running.", "Wait for it to finish.")
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	inc := included(dbs)
	steps := []Step{{Key: "history", Title: "Record the finished run"}, {Key: "stop", Title: "Stop the engines"}, {Key: "freeze", Title: "Undo the write freeze"}}
	for _, d := range inc {
		steps = append(steps, Step{Key: "db:" + d.SourceName, Title: "Reset " + d.SourceName})
	}
	steps = append(steps, Step{Key: "reset", Title: "Return to the definition"})
	op, err := o.newOp(ctx, m.ID, "rerun", key, a.Username, steps, map[string]any{"keep_target": opts.KeepTarget})
	if err != nil {
		return op, err
	}
	o.audit.Append(ctx, a, "migration.rerun", m.ID, map[string]any{"state": m.State, "verdict": m.Flags.Verdict}, map[string]any{"operation": op.ID, "keep_target": opts.KeepTarget})
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.CleanupRunning = true })
	go func() {
		bg := context.Background()
		unlock := o.lock(m.ID) // the supervisor skips the migration meanwhile
		defer unlock()
		fail := func(step string, err error) {
			op.StepEnd(step, "failed", err.Error())
			o.updateFlags(bg, m.ID, func(f *Flags) { f.CleanupRunning = false })
			op.Finish(fmt.Errorf("running again stopped at %q: %w; the migration is unchanged from there on", step, err), nil)
		}
		op.StepStart("history", "")
		outcome := m.Flags.Verdict
		if outcome == "" {
			outcome = map[bool]string{true: "aborted", false: "replaced"}[m.Flags.Aborted]
		}
		o.archiveRun(bg, m.ID, outcome)
		op.StepEnd("history", "done", "outcome "+outcome)

		op.StepStart("stop", "")
		var stopped []string
		for _, d := range inc {
			o.setDB(bg, d.ID, map[string]any{"next_retry_at": nil})
			if activeDB[d.State] || d.State == DStopped {
				if _, err := o.stopUnit(bg, m, d); err != nil {
					fail("stop", fmt.Errorf("%s: %w", d.SourceName, err))
					return
				}
				o.endAttempt(bg, d, nil, "run again by "+a.Username)
				stopped = append(stopped, d.SourceName)
			}
		}
		op.StepEnd("stop", "done", strings.Join(stopped, ", "))

		op.StepStart("freeze", "")
		if err := o.undoFreeze(bg, m.ID, a.Username, "the migration is being run again"); err != nil {
			fail("freeze", err)
			return
		}
		op.StepEnd("freeze", "done", "")

		for _, d := range inc {
			step := "db:" + d.SourceName
			op.StepStart(step, "")
			spec, err := o.dbSpec(bg, m, d)
			if err != nil {
				fail(step, fmt.Errorf("%w (clean up removed the stored passwords: enter them again in the migration's connections, or create a new migration)", err))
				return
			}
			did, err := o.eng.Cleanup(bg, spec, engine.CleanupOptions{DropHeartbeat: true, RemoveWorkDir: true})
			if err != nil {
				fail(step, err)
				return
			}
			if !opts.KeepTarget {
				if err := resetTargetDB(bg, spec); err != nil {
					fail(step, err)
					return
				}
				did = append(did, "reset target database "+d.TargetName)
			}
			o.setDB(bg, d.ID, map[string]any{"state": DPending, "base_copy_done": 0, "last_error": nil, "error_class": nil, "next_retry_at": nil, "retry_count": 0,
				"hb_before_lsn": nil, "hb_token": nil, "endpos": nil, "origin_lsn": nil, "verdict": nil, "in_sync_since": nil, "replay_lsn": nil, "write_lsn": nil,
				"drained": 0, "auto_restarts": 0, "writes_baseline": nil, "backlog_bytes": nil})
			op.StepEnd(step, "done", strings.Join(did, "; "))
		}

		op.StepStart("reset", "")
		o.updateFlags(bg, m.ID, func(f *Flags) {
			*f = Flags{RolesCopied: f.RolesCopied}
		})
		o.st.DB.ExecContext(bg, `UPDATE migrations SET started_at=NULL, settings_snapshot=NULL WHERE id=?`, m.ID)
		o.Event(bg, m.ID, "", "rerun", "warning", fmt.Sprintf("%s reset the migration to run it again (previous run: %s); run preflight, then start", a.Username, outcome), map[string]any{"outcome": outcome})
		o.refreshState(bg, m.ID)
		op.StepEnd("reset", "done", "")
		op.Finish(nil, map[string]any{"databases": len(inc), "previous_outcome": outcome})
	}()
	return op, nil
}
