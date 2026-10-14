package orch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/pgpack"
	"github.com/pagombin/upwell/internal/store"
)

// ApplyMeasurement is one measured engine apply rate (E1).
type ApplyMeasurement struct {
	ID          string  `json:"id"`
	MigrationID string  `json:"migration_id"`
	TS          int64   `json:"ts"`
	TPS         float64 `json:"tps"`
	Txns        int     `json:"txns"`
	Seconds     float64 `json:"seconds"`
	LowerBound  bool    `json:"lower_bound"`
	Suggested   float64 `json:"suggested"`
	AssumedThen float64 `json:"assumed_then"`
	By          string  `json:"by"`
}

// Measurement limits: a burst of single-row transactions written as fast as
// the source takes them, then the engine's apply rate over at most two
// minutes of backlog.
var (
	rateWriteFor   = 15 * time.Second
	rateMaxTxns    = 3000
	rateObserveFor = 2 * time.Minute
	rateFirstApply = 3 * time.Minute
	rateReadyAfter = 10 * time.Minute
)

// MeasureApplyRate runs the engine on scratch databases created on both of
// a migration's clusters, writes single-row transactions on the source and
// measures how many a second the engine applies on the target. Everything
// it creates is removed afterwards, whether the measurement worked or not.
func (o *Orchestrator) MeasureApplyRate(ctx context.Context, a audit.Actor, id, key string) (*Operation, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := o.requirePermission(ctx, m); err != nil {
		return nil, err
	}
	if m.Flags.Cutover != nil && m.Flags.Verdict == "" && !m.Flags.Aborted {
		return nil, uerr("cutover", "The apply rate cannot be measured during a cutover.", "")
	}
	src, dst, err := o.Conns(ctx, m)
	if err != nil {
		return nil, err
	}
	short := strings.ToLower(newShortID())
	name := "upwell_rate_" + short
	spec := engine.DatabaseSpec{MigrationID: m.ID, Source: name, Target: name, SourceConn: src, TargetConn: dst,
		Instance: "rate-" + short, SlotName: name, OriginName: name, RunDir: filepath.Join(o.cfg.RunsDir(), "rate-"+short), TableJobs: 1, IndexJobs: 1}
	steps := []Step{{Key: "prepare", Title: "Create scratch databases"}, {Key: "engine", Title: "Start the engine"}, {Key: "measure", Title: "Write and measure"}, {Key: "cleanup", Title: "Remove the scratch databases"}}
	op, err := o.newOp(ctx, m.ID, "apply_rate", key, a.Username, steps, map[string]any{"scratch": name})
	if err != nil {
		return op, err
	}
	o.audit.Append(ctx, a, "migration.measure_apply_rate", m.ID, nil, map[string]any{"operation": op.ID, "scratch": name})
	go func() {
		bg := context.Background()
		res, err := o.measureApply(bg, op, spec)
		op.StepStart("cleanup", "")
		if cerr := o.cleanupScratch(bg, spec); cerr != nil {
			op.StepEnd("cleanup", "failed", cerr.Error())
			err = errors.Join(err, fmt.Errorf("cleanup of %s failed: %w", name, cerr))
		} else {
			op.StepEnd("cleanup", "done", "dropped "+name+" on both clusters, its slot, publication and origin")
		}
		if err != nil {
			op.Finish(err, nil)
			return
		}
		g := o.globalSettings(bg)
		res.ID, res.MigrationID, res.TS, res.By, res.AssumedThen = store.NewID(), m.ID, store.Now(), a.Username, g.Float("assumed_apply_tps")
		if _, err := o.st.DB.ExecContext(bg, `INSERT INTO apply_measurements(id,migration_id,ts,tps,txns,seconds,lower_bound,suggested,assumed_then,by_user) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			res.ID, res.MigrationID, res.TS, res.TPS, res.Txns, res.Seconds, res.LowerBound, res.Suggested, res.AssumedThen, res.By); err != nil {
			op.Finish(fmt.Errorf("the measurement could not be recorded: %w", err), nil)
			return
		}
		o.Event(bg, m.ID, "", "apply_rate", "info", fmt.Sprintf("Measured engine apply rate: %s single-row transactions a second (assumed_apply_tps is %g; suggested %g)", rateText(res), res.AssumedThen, res.Suggested), nil)
		op.Finish(nil, map[string]any{"tps": res.TPS, "txns": res.Txns, "seconds": res.Seconds, "lower_bound": res.LowerBound, "suggested": res.Suggested, "assumed_then": res.AssumedThen})
	}()
	return op, nil
}

func rateText(r ApplyMeasurement) string {
	s := fmt.Sprintf("%.1f", r.TPS)
	if r.LowerBound {
		s = "at least " + s
	}
	return s
}

// suggestTPS keeps a 20% margin under the measured rate.
func suggestTPS(tps float64) float64 { return math.Max(0.1, math.Floor(tps*0.8*10)/10) }

func (o *Orchestrator) measureApply(ctx context.Context, op *Operation, spec engine.DatabaseSpec) (ApplyMeasurement, error) {
	var res ApplyMeasurement
	name := spec.Source
	op.StepStart("prepare", "")
	sm, err := pg.Connect(ctx, spec.SourceConn, 30*time.Second)
	if err != nil {
		op.StepEnd("prepare", "failed", err.Error())
		return res, fmt.Errorf("source: %w", err)
	}
	_, err = pg.Exec(ctx, sm, `CREATE DATABASE `+pg.QuoteIdent(name))
	sm.Close(ctx)
	if err != nil {
		op.StepEnd("prepare", "failed", err.Error())
		return res, fmt.Errorf("creating %s on the source (the admin user needs CREATEDB): %w", name, err)
	}
	s, err := pg.Connect(ctx, spec.SourceConn.WithDB(name), 30*time.Second)
	if err != nil {
		op.StepEnd("prepare", "failed", err.Error())
		return res, err
	}
	defer s.Close(ctx)
	if _, err := pg.Exec(ctx, s, `CREATE TABLE sink(id bigserial PRIMARY KEY, a int, b text DEFAULT 'upwell apply-rate measurement')`); err != nil {
		op.StepEnd("prepare", "failed", err.Error())
		return res, err
	}
	if _, err := pgpack.EnsureTargetDB(ctx, spec.SourceConn, spec.TargetConn, name, name); err != nil {
		op.StepEnd("prepare", "failed", err.Error())
		return res, fmt.Errorf("creating %s on the target: %w", name, err)
	}
	t, err := pg.Connect(ctx, spec.TargetConn.WithDB(name), 30*time.Second)
	if err != nil {
		op.StepEnd("prepare", "failed", err.Error())
		return res, err
	}
	defer t.Close(ctx)
	var ver int
	if err := pg.QueryRow(ctx, t, `SELECT current_setting('server_version_num')::int`).Scan(&ver); err != nil {
		op.StepEnd("prepare", "failed", err.Error())
		return res, err
	}
	ap, err := pgpack.TargetApplyPrivileges(ctx, t, ver)
	if err == nil && !ap.OK {
		err = fmt.Errorf("the target user cannot %s in %s, so the engine would apply nothing (F2). %s", strings.Join(ap.Missing, " or "), name, pgpack.ApplyPrivilegeRemediation(spec.TargetConn.User, ver))
	}
	if err != nil {
		op.StepEnd("prepare", "failed", err.Error())
		return res, err
	}
	op.StepEnd("prepare", "done", name+" on both clusters")

	op.StepStart("engine", "")
	os.RemoveAll(spec.RunDir)
	plan, err := o.eng.Plan(spec, false)
	if err == nil {
		err = os.MkdirAll(plan.RunDir, 0o750)
	}
	if err == nil {
		err = o.eng.Start(ctx, plan, spec)
	}
	if err != nil {
		op.StepEnd("engine", "failed", err.Error())
		return res, err
	}
	deadline := time.Now().Add(rateReadyAfter)
	for {
		pr, _ := o.eng.Observe(ctx, plan, spec)
		if pr.Phase == engine.PhaseCDC {
			break
		}
		if st, serr := o.run.Status(ctx, spec.Instance); serr == nil && !st.Active {
			err = fmt.Errorf("the engine stopped before streaming (log: %s)", plan.LogFile)
		} else if time.Now().After(deadline) {
			err = fmt.Errorf("the engine was not streaming after %s", rateReadyAfter)
		}
		if err != nil {
			op.StepEnd("engine", "failed", err.Error())
			return res, err
		}
		time.Sleep(2 * time.Second)
	}
	op.StepEnd("engine", "done", plan.Unit+" is streaming")

	op.StepStart("measure", "")
	start := time.Now()
	k := 0
	for k < rateMaxTxns && time.Since(start) < rateWriteFor {
		if _, err := pg.Exec(ctx, s, `INSERT INTO sink(a) VALUES ($1)`, k); err != nil {
			op.StepEnd("measure", "failed", err.Error())
			return res, err
		}
		k++
	}
	wrote := time.Now()
	op.StepDetail("measure", fmt.Sprintf("wrote %d single-row transactions in %s; waiting for the target", k, wrote.Sub(start).Round(time.Millisecond)), 0.2)
	count := func() (int, error) {
		var n int
		err := pg.QueryRow(ctx, t, `SELECT count(*) FROM sink`).Scan(&n)
		return n, err
	}
	var t1 time.Time
	var c1 int
	for {
		n, err := count()
		if err != nil {
			op.StepEnd("measure", "failed", err.Error())
			return res, err
		}
		if n > 0 {
			t1, c1 = time.Now(), n
			break
		}
		if time.Since(wrote) > rateFirstApply {
			err := fmt.Errorf("the engine applied none of %d transactions within %s: it may be applying nothing without saying so (F2); check the target user's privileges and the engine log (%s)", k, rateFirstApply, plan.LogFile)
			op.StepEnd("measure", "failed", err.Error())
			return res, err
		}
		time.Sleep(time.Second)
	}
	t2, c2 := t1, c1
	for c2 < k && time.Since(t1) < rateObserveFor {
		time.Sleep(time.Second)
		n, err := count()
		if err != nil {
			op.StepEnd("measure", "failed", err.Error())
			return res, err
		}
		t2, c2 = time.Now(), n
		op.StepDetail("measure", fmt.Sprintf("%d of %d applied", c2, k), 0.2+0.8*float64(c2)/float64(k))
	}
	res.Txns = k
	if c2 > c1 && t2.Sub(t1) >= 3*time.Second {
		res.Seconds = t2.Sub(t1).Seconds()
		res.TPS = float64(c2-c1) / res.Seconds
		res.LowerBound = c2 == k // the backlog ran out: the engine may be faster
	} else {
		// Everything (or nearly) arrived at once: only a lower bound is known.
		res.Seconds = t2.Sub(wrote).Seconds()
		res.TPS = float64(c2) / math.Max(res.Seconds, 1)
		res.LowerBound = true
	}
	res.Suggested = suggestTPS(res.TPS)
	op.StepEnd("measure", "done", fmt.Sprintf("%s transactions a second (%d applied over %.0f s)", rateText(res), c2-c1, res.Seconds))
	return res, nil
}

// cleanupScratch stops the engine and removes the scratch databases, the
// slot, publication, origin and work directory.
func (o *Orchestrator) cleanupScratch(ctx context.Context, spec engine.DatabaseSpec) error {
	_, cerr := o.eng.Cleanup(ctx, spec, engine.CleanupOptions{RemoveWorkDir: true})
	var errs []error
	if cerr != nil {
		errs = append(errs, cerr)
	}
	for _, c := range []pg.Conn{spec.SourceConn, spec.TargetConn} {
		conn, err := pg.Connect(ctx, c, 30*time.Second)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := pg.Exec(ctx, conn, `DROP DATABASE IF EXISTS `+pg.QuoteIdent(spec.Source)+` WITH (FORCE)`); err != nil {
			errs = append(errs, fmt.Errorf("dropping %s on %s: %w", spec.Source, c.Host, err))
		}
		conn.Close(ctx)
	}
	os.RemoveAll(spec.RunDir)
	return errors.Join(errs...)
}

// ApplyMeasurements lists measured apply rates, newest first.
func (o *Orchestrator) ApplyMeasurements(ctx context.Context, limit int) ([]ApplyMeasurement, error) {
	rows, err := o.st.DB.QueryContext(ctx, `SELECT id, migration_id, ts, tps, txns, seconds, lower_bound, suggested, assumed_then, by_user FROM apply_measurements ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApplyMeasurement{}
	for rows.Next() {
		var r ApplyMeasurement
		if err := rows.Scan(&r.ID, &r.MigrationID, &r.TS, &r.TPS, &r.Txns, &r.Seconds, &r.LowerBound, &r.Suggested, &r.AssumedThen, &r.By); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
