// Phase 0 spike S06: measured SQLite storage for the proposed metrics budget
// (2 GB for a 7-day, 10-database migration), with the retention the spec proposes:
// raw 5 s samples for 72 h, 1-minute rollups for 90 days, 15-minute rollups forever.
//
// It writes the state of the store at the END of day 7 (the largest point inside the
// budget window) for two schema variants and reports file sizes and query latency:
//
//	spec:    metrics(id ULID text PK, migration_id, database, name, ts, value, created_at)
//	         exactly the narrow table the Data model section lists.
//	compact: series(id, migration_id, database, name) + samples(series_id, ts, value)
//	         WITHOUT ROWID, primary key (series_id, ts).
//
// Usage: go run . -dir /path/to/scratch [-days 7] [-dbs 10] [-interval 5]
package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite"
)

type series struct {
	db, name string
	every    int // seconds
	active   func(t int) bool
}

func catalog(dbs, interval int, copyEnd int) []series {
	always := func(int) bool { return true }
	duringCopy := func(t int) bool { return t < copyEnd }
	var s []series
	perDB5 := []string{"phase", "attempt", "copy_rate_instant", "copy_rate_smoothed", "copy_rate_avg",
		"received_lsn", "replayed_lsn", "end_lsn", "cdc_backlog_bytes", "replay_rate",
		"heartbeat_latency_s", "slot_active", "slot_wal_status", "slot_retained_bytes",
		"slot_headroom_bytes", "log_errors", "log_warnings", "vacuum_in_progress"}
	for d := 0; d < dbs; d++ {
		db := fmt.Sprintf("db%02d", d)
		for _, n := range perDB5 {
			s = append(s, series{db, n, interval, always})
		}
		for _, n := range []string{"source_size_bytes", "target_size_bytes", "target_rows_inserted"} {
			s = append(s, series{db, n, 30, always})
		}
		for i := 0; i < 10; i++ { // dead tuples, top-10 churn tables
			s = append(s, series{db, fmt.Sprintf("dead_tuples:public.t%02d", i), 60, always})
		}
		for j := 0; j < 4; j++ { // active COPY bytes+tuples, 4 table jobs; index builds 2
			s = append(s, series{db, fmt.Sprintf("copy_bytes:job%d", j), interval, duringCopy})
			s = append(s, series{db, fmt.Sprintf("copy_tuples:job%d", j), interval, duringCopy})
		}
		for j := 0; j < 2; j++ {
			s = append(s, series{db, fmt.Sprintf("index_pct:job%d", j), interval, duringCopy})
		}
	}
	for _, n := range []string{"src_wal_rate", "src_snapshot_age_s", "src_xmin_age", "src_connections", "dst_connections"} {
		s = append(s, series{"", n, interval, always})
	}
	for _, n := range []string{"work_volume_pct", "staging_growth", "cpu_pct", "load1", "mem_pct", "net_rx", "net_tx"} {
		s = append(s, series{"", n, interval, always})
	}
	s = append(s, series{"", "staging_bytes", 60, always})
	return s
}

type result struct {
	Variant          string  `json:"variant"`
	Series           int     `json:"series"`
	RawRows          int64   `json:"raw_rows"`
	Rollup1mRows     int64   `json:"rollup_1m_rows"`
	Rollup15mRows    int64   `json:"rollup_15m_rows"`
	FileBytes        int64   `json:"file_bytes"`
	BytesPerRawRow   float64 `json:"bytes_per_row_overall"`
	LoadSeconds      float64 `json:"load_seconds"`
	Chart6hMs        float64 `json:"query_chart_6h_1series_ms"`
	Overview24hMs    float64 `json:"query_overview_24h_all_db_backlog_ms"`
	ProjectedGB30d   float64 `json:"projected_gb_30_day_run"`
}

func main() {
	dir := flag.String("dir", os.TempDir(), "scratch directory")
	days := flag.Int("days", 7, "migration length in days")
	dbs := flag.Int("dbs", 10, "databases")
	interval := flag.Int("interval", 5, "sample interval seconds")
	flag.Parse()

	dur := *days * 86400
	copyEnd := dur / 3 // base copy for the first third of the run
	cat := catalog(*dbs, *interval, copyEnd)
	rawFrom := dur - 72*3600
	var out []result
	for _, v := range []string{"spec", "compact"} {
		r := run(filepath.Join(*dir, "metrics-"+v+".db"), v, cat, dur, rawFrom)
		out = append(out, r)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"days": *days, "databases": *dbs, "interval_s": *interval,
		"raw_retention_h": 72, "base_copy_fraction": "first third", "results": out})
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func run(path, variant string, cat []series, dur, rawFrom int) result {
	for _, suf := range []string{"", "-wal", "-shm"} {
		os.Remove(path + suf)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	must(err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	start := time.Now()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix()
	ent := ulid.Monotonic(rand.Reader, 0)

	tables := []string{"metrics", "metrics_1m", "metrics_15m"}
	if variant == "spec" {
		for _, t := range tables {
			must(exec(db, `create table `+t+` (id text primary key, migration_id text not null, database text,
				name text not null, ts integer not null, value real, vmin real, vmax real, vlast real, created_at integer not null)`))
			must(exec(db, `create index `+t+`_q on `+t+`(migration_id, name, database, ts)`))
		}
	} else {
		must(exec(db, `create table series (id integer primary key, migration_id text not null, database text, name text not null, unique(migration_id, database, name))`))
		for _, t := range tables {
			must(exec(db, `create table `+t+` (series_id integer not null, ts integer not null, value real,
				vmin real, vmax real, vlast real, primary key(series_id, ts)) without rowid`))
		}
		for i, s := range cat {
			must(exec(db, `insert into series values (?,?,?,?)`, i+1, "m42", nullable(s.db), s.name))
		}
	}

	var raw, r1, r15 int64
	tx, _ := db.Begin()
	n := 0
	ins := map[string]*sql.Stmt{}
	prep := func() {
		for _, t := range tables {
			var q string
			if variant == "spec" {
				q = `insert into ` + t + ` values (?,?,?,?,?,?,?,?,?,?)`
			} else {
				q = `insert into ` + t + ` values (?,?,?,?,?,?)`
			}
			st, err := tx.Prepare(q)
			must(err)
			ins[t] = st
		}
	}
	prep()
	put := func(t string, i int, s series, ts int64, v, mn, mx, last any) {
		if variant == "spec" {
			id := ulid.MustNew(ulid.Timestamp(time.Unix(ts, 0)), ent).String()
			_, err := ins[t].Exec(id, "m42", nullable(s.db), s.name, ts, v, mn, mx, last, ts)
			must(err)
		} else {
			_, err := ins[t].Exec(i+1, ts, v, mn, mx, last)
			must(err)
		}
		n++
		if n%50000 == 0 {
			must(tx.Commit())
			tx, _ = db.Begin()
			prep()
		}
	}
	val := func(i, t int) float64 { return math.Sin(float64(t)/600+float64(i)) * 1e6 }
	for i, s := range cat {
		for t := 0; t < dur; t += 60 { // 1-minute rollups over the whole run
			if s.active(t) {
				put("metrics_1m", i, s, base+int64(t), val(i, t), val(i, t)-1, val(i, t)+1, val(i, t))
				r1++
			}
		}
		for t := 0; t < dur; t += 900 {
			if s.active(t) {
				put("metrics_15m", i, s, base+int64(t), val(i, t), val(i, t)-1, val(i, t)+1, val(i, t))
				r15++
			}
		}
		for t := rawFrom; t < dur; t += s.every { // raw: last 72 h only
			if s.active(t) {
				put("metrics", i, s, base+int64(t), val(i, t), nil, nil, nil)
				raw++
			}
		}
	}
	must(tx.Commit())
	_, _ = db.Exec(`pragma wal_checkpoint(TRUNCATE)`)
	load := time.Since(start).Seconds()

	// Typical UI queries.
	t0 := time.Now()
	var c int
	if variant == "spec" {
		must(db.QueryRow(`select count(*) from metrics where migration_id='m42' and name='cdc_backlog_bytes' and database='db03' and ts >= ?`, base+int64(dur-6*3600)).Scan(&c))
	} else {
		must(db.QueryRow(`select count(*) from metrics m join series s on s.id=m.series_id where s.migration_id='m42' and s.name='cdc_backlog_bytes' and s.database='db03' and m.ts >= ?`, base+int64(dur-6*3600)).Scan(&c))
	}
	chart := float64(time.Since(t0).Microseconds()) / 1000
	t0 = time.Now()
	if variant == "spec" {
		must(db.QueryRow(`select count(*) from metrics_1m where migration_id='m42' and name='cdc_backlog_bytes' and ts >= ?`, base+int64(dur-86400)).Scan(&c))
	} else {
		must(db.QueryRow(`select count(*) from metrics_1m m join series s on s.id=m.series_id where s.migration_id='m42' and s.name='cdc_backlog_bytes' and m.ts >= ?`, base+int64(dur-86400)).Scan(&c))
	}
	overview := float64(time.Since(t0).Microseconds()) / 1000

	fi, err := os.Stat(path)
	must(err)
	total := raw + r1 + r15
	// 30-day projection: raw stays at 72 h; rollups grow linearly.
	perRollupRow := float64(fi.Size()) / float64(total)
	proj := (float64(raw) + float64(r1+r15)*30/float64(dur/86400)) * perRollupRow / 1e9
	return result{variant, len(cat), raw, r1, r15, fi.Size(), float64(fi.Size()) / float64(total), load, chart, overview, proj}
}

func exec(db *sql.DB, q string, args ...any) error { _, err := db.Exec(q, args...); return err }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
