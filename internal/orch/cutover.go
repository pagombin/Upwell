package orch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/check"
	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/pgpack"
	"github.com/pagombin/upwell/internal/store"
)

// Condition is one readiness gate line.
type Condition struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail"`
	Database string `json:"database,omitempty"`
}

// Readiness evaluates the cutover readiness gate.
func (o *Orchestrator) Readiness(ctx context.Context, id string) ([]Condition, bool, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, false, err
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	v := o.Settings(ctx, m)
	stable := int64(o.globalSettings(ctx).Int("cutover_stable_seconds")) * 1000
	thr := int64(v.Int("cutover_lag_threshold_mb")) << 20
	now := store.Now()
	var out []Condition
	all := true
	out = append(out, Condition{Key: "started", Label: "Migration is streaming", OK: m.Flags.Started && !m.Flags.Aborted && !m.Flags.CleanedUp && m.Flags.Verdict == "", Detail: Label(m.State)})
	for _, d := range included(dbs) {
		c := Condition{Key: "in_sync", Label: d.SourceName + " in sync", Database: d.SourceName}
		switch {
		case d.State != DInSync:
			c.Detail = "State is " + Label(d.State)
		case d.BacklogBytes == nil || *d.BacklogBytes < 0 || *d.BacklogBytes >= thr:
			c.Detail = "Backlog is above the threshold"
		case d.InSyncSince == nil || now-*d.InSyncSince < stable:
			left := stable
			if d.InSyncSince != nil {
				left = stable - (now - *d.InSyncSince)
			}
			c.Detail = fmt.Sprintf("Stable for less than %d s (%d s to go)", stable/1000, left/1000)
		default:
			c.OK = true
			c.Detail = fmt.Sprintf("Backlog %s, stable for %d s", humanSize(*d.BacklogBytes), (now-*d.InSyncSince)/1000)
		}
		out = append(out, c)
	}
	alerts, _ := o.ListAlerts(ctx, "firing", 100)
	crit := 0
	for _, a := range alerts {
		if a.Severity == "critical" && (a.MigrationID == m.ID || a.MigrationID == "") {
			crit++
		}
	}
	out = append(out, Condition{Key: "alerts", Label: "No critical alerts firing", OK: crit == 0, Detail: fmt.Sprintf("%d critical alert(s) firing", crit)})
	hb := v.Bool("heartbeat")
	out = append(out, Condition{Key: "heartbeat", Label: "Heartbeat on (proof the last writes arrive)", OK: true, Detail: map[bool]string{true: "On", false: "Off: accepted risk, GO relies on data comparison only"}[hb]})
	for _, c := range out {
		if !c.OK {
			all = false
		}
	}
	return out, all, nil
}

// CutoverParams are the operator's inputs.
type CutoverParams struct {
	ConfirmWritersStopped bool   `json:"confirm_writers_stopped"`
	OverrideConfirm       string `json:"override_confirm"`
}

var cutoverCancels sync.Map // op id -> context.CancelFunc

// StartCutover runs the coordinated cutover as an operation.
func (o *Orchestrator) StartCutover(ctx context.Context, a audit.Actor, role, id, key string, p CutoverParams) (*Operation, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, err
	}
	if m.Fixture {
		return nil, uerr("fixture", "This is a demonstration fixture.", "")
	}
	continuing := m.Flags.Cutover != nil && m.Flags.Cutover.EndedAt == 0
	if continuing {
		if prev, err := o.GetOperation(ctx, m.Flags.Cutover.OpID); err == nil && prev.State == "running" {
			if key != "" && prev.Key == key {
				return prev, ErrDuplicate
			}
			return prev, uerr("cutover_running", "A cutover is already running.", "")
		}
	} else {
		if key != "" {
			var existing string
			if o.st.DB.QueryRowContext(ctx, `SELECT id FROM operations WHERE operation_key=?`, key).Scan(&existing) == nil {
				op, _ := o.GetOperation(ctx, existing)
				return op, ErrDuplicate
			}
		}
		if m.Flags.Verdict == "NO-GO" && noEndPositions(o, ctx, m.ID) {
			// The last cutover stopped before any end position was set: streaming
			// continued, so a new cutover may start.
			o.updateFlags(ctx, m.ID, func(f *Flags) { f.Verdict = ""; f.Cutover = nil })
			dbs, _ := o.ListDatabases(ctx, m.ID)
			for _, d := range dbs {
				o.setDB(ctx, d.ID, map[string]any{"verdict": nil, "hb_token": nil, "hb_before_lsn": nil})
			}
			m.Flags.Verdict = ""
		}
		if m.Flags.Verdict != "" {
			return nil, uerr("verdict", "This migration already has a verdict.", "Restart the failed databases to try again.")
		}
		if !p.ConfirmWritersStopped {
			return nil, uerr("confirm_writers", "Confirm that the customer has stopped every writer.", "Tick the confirmation in the cutover console.")
		}
		conds, ready, err := o.Readiness(ctx, m.ID)
		if err != nil {
			return nil, err
		}
		if !ready {
			var bad []string
			for _, c := range conds {
				if !c.OK {
					bad = append(bad, c.Label+" ("+c.Detail+")")
				}
			}
			return nil, uerr("not_ready", "The readiness gate has not passed: "+strings.Join(bad, "; ")+".", "Wait until every database is in sync and stable.")
		}
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	inc := included(dbs)
	steps := []Step{{Key: "readiness", Title: "Readiness gate"}, {Key: "writers", Title: "Confirm writers stopped"}, {Key: "heartbeat", Title: "Write final heartbeat"},
		{Key: "await", Title: "Wait for the final heartbeat on the target"}, {Key: "endpos", Title: "Set end positions and nudge"}, {Key: "drain", Title: "Drain to end positions"}, {Key: "origin", Title: "Check target origin reached the final heartbeat"},
		{Key: "sequences", Title: "Sync sequences"}, {Key: "verify", Title: "Verify data"}, {Key: "verdict", Title: "Verdict"}}
	op, err := o.newOp(ctx, m.ID, "cutover", key, a.Username, steps, map[string]any{"confirm_writers_stopped": p.ConfirmWritersStopped, "override": p.OverrideConfirm != "", "continuing": continuing})
	if err != nil {
		return op, err
	}
	o.updateFlags(ctx, m.ID, func(f *Flags) {
		if f.Cutover == nil || f.Cutover.EndedAt != 0 {
			f.Cutover = &CutoverState{StartedAt: store.Now()}
		}
		f.Cutover.OpID = op.ID
		if f.Cutover.Phase == "" {
			f.Cutover.Phase = "readiness"
		}
	})
	o.audit.Append(ctx, a, "cutover.start", m.ID, nil, map[string]any{"operation": op.ID, "confirm_writers_stopped": p.ConfirmWritersStopped, "continuing": continuing})
	o.refreshState(ctx, m.ID)
	cctx, cancel := context.WithCancel(context.Background())
	cutoverCancels.Store(op.ID, cancel)
	go func() {
		defer cancel()
		defer cutoverCancels.Delete(op.ID)
		o.cutoverWorker(cctx, m, inc, op, a, role, p)
	}()
	return op, nil
}

func (o *Orchestrator) setPhase(ctx context.Context, id, phase string) {
	o.updateFlags(ctx, id, func(f *Flags) {
		if f.Cutover != nil {
			f.Cutover.Phase = phase
		}
	})
	o.refreshState(ctx, id)
}

func (o *Orchestrator) cutoverWorker(ctx context.Context, m Migration, dbs []Database, op *Operation, a audit.Actor, role string, p CutoverParams) {
	bg := context.Background()
	fail := func(step string, err error) {
		op.StepEnd(step, "failed", err.Error())
		// Before end positions are set the cutover simply ends: streaming continues.
		f, _ := o.updateFlags(bg, m.ID, func(f *Flags) {
			if f.Cutover != nil && (f.Cutover.Phase == "readiness" || f.Cutover.Phase == "writers" || f.Cutover.Phase == "heartbeat" || f.Cutover.Phase == "await") {
				f.Cutover = nil
			}
		})
		if f.Cutover == nil {
			for _, d := range dbs {
				o.setDB(bg, d.ID, map[string]any{"hb_token": nil, "hb_before_lsn": nil})
			}
		}
		o.refreshState(bg, m.ID)
		op.Finish(err, nil)
	}
	src, dst, err := o.Conns(bg, m)
	if err != nil {
		fail("readiness", err)
		return
	}
	v := o.Settings(bg, m)
	cur, _ := o.GetMigration(bg, m.ID)
	phase := cur.Flags.Cutover.Phase
	past := func(ph string) bool {
		order := []string{"readiness", "writers", "heartbeat", "await", "endpos", "drain", "origin", "sequences", "verify", "done"}
		pi, qi := 0, 0
		for i, x := range order {
			if x == phase {
				pi = i
			}
			if x == ph {
				qi = i
			}
		}
		return pi > qi
	}

	// 1. Readiness (already checked synchronously for a fresh cutover).
	op.StepStart("readiness", "")
	if past("readiness") {
		op.StepEnd("readiness", "skipped", "continuing an interrupted cutover")
	} else {
		op.StepEnd("readiness", "done", "every database in sync and stable")
	}

	// 2. Writers stopped: list sessions, sample row changes twice.
	if !past("writers") {
		o.setPhase(bg, m.ID, "writers")
		op.StepStart("writers", "")
		secs := o.globalSettings(bg).Int("cutover_write_sample_seconds")
		if secs <= 0 {
			secs = 10
		}
		first := map[string]int64{}
		for _, d := range dbs {
			n, sessions, err := pgpack.WriteActivity(bg, src.WithDB(d.SourceName))
			if err != nil {
				fail("writers", fmt.Errorf("%s: %w", d.SourceName, err))
				return
			}
			first[d.SourceName] = n
			if len(sessions) > 0 {
				op.LogDB("warn", d.SourceName, "%d client session(s) still connected: %s", len(sessions), strings.Join(sessions, "; "))
			} else {
				op.LogDB("info", d.SourceName, "no client sessions besides the engine and Upwell")
			}
		}
		op.StepDetail("writers", fmt.Sprintf("sampling row changes for %d s", secs), 0.3)
		select {
		case <-ctx.Done():
			fail("writers", errors.New("cutover cancelled"))
			return
		case <-time.After(time.Duration(secs) * time.Second):
		}
		var changing []string
		for _, d := range dbs {
			n, _, err := pgpack.WriteActivity(bg, src.WithDB(d.SourceName))
			if err != nil {
				fail("writers", fmt.Errorf("%s: %w", d.SourceName, err))
				return
			}
			if n != first[d.SourceName] {
				changing = append(changing, fmt.Sprintf("%s (%d row changes in %d s)", d.SourceName, n-first[d.SourceName], secs))
			}
		}
		override := false
		if len(changing) > 0 {
			if role != "admin" || p.OverrideConfirm != m.Name {
				fail("writers", fmt.Errorf("rows are still changing in %s. Stop every writer, or an Admin can override by typing the migration name", strings.Join(changing, ", ")))
				return
			}
			override = true
			o.audit.Append(bg, a, "cutover.override_writes", m.ID, nil, map[string]any{"changing": changing})
			op.Log("warn", "Admin %s overrode detected writes: %s", a.Username, strings.Join(changing, ", "))
		}
		now := store.Now()
		o.updateFlags(bg, m.ID, func(f *Flags) {
			if f.Cutover != nil {
				f.Cutover.WritesStoppedAt = now
				f.Cutover.Override = override
			}
		})
		op.StepEnd("writers", "done", map[bool]string{true: "writes still detected; Admin override recorded", false: "no row changes detected"}[override])
		o.Event(bg, m.ID, "", "writes_stopped", "info", "Writers confirmed stopped; the write pause starts now", nil)
	} else {
		op.StepStart("writers", "")
		op.StepEnd("writers", "skipped", "already confirmed")
	}

	// 3. Final heartbeat (PC13).
	if !past("heartbeat") {
		o.setPhase(bg, m.ID, "heartbeat")
		op.StepStart("heartbeat", "")
		if !v.Bool("heartbeat") {
			op.StepEnd("heartbeat", "skipped", "heartbeat is off for this migration (accepted risk)")
		} else {
			for _, d := range dbs {
				cur, _ := o.GetDatabase(bg, m.ID, d.ID)
				if cur.HBToken != "" {
					continue
				}
				token := "final-" + op.ID + "-" + d.SourceName
				before, err := pgpack.WriteHeartbeat(bg, src.WithDB(d.SourceName), "final", token)
				if err != nil {
					fail("heartbeat", fmt.Errorf("%s: %w", d.SourceName, err))
					return
				}
				o.setDB(bg, d.ID, map[string]any{"hb_token": token, "hb_before_lsn": before})
				op.LogDB("info", d.SourceName, "final heartbeat written after %s", before)
			}
			op.StepEnd("heartbeat", "done", fmt.Sprintf("%d database(s)", len(dbs)))
		}
	} else {
		op.StepStart("heartbeat", "")
		op.StepEnd("heartbeat", "skipped", "already written")
	}

	// 3b. Wait until every final heartbeat is on the target before ending the
	// streams (D10). pgcopydb 0.18 can drop the last transaction before an idle
	// period when the end position is reached first (F9); nudging until the
	// heartbeat arrives makes it apply, and its arrival proves every write
	// before it arrived. Without it, the verdict is NO-GO and streaming continues.
	if v.Bool("heartbeat") && !past("await") {
		o.setPhase(bg, m.ID, "await")
		op.StepStart("await", "")
		limit := time.Duration(o.globalSettings(bg).Int("cutover_drain_timeout_seconds")) * time.Second
		if limit <= 0 || limit > 15*time.Minute {
			limit = 15 * time.Minute
		}
		deadline := time.Now().Add(limit)
		pending := map[string]Database{}
		for _, d := range dbs {
			cur, _ := o.GetDatabase(bg, m.ID, d.ID)
			pending[d.SourceName] = cur
		}
		lastNudge := time.Now().Add(-4 * time.Second)
		for len(pending) > 0 {
			if ctx.Err() != nil {
				fail("await", errors.New("cutover cancelled"))
				return
			}
			for name, d := range pending {
				if ok, err := pgpack.HeartbeatOnTarget(bg, dst.WithDB(d.TargetName), d.HBToken); err == nil && ok {
					op.LogDB("info", name, "final heartbeat is on the target")
					delete(pending, name)
				}
			}
			op.StepDetail("await", fmt.Sprintf("%d of %d final heartbeats on the target", len(dbs)-len(pending), len(dbs)), float64(len(dbs)-len(pending))/float64(max(len(dbs), 1)))
			if len(pending) == 0 {
				break
			}
			if time.Now().After(deadline) {
				var names []string
				for n := range pending {
					names = append(names, n)
				}
				err := fmt.Errorf("the final heartbeat did not reach the target for %s within %s, so changes may be missing there. End positions were not set and streaming continues; restart the affected databases from zero or investigate the engine logs, then run the cutover again", strings.Join(names, ", "), limit)
				op.StepEnd("await", "failed", err.Error())
				for name, d := range pending {
					o.setDB(bg, d.ID, map[string]any{"verdict": "NO-GO", "last_error": "NO-GO: the final heartbeat never reached the target"})
					op.LogDB("error", name, "NO-GO: the final heartbeat never reached the target")
				}
				o.finishCutover(bg, m, op, a, err)
				return
			}
			if time.Since(lastNudge) > 5*time.Second {
				// pgcopydb applies a transaction only once a later decodable one
				// arrives; a logical message does not count under pgoutput. Push with
				// a small heartbeat row (ignored by verification).
				for name, d := range pending {
					if _, err := pgpack.WriteHeartbeat(bg, src.WithDB(d.SourceName), "push", store.NewID()); err != nil {
						op.LogDB("warn", name, "heartbeat push failed: %v", err)
					}
				}
				lastNudge = time.Now()
			}
			time.Sleep(time.Second)
		}
		op.StepEnd("await", "done", "every final heartbeat is on the target")
	} else {
		op.StepStart("await", "")
		op.StepEnd("await", "skipped", map[bool]string{true: "already confirmed", false: "heartbeat is off for this migration"}[v.Bool("heartbeat")])
	}

	// 4. End positions, then an immediate nudge (PC3). No abort after this.
	o.setPhase(bg, m.ID, "endpos")
	op.StepStart("endpos", "")
	for _, d := range dbs {
		cur, _ := o.GetDatabase(bg, m.ID, d.ID)
		spec, err := o.dbSpec(bg, m, cur)
		if err != nil {
			op.StepEnd("endpos", "failed", err.Error())
			o.finishCutover(bg, m, op, a, err)
			return
		}
		if cur.EndPos == "" {
			plan, _ := o.eng.Plan(spec, true)
			lsn, err := o.eng.SetEndPosition(bg, plan, spec)
			if err != nil {
				op.StepEnd("endpos", "failed", fmt.Sprintf("%s: %v", d.SourceName, err))
				o.finishCutover(bg, m, op, a, err)
				return
			}
			o.setDB(bg, d.ID, map[string]any{"endpos": lsn, "state": DDraining})
			op.LogDB("info", d.SourceName, "end position set to %s", lsn)
		} else if cur.State == DInSync || cur.State == DCatchUp {
			o.setDB(bg, d.ID, map[string]any{"state": DDraining})
		}
	}
	if n := o.globalSettings(bg).Int("cutover_nudge_seconds"); n > 0 {
		time.Sleep(time.Duration(n) * time.Second)
	}
	for _, d := range dbs {
		spec, _ := o.dbSpec(bg, m, d)
		if err := o.eng.Nudge(bg, spec); err != nil {
			op.LogDB("warn", d.SourceName, "drain nudge failed: %v", err)
		}
	}
	op.StepEnd("endpos", "done", "end positions set; nudged every database")

	// 5. Drain.
	o.setPhase(bg, m.ID, "drain")
	op.StepStart("drain", "")
	timeout := time.Duration(o.globalSettings(bg).Int("cutover_drain_timeout_seconds")) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	lastNudge := time.Now()
	var undrained []string
	for {
		go o.Tick(bg)
		time.Sleep(2 * time.Second)
		cur, _ := o.ListDatabases(bg, m.ID)
		drained, broken := 0, []string{}
		undrained = nil
		for _, d := range included(cur) {
			switch {
			case d.State == DDrained || d.State == DVerified:
				drained++
			case d.State == DFailed || d.State == DRestartRequired || (d.State == DStopped && d.NextRetryAt == nil):
				broken = append(broken, d.SourceName+" ("+Label(d.State)+": "+d.LastError+")")
			default:
				undrained = append(undrained, d.SourceName)
			}
		}
		op.StepDetail("drain", fmt.Sprintf("%d of %d databases drained", drained, len(dbs)), float64(drained)/float64(max(len(dbs), 1)))
		if len(undrained) == 0 {
			if len(broken) > 0 {
				op.StepEnd("drain", "failed", "did not drain: "+strings.Join(broken, "; "))
			} else {
				op.StepEnd("drain", "done", fmt.Sprintf("all %d databases reached their end positions", len(dbs)))
			}
			break
		}
		if time.Now().After(deadline) {
			op.StepEnd("drain", "failed", "timed out waiting for "+strings.Join(undrained, ", "))
			break
		}
		if time.Since(lastNudge) > 60*time.Second {
			for _, d := range dbs {
				spec, _ := o.dbSpec(bg, m, d)
				o.eng.Nudge(bg, spec)
			}
			lastNudge = time.Now()
		}
	}
	o.verifyPhase(bg, m, op, a)
}

// verifyPhase runs the origin check, sequence sync, verification and verdict.
func (o *Orchestrator) verifyPhase(ctx context.Context, m Migration, op *Operation, a audit.Actor) {
	src, dst, err := o.Conns(ctx, m)
	if err != nil {
		o.finishCutover(ctx, m, op, a, err)
		return
	}
	v := o.Settings(ctx, m)
	dbs, _ := o.ListDatabases(ctx, m.ID)
	inc := included(dbs)
	hbOn := v.Bool("heartbeat")

	// 6. Origin progress (D3).
	o.setPhase(ctx, m.ID, "origin")
	op.StepStart("origin", "")
	originFail := map[string]string{}
	for _, d := range inc {
		if d.HBBeforeLSN == "" {
			if hbOn {
				originFail[d.SourceName] = "no final heartbeat position recorded"
			}
			continue
		}
		origin, err := pgpack.OriginProgress(ctx, dst, d.OriginName)
		if err != nil {
			originFail[d.SourceName] = err.Error()
			continue
		}
		o.setDB(ctx, d.ID, map[string]any{"origin_lsn": origin})
		if origin == "" || !pg.LSNGreaterOrEqual(origin, d.HBBeforeLSN) {
			originFail[d.SourceName] = fmt.Sprintf("origin at %q, final heartbeat after %s", origin, d.HBBeforeLSN)
			op.LogDB("error", d.SourceName, "NO-GO: the target applied changes only up to %q, short of the final heartbeat after %s", origin, d.HBBeforeLSN)
		} else {
			op.LogDB("info", d.SourceName, "target origin %s is past the final heartbeat at %s", origin, d.HBBeforeLSN)
		}
	}
	if len(originFail) > 0 {
		var parts []string
		for k, x := range originFail {
			parts = append(parts, k+": "+x)
		}
		op.StepEnd("origin", "failed", "NO-GO for "+strings.Join(parts, "; "))
	} else {
		op.StepEnd("origin", "done", "every database's target origin reached its final heartbeat")
	}

	// 7. Sequences.
	o.setPhase(ctx, m.ID, "sequences")
	op.StepStart("sequences", "")
	total := 0
	for _, d := range inc {
		n, err := pgpack.SyncSequences(ctx, src.WithDB(d.SourceName), dst.WithDB(d.TargetName))
		if err != nil {
			op.LogDB("warn", d.SourceName, "sequence sync: %v", err)
		}
		total += n
	}
	op.StepEnd("sequences", "done", fmt.Sprintf("%d sequence(s) synced", total))

	// 8. Verification.
	o.setPhase(ctx, m.ID, "verify")
	op.StepStart("verify", "")
	runID := op.ID
	verdicts := map[string]bool{}
	for i, d := range inc {
		op.StepDetail("verify", "verifying "+d.SourceName, float64(i)/float64(max(len(inc), 1)))
		if o.testHooks.BeforeVerify != nil {
			o.testHooks.BeforeVerify(ctx, d)
		}
		cur, _ := o.GetDatabase(ctx, m.ID, d.ID)
		res, err := pgpack.Verify(ctx, pgpack.VerifyInput{Source: src.WithDB(d.SourceName), Target: dst.WithDB(d.TargetName), TargetMaint: dst, Origin: d.OriginName,
			HeartbeatRequired: hbOn, HeartbeatToken: cur.HBToken, HBBeforeLSN: cur.HBBeforeLSN, OriginLSN: cur.OriginLSN,
			ExactCountMaxBytes: int64(v.Int("exact_count_max_mb")) << 20, ChecksumBudget: time.Duration(v.Int("verify_checksum_budget_seconds")) * time.Second, Database: d.SourceName})
		if err != nil {
			res.GO = false
			res.Results = append(res.Results, check.Result{CheckID: "verify_error", Level: check.Blocker, Hard: true, Title: "Verification completed", Message: err.Error(), Database: d.SourceName})
		}
		if msg, bad := originFail[d.SourceName]; bad {
			res.GO = false
			_ = msg
		}
		for _, r := range res.Results {
			o.st.DB.ExecContext(ctx, `INSERT INTO verifications(id,migration_id,database,run_id,check_id,result,title,detail,ts) VALUES (?,?,?,?,?,?,?,?,?)`,
				store.NewID(), m.ID, d.SourceName, runID, r.CheckID, r.Level, r.Title, toJSON(map[string]any{"message": r.Message, "evidence": r.Evidence}), store.Now())
			lvl := "info"
			if r.Level == check.Blocker {
				lvl = "error"
			} else if r.Level == check.Warning {
				lvl = "warn"
			}
			op.LogDB(lvl, d.SourceName, "%s: %s", r.Title, r.Message)
		}
		verdicts[d.SourceName] = res.GO
		state, verdict, lastErr := DVerified, "GO", ""
		if !res.GO {
			state, verdict = DFailed, "NO-GO"
			var why []string
			for _, r := range res.Results {
				if r.Level == check.Blocker {
					why = append(why, r.Title)
				}
			}
			if m, bad := originFail[d.SourceName]; bad {
				why = append([]string{"target origin did not reach the final heartbeat (" + m + ")"}, why...)
			}
			lastErr = "NO-GO: " + strings.Join(why, "; ")
		}
		o.setDB(ctx, d.ID, map[string]any{"state": state, "verdict": verdict, "last_error": store.NullString(lastErr)})
	}
	op.StepEnd("verify", "done", "")
	o.finishCutover(ctx, m, op, a, nil)
}

// finishCutover computes the verdict from the stored per-database verdicts.
func (o *Orchestrator) finishCutover(ctx context.Context, m Migration, op *Operation, a audit.Actor, ferr error) {
	op.StepStart("verdict", "")
	dbs, _ := o.ListDatabases(ctx, m.ID)
	goAll := ferr == nil
	var failed []string
	for _, d := range included(dbs) {
		if d.Verdict != "GO" {
			goAll = false
			failed = append(failed, d.SourceName)
			if d.Verdict == "" {
				o.setDB(ctx, d.ID, map[string]any{"verdict": "NO-GO"})
			}
		}
	}
	verdict := "NO-GO"
	if goAll {
		verdict = "GO"
	}
	now := store.Now()
	f, _ := o.updateFlags(ctx, m.ID, func(f *Flags) {
		f.Verdict = verdict
		f.VerdictAt = now
		f.VerifyRunID = op.ID
		if f.Cutover != nil {
			f.Cutover.EndedAt = now
			f.Cutover.Verdict = verdict
			f.Cutover.Phase = "done"
			if f.Cutover.WritesStoppedAt > 0 {
				f.Cutover.WritePauseMS = now - f.Cutover.WritesStoppedAt
			}
		}
	})
	pause := int64(0)
	if f.Cutover != nil {
		pause = f.Cutover.WritePauseMS
	}
	if verdict == "GO" {
		op.StepEnd("verdict", "done", fmt.Sprintf("GO: every database verified; write pause %s", (time.Duration(pause) * time.Millisecond).Round(time.Second)))
		o.Event(ctx, m.ID, "", "verdict", "info", fmt.Sprintf("Cutover verdict GO: every database verified (write pause %s)", (time.Duration(pause) * time.Millisecond).Round(time.Second)), map[string]any{"verdict": verdict})
		if o.Settings(ctx, m).Bool("analyze_after_go") {
			_, dst, _ := o.Conns(ctx, m)
			for _, d := range included(dbs) {
				if c, err := pg.Connect(ctx, dst.WithDB(d.TargetName), 0); err == nil {
					c.Exec(ctx, "ANALYZE")
					c.Close(ctx)
				}
			}
		}
	} else {
		detail := "NO-GO for " + strings.Join(failed, ", ")
		if ferr != nil {
			detail = "NO-GO: " + ferr.Error()
		}
		op.StepEnd("verdict", "failed", detail)
		o.Event(ctx, m.ID, "", "verdict", "critical", "Cutover verdict NO-GO: "+detail+". Applications must keep using the source.", map[string]any{"verdict": verdict, "failed": failed})
	}
	o.audit.Append(ctx, a, "cutover.verdict", m.ID, nil, map[string]any{"verdict": verdict, "failed": failed, "write_pause_ms": pause})
	o.refreshState(ctx, m.ID)
	res := map[string]any{"verdict": verdict, "failed": failed, "write_pause_ms": pause}
	if verdict == "GO" {
		op.Finish(nil, res)
	} else {
		op.Finish(fmt.Errorf("verdict NO-GO for %s", strings.Join(failed, ", ")), res)
	}
}

// AbortCutover cancels a cutover before end positions are set.
func (o *Orchestrator) AbortCutover(ctx context.Context, a audit.Actor, id string) error {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return err
	}
	c := m.Flags.Cutover
	if c == nil || c.EndedAt != 0 {
		return uerr("no_cutover", "No cutover is in progress.", "")
	}
	if c.Phase != "readiness" && c.Phase != "writers" && c.Phase != "heartbeat" && c.Phase != "await" {
		return uerr("too_late", "End positions are set; the cutover cannot be aborted now.", "Wait for the verdict.")
	}
	if cancel, ok := cutoverCancels.Load(c.OpID); ok {
		cancel.(context.CancelFunc)()
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	for _, d := range dbs {
		o.setDB(ctx, d.ID, map[string]any{"hb_token": nil, "hb_before_lsn": nil})
	}
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.Cutover = nil })
	o.audit.Append(ctx, a, "cutover.abort", m.ID, nil, nil)
	o.Event(ctx, m.ID, "", "cutover_aborted", "warning", "Cutover aborted by "+a.Username+" before end positions were set; streaming continues", nil)
	o.refreshState(ctx, m.ID)
	return nil
}

// Reverify re-runs the origin check and verification after a drain.
func (o *Orchestrator) Reverify(ctx context.Context, a audit.Actor, id, key string) (*Operation, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, err
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	for _, d := range included(dbs) {
		if d.EndPos == "" || (d.State != DDrained && d.State != DVerified && d.State != DFailed) {
			return nil, uerr("not_drained", "Verification runs after the cutover drained every database.", "")
		}
	}
	steps := []Step{{Key: "origin", Title: "Check target origin reached the final heartbeat"}, {Key: "sequences", Title: "Sync sequences"}, {Key: "verify", Title: "Verify data"}, {Key: "verdict", Title: "Verdict"}}
	op, err := o.newOp(ctx, m.ID, "verify", key, a.Username, steps, nil)
	if err != nil {
		return op, err
	}
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.Verifying = true; f.Verdict = "" })
	for _, d := range included(dbs) {
		o.setDB(ctx, d.ID, map[string]any{"verdict": nil})
	}
	o.audit.Append(ctx, a, "verify.run", m.ID, nil, map[string]any{"operation": op.ID})
	go func() {
		bg := context.Background()
		o.verifyPhase(bg, m, op, a)
		o.updateFlags(bg, m.ID, func(f *Flags) { f.Verifying = false })
		o.refreshState(bg, m.ID)
	}()
	return op, nil
}

// VerificationRow is one stored verification result.
type VerificationRow struct {
	Database string          `json:"database"`
	CheckID  string          `json:"check_id"`
	Result   string          `json:"result"`
	Title    string          `json:"title"`
	Detail   json.RawMessage `json:"detail"`
	TS       int64           `json:"ts"`
}

// Verification returns the latest verification run's results.
func (o *Orchestrator) Verification(ctx context.Context, id string) (string, []VerificationRow, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return "", nil, err
	}
	run := m.Flags.VerifyRunID
	if run == "" {
		o.st.DB.QueryRowContext(ctx, `SELECT run_id FROM verifications WHERE migration_id=? ORDER BY ts DESC LIMIT 1`, m.ID).Scan(&run)
	}
	rows, err := o.st.DB.QueryContext(ctx, `SELECT database, check_id, result, title, detail, ts FROM verifications WHERE migration_id=? AND run_id=? ORDER BY database, ts`, m.ID, run)
	if err != nil {
		return run, nil, err
	}
	defer rows.Close()
	out := []VerificationRow{}
	for rows.Next() {
		var r VerificationRow
		var detail string
		rows.Scan(&r.Database, &r.CheckID, &r.Result, &r.Title, &detail, &r.TS)
		r.Detail = json.RawMessage(detail)
		out = append(out, r)
	}
	return run, out, nil
}

var _ = engine.FailNone

func noEndPositions(o *Orchestrator, ctx context.Context, id string) bool {
	dbs, _ := o.ListDatabases(ctx, id)
	for _, d := range included(dbs) {
		if d.EndPos != "" {
			return false
		}
	}
	return true
}
