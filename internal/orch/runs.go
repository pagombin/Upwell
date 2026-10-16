package orch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/store"
)

// D105: a run is one pass of a migration from its start to its verdict, its
// abort, or the re-run that replaced it. Its headline numbers and its report
// are kept so that repeated runs of the same definition can be compared, and
// survive both the re-run and the migration's deletion.

// RunDB is one database of a run.
type RunDB struct {
	Name       string `json:"name"`
	Target     string `json:"target"`
	SizeBytes  int64  `json:"size_bytes"`
	Rows       int64  `json:"rows_estimate"`
	Tables     int    `json:"tables"`
	BaseCopyMs int64  `json:"base_copy_ms,omitempty"`
	Verdict    string `json:"verdict,omitempty"`
	State      string `json:"state"`
	Attempts   int    `json:"attempts"`
}

// RunSummary is what the run history shows without opening a report.
type RunSummary struct {
	Name            string  `json:"name"`
	ShortID         string  `json:"short_id"`
	Mode            string  `json:"mode"`
	Source          string  `json:"source"`
	Target          string  `json:"target"`
	StartedAt       int64   `json:"started_at"`
	EndedAt         int64   `json:"ended_at"`
	DurationMs      int64   `json:"duration_ms"`
	Outcome         string  `json:"outcome"`
	WritePauseMs    int64   `json:"write_pause_ms,omitempty"`
	Unverified      int     `json:"unverified_count,omitempty"`
	TotalBytes      int64   `json:"total_bytes"`
	TotalRows       int64   `json:"total_rows"`
	BaseCopyMs      int64   `json:"base_copy_ms,omitempty"` // the longest database's
	CopyBytesPerSec float64 `json:"copy_bytes_per_s,omitempty"`
	ApplyAvgBps     float64 `json:"apply_avg_bytes_per_s,omitempty"`
	ApplyPeakBps    float64 `json:"apply_peak_bytes_per_s,omitempty"`
	SourceWALAvgBps float64 `json:"source_wal_avg_bytes_per_s,omitempty"`
	PeakBacklog     float64 `json:"peak_backlog_bytes,omitempty"`
	PeakLagSeconds  float64 `json:"peak_lag_seconds,omitempty"`
	Critical        int     `json:"critical_events"`
	Warnings        int     `json:"warning_events"`
	Databases       []RunDB `json:"databases"`
}

// Run is a stored run.
type Run struct {
	ID          string     `json:"id"`
	MigrationID string     `json:"migration_id"`
	Number      int        `json:"number"`
	Outcome     string     `json:"outcome"`
	StartedAt   int64      `json:"started_at"`
	EndedAt     int64      `json:"ended_at"`
	Summary     RunSummary `json:"summary"`
	HasReport   bool       `json:"has_report"`
}

// RunReporter renders a migration's report (HTML, Markdown); set by the API
// layer, which owns the report package.
var RunReporter func(ctx context.Context, migID string) (html, md []byte, err error)

// runSummary gathers the current run's numbers.
func (o *Orchestrator) runSummary(ctx context.Context, m Migration, outcome string, endedAt int64) RunSummary {
	s := RunSummary{Name: m.Name, ShortID: m.ShortID, Mode: m.Mode, EndedAt: endedAt, Outcome: outcome, Unverified: m.Flags.UnverifiedCount}
	if m.StartedAt != nil {
		s.StartedAt = *m.StartedAt
		s.DurationMs = endedAt - *m.StartedAt
	}
	if c := m.Flags.Cutover; c != nil {
		s.WritePauseMs = c.WritePauseMS
	}
	if src, dst, err := o.Conns(ctx, m); err == nil {
		s.Source = hostPort(src.Host, src.Port)
		s.Target = hostPort(dst.Host, dst.Port)
	}
	took := o.BaseCopyDurations(ctx, m.ID)
	dbs, _ := o.ListDatabases(ctx, m.ID)
	for _, d := range included(dbs) {
		var n int
		o.st.DB.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE database_id=?`, d.ID).Scan(&n)
		s.Databases = append(s.Databases, RunDB{Name: d.SourceName, Target: d.TargetName, SizeBytes: d.SizeBytes, Rows: d.RowsEstimate, Tables: d.TableCount,
			BaseCopyMs: took[d.SourceName], Verdict: d.Verdict, State: d.State, Attempts: n})
		s.TotalBytes += d.SizeBytes
		s.TotalRows += d.RowsEstimate
		s.BaseCopyMs = max(s.BaseCopyMs, took[d.SourceName])
	}
	if s.BaseCopyMs > 0 {
		s.CopyBytesPerSec = float64(s.TotalBytes) / (float64(s.BaseCopyMs) / 1000)
	}
	if s.StartedAt > 0 {
		series, _ := o.Metrics(ctx, m.ID, []string{"replay_rate", "src_wal_rate", "backlog_bytes", "heartbeat_latency_s"}, s.StartedAt, endedAt)
		sums := map[string][2]float64{}
		for _, se := range series {
			for _, p := range se.Points {
				switch se.Name {
				case "replay_rate":
					s.ApplyPeakBps = max(s.ApplyPeakBps, p.V)
				case "backlog_bytes":
					s.PeakBacklog = max(s.PeakBacklog, p.V)
				case "heartbeat_latency_s":
					s.PeakLagSeconds = max(s.PeakLagSeconds, p.V)
				}
				x := sums[se.Name]
				sums[se.Name] = [2]float64{x[0] + p.V, x[1] + 1}
			}
		}
		if x := sums["replay_rate"]; x[1] > 0 {
			s.ApplyAvgBps = x[0] / x[1]
		}
		if x := sums["src_wal_rate"]; x[1] > 0 {
			s.SourceWALAvgBps = x[0] / x[1]
		}
		o.st.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(severity='critical'),0), COALESCE(SUM(severity='warning'),0) FROM events WHERE migration_id=? AND ts BETWEEN ? AND ?`,
			m.ID, s.StartedAt, endedAt).Scan(&s.Critical, &s.Warnings)
	}
	return s
}

func hostPort(h string, p int) string { return h + ":" + strconv.Itoa(p) }

// archiveRun stores (or updates) the current run of the migration. Best
// effort: a failure is logged, never fails the caller.
func (o *Orchestrator) archiveRun(ctx context.Context, migID, outcome string) {
	m, err := o.GetMigration(ctx, migID)
	if err != nil || m.StartedAt == nil || m.Fixture {
		return
	}
	now := store.Now()
	s := o.runSummary(ctx, m, outcome, now)
	sum, _ := json.Marshal(s)
	var html, md []byte
	if RunReporter != nil {
		if h, mk, err := RunReporter(ctx, m.ID); err == nil {
			html, md = h, mk
		} else {
			o.logf("warn", "orchestrator", m.ID, "", "", "the run's report could not be kept: %v", err)
		}
	}
	var n int
	o.st.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(number),0)+1 FROM runs WHERE migration_id=?`, m.ID).Scan(&n)
	_, err = o.st.DB.ExecContext(ctx, `INSERT INTO runs(id,migration_id,migration_name,short_id,number,started_at,ended_at,outcome,summary,report_html,report_md,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(migration_id, started_at) DO UPDATE SET ended_at=excluded.ended_at, outcome=excluded.outcome, summary=excluded.summary,
			report_html=excluded.report_html, report_md=excluded.report_md, migration_name=excluded.migration_name`,
		store.NewID(), m.ID, m.Name, m.ShortID, n, *m.StartedAt, now, outcome, string(sum), store.NullString(string(html)), store.NullString(string(md)), now)
	if err != nil {
		o.logf("error", "orchestrator", m.ID, "", "", "the run could not be recorded in the history: %v", err)
	}
}

// ListRuns returns every recorded run, newest first.
func (o *Orchestrator) ListRuns(ctx context.Context) ([]Run, error) {
	rows, err := o.st.DB.QueryContext(ctx, `SELECT id, migration_id, number, outcome, started_at, ended_at, summary, report_html IS NOT NULL FROM runs ORDER BY ended_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var r Run
		var sum string
		if err := rows.Scan(&r.ID, &r.MigrationID, &r.Number, &r.Outcome, &r.StartedAt, &r.EndedAt, &sum, &r.HasReport); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(sum), &r.Summary)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunReport returns a run's stored report.
func (o *Orchestrator) RunReport(ctx context.Context, id, format string) ([]byte, error) {
	col := "report_html"
	if format == "md" {
		col = "report_md"
	}
	var b sql.NullString
	err := o.st.DB.QueryRowContext(ctx, `SELECT `+col+` FROM runs WHERE id=?`, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !b.Valid {
		return nil, uerr("no_report", "This run's report was not kept.", "")
	}
	return []byte(b.String), nil
}

// DeleteRun removes a run from the history.
func (o *Orchestrator) DeleteRun(ctx context.Context, a audit.Actor, id string) error {
	var name string
	var ended int64
	err := o.st.DB.QueryRowContext(ctx, `SELECT migration_name, ended_at FROM runs WHERE id=?`, id).Scan(&name, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := o.st.DB.ExecContext(ctx, `DELETE FROM runs WHERE id=?`, id); err != nil {
		return err
	}
	o.audit.Append(ctx, a, "run.delete", id, map[string]any{"migration": name, "ended_at": time.UnixMilli(ended).UTC().Format(time.RFC3339)}, nil)
	return nil
}

// CurrentRunSummary returns the headline numbers of the migration's current
// run, as the run history would record them now.
func (o *Orchestrator) CurrentRunSummary(ctx context.Context, id string) (RunSummary, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return RunSummary{}, err
	}
	outcome := m.Flags.Verdict
	switch {
	case outcome != "":
	case m.Flags.Aborted:
		outcome = "aborted"
	case m.Flags.Started:
		outcome = "running"
	default:
		outcome = "not started"
	}
	end := store.Now()
	if m.Flags.VerdictAt > 0 {
		end = m.Flags.VerdictAt
	}
	return o.runSummary(ctx, m, outcome, end), nil
}
