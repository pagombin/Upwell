package orch

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"syscall"
	"time"

	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/store"
)

// Run is the orchestrator loop: reconcile once, then supervise every sample
// interval until ctx ends.
func (o *Orchestrator) Run(ctx context.Context) {
	o.Reconcile(ctx)
	go o.metrics.rollupLoop(ctx)
	for {
		o.Tick(ctx)
		iv := o.globalSettings(ctx).Int("sample_interval_seconds")
		if iv < 2 {
			iv = 5
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(iv) * time.Second):
		}
	}
}

// LastLoop returns when the loop last completed a tick (for the watchdog).
func (o *Orchestrator) LastLoop() time.Time {
	o.loopMu.Lock()
	defer o.loopMu.Unlock()
	return o.lastLoop
}

var tickMu = make(chan struct{}, 1)

// Tick supervises every active migration once.
func (o *Orchestrator) Tick(ctx context.Context) {
	select {
	case tickMu <- struct{}{}:
	default:
		return // a tick is already running
	}
	defer func() { <-tickMu }()
	defer func() {
		if r := recover(); r != nil {
			o.logf("error", "orchestrator", "", "", "", "supervision panicked: %v", r)
		}
	}()
	ms, err := o.ListMigrations(ctx)
	if err == nil {
		for _, m := range ms {
			if m.Fixture || !m.Flags.Started || m.Flags.CleanedUp {
				continue
			}
			o.superviseMigration(ctx, m)
		}
	}
	o.metrics.hostSample(ctx)
	o.loopMu.Lock()
	o.lastLoop = time.Now()
	o.loopMu.Unlock()
	if o.Heartbeat != nil {
		o.Heartbeat("orchestrator")
	}
}

func (o *Orchestrator) superviseMigration(ctx context.Context, m Migration) {
	unlock := o.lock(m.ID)
	defer unlock()
	m, err := o.GetMigration(ctx, m.ID)
	if err != nil {
		return
	}
	dbs, err := o.ListDatabases(ctx, m.ID)
	if err != nil {
		return
	}
	v := o.Settings(ctx, m)
	for _, d := range included(dbs) {
		if m.Flags.Aborted {
			break
		}
		o.superviseDB(ctx, m, d)
	}
	if !m.Flags.Aborted && !m.Flags.Paused {
		o.schedule(ctx, m, v.Int("max_concurrent_base_copies"))
		o.retries(ctx, m)
	}
	dbs, _ = o.ListDatabases(ctx, m.ID)
	o.metrics.sample(ctx, m, included(dbs))
	o.alerts.evaluate(ctx, m, included(dbs))
	o.refreshState(ctx, m.ID)
	o.bus.Publish("migration", "progress", m.ID, map[string]any{"databases": included(dbs)})
}

// superviseDB checks one database's unit, progress and log.
func (o *Orchestrator) superviseDB(ctx context.Context, m Migration, d Database) {
	if !activeDB[d.State] {
		return
	}
	if d.State == DPreparing {
		return // launch is in progress in this tick or another goroutine
	}
	spec, err := o.dbSpec(ctx, m, d)
	if err != nil {
		return
	}
	plan, _ := o.eng.Plan(spec, d.BaseCopyDone)
	// New log lines: classify since the attempt started.
	o.tails.attach(m.ID, d.SourceName, plan.LogFile, o.attemptNumber(ctx, d.ID))
	classes := o.tails.poll(m.ID, d.SourceName)
	st, _ := o.run.Status(ctx, d.Instance)

	if st.Active {
		// Degraded: main process dead while the unit still has processes.
		if !st.MainLive && len(st.Procs) > 0 && st.MainPID > 0 {
			o.Event(ctx, m.ID, d.SourceName, "degraded", "critical", fmt.Sprintf("%s is degraded: the engine's main process died but %d sub-process(es) still run; stopping the unit", d.SourceName, len(st.Procs)), nil)
			o.setDB(ctx, d.ID, map[string]any{"state": DDegraded})
			o.stopUnit(ctx, m, d)
			o.endAttempt(ctx, d, nil, "degraded: main process died")
			o.afterExit(ctx, m, d, engine.FailTransient, "the engine's main process died")
			return
		}
		if classes.slotLost {
			o.stopUnit(ctx, m, d)
			o.endAttempt(ctx, d, nil, "replication slot invalidated")
			o.afterExit(ctx, m, d, engine.FailSlotLost, classes.lastError)
			return
		}
		// F10: a failed base copy leaves the run streaming. Detect it from the log.
		if !d.BaseCopyDone && (classes.baseCopyFailed || classes.errors > 0) {
			o.Event(ctx, m.ID, d.SourceName, "base_copy_failed", "critical", "The base copy of "+d.SourceName+" failed ("+classes.lastError+"); stopping the unit, because pgcopydb keeps streaming after such a failure", nil)
			o.stopUnit(ctx, m, d)
			o.endAttempt(ctx, d, nil, "base copy failed: "+classes.lastError)
			o.afterExit(ctx, m, d, engine.FailBaseCopy, classes.lastError)
			return
		}
		pr, err := o.eng.Observe(ctx, plan, spec)
		if err != nil {
			return
		}
		cols := map[string]any{"write_lsn": pr.WriteLSN, "replay_lsn": pr.ReplayLSN}
		if pr.ApplyEnabled && !d.BaseCopyDone {
			cols["base_copy_done"] = 1
			d.BaseCopyDone = true
			o.Event(ctx, m.ID, d.SourceName, "base_copy_done", "info", "Base copy of "+d.SourceName+" finished; streaming changes", nil)
		}
		if d.BaseCopyDone && d.State != DDraining {
			backlog := o.backlog(ctx, spec, pr.ReplayLSN)
			cols["backlog_bytes"] = backlog
			thr := int64(o.Settings(ctx, m).Int("cutover_lag_threshold_mb")) << 20
			if thr == 0 {
				thr = 16 << 20
			}
			if backlog >= 0 && backlog < thr {
				if d.State != DInSync {
					cols["state"] = DInSync
					cols["in_sync_since"] = store.Now()
					o.Event(ctx, m.ID, d.SourceName, "db_state", "info", d.SourceName+" is in sync", nil)
				}
			} else if d.State != DCatchUp {
				cols["state"] = DCatchUp
				cols["in_sync_since"] = nil
			}
		}
		o.setDB(ctx, d.ID, cols)
		o.maybeHeartbeat(ctx, m, d, spec)
		return
	}

	// The unit is no longer running.
	var reason string
	cls := engine.FailNone
	switch {
	case classes.endposReached || (d.State == DDraining && classes.errors == 0):
		cls = engine.FailEndposReached
		reason = "reached its end position"
	case classes.slotLost:
		cls, reason = engine.FailSlotLost, classes.lastError
	case !d.BaseCopyDone:
		cls, reason = engine.FailBaseCopy, firstNonEmpty(classes.lastError, "the engine exited during the base copy")
	case classes.permanent:
		cls, reason = engine.FailPermanent, classes.lastError
	default:
		cls, reason = engine.FailTransient, firstNonEmpty(classes.lastError, "the engine exited unexpectedly")
	}
	o.endAttempt(ctx, d, st.ExitCode, reason)
	if st.ExitCode != nil {
		reason = fmt.Sprintf("%s (exit code %d)", reason, *st.ExitCode)
	}
	o.afterExit(ctx, m, d, cls, reason)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (o *Orchestrator) attemptNumber(ctx context.Context, dbID string) int {
	var n int
	o.st.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(number),0) FROM attempts WHERE database_id=?`, dbID).Scan(&n)
	return n
}

// afterExit applies the retry policy to a database whose unit ended.
func (o *Orchestrator) afterExit(ctx context.Context, m Migration, d Database, cls, reason string) {
	o.tails.reset(m.ID, d.SourceName)
	switch cls {
	case engine.FailEndposReached:
		if d.State == DDraining {
			o.setDB(ctx, d.ID, map[string]any{"state": DDrained})
			o.Event(ctx, m.ID, d.SourceName, "db_state", "info", d.SourceName+" reached its end position", nil)
			return
		}
		// An end position reached outside a cutover: treat as stopped.
		o.setDB(ctx, d.ID, map[string]any{"state": DStopped, "last_error": "the engine reached an end position outside a cutover"})
	case engine.FailSlotLost:
		o.setDB(ctx, d.ID, map[string]any{"state": DRestartRequired, "error_class": cls, "last_error": "The replication slot was invalidated (wal_status lost): " + reason})
		o.Event(ctx, m.ID, d.SourceName, "slot_lost", "critical", d.SourceName+": the replication slot was invalidated, so this database must restart from zero. Upwell never does this automatically.", map[string]any{"reason": reason})
	case engine.FailBaseCopy:
		v := o.globalSettings(ctx)
		if v.Bool("auto_restart_base_copy") && d.SizeBytes <= int64(v.Int("auto_restart_base_copy_max_gb"))<<30 {
			o.Event(ctx, m.ID, d.SourceName, "auto_restart", "warning", d.SourceName+": base copy failed ("+reason+"); restarting from zero automatically (under the size limit)", nil)
			spec, err := o.dbSpec(ctx, m, d)
			if err == nil {
				o.eng.Cleanup(ctx, spec, engine.CleanupOptions{RemoveWorkDir: true})
				resetTargetDB(ctx, spec)
				o.setDB(ctx, d.ID, map[string]any{"state": DPending, "base_copy_done": 0, "last_error": reason, "error_class": cls})
				return
			}
		}
		o.setDB(ctx, d.ID, map[string]any{"state": DRestartRequired, "error_class": cls, "last_error": "The base copy failed: " + reason})
		o.Event(ctx, m.ID, d.SourceName, "base_copy_failed", "critical", d.SourceName+": the base copy failed ("+reason+"). Restart it from zero; resuming a base copy cannot guarantee consistency.", nil)
	case engine.FailPermanent:
		o.setDB(ctx, d.ID, map[string]any{"state": DFailed, "error_class": cls, "last_error": reason})
		o.Event(ctx, m.ID, d.SourceName, "failed", "critical", d.SourceName+" failed: "+reason, nil)
	default:
		// Transient during CDC: automatic resume with backoff.
		v := o.globalSettings(ctx)
		now := store.Now()
		window := int64(0)
		if d.RetryWindow != nil {
			window = *d.RetryWindow
		}
		count := d.RetryCount
		if now-window > int64(time.Hour/time.Millisecond) {
			window, count = now, 0
		}
		maxPerHour := v.Int("retry_max_per_hour")
		if count >= maxPerHour {
			o.setDB(ctx, d.ID, map[string]any{"state": DFailed, "error_class": cls, "last_error": fmt.Sprintf("%s; %d automatic resumes in the last hour, so Upwell stopped retrying", reason, count), "next_retry_at": nil})
			o.Event(ctx, m.ID, d.SourceName, "failed", "critical", fmt.Sprintf("%s needs attention: %d automatic resumes in an hour (%s)", d.SourceName, count, reason), nil)
			return
		}
		base := float64(v.Int("retry_base_seconds"))
		cp := float64(v.Int("retry_cap_seconds"))
		delay := base
		for i := 0; i < count; i++ {
			delay *= 2
		}
		if delay > cp {
			delay = cp
		}
		delay = delay * (0.8 + 0.4*rand.Float64())
		next := now + int64(delay*1000)
		o.setDB(ctx, d.ID, map[string]any{"state": DStopped, "error_class": cls, "last_error": reason, "next_retry_at": next, "retry_count": count + 1, "retry_window_start": window})
		o.Event(ctx, m.ID, d.SourceName, "retry", "warning", fmt.Sprintf("%s stopped (%s); resuming automatically in %.0f s (attempt %d of %d this hour)", d.SourceName, reason, delay, count+1, maxPerHour), nil)
	}
}

// retries resumes databases whose automatic retry is due.
func (o *Orchestrator) retries(ctx context.Context, m Migration) {
	dbs, _ := o.ListDatabases(ctx, m.ID)
	now := store.Now()
	for _, d := range included(dbs) {
		if d.State == DStopped && d.NextRetryAt != nil && *d.NextRetryAt <= now {
			if err := o.resumeDB(ctx, m, d, "automatic resume"); err != nil {
				next := now + 30000
				o.setDB(ctx, d.ID, map[string]any{"next_retry_at": next, "last_error": "automatic resume failed: " + err.Error()})
				o.logf("warn", "orchestrator", m.ID, d.SourceName, "", "automatic resume failed: %v", err)
			}
		}
	}
}

// schedule starts pending databases up to the concurrency limit.
func (o *Orchestrator) schedule(ctx context.Context, m Migration, limit int) {
	if limit < 1 {
		limit = 4
	}
	if m.Flags.Cutover != nil && m.Flags.Cutover.EndedAt == 0 {
		return
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	running := 0
	for _, d := range included(dbs) {
		if d.State == DPreparing || d.State == DBaseCopy {
			running++
		}
	}
	for _, d := range included(dbs) {
		if running >= limit {
			return
		}
		if d.State == DPending {
			running++
			if err := o.launch(ctx, m, d); err != nil {
				o.logf("error", "orchestrator", m.ID, d.SourceName, "", "launch failed: %v", err)
			}
		}
	}
}

// dbFailed marks a database failed with a class.
func (o *Orchestrator) dbFailed(ctx context.Context, m Migration, d Database, cls, msg string) error {
	st := DFailed
	if cls == engine.FailTransient {
		st = DRestartRequired
	}
	o.setDB(ctx, d.ID, map[string]any{"state": st, "error_class": cls, "last_error": msg})
	o.Event(ctx, m.ID, d.SourceName, "failed", "critical", d.SourceName+": "+msg, nil)
	return fmt.Errorf("%s", msg)
}

// backlog is the source's current WAL position minus the engine's replay
// position (progress, not proof).
func (o *Orchestrator) backlog(ctx context.Context, spec engine.DatabaseSpec, replay string) int64 {
	if replay == "" || replay == "0/0" {
		return -1
	}
	cur, err := o.metrics.currentLSN(ctx, spec.SourceConn.WithDB(spec.Source))
	if err != nil {
		return -1
	}
	return pg.LSNDiff(cur, replay)
}

// maybeHeartbeat writes a periodic heartbeat every 10 s while streaming.
func (o *Orchestrator) maybeHeartbeat(ctx context.Context, m Migration, d Database, spec engine.DatabaseSpec) {
	if !o.Settings(ctx, m).Bool("heartbeat") || !d.BaseCopyDone || d.State == DDraining {
		return
	}
	if m.Flags.Cutover != nil && m.Flags.Cutover.EndedAt == 0 {
		return // no periodic heartbeats once a cutover begins
	}
	if !o.metrics.dueHeartbeat(d.ID) {
		return
	}
	c, err := pg.Connect(ctx, spec.SourceConn.WithDB(spec.Source), 10*time.Second)
	if err != nil {
		return
	}
	defer c.Close(ctx)
	c.Exec(ctx, `INSERT INTO upwell.heartbeat(kind, token) VALUES ('periodic', $1)`, store.NewID())
	c.Exec(ctx, `DELETE FROM upwell.heartbeat WHERE kind='periodic' AND written_at < now() - interval '1 hour'`)
}

// Reconcile runs once at startup: re-attach to running units, finish or roll
// back open intents, and resume what is safe after a reboot.
func (o *Orchestrator) Reconcile(ctx context.Context) {
	defer func() { o.Reconciled = true }()
	boot := bootID()
	var last string
	o.st.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='_boot_id'`).Scan(&last)
	o.Rebooted = last != "" && last != `"`+boot+`"`
	o.st.DB.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES ('_boot_id',?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, `"`+boot+`"`, store.Now())
	// Open intents: record what the real world shows now.
	rows, err := o.st.DB.QueryContext(ctx, `SELECT id, action, target FROM intents WHERE outcome IS NULL`)
	if err == nil {
		type it struct{ id, action, target string }
		var open []it
		for rows.Next() {
			var x it
			rows.Scan(&x.id, &x.action, &x.target)
			open = append(open, x)
		}
		rows.Close()
		for _, x := range open {
			o.st.DB.ExecContext(ctx, `UPDATE intents SET outcome=?, outcome_at=? WHERE id=?`, "reconciled after restart", store.Now(), x.id)
		}
	}
	// Interrupted operations.
	o.st.DB.ExecContext(ctx, `UPDATE operations SET state='interrupted', ended_at=?, error='The app restarted while this operation ran; run it again to continue.' WHERE state='running'`, store.Now())
	ms, _ := o.ListMigrations(ctx)
	g := o.globalSettings(ctx)
	for _, m := range ms {
		if m.Fixture || !m.Flags.Started || m.Flags.CleanedUp {
			continue
		}
		o.updateFlags(ctx, m.ID, func(f *Flags) {
			f.PreflightRunning = false
			f.Verifying = false
			f.CleanupRunning = false
		})
		dbs, _ := o.ListDatabases(ctx, m.ID)
		var resumed, attention []string
		for _, d := range included(dbs) {
			if !activeDB[d.State] {
				continue
			}
			st, _ := o.run.Status(ctx, d.Instance)
			if st.Active {
				// Engines outlive the app: re-attach the log tailer at its stored offset.
				o.tails.attach(m.ID, d.SourceName, o.runLog(d), o.attemptNumber(ctx, d.ID))
				continue
			}
			if !o.Rebooted {
				continue // supervision classifies the exit from the log
			}
			o.endAttempt(ctx, d, nil, "droplet rebooted")
			switch {
			case d.State == DPreparing || !d.BaseCopyDone:
				o.setDB(ctx, d.ID, map[string]any{"state": DRestartRequired, "last_error": "The droplet rebooted during the base copy."})
				attention = append(attention, d.SourceName)
			case g.Bool("auto_resume_after_reboot") && !m.Flags.Paused && !m.Flags.Aborted:
				o.setDB(ctx, d.ID, map[string]any{"state": DStopped, "next_retry_at": store.Now(), "last_error": "The droplet rebooted; resuming once the clusters are reachable."})
				resumed = append(resumed, d.SourceName)
			default:
				o.setDB(ctx, d.ID, map[string]any{"state": DStopped, "last_error": "The droplet rebooted; resume when ready."})
				attention = append(attention, d.SourceName)
			}
		}
		if o.Rebooted {
			o.Event(ctx, m.ID, "", "rebooted", "warning", fmt.Sprintf("The droplet rebooted. Resuming: %v. Needs attention: %v.", resumed, attention), map[string]any{"resumed": resumed, "attention": attention})
		} else {
			o.Event(ctx, m.ID, "", "app_restarted", "info", "Console restarted at "+time.Now().UTC().Format("15:04")+" UTC; migrations continued without interruption", nil)
		}
		o.refreshState(ctx, m.ID)
	}
}

func (o *Orchestrator) runLog(d Database) string {
	return o.cfg.RunsDir() + "/" + d.Instance + "/engine.log"
}

func diskUsage(path string) (pct float64, free int64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	total := float64(st.Blocks) * float64(st.Bsize)
	avail := float64(st.Bavail) * float64(st.Bsize)
	if total == 0 {
		return 0, 0, nil
	}
	return (total - avail) / total * 100, int64(avail), nil
}

var _ = os.Getpid
