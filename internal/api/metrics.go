package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Prometheus exposition (overnight item 5): upwell_* gauges labelled with
// migration, database and scope, read from the collector's latest samples.
// No credentials, connection strings or hostnames are exported; migrations
// are labelled by their short id and name.

type promMetric struct {
	name, help, kind string
	samples          []promSample
}

type promSample struct {
	labels map[string]string
	value  float64
}

func promEscape(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(v)
}

func (m *promMetric) add(v float64, kv ...string) {
	l := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		l[kv[i]] = kv[i+1]
	}
	m.samples = append(m.samples, promSample{l, v})
}

func (m *promMetric) write(b *strings.Builder) {
	if len(m.samples) == 0 {
		return
	}
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.kind)
	for _, s := range m.samples {
		b.WriteString(m.name)
		if len(s.labels) > 0 {
			keys := make([]string, 0, len(s.labels))
			for k := range s.labels {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteByte('{')
			for i, k := range keys {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(b, `%s="%s"`, k, promEscape(s.labels[k]))
			}
			b.WriteByte('}')
		}
		b.WriteByte(' ')
		b.WriteString(strconv.FormatFloat(s.value, 'g', -1, 64))
		b.WriteByte('\n')
	}
}

func (s *Server) prometheus(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	gauge := func(name, help string) *promMetric { return &promMetric{name: name, help: help, kind: "gauge"} }
	build := gauge("upwell_build_info", "Upwell version.")
	build.add(1, "version", s.Version)
	migState := gauge("upwell_migration_info", "One series per migration, with its state and mode.")
	verdict := gauge("upwell_migration_verdict", "Cutover verdict: 1 GO, 0 NO-GO, -1 none yet.")
	freeze := gauge("upwell_write_freeze_applied", "1 while a write freeze is applied on the migration's source.")
	dbState := gauge("upwell_database_state", "One series per database, with its state.")
	alerts := gauge("upwell_alerts_firing", "Firing alerts by severity.")
	perDB := []struct {
		key string
		m   *promMetric
	}{
		{"backlog_bytes", gauge("upwell_database_backlog_bytes", "Changes received but not yet applied on the target.")},
		{"heartbeat_latency_s", gauge("upwell_heartbeat_latency_seconds", "How far the target is behind, from the heartbeat.")},
		{"replay_rate", gauge("upwell_replay_rate_bytes_per_second", "Rate at which the target applies WAL.")},
		{"apply_idle_s", gauge("upwell_apply_idle_seconds", "Seconds since the target's replication origin last moved (apply watchdog).")},
		{"apply_gap_bytes", gauge("upwell_apply_gap_bytes", "Source slot confirmed position minus the target origin.")},
		{"slot_retained_bytes", gauge("upwell_slot_retained_bytes", "WAL the source keeps for the replication slot.")},
		{"slot_headroom_bytes", gauge("upwell_slot_headroom_bytes", "WAL the slot may still retain before it is at risk.")},
		{"source_size_bytes", gauge("upwell_database_size_bytes", "Database size on the source.")},
		{"target_size_bytes", gauge("upwell_target_database_size_bytes", "Database size on the target.")},
		{"log_errors", gauge("upwell_engine_log_errors", "Engine error lines in the log for the database.")},
	}
	perCluster := []struct {
		key, scope string
		m          *promMetric
	}{
		{"src_connections", "source", gauge("upwell_cluster_connections", "Sessions connected to the cluster.")},
		{"dst_connections", "target", nil},
		{"src_wal_rate", "source", gauge("upwell_cluster_wal_bytes_per_second", "WAL the source writes per second.")},
		{"src_oldest_xact_s", "source", gauge("upwell_cluster_oldest_transaction_seconds", "Age of the oldest open transaction on the source.")},
	}
	perCluster[1].m = perCluster[0].m
	host := []struct {
		key string
		m   *promMetric
	}{
		{"work_volume_pct", gauge("upwell_host_work_volume_used_percent", "Used space on the volume holding the work directories.")},
		{"cpu_pct", gauge("upwell_host_cpu_percent", "Droplet CPU use.")},
		{"mem_pct", gauge("upwell_host_memory_percent", "Droplet memory use.")},
		{"load1", gauge("upwell_host_load1", "One-minute load average.")},
		{"staging_bytes", gauge("upwell_host_staging_bytes", "Change staging on disk.")},
	}
	ms, err := s.Orch.ListMigrations(ctx)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.Fixture {
			continue
		}
		mig := m.ShortID
		mode := "online"
		if s.Orch.IsOffline(ctx, m) {
			mode = "offline"
		}
		migState.add(1, "migration", mig, "name", m.Name, "state", m.State, "mode", mode)
		v := -1.0
		switch m.Flags.Verdict {
		case "GO":
			v = 1
		case "NO-GO":
			v = 0
		}
		verdict.add(v, "migration", mig)
		f := 0.0
		if m.Flags.Freeze.Active() {
			f = 1
		}
		freeze.add(f, "migration", mig)
		for _, c := range perCluster {
			if x, ok := s.Orch.LastValue(m.ID, "", c.key); ok {
				c.m.add(x, "migration", mig, "scope", c.scope)
			}
		}
		dbs, _ := s.Orch.ListDatabases(ctx, m.ID)
		for _, d := range dbs {
			if !d.Include {
				continue
			}
			dbState.add(1, "migration", mig, "database", d.SourceName, "state", d.State)
			for _, p := range perDB {
				if x, ok := s.Orch.LastValue(m.ID, d.SourceName, p.key); ok {
					p.m.add(x, "migration", mig, "database", d.SourceName)
				}
			}
		}
	}
	for _, h := range host {
		if x, ok := s.Orch.LastValue("_host", "", h.key); ok {
			h.m.add(x, "scope", "host")
		}
	}
	as, err := s.Orch.ListAlerts(ctx, "firing", 1000)
	if err != nil {
		return err
	}
	bySev := map[string]float64{"critical": 0, "warning": 0}
	for _, a := range as {
		bySev[a.Severity]++
	}
	for _, sev := range []string{"critical", "warning"} {
		alerts.add(bySev[sev], "severity", sev)
	}
	var b strings.Builder
	all := []*promMetric{build, migState, verdict, freeze, dbState, alerts}
	for _, p := range perDB {
		all = append(all, p.m)
	}
	seen := map[*promMetric]bool{}
	for _, c := range perCluster {
		if !seen[c.m] {
			all = append(all, c.m)
			seen[c.m] = true
		}
	}
	for _, h := range host {
		all = append(all, h.m)
	}
	for _, m := range all {
		m.write(&b)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(200)
	_, err = w.Write([]byte(b.String()))
	return err
}
