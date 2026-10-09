package orch

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/store"
)

// collector samples metrics into the compact series-plus-samples store (PC7).
type collector struct {
	o         *Orchestrator
	mu        sync.Mutex
	series    map[string]int64
	prev      map[string]prevSample
	slow      map[string]time.Time
	hb        map[string]time.Time
	cpuPrev   [2]uint64
	stale     map[string]int
	last      map[string]float64
	lastWrite time.Time
}

type prevSample struct {
	lsn string
	ts  time.Time
}

func newCollector(o *Orchestrator) *collector {
	return &collector{o: o, series: map[string]int64{}, prev: map[string]prevSample{}, slow: map[string]time.Time{}, hb: map[string]time.Time{}, stale: map[string]int{}, last: map[string]float64{}}
}

func (c *collector) seriesID(ctx context.Context, mig, db, name string) int64 {
	k := mig + "|" + db + "|" + name
	c.mu.Lock()
	id, ok := c.series[k]
	c.mu.Unlock()
	if ok {
		return id
	}
	c.o.st.DB.ExecContext(ctx, `INSERT OR IGNORE INTO metric_series(migration_id, database, name) VALUES (?,?,?)`, mig, db, name)
	c.o.st.DB.QueryRowContext(ctx, `SELECT id FROM metric_series WHERE migration_id=? AND database=? AND name=?`, mig, db, name).Scan(&id)
	c.mu.Lock()
	c.series[k] = id
	c.mu.Unlock()
	return id
}

// put writes one sample.
func (c *collector) put(ctx context.Context, mig, db, name string, v float64, ts int64) {
	id := c.seriesID(ctx, mig, db, name)
	c.o.st.DB.ExecContext(ctx, `INSERT OR REPLACE INTO metric_samples(series_id, ts, value) VALUES (?,?,?)`, id, ts, v)
	c.mu.Lock()
	c.last[mig+"|"+db+"|"+name] = v
	c.lastWrite = time.Now()
	c.mu.Unlock()
}

// Last returns the most recent value of a series in memory.
func (c *collector) Last(mig, db, name string) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.last[mig+"|"+db+"|"+name]
	return v, ok
}

func (c *collector) due(k string, every time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.slow[k]) < every {
		return false
	}
	c.slow[k] = time.Now()
	return true
}

func (c *collector) dueHeartbeat(dbID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.hb[dbID]) < 10*time.Second {
		return false
	}
	c.hb[dbID] = time.Now()
	return true
}

func (c *collector) currentLSN(ctx context.Context, conn pg.Conn) (string, error) {
	cn, err := pg.Connect(ctx, conn, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer cn.Close(ctx)
	var s string
	err = cn.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&s)
	return s, err
}

var stateCode = map[string]float64{DPending: 0, DPreparing: 1, DBaseCopy: 2, DCatchUp: 3, DInSync: 4, DDraining: 5, DDrained: 6, DVerified: 7, DStopped: 8, DFailed: 9, DDegraded: 10, DRestartRequired: 11}

// sample collects per-database and cluster metrics for one migration.
func (c *collector) sample(ctx context.Context, m Migration, dbs []Database) {
	now := time.Now()
	ts := now.UnixMilli()
	src, dst, err := c.o.Conns(ctx, m)
	if err != nil {
		return
	}
	scn, err := pg.Connect(ctx, src, 5*time.Second)
	if err != nil {
		c.markStale(ctx, m, "source", err)
		return
	}
	defer scn.Close(ctx)
	c.clearStale(m.ID, "source")
	var cur string
	scn.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&cur)
	if p, ok := c.prev[m.ID+"|wal"]; ok && cur != "" {
		dt := now.Sub(p.ts).Seconds()
		if dt > 0 {
			c.put(ctx, m.ID, "", "src_wal_rate", float64(pg.LSNDiff(cur, p.lsn))/dt, ts)
		}
	}
	c.prev[m.ID+"|wal"] = prevSample{cur, now}
	var conns, oldestXact, xminAge float64
	scn.QueryRow(ctx, `SELECT count(*), COALESCE(max(extract(epoch FROM now()-xact_start)),0), COALESCE(max(age(backend_xmin)),0) FROM pg_stat_activity`).Scan(&conns, &oldestXact, &xminAge)
	c.put(ctx, m.ID, "", "src_connections", conns, ts)
	c.put(ctx, m.ID, "", "src_oldest_xact_s", oldestXact, ts)
	c.put(ctx, m.ID, "", "src_xmin_age", xminAge, ts)
	slowSizes := c.due(m.ID+"|sizes", 30*time.Second)
	var dcn interface {
		Close(context.Context) error
	}
	if tc, err := pg.Connect(ctx, dst, 5*time.Second); err == nil {
		dcn = tc
		c.clearStale(m.ID, "target")
		var dconns float64
		tc.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity`).Scan(&dconns)
		c.put(ctx, m.ID, "", "dst_connections", dconns, ts)
		if slowSizes {
			for _, d := range dbs {
				var tsz float64
				if tc.QueryRow(ctx, `SELECT pg_database_size($1)`, d.TargetName).Scan(&tsz) == nil {
					c.put(ctx, m.ID, d.SourceName, "target_size_bytes", tsz, ts)
				}
			}
		}
	} else {
		c.markStale(ctx, m, "target", err)
	}
	if dcn != nil {
		defer func() { _ = dcn.Close(ctx) }()
	}
	for _, d := range dbs {
		c.put(ctx, m.ID, d.SourceName, "state", stateCode[d.State], ts)
		if d.BacklogBytes != nil && *d.BacklogBytes >= 0 {
			c.put(ctx, m.ID, d.SourceName, "backlog_bytes", float64(*d.BacklogBytes), ts)
		}
		if d.ReplayLSN != "" && d.ReplayLSN != "0/0" {
			k := m.ID + "|" + d.ID + "|replay"
			if p, ok := c.prev[k]; ok {
				dt := now.Sub(p.ts).Seconds()
				if dt > 0 {
					c.put(ctx, m.ID, d.SourceName, "replay_rate", float64(pg.LSNDiff(d.ReplayLSN, p.lsn))/dt, ts)
				}
			}
			c.prev[k] = prevSample{d.ReplayLSN, now}
		}
		var active *bool
		var walStatus *string
		var retained, safe *float64
		err := scn.QueryRow(ctx, `SELECT active, wal_status, pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)::float8, safe_wal_size::float8 FROM pg_replication_slots WHERE slot_name=$1`, d.SlotName).Scan(&active, &walStatus, &retained, &safe)
		if err == nil {
			a := 0.0
			if active != nil && *active {
				a = 1
			}
			c.put(ctx, m.ID, d.SourceName, "slot_active", a, ts)
			if retained != nil {
				c.put(ctx, m.ID, d.SourceName, "slot_retained_bytes", *retained, ts)
			}
			if safe != nil {
				c.put(ctx, m.ID, d.SourceName, "slot_headroom_bytes", *safe, ts)
			}
			if walStatus != nil {
				c.put(ctx, m.ID, d.SourceName, "slot_wal_status", map[string]float64{"reserved": 0, "extended": 1, "unreserved": 2, "lost": 3}[*walStatus], ts)
			}
		}
		if slowSizes && activeDB[d.State] {
			var ssz float64
			if scn.QueryRow(ctx, `SELECT pg_database_size($1)`, d.SourceName).Scan(&ssz) == nil {
				c.put(ctx, m.ID, d.SourceName, "source_size_bytes", ssz, ts)
			}
		}
		if d.BaseCopyDone && activeDB[d.State] && c.o.Settings(ctx, m).Bool("heartbeat") && c.due(d.ID+"|hblat", 10*time.Second) {
			if s, err := pg.Connect(ctx, src.WithDB(d.SourceName), 5*time.Second); err == nil {
				if t, err := pg.Connect(ctx, dst.WithDB(d.TargetName), 5*time.Second); err == nil {
					var sMax, tMax *time.Time
					s.QueryRow(ctx, `SELECT max(written_at) FROM upwell.heartbeat`).Scan(&sMax)
					t.QueryRow(ctx, `SELECT max(written_at) FROM upwell.heartbeat`).Scan(&tMax)
					if sMax != nil && tMax == nil {
						// No heartbeat has reached the target at all: it is at
						// least as far behind as the oldest one on the source.
						s.QueryRow(ctx, `SELECT min(written_at) FROM upwell.heartbeat`).Scan(&tMax)
					}
					if sMax != nil && tMax != nil {
						c.put(ctx, m.ID, d.SourceName, "heartbeat_latency_s", sMax.Sub(*tMax).Seconds(), ts)
					}
					t.Close(ctx)
				}
				s.Close(ctx)
			}
		}
		var errs float64
		c.o.st.DB.QueryRowContext(ctx, `SELECT count(*) FROM logs WHERE migration_id=? AND database=? AND component='engine' AND level IN ('error','fatal')`, m.ID, d.SourceName).Scan(&errs)
		c.put(ctx, m.ID, d.SourceName, "log_errors", errs, ts)
	}
	c.o.bus.Publish("migration", "metrics", m.ID, map[string]any{"ts": ts})
}

func (c *collector) markStale(ctx context.Context, m Migration, side string, err error) {
	c.mu.Lock()
	c.stale[m.ID+"|"+side]++
	n := c.stale[m.ID+"|"+side]
	c.mu.Unlock()
	if n == 3 {
		c.o.alerts.fire(ctx, m.ID, "cluster_unreachable", side, "critical", fmt.Sprintf("The %s cluster has not answered for 3 samples: %v", side, err))
	}
}

func (c *collector) clearStale(mig, side string) {
	c.mu.Lock()
	n := c.stale[mig+"|"+side]
	c.stale[mig+"|"+side] = 0
	c.mu.Unlock()
	if n >= 3 {
		c.o.alerts.resolve(context.Background(), mig, "cluster_unreachable", side)
	}
}

// hostSample records droplet metrics under the pseudo-migration "_host".
func (c *collector) hostSample(ctx context.Context) {
	ts := store.Now()
	if pct, free, err := diskUsage(c.o.cfg.DataDir); err == nil {
		c.put(ctx, "_host", "", "work_volume_pct", pct, ts)
		c.put(ctx, "_host", "", "work_volume_free_bytes", float64(free), ts)
	}
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		f := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
		var total, idle uint64
		for i, x := range f[1:] {
			n, _ := strconv.ParseUint(x, 10, 64)
			total += n
			if i == 3 || i == 4 {
				idle += n
			}
		}
		if c.cpuPrev[0] > 0 && total > c.cpuPrev[0] {
			c.put(ctx, "_host", "", "cpu_pct", 100*(1-float64(idle-c.cpuPrev[1])/float64(total-c.cpuPrev[0])), ts)
		}
		c.cpuPrev = [2]uint64{total, idle}
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		var total, avail float64
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) >= 2 {
				v, _ := strconv.ParseFloat(fs[1], 64)
				switch fs[0] {
				case "MemTotal:":
					total = v
				case "MemAvailable:":
					avail = v
				}
			}
		}
		f.Close()
		if total > 0 {
			c.put(ctx, "_host", "", "mem_pct", 100*(1-avail/total), ts)
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if v, err := strconv.ParseFloat(strings.Fields(string(b))[0], 64); err == nil {
			c.put(ctx, "_host", "", "load1", v, ts)
		}
	}
	if c.due("_host|staging", 60*time.Second) {
		var total int64
		filepath.Walk(c.o.cfg.RunsDir(), func(_ string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				total += fi.Size()
			}
			return nil
		})
		c.put(ctx, "_host", "", "staging_bytes", float64(total), ts)
	}
	c.o.alerts.evaluateHost(ctx)
	c.o.bus.Publish("system", "metrics", "", map[string]any{"ts": ts})
}

// rollupLoop aggregates raw samples into 1-minute and 15-minute rollups and
// applies retention.
func (c *collector) rollupLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.rollup(ctx)
	}
}

func (c *collector) rollup(ctx context.Context) {
	now := store.Now()
	minuteEnd := now - now%60000
	db := c.o.st.DB
	db.ExecContext(ctx, `INSERT OR REPLACE INTO metric_rollups_1m(series_id, ts, vmin, vmax, vavg, vlast)
		SELECT series_id, (ts/60000)*60000 AS m, min(value), max(value), avg(value),
		  (SELECT value FROM metric_samples s2 WHERE s2.series_id=s.series_id AND s2.ts < ? AND s2.ts >= (s.ts/60000)*60000 ORDER BY ts DESC LIMIT 1)
		FROM metric_samples s WHERE ts >= ? AND ts < ? GROUP BY series_id, m`, minuteEnd, minuteEnd-5*60000, minuteEnd)
	q := minuteEnd - minuteEnd%(15*60000)
	db.ExecContext(ctx, `INSERT OR REPLACE INTO metric_rollups_15m(series_id, ts, vmin, vmax, vavg, vlast)
		SELECT series_id, (ts/900000)*900000 AS q, min(vmin), max(vmax), avg(vavg), max(vlast)
		FROM metric_rollups_1m WHERE ts >= ? AND ts < ? GROUP BY series_id, q`, q-2*900000, minuteEnd)
	hours := c.o.globalSettings(ctx).Int("metrics_raw_retention_hours")
	if hours <= 0 {
		hours = 72
	}
	db.ExecContext(ctx, `DELETE FROM metric_samples WHERE ts < ?`, now-int64(hours)*3600*1000)
	db.ExecContext(ctx, `DELETE FROM metric_rollups_1m WHERE ts < ?`, now-90*24*3600*1000)
	days := c.o.globalSettings(ctx).Int("log_retention_days")
	if days > 0 {
		// Keep logs of migrations that are not cleaned up.
		db.ExecContext(ctx, `DELETE FROM logs WHERE ts < ? AND (migration_id IS NULL OR migration_id IN (SELECT id FROM migrations WHERE json_extract(flags,'$.cleaned_up')=1))`, now-int64(days)*86400*1000)
	}
}

// Point is one time-series point.
type Point struct {
	TS  int64    `json:"ts"`
	V   float64  `json:"v"`
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
}

// Series is one named series.
type Series struct {
	Name     string  `json:"name"`
	Database string  `json:"database,omitempty"`
	Points   []Point `json:"points"`
}

// Metrics returns series for a migration between from and to (ms). Ranges
// over 6 hours use 1-minute rollups.
func (o *Orchestrator) Metrics(ctx context.Context, mig string, names []string, from, to int64) ([]Series, error) {
	if to == 0 {
		to = store.Now()
	}
	if from == 0 {
		from = to - 3600*1000
	}
	table, valCol := "metric_samples", "value"
	extra := "NULL, NULL"
	if to-from > 6*3600*1000 {
		table, valCol, extra = "metric_rollups_1m", "vavg", "vmin, vmax"
	}
	q := `SELECT s.name, s.database, x.ts, x.` + valCol + `, ` + extra + ` FROM metric_series s JOIN ` + table + ` x ON x.series_id=s.id
		WHERE s.migration_id=? AND x.ts BETWEEN ? AND ?`
	args := []any{mig, from, to}
	if len(names) > 0 {
		q += ` AND s.name IN (` + strings.TrimSuffix(strings.Repeat("?,", len(names)), ",") + `)`
		for _, n := range names {
			args = append(args, n)
		}
	}
	q += ` ORDER BY s.name, s.database, x.ts`
	rows, err := o.st.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Series
	idx := map[string]int{}
	for rows.Next() {
		var name, db string
		var p Point
		var mn, mx sql.NullFloat64
		var v sql.NullFloat64
		if err := rows.Scan(&name, &db, &p.TS, &v, &mn, &mx); err != nil {
			return nil, err
		}
		p.V = v.Float64
		if mn.Valid {
			p.Min, p.Max = &mn.Float64, &mx.Float64
		}
		k := name + "|" + db
		i, ok := idx[k]
		if !ok {
			out = append(out, Series{Name: name, Database: db})
			i = len(out) - 1
			idx[k] = i
		}
		out[i].Points = append(out[i].Points, p)
	}
	return out, rows.Err()
}

// LastWrite returns when the recorder last wrote a sample.
func (o *Orchestrator) LastMetricWrite() time.Time {
	o.metrics.mu.Lock()
	defer o.metrics.mu.Unlock()
	return o.metrics.lastWrite
}

// LastValue returns the newest in-memory value of a series.
func (o *Orchestrator) LastValue(mig, db, name string) (float64, bool) {
	return o.metrics.Last(mig, db, name)
}

// ---------- alerts

type alerter struct {
	o       *Orchestrator
	mu      sync.Mutex
	backlog map[string][]int64
}

func newAlerter(o *Orchestrator) *alerter { return &alerter{o: o, backlog: map[string][]int64{}} }

// Alert is one alert.
type Alert struct {
	ID          string `json:"id"`
	MigrationID string `json:"migration_id,omitempty"`
	Rule        string `json:"rule"`
	Scope       string `json:"scope"`
	Severity    string `json:"severity"`
	State       string `json:"state"`
	Message     string `json:"message"`
	FirstAt     int64  `json:"first_at"`
	LastAt      int64  `json:"last_at"`
	AckedBy     string `json:"acked_by,omitempty"`
	AckNote     string `json:"ack_note,omitempty"`
	AckedAt     *int64 `json:"acked_at,omitempty"`
	ResolvedAt  *int64 `json:"resolved_at,omitempty"`
}

func (a *alerter) fire(ctx context.Context, mig, rule, scope, severity, msg string) {
	var id, state string
	err := a.o.st.DB.QueryRowContext(ctx, `SELECT id, state FROM alerts WHERE COALESCE(migration_id,'')=? AND rule=? AND scope=? AND state<>'resolved'`, mig, rule, scope).Scan(&id, &state)
	now := store.Now()
	if err == nil {
		a.o.st.DB.ExecContext(ctx, `UPDATE alerts SET last_at=?, message=?, severity=? WHERE id=?`, now, msg, severity, id)
		return
	}
	id = store.NewID()
	a.o.st.DB.ExecContext(ctx, `INSERT INTO alerts(id,migration_id,rule,scope,severity,state,message,first_at,last_at) VALUES (?,?,?,?,?,?,?,?,?)`, id, store.NullString(mig), rule, scope, severity, "firing", msg, now, now)
	a.o.Event(ctx, mig, "", "alert", severity, "Alert: "+msg, map[string]any{"rule": rule, "scope": scope, "alert_id": id})
	a.o.bus.Publish("alerts", "alert", mig, map[string]any{"id": id, "state": "firing"})
}

func (a *alerter) resolve(ctx context.Context, mig, rule, scope string) {
	var id string
	if a.o.st.DB.QueryRowContext(ctx, `SELECT id FROM alerts WHERE COALESCE(migration_id,'')=? AND rule=? AND scope=? AND state<>'resolved'`, mig, rule, scope).Scan(&id) != nil {
		return
	}
	a.o.st.DB.ExecContext(ctx, `UPDATE alerts SET state='resolved', resolved_at=? WHERE id=?`, store.Now(), id)
	a.o.Event(ctx, mig, "", "alert_resolved", "info", "Resolved: "+rule+" ("+scope+")", map[string]any{"rule": rule, "scope": scope, "alert_id": id})
	a.o.bus.Publish("alerts", "alert", mig, map[string]any{"id": id, "state": "resolved"})
}

func (a *alerter) cond(ctx context.Context, on bool, mig, rule, scope, sev, msg string) {
	if on {
		a.fire(ctx, mig, rule, scope, sev, msg)
	} else {
		a.resolve(ctx, mig, rule, scope)
	}
}

func (a *alerter) evaluate(ctx context.Context, m Migration, dbs []Database) {
	g := a.o.globalSettings(ctx)
	for _, d := range dbs {
		a.cond(ctx, d.State == DFailed, m.ID, "engine_failed", d.SourceName, "critical", d.SourceName+": the engine failed: "+d.LastError)
		a.cond(ctx, d.State == DDegraded, m.ID, "engine_degraded", d.SourceName, "critical", d.SourceName+": the engine's main process died while sub-processes ran")
		ws, ok := a.o.metrics.Last(m.ID, d.SourceName, "slot_wal_status")
		a.cond(ctx, d.ErrorClass == "slot_lost" || (ok && ws == 3), m.ID, "slot_invalidated", d.SourceName, "critical", d.SourceName+": the replication slot was invalidated")
		a.cond(ctx, ok && ws == 2 && activeDB[d.State], m.ID, "slot_at_risk", d.SourceName, "critical", d.SourceName+": the slot's WAL is no longer reserved and may be removed")
		if d.BacklogBytes != nil && activeDB[d.State] && d.BaseCopyDone {
			a.mu.Lock()
			h := append(a.backlog[d.ID], *d.BacklogBytes)
			if len(h) > 6 {
				h = h[len(h)-6:]
			}
			a.backlog[d.ID] = h
			a.mu.Unlock()
			growing := len(h) == 6
			for i := 1; i < len(h) && growing; i++ {
				if h[i] <= h[i-1] {
					growing = false
				}
			}
			a.cond(ctx, growing, m.ID, "backlog_growing", d.SourceName, "warning", d.SourceName+": the change backlog grew for 5 consecutive samples")
		}
		if lat, ok := a.o.metrics.Last(m.ID, d.SourceName, "heartbeat_latency_s"); ok {
			lim := float64(g.Int("alert_heartbeat_seconds"))
			a.cond(ctx, d.State == DInSync && lim > 0 && lat > lim, m.ID, "heartbeat_latency", d.SourceName, "warning", fmt.Sprintf("%s: heartbeat latency is %.0f s", d.SourceName, lat))
		}
	}
}

func (a *alerter) evaluateHost(ctx context.Context) {
	g := a.o.globalSettings(ctx)
	pct, ok := a.o.metrics.Last("_host", "", "work_volume_pct")
	if !ok {
		return
	}
	crit, warn := float64(g.Int("alert_disk_crit_pct")), float64(g.Int("alert_disk_warn_pct"))
	a.cond(ctx, pct >= crit, "", "work_volume_critical", "host", "critical", fmt.Sprintf("The work volume is %.0f%% full", pct))
	a.cond(ctx, pct >= warn && pct < crit, "", "work_volume_warning", "host", "warning", fmt.Sprintf("The work volume is %.0f%% full", pct))
	// Disk guard: stop the fastest-growing database (largest work directory).
	if guard := float64(g.Int("disk_guard_stop_pct")); guard > 0 && pct >= guard {
		a.o.diskGuard(ctx, pct)
	}
}

// diskGuard stops the database with the largest staging directory.
func (o *Orchestrator) diskGuard(ctx context.Context, pct float64) {
	ms, _ := o.ListMigrations(ctx)
	var bestM Migration
	var bestD Database
	var best int64 = -1
	for _, m := range ms {
		if m.Fixture || !m.Flags.Started || m.Flags.CleanedUp {
			continue
		}
		dbs, _ := o.ListDatabases(ctx, m.ID)
		for _, d := range included(dbs) {
			if !activeDB[d.State] {
				continue
			}
			var size int64
			filepath.Walk(filepath.Join(o.cfg.RunsDir(), d.Instance), func(_ string, fi os.FileInfo, err error) error {
				if err == nil && !fi.IsDir() {
					size += fi.Size()
				}
				return nil
			})
			if size > best {
				best, bestM, bestD = size, m, d
			}
		}
	}
	if best < 0 {
		return
	}
	o.mustStop(ctx, bestM, bestD, "disk guard")
	o.endAttempt(ctx, bestD, nil, "disk guard")
	o.setDB(ctx, bestD.ID, map[string]any{"state": DStopped, "next_retry_at": nil, "last_error": fmt.Sprintf("Stopped by the disk guard: the work volume reached %.0f%%", pct)})
	o.Event(ctx, bestM.ID, bestD.SourceName, "disk_guard", "critical", fmt.Sprintf("Disk guard stopped %s (largest staging, %s) at %.0f%% work-volume use", bestD.SourceName, humanSize(best), pct), nil)
}

func humanSize(b int64) string {
	f := float64(b)
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// ListAlerts returns alerts (state filter optional).
func (o *Orchestrator) ListAlerts(ctx context.Context, state string, limit int) ([]Alert, error) {
	q := `SELECT id, COALESCE(migration_id,''), rule, scope, severity, state, message, first_at, last_at, COALESCE(acked_by,''), COALESCE(ack_note,''), acked_at, resolved_at FROM alerts`
	var args []any
	if state != "" {
		q += ` WHERE state=?`
		args = append(args, state)
	}
	q += ` ORDER BY CASE state WHEN 'firing' THEN 0 WHEN 'acknowledged' THEN 1 ELSE 2 END, last_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := o.st.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		var a Alert
		var acked, resolved sql.NullInt64
		if err := rows.Scan(&a.ID, &a.MigrationID, &a.Rule, &a.Scope, &a.Severity, &a.State, &a.Message, &a.FirstAt, &a.LastAt, &a.AckedBy, &a.AckNote, &acked, &resolved); err != nil {
			return nil, err
		}
		if acked.Valid {
			a.AckedAt = &acked.Int64
		}
		if resolved.Valid {
			a.ResolvedAt = &resolved.Int64
		}
		out = append(out, a)
	}
	return out, nil
}

// AckAlert acknowledges a firing alert.
func (o *Orchestrator) AckAlert(ctx context.Context, id, by, note string) error {
	res, err := o.st.DB.ExecContext(ctx, `UPDATE alerts SET state='acknowledged', acked_by=?, ack_note=?, acked_at=? WHERE id=? AND state='firing'`, by, note, store.Now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return uerr("not_firing", "That alert is not firing.", "")
	}
	o.bus.Publish("alerts", "alert", "", map[string]any{"id": id, "state": "acknowledged"})
	return nil
}

func humanDuration(s float64) string {
	d := time.Duration(s * float64(time.Second)).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	return d.String()
}
