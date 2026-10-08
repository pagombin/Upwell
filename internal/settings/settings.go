// Package settings is the settings catalog: every default, its range, scope
// and explanation, with overrides stored in the settings table.
package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"

	"github.com/pagombin/upwell/internal/store"
)

// Def describes one setting.
type Def struct {
	Key         string   `json:"key"`
	Type        string   `json:"type"` // int, float, bool, enum, size, string
	Default     any      `json:"default"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Scope       string   `json:"scope"` // global or migration
	Group       string   `json:"group"`
	Description string   `json:"description"`
	Unit        string   `json:"unit,omitempty"`
}

func rng(a, b float64) (*float64, *float64) { return &a, &b }

func def(key, typ string, dflt any, min, max float64, scope, group, unit, desc string) Def {
	d := Def{Key: key, Type: typ, Default: dflt, Scope: scope, Group: group, Unit: unit, Description: desc}
	if typ == "int" || typ == "float" {
		d.Min, d.Max = rng(min, max)
	}
	return d
}

func enum(key string, dflt string, vals []string, scope, group, desc string) Def {
	return Def{Key: key, Type: "enum", Default: dflt, Enum: vals, Scope: scope, Group: group, Description: desc}
}

func boolean(key string, dflt bool, scope, group, desc string) Def {
	return Def{Key: key, Type: "bool", Default: dflt, Scope: scope, Group: group, Description: desc}
}

// Catalog is every setting, in display order.
var Catalog = []Def{
	enum("mode", "online", []string{"online", "offline"}, "migration", "Migration defaults", "Online copies and then streams changes until cutover; offline is a one-time copy with writes stopped (coming soon)."),
	def("table_jobs", "int", 0, 0, 64, "migration", "Migration defaults", "workers", "Parallel COPY workers per database. 0 means automatic: CPU count (4 to 16) divided by the number of databases, at least 2."),
	def("index_jobs", "int", 0, 0, 32, "migration", "Migration defaults", "workers", "Parallel CREATE INDEX workers per database. 0 means automatic: half the CPU count (2 to 8) divided by databases, at least 1."),
	{Key: "split_tables_larger_than", Type: "string", Default: "auto", Scope: "migration", Group: "Migration defaults", Description: "Same-table parallel copy: auto (largest table divided by table jobs when that is 10 GB or more), off, or a size such as 50GB."},
	enum("decoding_plugin", "auto", []string{"auto", "pgoutput", "test_decoding", "wal2json"}, "migration", "Migration defaults", "Logical decoding plugin. Auto uses pgoutput and switches a database to test_decoding when pgoutput cannot work there."),
	boolean("target_synchronous_commit_off", true, "migration", "Migration defaults", "Faster loading: engine sessions on the target run with synchronous_commit off."),
	{Key: "target_maintenance_work_mem", Type: "string", Default: "", Scope: "migration", Group: "Migration defaults", Description: "Index build memory for engine sessions, such as 1GB. Empty leaves the server default."},
	boolean("create_target_databases", true, "migration", "Migration defaults", "Create missing target databases with the source's encoding and locale."),
	boolean("copy_roles", true, "migration", "Migration defaults", "Copy cluster roles once before the first database starts."),
	boolean("heartbeat", true, "migration", "Data safety", "Heartbeat table for end-to-end latency and the final cutover heartbeat. Turning it off removes a GO requirement and is recorded as an accepted risk."),
	def("max_concurrent_base_copies", "int", 4, 1, 32, "migration", "Migration defaults", "databases", "Databases copying their base data at once. The rest wait and start as earlier ones reach catch-up."),
	def("exact_count_max_mb", "int", 1024, 0, 1048576, "migration", "Data safety", "MB", "Tables up to this size get exact row counts in verification; larger ones are compared by planner estimates."),
	def("verify_checksum_budget_seconds", "int", 600, 0, 86400, "migration", "Data safety", "s", "Time verification may spend on row checksums per database, smallest tables first. Tables beyond the budget are reported as not checksummed."),
	def("cutover_lag_threshold_mb", "int", 16, 1, 1024, "migration", "Cutover", "MB", "Backlog per database below which writes may stop."),
	def("cutover_stable_seconds", "int", 60, 0, 600, "global", "Cutover", "s", "How long the backlog must stay under the threshold before the readiness gate passes."),
	def("assumed_apply_tps", "float", 5, 0.1, 100000, "global", "Preflight", "transactions/s", "Transactions a second the engine applies per database, for the apply_rate check. 5 was measured with pgcopydb 0.18 in the build container (F11); calibrate it with a test migration on the droplet."),
	def("cutover_max_lag_seconds", "int", 180, 10, 86400, "global", "Cutover", "s", "Largest heartbeat lag (how far the target is behind, in time) at which the readiness gate passes. The write pause lasts at least this long. A backlog in bytes can hide a long apply time (F11)."),
	def("cutover_write_sample_seconds", "int", 10, 5, 120, "global", "Cutover", "s", "Window between the two row-change samples taken after the operator confirms writers stopped."),
	def("cutover_drain_timeout_seconds", "int", 1800, 60, 86400, "global", "Cutover", "s", "Maximum wait for every database to reach its end position."),
	def("cutover_nudge_seconds", "int", 0, 0, 600, "global", "Cutover", "s", "Delay before the drain nudge. 0 nudges immediately after setting the end position."),
	boolean("analyze_after_go", false, "migration", "Cutover", "Run ANALYZE on the target databases after a GO verdict."),
	def("wal_sample_seconds", "int", 60, 10, 600, "global", "Preflight", "s", "Source WAL rate sampling window in preflight."),
	def("assumed_throughput_mibs", "float", 102, 1, 2000, "global", "Preflight", "MiB/s", "Copy rate used for estimates before a throughput sample exists."),
	def("long_transaction_minutes", "int", 5, 1, 120, "global", "Preflight", "min", "Transactions open longer than this raise a preflight warning."),
	def("target_disk_factor", "float", 1.3, 1, 3, "global", "Preflight", "×", "Target capacity check multiplier."),
	def("staging_factor", "float", 1.5, 1, 5, "global", "Preflight", "×", "Staging estimate as a multiple of retained WAL."),
	def("retry_base_seconds", "int", 10, 1, 3600, "global", "Recovery", "s", "First backoff for automatic resume."),
	def("retry_cap_seconds", "int", 600, 1, 3600, "global", "Recovery", "s", "Longest backoff for automatic resume."),
	def("retry_max_per_hour", "int", 5, 0, 60, "global", "Recovery", "per hour", "Automatic resumes per database per hour before it needs attention."),
	boolean("auto_restart_base_copy", false, "global", "Recovery", "Restart a database whose base copy failed, when it is under the size limit below."),
	def("auto_restart_base_copy_max_gb", "int", 50, 1, 100000, "global", "Recovery", "GB", "Size limit for automatic base copy restarts."),
	boolean("auto_resume_after_reboot", true, "global", "Recovery", "Resume streaming databases when the app starts after a reboot."),
	def("sample_interval_seconds", "int", 5, 2, 60, "global", "Monitoring", "s", "Collector cadence."),
	def("metrics_raw_retention_hours", "int", 72, 6, 720, "global", "Monitoring", "h", "Raw samples kept; older data survives as 1-minute rollups."),
	def("alert_disk_warn_pct", "int", 70, 50, 99, "global", "Alerts", "%", "Work volume warning."),
	def("alert_disk_crit_pct", "int", 85, 50, 99, "global", "Alerts", "%", "Work volume critical alert."),
	def("disk_guard_stop_pct", "int", 95, 80, 99, "global", "Alerts", "%", "Stop the fastest-growing database to protect the volume."),
	def("alert_slot_headroom_hours_warn", "float", 4, 0.1, 48, "global", "Alerts", "h", "Warn when a slot's WAL headroom runs out within this time."),
	def("alert_slot_headroom_hours_crit", "float", 1, 0.1, 48, "global", "Alerts", "h", "Critical when a slot's WAL headroom runs out within this time."),
	def("alert_heartbeat_seconds", "int", 300, 5, 3600, "global", "Alerts", "s", "Heartbeat latency warning while in sync. pgcopydb applies an idle database in batches, so 30 to 90 s is normal there."),
	enum("log_level", "info", []string{"trace", "debug", "info", "warn", "error"}, "global", "Logging", "App log verbosity. Engine logs are always kept in full."),
	def("log_retention_days", "int", 30, 1, 365, "global", "Logging", "days", "Log retention."),
	def("session_idle_minutes", "int", 30, 5, 240, "global", "Access", "min", "Sign-in sessions end after this much inactivity."),
	def("session_max_hours", "int", 12, 1, 72, "global", "Access", "h", "Sign-in sessions end after this long regardless of activity."),
}

// Lookup returns the definition for key.
func Lookup(key string) (Def, bool) {
	for _, d := range Catalog {
		if d.Key == key {
			return d, true
		}
	}
	return Def{}, false
}

// Validate coerces and checks a value for key.
func Validate(key string, v any) (any, error) {
	d, ok := Lookup(key)
	if !ok {
		return nil, fmt.Errorf("unknown setting %s", key)
	}
	switch d.Type {
	case "int", "float":
		f, ok := v.(float64)
		if !ok {
			if i, ok2 := v.(int); ok2 {
				f, ok = float64(i), true
			}
		}
		if !ok {
			return nil, fmt.Errorf("%s needs a number", key)
		}
		if f < *d.Min || f > *d.Max {
			return nil, fmt.Errorf("%s must be between %g and %g", key, *d.Min, *d.Max)
		}
		if d.Type == "int" {
			if f != float64(int64(f)) {
				return nil, fmt.Errorf("%s needs a whole number", key)
			}
			return int64(f), nil
		}
		return f, nil
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s needs true or false", key)
		}
		return b, nil
	case "enum":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s needs one of %v", key, d.Enum)
		}
		for _, e := range d.Enum {
			if e == s {
				return s, nil
			}
		}
		return nil, fmt.Errorf("%s needs one of %v", key, d.Enum)
	default:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s needs text", key)
		}
		if len(s) > 200 {
			return nil, fmt.Errorf("%s is too long", key)
		}
		return s, nil
	}
}

// Values is a resolved settings map.
type Values map[string]any

// Int returns an integer setting.
func (v Values) Int(k string) int {
	switch x := v[k].(type) {
	case int64:
		return int(x)
	case int:
		return x
	case float64:
		return int(x)
	}
	if d, ok := Lookup(k); ok {
		if i, ok := d.Default.(int); ok {
			return i
		}
	}
	return 0
}

// Float returns a float setting.
func (v Values) Float(k string) float64 {
	switch x := v[k].(type) {
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

// Bool returns a boolean setting.
func (v Values) Bool(k string) bool { b, _ := v[k].(bool); return b }

// Str returns a string setting.
func (v Values) Str(k string) string { s, _ := v[k].(string); return s }

// Service reads and writes overrides.
type Service struct{ Store *store.Store }

// Item is a setting with its effective value and last change.
type Item struct {
	Def
	Value     any    `json:"value"`
	UpdatedBy string `json:"updated_by,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

// Global returns the effective global values (defaults plus overrides).
func (s *Service) Global(ctx context.Context) (Values, error) {
	items, err := s.Items(ctx)
	if err != nil {
		return nil, err
	}
	v := Values{}
	for _, it := range items {
		v[it.Key] = it.Value
	}
	return v, nil
}

// Items lists every setting.
func (s *Service) Items(ctx context.Context) ([]Item, error) {
	rows, err := s.Store.DB.QueryContext(ctx, `SELECT key, value, COALESCE(updated_by,''), updated_at FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	over := map[string]Item{}
	for rows.Next() {
		var k, raw, by string
		var at int64
		if err := rows.Scan(&k, &raw, &by, &at); err != nil {
			return nil, err
		}
		var val any
		json.Unmarshal([]byte(raw), &val)
		over[k] = Item{Value: val, UpdatedBy: by, UpdatedAt: at}
	}
	out := make([]Item, 0, len(Catalog))
	for _, d := range Catalog {
		it := Item{Def: d, Value: d.Default}
		if o, ok := over[d.Key]; ok {
			if cv, err := Validate(d.Key, o.Value); err == nil {
				it.Value = cv
			}
			it.UpdatedBy, it.UpdatedAt = o.UpdatedBy, o.UpdatedAt
		}
		out = append(out, it)
	}
	return out, nil
}

// Set stores an override, returning the previous effective value.
func (s *Service) Set(ctx context.Context, key string, value any, by string) (any, any, error) {
	cv, err := Validate(key, value)
	if err != nil {
		return nil, nil, err
	}
	var raw string
	var prev any
	err = s.Store.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&raw)
	if err == nil {
		json.Unmarshal([]byte(raw), &prev)
	} else if err == sql.ErrNoRows {
		d, _ := Lookup(key)
		prev = d.Default
	} else {
		return nil, nil, err
	}
	b, _ := json.Marshal(cv)
	_, err = s.Store.DB.ExecContext(ctx, `INSERT INTO settings(key,value,updated_by,updated_at) VALUES (?,?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		key, string(b), by, store.Now())
	return prev, cv, err
}

// ForMigration merges global values with per-migration overrides.
func ForMigration(global Values, overrides map[string]any) (Values, error) {
	out := Values{}
	for k, v := range global {
		out[k] = v
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d, ok := Lookup(k)
		if !ok || d.Scope != "migration" {
			return nil, fmt.Errorf("%s cannot be set per migration", k)
		}
		cv, err := Validate(k, overrides[k])
		if err != nil {
			return nil, err
		}
		out[k] = cv
	}
	return out, nil
}

// Jobs resolves table_jobs and index_jobs for n databases running at once.
func Jobs(v Values, concurrent int) (table, index int) {
	if concurrent < 1 {
		concurrent = 1
	}
	cpu := runtime.NumCPU()
	table = v.Int("table_jobs")
	if table == 0 {
		c := min(max(cpu, 4), 16)
		table = max(c/concurrent, 2)
	}
	index = v.Int("index_jobs")
	if index == 0 {
		c := min(max(cpu/2, 2), 8)
		index = max(c/concurrent, 1)
	}
	return
}
