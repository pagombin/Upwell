package orch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	Key      string `json:"key"`
	Label    string `json:"label"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
	Database string `json:"database,omitempty"`
	// SampleAgeMS is how old the measurement behind the condition is (nil
	// when it is not a measurement).
	SampleAgeMS *int64 `json:"sample_age_ms,omitempty"`
}

func ageMS(at time.Time) *int64 {
	ms := time.Since(at).Milliseconds()
	return &ms
}

func ago(at time.Time) string {
	return humanDuration(time.Since(at).Seconds()) + " ago"
}

// Readiness evaluates the cutover readiness gate.
func (o *Orchestrator) Readiness(ctx context.Context, id string) ([]Condition, bool, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, false, err
	}
	dbs, err := o.ListDatabases(ctx, m.ID)
	if err != nil {
		// Not "every database ready" (an empty list would be).
		return nil, false, err
	}
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
		// Lag in time, from the heartbeat: bytes of backlog can hide a long
		// apply (pgcopydb applies a few transactions a second, F11), and the
		// write pause lasts at least as long as the target is behind.
		// Measurements older than three collection intervals no longer count:
		// a collector that cannot reach a cluster must not leave the last good
		// value green (D50).
		iv := max(o.globalSettings(ctx).Int("sample_interval_seconds"), 5)
		lagMaxAge := time.Duration(3*max(10, iv)) * time.Second
		driftMaxAge := time.Duration(3*max(60, iv)) * time.Second
		if v.Bool("heartbeat") {
			maxLag := float64(o.globalSettings(ctx).Int("cutover_max_lag_seconds"))
			lc := Condition{Key: "lag", Label: d.SourceName + " target lag", Database: d.SourceName}
			lag, at, ok := o.metrics.LastAt(m.ID, d.SourceName, "heartbeat_latency_s")
			switch {
			case !ok:
				lc.Detail = "Not measured yet: waiting for the heartbeat to reach the target"
			case time.Since(at) > lagMaxAge:
				lc.SampleAgeMS = ageMS(at)
				lc.Detail = fmt.Sprintf("Not measured for %s (last measured %s, when the target was %s behind): Upwell may not be able to reach the source or the target", humanDuration(time.Since(at).Seconds()), ago(at), humanDuration(lag))
			case lag > maxLag:
				lc.SampleAgeMS = ageMS(at)
				lc.Detail = fmt.Sprintf("The target is %s behind (limit %s); the write pause would last at least that long (measured %s)", humanDuration(lag), humanDuration(maxLag), ago(at))
			default:
				lc.SampleAgeMS = ageMS(at)
				lc.OK = true
				lc.Detail = fmt.Sprintf("The target is %s behind (measured %s)", humanDuration(lag), ago(at))
			}
			out = append(out, lc)
		}
		// Apply progress, measured independently of the engine (E2): a
		// sample that is missing, failed or too old is not a pass.
		ac := Condition{Key: "apply", Label: d.SourceName + " target applying changes", Database: d.SourceName}
		w, seen := o.metrics.ApplyWatch(d.ID)
		if !w.At.IsZero() {
			ac.SampleAgeMS = ageMS(w.At)
		}
		switch {
		case !seen || (w.At.IsZero() && w.Err == nil):
			ac.Detail = "Not measured yet: the first sample is taken within seconds of streaming"
		case w.Err != nil && !w.ErrAt.Before(w.At):
			ac.Detail = fmt.Sprintf("Could not measure apply progress: %v", w.Err)
		case time.Since(w.At) > lagMaxAge:
			ac.Detail = fmt.Sprintf("Not measured for %s (last measured %s)", humanDuration(time.Since(w.At).Seconds()), ago(w.At))
		case w.Stalled:
			ac.Detail = fmt.Sprintf("Stalled: the target origin has not moved for %s while the source's tables changed %d time(s) (measured %s)", humanDuration(time.Since(w.OriginChangedAt).Seconds()), w.ChangesSince(), ago(w.At))
		case w.ChangesSince() == 0:
			ac.OK = true
			ac.Detail = fmt.Sprintf("Nothing changed on the source since the target last applied (origin at %s, measured %s)", orNone(w.Origin), ago(w.At))
		default:
			ac.OK = true
			ac.Detail = fmt.Sprintf("The target origin moved %s ago; %d source change(s) since (measured %s)", humanDuration(time.Since(w.OriginChangedAt).Seconds()), w.ChangesSince(), ago(w.At))
		}
		out = append(out, ac)
		// DDL is not streamed: a schema changed on the source ends NO-GO at
		// verification, so refuse the cutover now and say why. A comparison
		// that has not run, failed, or is too old is not a pass.
		sc := Condition{Key: "schema", Label: d.SourceName + " schema unchanged", Database: d.SourceName}
		drift, at, derr := o.metrics.DriftSample(d.ID)
		if !at.IsZero() {
			sc.SampleAgeMS = ageMS(at)
		}
		switch {
		case derr != nil:
			last := "never compared"
			if !at.IsZero() {
				last = "last compared " + ago(at)
			}
			sc.Detail = fmt.Sprintf("Could not compare the schemas: %v (%s)", derr, last)
		case at.IsZero():
			sc.Detail = "Not compared yet: the first comparison runs within a minute of the base copy finishing"
		case time.Since(at) > driftMaxAge:
			sc.Detail = fmt.Sprintf("Not compared for %s (last compared %s)", humanDuration(time.Since(at).Seconds()), ago(at))
		case len(drift) > 0:
			sc.Detail = "Changed on the source: " + strings.Join(limitStrings(drift, 3), "; ") + " (compared " + ago(at) + ")"
		default:
			sc.OK = true
			sc.Detail = "Same tables, columns and enum values on both sides (compared " + ago(at) + ")"
		}
		out = append(out, sc)
	}
	alerts, aerr := o.ListAlerts(ctx, "firing", 100)
	crit := 0
	for _, a := range alerts {
		if a.Severity == "critical" && (a.MigrationID == m.ID || a.MigrationID == "") {
			crit++
		}
	}
	if aerr != nil {
		// Not "none firing" (fail closed).
		out = append(out, Condition{Key: "alerts", Label: "No critical alerts firing", OK: false, Detail: "the alerts could not be read: " + aerr.Error()})
	} else {
		out = append(out, Condition{Key: "alerts", Label: "No critical alerts firing", OK: crit == 0, Detail: fmt.Sprintf("%d critical alert(s) firing", crit)})
	}
	if len(included(dbs)) == 0 {
		out = append(out, Condition{Key: "databases", Label: "At least one database included", OK: false, Detail: "no database is included in this migration"})
	}
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
	// WriteFreeze applies the write freeze at the writers step; FreezeConfirm
	// must be the source cluster's name (FreezeConfirmName).
	WriteFreeze   bool   `json:"write_freeze"`
	FreezeConfirm string `json:"freeze_confirm"`
}

var cutoverCancels sync.Map // op id -> context.CancelFunc

// cutoverCancelReasons: why a running cutover was cancelled, recorded before
// the cancel so the cutover's own stop (which can run first) records the same
// reason as the abort (op id -> string).
var cutoverCancelReasons sync.Map

// cancelReason is the error a cancelled cutover stops with.
func cancelReason(opID string) error {
	if r, ok := cutoverCancelReasons.Load(opID); ok {
		return errors.New(r.(string))
	}
	return errors.New("cutover cancelled")
}

// StartCutover runs the coordinated cutover as an operation.
// cutoverStartMu serialises cutover starts and re-verifications, so two
// requests cannot both pass the "already running" check.
var cutoverStartMu sync.Mutex

func (o *Orchestrator) StartCutover(ctx context.Context, a audit.Actor, role, id, key string, p CutoverParams) (*Operation, error) {
	cutoverStartMu.Lock()
	defer cutoverStartMu.Unlock()
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, err
	}
	if m.Fixture {
		return nil, uerr("fixture", "This is a demonstration fixture.", "")
	}
	if o.IsOffline(ctx, m) && m.Flags.Started {
		return nil, uerr("offline", "An offline migration has no separate cutover: it verifies as soon as the copy finishes.", "Use Verify again after restarting a database.")
	}
	continuing := m.Flags.Cutover != nil && m.Flags.Cutover.EndedAt == 0
	if continuing {
		if prev, err := o.GetOperation(ctx, m.Flags.Cutover.OpID); err == nil && prev.State == "running" {
			if key != "" && prev.Key == opKey(m.ID, key) {
				return prev, ErrDuplicate
			}
			return prev, uerr("cutover_running", "A cutover is already running.", "")
		}
	} else {
		if key != "" {
			var existing string
			if o.st.DB.QueryRowContext(ctx, `SELECT id FROM operations WHERE operation_key=?`, opKey(m.ID, key)).Scan(&existing) == nil {
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
		if p.WriteFreeze {
			if role != "admin" {
				return nil, uerr("admin_only", "Only an Admin can apply the write freeze.", "Ask an Admin, or stop writers by hand and confirm it.")
			}
			src, _, err := o.Conns(ctx, m)
			if err != nil {
				return nil, err
			}
			if want := FreezeConfirmName(src.Host); p.FreezeConfirm != want {
				return nil, uerr("freeze_confirm", "Type the source cluster's name ("+want+") to apply the write freeze.", "Review the freeze preview first.")
			}
		} else if !p.ConfirmWritersStopped {
			return nil, uerr("confirm_writers", "Confirm that the customer has stopped every writer.", "Tick the confirmation in the cutover console, or apply the write freeze.")
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
	op, err := o.newOp(ctx, m.ID, "cutover", key, a.Username, steps, map[string]any{"confirm_writers_stopped": p.ConfirmWritersStopped, "override": p.OverrideConfirm != "", "write_freeze": p.WriteFreeze, "continuing": continuing})
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
	o.audit.Append(ctx, a, "cutover.start", m.ID, nil, map[string]any{"operation": op.ID, "confirm_writers_stopped": p.ConfirmWritersStopped, "write_freeze": p.WriteFreeze, "continuing": continuing})
	o.refreshState(ctx, m.ID)
	cctx, cancel := context.WithCancel(context.Background())
	cutoverCancels.Store(op.ID, cancel)
	go func() {
		defer cancel()
		defer cutoverCancels.Delete(op.ID)
		defer cutoverCancelReasons.Delete(op.ID)
		defer func() {
			if r := recover(); r != nil {
				o.logf("error", "orchestrator", m.ID, "", op.ID, "cutover worker crashed: %v", r)
				op.Finish(fmt.Errorf("the cutover stopped on an internal error: %v", r), nil)
			}
		}()
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
				o.setDB(bg, d.ID, map[string]any{"hb_token": nil, "hb_before_lsn": nil, "writes_baseline": nil})
			}
			// Streaming continues and applications keep the source.
			if uerr := o.undoFreeze(bg, m.ID, "Upwell", "the cutover stopped before end positions: "+err.Error()); uerr != nil {
				op.Log("error", "%v", uerr)
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
	cur, err := o.GetMigration(bg, m.ID)
	if err != nil || cur.Flags.Cutover == nil || cur.Flags.Cutover.OpID != op.ID || ctx.Err() != nil {
		fail("readiness", errors.New("the cutover was cancelled"))
		return
	}
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
		if p.WriteFreeze {
			op.StepDetail("writers", "applying the write freeze", 0.1)
			if err := o.applyFreeze(bg, m, a, dbs, src, op); err != nil {
				fail("writers", err)
				return
			}
		}
		first := map[string]pgpack.Activity{}
		for _, d := range dbs {
			act, err := pgpack.WriteActivity(bg, src.WithDB(d.SourceName))
			if err != nil {
				fail("writers", fmt.Errorf("%s: could not sample writes: %w", d.SourceName, err))
				return
			}
			first[d.SourceName] = act
			if len(act.Sessions) > 0 {
				op.LogDB("warn", d.SourceName, "%d client session(s) still connected: %s", len(act.Sessions), strings.Join(act.Sessions, "; "))
			} else {
				op.LogDB("info", d.SourceName, "no client sessions besides the engine and Upwell")
			}
		}
		op.StepDetail("writers", fmt.Sprintf("sampling row changes for %d s", secs), 0.3)
		select {
		case <-ctx.Done():
			fail("writers", cancelReason(op.ID))
			return
		case <-time.After(time.Duration(secs) * time.Second):
		}
		// Writes show in the counters when their transaction ends; a writer
		// inside an open transaction shows as a session holding a
		// transaction ID. Either one means writes have not stopped.
		var changing []string
		baseline := map[string]int64{}
		fingerprint := map[string]string{}
		for _, d := range dbs {
			act, err := pgpack.WriteActivity(bg, src.WithDB(d.SourceName))
			if err != nil {
				fail("writers", fmt.Errorf("%s: could not sample writes: %w", d.SourceName, err))
				return
			}
			baseline[d.SourceName] = act.Changes
			fingerprint[d.SourceName] = act.Fingerprint
			if n := act.Changes - first[d.SourceName].Changes; n != 0 {
				changing = append(changing, fmt.Sprintf("%s (%d row changes in %d s)", d.SourceName, n, secs))
			}
			if act.Fingerprint != first[d.SourceName].Fingerprint {
				changing = append(changing, d.SourceName+" (a table was truncated or rewritten, or a sequence or large object changed)")
			}
			if len(act.OpenWriters) > 0 {
				changing = append(changing, fmt.Sprintf("%s (%d open write transaction(s), prepared ones included: %s)", d.SourceName, len(act.OpenWriters), strings.Join(limitStrings(act.OpenWriters, 3), "; ")))
			}
		}
		override := false
		if len(changing) > 0 {
			if role != "admin" || p.OverrideConfirm != m.Name {
				fail("writers", fmt.Errorf("writes have not stopped: %s. Stop every writer (and end open transactions), or an Admin can override by typing the migration name; verification then ends NO-GO if the source changed at all", strings.Join(changing, ", ")))
				return
			}
			// An override that is not on the audit record does not happen (D58).
			if _, err := o.audit.Append(bg, a, "cutover.override_writes", m.ID, nil, map[string]any{"changing": changing}); err != nil {
				fail("writers", fmt.Errorf("the override of detected writes was refused because it could not be recorded in the audit log: %w", err))
				return
			}
			override = true
			op.Log("warn", "Admin %s overrode detected writes: %s", a.Username, strings.Join(changing, ", "))
		}
		for _, d := range dbs {
			o.setDB(bg, d.ID, map[string]any{"writes_baseline": baseline[d.SourceName], "writes_fingerprint": fingerprint[d.SourceName]})
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
				cur, gerr := o.GetDatabase(bg, m.ID, d.ID)
				if gerr != nil {
					fail("heartbeat", fmt.Errorf("%s: reading its state: %w", d.SourceName, gerr))
					return
				}
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
			cur, gerr := o.GetDatabase(bg, m.ID, d.ID)
			if gerr != nil {
				fail("await", fmt.Errorf("%s: reading its state: %w", d.SourceName, gerr))
				return
			}
			pending[d.SourceName] = cur
		}
		lastNudge := time.Now().Add(-4 * time.Second)
		lastErr := map[string]error{} // why a heartbeat could not be looked for
		for len(pending) > 0 {
			if ctx.Err() != nil {
				fail("await", cancelReason(op.ID))
				return
			}
			for name, d := range pending {
				ok, err := pgpack.HeartbeatOnTarget(bg, dst.WithDB(d.TargetName), d.HBToken)
				if err != nil {
					if lastErr[name] == nil {
						op.LogDB("warn", name, "looking for the final heartbeat on the target failed (retrying): %v", err)
					}
					lastErr[name] = err
					continue
				}
				delete(lastErr, name)
				if ok {
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
					if lastErr[n] != nil {
						n += " (the target could not be read: " + lastErr[n].Error() + ")"
					}
					names = append(names, n)
				}
				sort.Strings(names)
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
	// Every database must be streaming: an end position on a unit that is
	// not running would never be reached (or would look reached).
	if !past("endpos") {
		var notStreaming []string
		for _, d := range dbs {
			cur, gerr := o.GetDatabase(bg, m.ID, d.ID)
			if gerr != nil {
				fail("endpos", fmt.Errorf("%s: reading its state: %w; no end position was set and streaming continues", d.SourceName, gerr))
				return
			}
			if cur.EndPos == "" && cur.State != DInSync && cur.State != DCatchUp {
				notStreaming = append(notStreaming, d.SourceName+" ("+Label(cur.State)+")")
			}
		}
		if len(notStreaming) > 0 {
			fail("endpos", fmt.Errorf("not streaming, so end positions were not set and streaming continues: %s. Resume or fix these databases and run the cutover again", strings.Join(notStreaming, ", ")))
			return
		}
	}
	// Every database's engine command must be known before the point of no
	// return: a plan error stops the cutover with streaming intact (D55).
	plans := map[string]engine.RunPlan{}
	specs := map[string]engine.DatabaseSpec{}
	for _, d := range dbs {
		cur, err := o.GetDatabase(bg, m.ID, d.ID)
		var spec engine.DatabaseSpec
		if err == nil {
			spec, err = o.dbSpec(bg, m, cur)
		}
		if err == nil {
			var plan engine.RunPlan
			if plan, err = o.eng.Plan(spec, true); err == nil {
				plans[d.ID], specs[d.ID] = plan, spec
			}
		}
		if err != nil {
			fail("endpos", fmt.Errorf("%s: the engine command could not be prepared, so no end position was set and streaming continues: %w", d.SourceName, err))
			return
		}
	}
	// Check-and-set against an abort racing this step: only the cutover
	// that is still current, and not cancelled, may move past the point of
	// no return.
	moved := past("endpos") // a continued cutover is already past this point
	o.updateFlags(bg, m.ID, func(f *Flags) {
		if !moved && ctx.Err() == nil && f.Cutover != nil && f.Cutover.OpID == op.ID && !f.Aborted {
			f.Cutover.Phase = "endpos"
			moved = true
		}
	})
	if !moved {
		fail("endpos", errors.New("the cutover was cancelled before end positions were set; streaming continues"))
		return
	}
	o.refreshState(bg, m.ID)
	op.StepStart("endpos", "")
	for _, d := range dbs {
		cur, gerr := o.GetDatabase(bg, m.ID, d.ID)
		if gerr != nil {
			op.StepEnd("endpos", "failed", fmt.Sprintf("%s: reading its state: %v", d.SourceName, gerr))
			o.finishCutover(bg, m, op, a, gerr)
			return
		}
		spec, plan := specs[d.ID], plans[d.ID]
		if cur.EndPos == "" {
			lsn, err := o.eng.SetEndPosition(bg, plan, spec)
			if err != nil {
				op.StepEnd("endpos", "failed", fmt.Sprintf("%s: %v", d.SourceName, err))
				o.finishCutover(bg, m, op, a, err)
				return
			}
			o.setDB(bg, d.ID, map[string]any{"endpos": lsn, "state": DDraining, "drained": 0})
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
		cur, lerr := o.cutoverDBs(bg, m.ID)
		if lerr != nil {
			// Not "nothing left to drain" (Part 1, fail closed): wait, until the deadline.
			op.Log("warn", "reading the databases' drain state failed: %v", lerr)
			if time.Now().After(deadline) {
				op.StepEnd("drain", "failed", "could not read the databases' drain state: "+lerr.Error())
				break
			}
			continue
		}
		drained, broken := 0, []string{}
		undrained = nil
		for _, d := range included(cur) {
			switch {
			case d.Drained:
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
				if err := o.eng.Nudge(bg, spec); err != nil {
					op.LogDB("warn", d.SourceName, "nudge failed: %v", err)
				}
			}
			lastNudge = time.Now()
		}
	}
	o.verifyPhase(bg, m, op, a)
}

// stopUndrained stops the engine of every database that did not reach its end
// position, so nothing keeps applying to the target after the verdict, and
// returns their names.
func (o *Orchestrator) stopUndrained(ctx context.Context, m Migration, dbs []Database) map[string]bool {
	out := map[string]bool{}
	for _, d := range dbs {
		if d.Drained {
			continue
		}
		out[d.SourceName] = true
		if st, err := o.run.Status(ctx, d.Instance); err != nil || st.Active {
			o.mustStop(ctx, m, d, "did not reach its end position during the cutover")
		}
	}
	return out
}

// verifyPhase runs the origin check, sequence sync, verification and verdict.
func (o *Orchestrator) verifyPhase(ctx context.Context, m Migration, op *Operation, a audit.Actor) {
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.UnverifiedCount, f.Unverified = 0, nil })
	src, dst, err := o.Conns(ctx, m)
	if err != nil {
		o.finishCutover(ctx, m, op, a, err)
		return
	}
	v := o.Settings(ctx, m)
	dbs, lerr := o.cutoverDBs(ctx, m.ID)
	if lerr != nil {
		o.finishCutover(ctx, m, op, a, fmt.Errorf("the databases to verify could not be read: %w", lerr))
		return
	}
	inc := included(dbs)
	offline := v.Str("mode") == "offline"
	hbOn := v.Bool("heartbeat") && !offline
	// Only a database whose engine reached the end position (offline: whose
	// copy finished) can be GO.
	undrained := o.stopUndrained(ctx, m, inc)

	// 6. Origin progress (D3). Offline there is no origin: nothing streamed.
	originFail := map[string]string{}
	if !offline {
		o.setPhase(ctx, m.ID, "origin")
		op.StepStart("origin", "")
	}
	for _, d := range inc {
		if offline {
			break
		}
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
	switch {
	case offline:
	case len(originFail) > 0:
		var parts []string
		for k, x := range originFail {
			parts = append(parts, k+": "+x)
		}
		op.StepEnd("origin", "failed", "NO-GO for "+strings.Join(parts, "; "))
	default:
		op.StepEnd("origin", "done", "every database's target origin reached its final heartbeat")
	}

	// 7. Sequences.
	o.setPhase(ctx, m.ID, "sequences")
	op.StepStart("sequences", "")
	total := 0
	var seqErrs []string
	for _, d := range inc {
		n, err := pgpack.SyncSequences(ctx, src.WithDB(d.SourceName), dst.WithDB(d.TargetName))
		if err != nil {
			op.LogDB("error", d.SourceName, "sequence sync failed: %v", err)
			seqErrs = append(seqErrs, fmt.Sprintf("%s: %v", d.SourceName, err))
		}
		total += n
	}
	if len(seqErrs) > 0 {
		// Verification still compares every sequence and decides (D56).
		op.StepEnd("sequences", "failed", fmt.Sprintf("%d sequence(s) synced; sync failed for %s; verification compares every sequence", total, strings.Join(seqErrs, "; ")))
	} else {
		op.StepEnd("sequences", "done", fmt.Sprintf("%d sequence(s) synced", total))
	}

	// 8. Verification.
	o.setPhase(ctx, m.ID, "verify")
	op.StepStart("verify", "")
	runID := op.ID
	verdicts := map[string]bool{}
	var unverified []string
	for i, d := range inc {
		op.StepDetail("verify", "verifying "+d.SourceName, float64(i)/float64(max(len(inc), 1)))
		if o.testHooks.BeforeVerify != nil {
			o.testHooks.BeforeVerify(ctx, d)
		}
		cur, gerr := o.GetDatabase(ctx, m.ID, d.ID)
		if gerr != nil {
			// Never verify against a database row Upwell could not read: its
			// heartbeat token and positions decide the first checks.
			cur = d
			op.LogDB("error", d.SourceName, "reading the database's state before verification failed: %v", gerr)
		}
		// D91: tables beyond the checksum budget compared row by row when the
		// setting (or a "verify fully" request) asks for it; ranges found equal
		// in this verification are not compared again after an interruption.
		full := v.Str("verify_large_tables") == "full" || op.Params["full"] == true
		runKey := d.ID + ":" + cur.HBToken + ":" + cur.EndPos
		if op.Kind == "verify" {
			runKey += ":" + op.ID
		}
		dbi, ndb := i, len(inc)
		res, err := pgpack.Verify(ctx, pgpack.VerifyInput{Source: src.WithDB(d.SourceName), Target: dst.WithDB(d.TargetName), TargetMaint: dst, Origin: d.OriginName,
			HeartbeatRequired: hbOn, Offline: offline, HeartbeatToken: cur.HBToken, HBBeforeLSN: cur.HBBeforeLSN, OriginLSN: cur.OriginLSN,
			ExactCountMaxBytes: int64(v.Int("exact_count_max_mb")) << 20, ChecksumBudget: time.Duration(v.Int("verify_checksum_budget_seconds")) * time.Second, Database: d.SourceName,
			FullLarge: full, ChunkBytes: int64(v.Int("verify_chunk_mb")) << 20, Workers: v.Int("verify_workers"), Tracker: chunkTracker{o: o, key: runKey},
			ChunkProgress: func(done, total int, table string) {
				op.StepDetail("verify", fmt.Sprintf("verifying %s: %s, key range %d of %d", d.SourceName, table, done, total), (float64(dbi)+float64(done)/float64(max(total, 1)))/float64(max(ndb, 1)))
			}})
		if err != nil {
			res.GO = false
			res.Results = append(res.Results, check.Result{CheckID: "verify_error", Level: check.Blocker, Hard: true, Title: "Verification completed", Message: err.Error(), Database: d.SourceName})
		}
		if o.testHooks.AfterVerify != nil {
			o.testHooks.AfterVerify(ctx, d)
		}
		// The source must not have changed since the writers step: a write
		// after it (a writer that resumed, an open transaction that committed,
		// writes an Admin overrode) is missing on the target (D44, D45).
		wr := o.writesSinceWritersStep(ctx, m, src.WithDB(d.SourceName), cur)
		res.Results = append(res.Results, wr)
		if wr.Level == check.Blocker {
			res.GO = false
		}
		if _, bad := originFail[d.SourceName]; bad {
			res.GO = false
		}
		if undrained[d.SourceName] {
			res.GO = false
			msg := "The engine did not reach this database's end position during the cutover, so changes before it may be missing on the target."
			if offline {
				msg = "The offline copy of this database did not finish."
			}
			res.Results = append(res.Results, check.Result{CheckID: "drained", Level: check.Blocker, Hard: true, Title: "Engine reached the end position", Database: d.SourceName, Message: msg})
		}
		for _, r := range res.Results {
			if r.CheckID == "coverage" && r.Level != check.OK {
				if ev, ok := r.Evidence["not_fully_compared"].([]string); ok {
					for _, tname := range ev {
						unverified = append(unverified, d.SourceName+": "+tname)
					}
				}
			}
		}
		table := "verifications"
		if o.testHooks.FailVerificationStore {
			table = "verifications_unavailable"
		}
		var storeErr error
		for _, r := range res.Results {
			if _, err := o.st.DB.ExecContext(ctx, `INSERT INTO `+table+`(id,migration_id,database,run_id,check_id,result,title,detail,ts) VALUES (?,?,?,?,?,?,?,?,?)`,
				store.NewID(), m.ID, d.SourceName, runID, r.CheckID, r.Level, r.Title, toJSON(map[string]any{"message": r.Message, "evidence": r.Evidence}), store.Now()); err != nil && storeErr == nil {
				storeErr = err
			}
			lvl := "info"
			if r.Level == check.Blocker {
				lvl = "error"
			} else if r.Level == check.Warning {
				lvl = "warn"
			}
			op.LogDB(lvl, d.SourceName, "%s: %s", r.Title, r.Message)
		}
		if storeErr != nil {
			// A verdict nobody can review is not a GO (D57).
			res.GO = false
			op.LogDB("error", d.SourceName, "verification results could not be recorded: %v", storeErr)
		}
		verdicts[d.SourceName] = res.GO
		state, verdict, lastErr := DVerified, "GO", ""
		if !res.GO {
			state, verdict = DFailed, "NO-GO"
			var why []string
			if storeErr != nil {
				why = append(why, "verification results could not be recorded ("+storeErr.Error()+")")
			}
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
	// Each database was resampled right after its own comparison; a write to
	// one of them while another was being compared would not be. Resample
	// every database once more, last, before the verdict (D83).
	if len(inc) > 1 {
		op.StepDetail("verify", "resampling writes on every database", 1)
		for _, d := range inc {
			cur, err := o.GetDatabase(ctx, m.ID, d.ID)
			if err != nil || cur.Verdict != "GO" {
				continue
			}
			wr := o.writesSinceWritersStep(ctx, m, src.WithDB(d.SourceName), cur)
			if wr.Level != check.Blocker {
				continue
			}
			wr.CheckID, wr.Title = "writes_stopped_final", "No writes on the source before the verdict"
			if _, err := o.st.DB.ExecContext(ctx, `INSERT INTO verifications(id,migration_id,database,run_id,check_id,result,title,detail,ts) VALUES (?,?,?,?,?,?,?,?,?)`,
				store.NewID(), m.ID, d.SourceName, runID, wr.CheckID, wr.Level, wr.Title, toJSON(map[string]any{"message": wr.Message, "evidence": wr.Evidence}), store.Now()); err != nil {
				op.LogDB("error", d.SourceName, "verification results could not be recorded: %v", err)
			}
			op.LogDB("error", d.SourceName, "%s: %s", wr.Title, wr.Message)
			o.setDB(ctx, d.ID, map[string]any{"state": DFailed, "verdict": "NO-GO", "last_error": "NO-GO: " + wr.Title})
		}
	}
	op.StepEnd("verify", "done", "")
	// D91: which tables this verdict rests on without a row-by-row comparison.
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.UnverifiedCount, f.Unverified = len(unverified), limitStrings(unverified, 500) })
	o.finishCutover(ctx, m, op, a, nil)
}

// finishCutover computes the verdict from the stored per-database verdicts.
func (o *Orchestrator) finishCutover(ctx context.Context, m Migration, op *Operation, a audit.Actor, ferr error) {
	op.StepStart("verdict", "")
	dbs, lerr := o.cutoverDBs(ctx, m.ID)
	goAll := ferr == nil
	// Fail closed: a verdict over databases that could not be read, or over
	// none, is never GO.
	if lerr != nil {
		goAll = false
		op.Log("error", "reading the databases' verdicts failed: %v", lerr)
	} else if len(included(dbs)) == 0 {
		goAll = false
	}
	if cur, err := o.GetMigration(ctx, m.ID); err != nil || cur.Flags.Aborted {
		goAll = false
	}
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
	if verdict != "GO" {
		// Applications stay on the source: give it its writes back.
		if uerr := o.undoFreeze(ctx, m.ID, "Upwell", "the verdict is NO-GO"); uerr != nil {
			op.Log("error", "%v", uerr)
		}
	} else if f.Freeze.Active() {
		o.Event(ctx, m.ID, "", "write_freeze", "info", "The write freeze stays applied after GO, so nothing writes to the old source; undo it from the cutover console when the customer wants the source writable", nil)
	}
	if verdict == "GO" {
		op.StepEnd("verdict", "done", fmt.Sprintf("GO: every database verified; write pause %s", (time.Duration(pause)*time.Millisecond).Round(time.Second)))
		o.Event(ctx, m.ID, "", "verdict", "info", fmt.Sprintf("Cutover verdict GO: every database verified (write pause %s)", (time.Duration(pause)*time.Millisecond).Round(time.Second)), map[string]any{"verdict": verdict})
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
	preEndpos := func(c *CutoverState) bool {
		return c.Phase == "readiness" || c.Phase == "writers" || c.Phase == "heartbeat" || c.Phase == "await"
	}
	if !preEndpos(c) {
		return uerr("too_late", "End positions are set; the cutover cannot be aborted now.", "Wait for the verdict.")
	}
	// F6: check-and-set under the flag lock, as the cutover moves past the
	// point of no return (it checks its context there under the same lock):
	// either the abort cancels it before end positions, or it is refused.
	aborted := false
	o.updateFlags(ctx, m.ID, func(f *Flags) {
		cur := f.Cutover
		if cur == nil || cur.EndedAt != 0 || cur.OpID != c.OpID || !preEndpos(cur) {
			return
		}
		if cancel, ok := cutoverCancels.Load(c.OpID); ok {
			cutoverCancelReasons.Store(c.OpID, "the cutover was aborted by "+a.Username)
			cancel.(context.CancelFunc)()
		}
		f.Cutover = nil
		aborted = true
	})
	if !aborted {
		return uerr("too_late", "End positions were set while the abort was requested; the cutover cannot be aborted now.", "Wait for the verdict.")
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	for _, d := range dbs {
		o.setDB(ctx, d.ID, map[string]any{"hb_token": nil, "hb_before_lsn": nil, "writes_baseline": nil})
	}
	o.audit.Append(ctx, a, "cutover.abort", m.ID, nil, nil)
	if err := o.undoFreeze(ctx, m.ID, a.Username, "the cutover was aborted"); err != nil {
		o.Event(ctx, m.ID, "", "write_freeze", "critical", err.Error(), nil)
	}
	o.Event(ctx, m.ID, "", "cutover_aborted", "warning", "Cutover aborted by "+a.Username+" before end positions were set; streaming continues", nil)
	o.refreshState(ctx, m.ID)
	return nil
}

// Reverify re-runs the origin check and verification after a drain.
// full compares every row of every table, by key range for the large ones
// (D91), whatever verify_large_tables says.
func (o *Orchestrator) Reverify(ctx context.Context, a audit.Actor, id, key string, full bool) (*Operation, error) {
	cutoverStartMu.Lock()
	defer cutoverStartMu.Unlock()
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, err
	}
	if m.Flags.Verifying || (m.Flags.Cutover != nil && m.Flags.Cutover.EndedAt == 0) {
		return nil, uerr("busy", "A cutover or verification is already running.", "Wait for its verdict.")
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	offline := o.IsOffline(ctx, m)
	for _, d := range included(dbs) {
		if offline {
			if !d.Drained || (d.State != DCopied && d.State != DVerified && d.State != DFailed) {
				return nil, uerr("not_copied", "Verification runs after every database's offline copy finished.", "")
			}
			continue
		}
		if d.EndPos == "" || (d.State != DDrained && d.State != DVerified && d.State != DFailed) {
			return nil, uerr("not_drained", "Verification runs after the cutover drained every database.", "")
		}
	}
	steps := []Step{{Key: "origin", Title: "Check target origin reached the final heartbeat"}, {Key: "sequences", Title: "Sync sequences"}, {Key: "verify", Title: "Verify data"}, {Key: "verdict", Title: "Verdict"}}
	if offline {
		steps = steps[1:]
	}
	op, err := o.newOp(ctx, m.ID, "verify", key, a.Username, steps, map[string]any{"full": full})
	if err != nil {
		return op, err
	}
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.Verifying = true; f.Verdict = "" })
	for _, d := range included(dbs) {
		o.setDB(ctx, d.ID, map[string]any{"verdict": nil})
	}
	o.audit.Append(ctx, a, "verify.run", m.ID, nil, map[string]any{"operation": op.ID, "full": full})
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

// writesSinceWritersStep resamples the source's writes after verification and
// compares them with the baseline taken when the writers step confirmed
// writes stopped. It fails closed: no baseline, or a failed sample, is a
// blocker.
func (o *Orchestrator) writesSinceWritersStep(ctx context.Context, m Migration, src pg.Conn, d Database) check.Result {
	res := check.Result{CheckID: "writes_stopped", Title: "No writes on the source since writers stopped", Database: d.SourceName, Hard: true}
	override := false
	if cur, err := o.GetMigration(ctx, m.ID); err == nil && cur.Flags.Cutover != nil {
		override = cur.Flags.Cutover.Override
	}
	overrideNote := ""
	if override {
		overrideNote = " An Admin overrode detected writes at the writers step, so this resample decides the verdict."
	}
	if d.WritesBaseline == nil {
		res.Level = check.Blocker
		res.Message = "There is no write sample from the writers step for this database, so Upwell cannot show that the source stopped changing."
		return res
	}
	// Statistics reach pg_stat_user_tables about a second after a transaction ends.
	time.Sleep(1500 * time.Millisecond)
	act, err := pgpack.WriteActivity(ctx, src)
	res.Evidence = map[string]any{"baseline": *d.WritesBaseline, "now": act.Changes, "open_writers": act.OpenWriters, "override": override, "fingerprint_checked": d.WritesFingerprint != ""}
	switch {
	case err != nil:
		res.Level = check.Blocker
		res.Message = "Could not resample the source's writes after verification: " + err.Error() + "."
	case act.Changes != *d.WritesBaseline:
		res.Level = check.Blocker
		res.Message = fmt.Sprintf("Rows changed on the source after the writers step confirmed writes stopped (%d row changes since then); those changes are not on the target.%s", act.Changes-*d.WritesBaseline, overrideNote)
	case d.WritesFingerprint != "" && act.Fingerprint != d.WritesFingerprint:
		res.Level = check.Blocker
		res.Message = "Since the writers step a table on the source was truncated or rewritten, or a sequence or large object changed; the row counter does not show those, and the target may not have them." + overrideNote
	case len(act.OpenWriters) > 0:
		res.Level = check.Blocker
		res.Message = fmt.Sprintf("%d open write transaction(s), prepared ones included on the source (%s): when they commit, their changes will be missing on the target.%s", len(act.OpenWriters), strings.Join(limitStrings(act.OpenWriters, 3), "; "), overrideNote)
	default:
		res.Level = check.OK
		res.Message = "No row changes on the source since writers were confirmed stopped, and no open write transaction."
	}
	return res
}

// cutoverDBs reads the migration's databases for the cutover's drain,
// verification and verdict.
func (o *Orchestrator) cutoverDBs(ctx context.Context, id string) ([]Database, error) {
	if o.testHooks.CutoverReadErr != nil {
		if err := o.testHooks.CutoverReadErr(); err != nil {
			return nil, err
		}
	}
	return o.ListDatabases(ctx, id)
}
