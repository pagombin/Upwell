package orch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/check"
	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/pgpack"
	"github.com/pagombin/upwell/internal/settings"
	"github.com/pagombin/upwell/internal/store"
)

// PreflightRun is one preflight execution.
type PreflightRun struct {
	ID          string        `json:"id"`
	MigrationID string        `json:"migration_id"`
	OperationID string        `json:"operation_id"`
	State       string        `json:"state"`
	StartedAt   int64         `json:"started_at"`
	EndedAt     *int64        `json:"ended_at,omitempty"`
	Summary     check.Summary `json:"summary"`
	Fingerprint string        `json:"fingerprint"`
}

// fingerprint captures everything a preflight result depends on.
func (o *Orchestrator) fingerprint(ctx context.Context, m Migration) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|", m.SourceConnID, m.TargetConnID, toJSON(m.Settings))
	dbs, _ := o.ListDatabases(ctx, m.ID)
	for _, d := range dbs {
		if d.Include {
			fmt.Fprintf(h, "%s>%s;", d.SourceName, d.TargetName)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (o *Orchestrator) preflightStillValid(ctx context.Context, m Migration) bool {
	run, _, err := o.LatestPreflight(ctx, m.ID)
	if err != nil || run == nil {
		return false
	}
	return run.Fingerprint == o.fingerprint(ctx, m)
}

// RunPreflight starts preflight in the background and returns its operation.
func (o *Orchestrator) RunPreflight(ctx context.Context, a audit.Actor, id, key string) (*Operation, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, err
	}
	if m.Flags.Started {
		return nil, uerr("started", "Preflight runs before the migration starts.", "")
	}
	if m.Flags.PreflightRunning {
		return nil, uerr("preflight_running", "Preflight is already running.", "")
	}
	if err := o.requirePermission(ctx, m); err != nil {
		return nil, err
	}
	if m.SourceConnID == "" || m.TargetConnID == "" {
		return nil, uerr("no_connection", "Enter the source and target connections first.", "")
	}
	steps := []Step{{Key: "migration", Title: "Customer permission"}, {Key: "host", Title: "Droplet and tools"}, {Key: "clusters", Title: "Clusters"},
		{Key: "databases", Title: "Databases"}, {Key: "sampling", Title: "WAL rate sample"}}
	op, err := o.newOp(ctx, m.ID, "preflight", key, a.Username, steps, nil)
	if err != nil {
		return op, err
	}
	runID := store.NewID()
	fp := o.fingerprint(ctx, m)
	o.st.DB.ExecContext(ctx, `INSERT INTO preflight_runs(id,migration_id,operation_id,state,started_at,summary) VALUES (?,?,?,?,?,?)`, runID, m.ID, op.ID, "running", store.Now(), toJSON(map[string]any{"fingerprint": fp}))
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.PreflightRunning = true; f.PreflightRunID = runID; f.PreflightOK = false })
	o.audit.Append(ctx, a, "preflight.run", m.ID, nil, map[string]any{"run_id": runID})
	go o.preflightWorker(m, op, runID, fp)
	return op, nil
}

func (o *Orchestrator) preflightWorker(m Migration, op *Operation, runID, fp string) {
	ctx := context.Background()
	defer func() {
		if r := recover(); r != nil {
			op.Finish(fmt.Errorf("preflight crashed: %v", r), nil)
			o.updateFlags(ctx, m.ID, func(f *Flags) { f.PreflightRunning = false })
		}
	}()
	src, dst, err := o.Conns(ctx, m)
	if err != nil {
		op.Finish(err, nil)
		o.updateFlags(ctx, m.ID, func(f *Flags) { f.PreflightRunning = false })
		return
	}
	_, tconn, _ := o.PGConn(ctx, m.TargetConnID)
	dbs, _ := o.ListDatabases(ctx, m.ID)
	var pdbs []pgpack.DB
	for _, d := range included(dbs) {
		pdbs = append(pdbs, pgpack.DB{Source: d.SourceName, Target: d.TargetName, SlotName: d.SlotName, Origin: d.OriginName})
	}
	perm, _ := o.GetPermission(ctx, m.ID)
	pp := pgpack.Permission{Present: perm != nil, Missing: perm.Missing()}
	if perm != nil {
		pp.Customer, pp.Ticket = perm.Customer, perm.Ticket
	}
	ver, verr := o.eng.Version(ctx)
	acc := o.acceptances(ctx, m.ID)
	var results []check.Result
	current := ""
	stepFor := map[string]string{check.ScopeMigration: "migration", check.ScopeHost: "host", check.ScopeClusters: "clusters", check.ScopeSource: "clusters", check.ScopeTarget: "clusters", check.ScopeDatabase: "databases"}
	emit := func(r check.Result) {
		step := stepFor[r.Scope]
		if r.CheckID == "throughput_sample" || r.CheckID == "cdc_throughput" || r.CheckID == "slot_wal_cap" || r.CheckID == "staging_space" {
			step = "sampling"
		}
		if r.Scope == "source" || r.Scope == "target" {
			step = "clusters"
		}
		if step != current {
			if current != "" {
				op.StepEnd(current, "done", "")
			}
			op.StepStart(step, "")
			current = step
		}
		r.ID = store.NewID()
		if a, ok := acc[accKey(r.CheckID, r.Scope, r.Database)]; ok && r.Level == check.Blocker && !r.Hard {
			if a.hash == r.EvidenceHash {
				r.Accepted = &check.Acceptance{ID: a.id, Reason: a.reason, AcceptedBy: a.by, AcceptedAt: a.at}
			} else {
				o.st.DB.Exec(`UPDATE acceptances SET lapsed_at=? WHERE id=?`, store.Now(), a.id)
				op.Log("warn", "acceptance of %s for %s lapsed: the evidence changed", r.CheckID, firstNonEmpty(r.Database, r.Scope))
			}
		}
		o.st.DB.Exec(`INSERT INTO check_results(id,run_id,check_id,check_version,scope,database,level,hard,title,message,evidence,evidence_hash,remediation,duration_ms,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, runID, r.CheckID, r.Version, r.Scope, r.Database, r.Level, r.Hard, r.Title, r.Message, toJSON(r.Evidence), r.EvidenceHash, r.Remediation, r.DurationMS, store.Now())
		results = append(results, r)
		lvl := "info"
		if r.Level == check.Blocker {
			lvl = "warn"
		}
		op.LogDB(lvl, r.Database, "%s [%s%s]: %s", r.Title, r.Level, map[bool]string{true: ", hard", false: ""}[r.Hard], r.Message)
		o.bus.Publish("migration", "check", m.ID, r)
	}
	runner := &pgpack.Runner{Env: pgpack.Env{Source: src, Target: dst, TargetStorageGB: tconn.StorageGB, Databases: pdbs, Settings: o.Settings(ctx, m),
		EngineVersion: ver, EngineErr: verr, PgBinDir: o.cfg.Engine.PgBinDir, WorkDir: o.cfg.RunsDir(), Permission: pp}, Emit: emit}
	runner.Run(ctx)
	if current != "" {
		op.StepEnd(current, "done", "")
	}
	sum := check.Summarize(results)
	now := store.Now()
	o.st.DB.Exec(`UPDATE preflight_runs SET state='done', ended_at=?, summary=? WHERE id=?`, now, toJSON(map[string]any{"fingerprint": fp, "summary": sum}), runID)
	o.updateFlags(ctx, m.ID, func(f *Flags) { f.PreflightRunning = false; f.PreflightOK = sum.CanStart })
	o.Event(ctx, m.ID, "", "preflight", map[bool]string{true: "info", false: "warning"}[sum.CanStart],
		fmt.Sprintf("Preflight finished: %d ok, %d warnings, %d blockers (%d hard), %d accepted", sum.OK, sum.Warnings, sum.Blockers+sum.Hard, sum.Hard, sum.Accepted), nil)
	o.refreshState(ctx, m.ID)
	op.Finish(nil, map[string]any{"run_id": runID, "summary": sum})
}

type accRow struct {
	id, hash, reason, by string
	at                   int64
}

func accKey(id, scope, db string) string { return id + "|" + scope + "|" + db }

func (o *Orchestrator) acceptances(ctx context.Context, mig string) map[string]accRow {
	rows, err := o.st.DB.QueryContext(ctx, `SELECT id, check_id, scope, database, evidence_hash, reason, accepted_by, accepted_at FROM acceptances WHERE migration_id=? AND lapsed_at IS NULL ORDER BY accepted_at`, mig)
	out := map[string]accRow{}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var a accRow
		var cid, scope, db string
		rows.Scan(&a.id, &cid, &scope, &db, &a.hash, &a.reason, &a.by, &a.at)
		out[accKey(cid, scope, db)] = a
	}
	return out
}

// LatestPreflight returns the newest run and its results with acceptances.
func (o *Orchestrator) LatestPreflight(ctx context.Context, mig string) (*PreflightRun, []check.Result, error) {
	var r PreflightRun
	var sum string
	var ended sql.NullInt64
	var opID sql.NullString
	err := o.st.DB.QueryRowContext(ctx, `SELECT id, migration_id, operation_id, state, started_at, ended_at, summary FROM preflight_runs WHERE migration_id=? ORDER BY started_at DESC LIMIT 1`, mig).
		Scan(&r.ID, &r.MigrationID, &opID, &r.State, &r.StartedAt, &ended, &sum)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	r.OperationID = opID.String
	if ended.Valid {
		r.EndedAt = &ended.Int64
	}
	var s struct {
		Fingerprint string        `json:"fingerprint"`
		Summary     check.Summary `json:"summary"`
	}
	scanJSON(sum, &s)
	r.Fingerprint, r.Summary = s.Fingerprint, s.Summary
	res, err := o.CheckResults(ctx, mig, r.ID)
	if err == nil && r.State == "done" {
		r.Summary = check.Summarize(res)
	}
	return &r, res, err
}

// PreflightRuns lists runs, newest first.
func (o *Orchestrator) PreflightRuns(ctx context.Context, mig string) ([]PreflightRun, error) {
	rows, err := o.st.DB.QueryContext(ctx, `SELECT id, state, started_at, ended_at, summary FROM preflight_runs WHERE migration_id=? ORDER BY started_at DESC LIMIT 50`, mig)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PreflightRun{}
	for rows.Next() {
		var r PreflightRun
		var ended sql.NullInt64
		var sum string
		rows.Scan(&r.ID, &r.State, &r.StartedAt, &ended, &sum)
		if ended.Valid {
			r.EndedAt = &ended.Int64
		}
		var s struct {
			Summary check.Summary `json:"summary"`
		}
		scanJSON(sum, &s)
		r.Summary = s.Summary
		r.MigrationID = mig
		out = append(out, r)
	}
	return out, nil
}

// CheckResults returns a run's results with current acceptances applied.
func (o *Orchestrator) CheckResults(ctx context.Context, mig, runID string) ([]check.Result, error) {
	rows, err := o.st.DB.QueryContext(ctx, `SELECT id, check_id, check_version, scope, database, level, hard, title, message, evidence, evidence_hash, remediation, duration_ms FROM check_results WHERE run_id=? ORDER BY created_at, rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	acc := o.acceptances(ctx, mig)
	out := []check.Result{}
	for rows.Next() {
		var r check.Result
		var ev string
		if err := rows.Scan(&r.ID, &r.CheckID, &r.Version, &r.Scope, &r.Database, &r.Level, &r.Hard, &r.Title, &r.Message, &ev, &r.EvidenceHash, &r.Remediation, &r.DurationMS); err != nil {
			return nil, err
		}
		scanJSON(ev, &r.Evidence)
		if a, ok := acc[accKey(r.CheckID, r.Scope, r.Database)]; ok && a.hash == r.EvidenceHash && r.Level == check.Blocker && !r.Hard {
			r.Accepted = &check.Acceptance{ID: a.id, Reason: a.reason, AcceptedBy: a.by, AcceptedAt: a.at}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Accept records an operator accepting an acceptable blocker from the latest run.
func (o *Orchestrator) Accept(ctx context.Context, a audit.Actor, mig, resultID, reason string) (check.Result, error) {
	reason = strings.TrimSpace(reason)
	if len(reason) < 10 {
		return check.Result{}, uerr("reason_required", "Write a reason of at least 10 characters for accepting this risk.", "")
	}
	m, err := o.GetMigration(ctx, mig)
	if err != nil {
		return check.Result{}, err
	}
	if m.Flags.Started {
		return check.Result{}, uerr("started", "Risks are accepted before the migration starts.", "")
	}
	run, results, err := o.LatestPreflight(ctx, m.ID)
	if err != nil || run == nil {
		return check.Result{}, uerr("no_preflight", "Run preflight first.", "")
	}
	for _, r := range results {
		if r.ID != resultID {
			continue
		}
		if r.Level != check.Blocker {
			return r, uerr("not_blocker", "Only blockers can be accepted.", "")
		}
		if r.Hard {
			return r, uerr("hard_blocker", "This blocker cannot be accepted: the migration would fail with it.", r.Remediation)
		}
		id := store.NewID()
		now := store.Now()
		o.st.DB.ExecContext(ctx, `UPDATE acceptances SET lapsed_at=? WHERE migration_id=? AND check_id=? AND scope=? AND database=? AND lapsed_at IS NULL`, now, m.ID, r.CheckID, r.Scope, r.Database)
		_, err := o.st.DB.ExecContext(ctx, `INSERT INTO acceptances(id,migration_id,check_id,scope,database,evidence_hash,reason,accepted_by,accepted_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			id, m.ID, r.CheckID, r.Scope, r.Database, r.EvidenceHash, reason, a.Username, now)
		if err != nil {
			return r, err
		}
		r.Accepted = &check.Acceptance{ID: id, Reason: reason, AcceptedBy: a.Username, AcceptedAt: now}
		o.audit.Append(ctx, a, "preflight.accept", m.ID, nil, map[string]any{"check": r.CheckID, "scope": r.Scope, "database": r.Database, "evidence_hash": r.EvidenceHash, "reason": reason})
		o.Event(ctx, m.ID, r.Database, "accepted", "warning", fmt.Sprintf("%s accepted the risk %q: %s", a.Username, r.Title, reason), nil)
		_, after, _ := o.LatestPreflight(ctx, m.ID)
		o.updateFlags(ctx, m.ID, func(f *Flags) { f.PreflightOK = check.Summarize(after).CanStart })
		o.refreshState(ctx, m.ID)
		return r, nil
	}
	return check.Result{}, store.ErrNotFound
}

// AcceptedRisks lists acceptances for the report.
func (o *Orchestrator) AcceptedRisks(ctx context.Context, mig string) []map[string]any {
	rows, err := o.st.DB.QueryContext(ctx, `SELECT check_id, scope, database, reason, accepted_by, accepted_at, lapsed_at FROM acceptances WHERE migration_id=? ORDER BY accepted_at`, mig)
	out := []map[string]any{}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var cid, scope, db, reason, by string
		var at int64
		var lapsed sql.NullInt64
		rows.Scan(&cid, &scope, &db, &reason, &by, &at, &lapsed)
		out = append(out, map[string]any{"check_id": cid, "scope": scope, "database": db, "reason": reason, "accepted_by": by, "accepted_at": at, "lapsed": lapsed.Valid})
	}
	return out
}

// PlanEntry is the dry run for one database.
type PlanEntry struct {
	Database   string         `json:"database"`
	Target     string         `json:"target"`
	Plan       engine.RunPlan `json:"plan"`
	SizeBytes  int64          `json:"size_bytes"`
	EstSeconds float64        `json:"estimated_copy_seconds"`
	Plugin     string         `json:"plugin"`
}

// Plan returns the exact engine command per database and estimates.
func (o *Orchestrator) Plan(ctx context.Context, mig string) ([]PlanEntry, map[string]any, error) {
	m, err := o.GetMigration(ctx, mig)
	if err != nil {
		return nil, nil, err
	}
	dbs, _ := o.ListDatabases(ctx, m.ID)
	v := o.Settings(ctx, m)
	rate := v.Float("assumed_throughput_mibs")
	if rate <= 0 {
		rate = 102
	}
	var out []PlanEntry
	var total int64
	for _, d := range included(dbs) {
		spec, err := o.dbSpec(ctx, m, d)
		if err != nil {
			return nil, nil, uerr("no_connection", err.Error(), "")
		}
		if spec.Plugin == "" && v.Str("decoding_plugin") != "auto" {
			spec.Plugin = v.Str("decoding_plugin")
		}
		p, err := o.eng.Plan(spec, false)
		if err != nil {
			return nil, nil, err
		}
		total += d.SizeBytes
		out = append(out, PlanEntry{Database: d.SourceName, Target: d.TargetName, Plan: p, SizeBytes: d.SizeBytes, EstSeconds: float64(d.SizeBytes) / (rate * 1024 * 1024), Plugin: firstNonEmpty(d.Plugin, "auto")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SizeBytes > out[j].SizeBytes })
	conc := max(1, min(len(out), v.Int("max_concurrent_base_copies")))
	est := map[string]any{"total_bytes": total, "assumed_mibs": rate, "concurrent": conc, "estimated_copy_seconds": float64(total) / (rate * 1024 * 1024),
		"target_disk_needed_bytes": int64(float64(total) * v.Float("target_disk_factor")), "work_dir": filepath.Clean(o.cfg.RunsDir())}
	// Connections: each running database uses table jobs + index jobs + about
	// three engine sessions (main, sentinel, CDC walsender) on the source.
	tj, ij := settings.Jobs(v, conc)
	est["table_jobs"], est["index_jobs"] = tj, ij
	est["source_connections_peak"] = conc*(tj+ij+3) + 2
	est["target_connections_peak"] = conc*(tj+ij+2) + 2
	// WAL retention: the slot holds WAL written during the base copy.
	if w, ok := o.LastValue(m.ID, "", "src_wal_rate"); ok && w > 0 {
		est["source_wal_rate_bps"] = w
		est["wal_retained_during_copy_bytes"] = int64(w * float64(total) / (rate * 1024 * 1024) / float64(conc))
	}
	return out, est, nil
}
