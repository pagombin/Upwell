package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pagombin/upwell/internal/check"
)

// ChecklistItem is one thing left to do after cutover (E5).
type ChecklistItem struct {
	Title     string   `json:"title"`
	Detail    string   `json:"detail"`
	Databases []string `json:"databases,omitempty"`
	SQL       string   `json:"sql,omitempty"`
}

// Checklist gathers what the cutover leaves to the operator, from the
// latest preflight's evidence and the role copy, so the warnings scattered
// across preflight end up in one place in the report.
func Checklist(r *Report) []ChecklistItem {
	var out []ChecklistItem
	by := map[string][]check.Result{}
	for _, p := range r.Preflight {
		by[p.CheckID] = append(by[p.CheckID], p)
	}
	dbsOf := func(rs []check.Result) []string {
		var s []string
		for _, x := range rs {
			if x.Database != "" {
				s = append(s, x.Database)
			}
		}
		sort.Strings(s)
		return s
	}
	warned := func(id string) []check.Result {
		var s []check.Result
		for _, x := range by[id] {
			if x.Level == check.Warning || x.Level == check.Blocker {
				s = append(s, x)
			}
		}
		return s
	}
	names := func(x check.Result) []string {
		var s []string
		if l, ok := x.Evidence["names"].([]any); ok {
			for _, v := range l {
				s = append(s, fmt.Sprint(v))
			}
		}
		if l, ok := x.Evidence["names"].([]string); ok {
			s = append(s, l...)
		}
		return s
	}

	// The apply privileges: one revoke, run in every database the grants were
	// run in (the parameter grant is cluster-wide).
	if ap := by["target_apply_privileges"]; len(ap) > 0 {
		script := ""
		super := true
		for _, x := range ap {
			if s, _ := x.Evidence["superuser"].(bool); !s {
				super = false
			}
			if s, _ := x.Evidence["revoke_script"].(string); s != "" && script == "" {
				script = s
			}
		}
		if !super {
			out = append(out, ChecklistItem{Title: "Revoke the migration's privileges on the target",
				Detail:    "SET on session_replication_role lets the target admin disable triggers and foreign-key enforcement in its sessions, and the origin functions let it create, advance and drop any replication origin. The platform runs the revoke script as a superuser in each target database and in template1.",
				Databases: dbsOf(ap), SQL: script})
		}
	}
	for _, e := range r.Events {
		if e["type"] != "roles" {
			continue
		}
		msg := fmt.Sprint(e["message"])
		if e["severity"] == "warning" {
			out = append(out, ChecklistItem{Title: "Create the application roles on the target", Detail: msg})
		} else if strings.Contains(msg, "Set passwords for") {
			out = append(out, ChecklistItem{Title: "Set passwords for the copied login roles", Detail: msg})
		}
	}
	if rs := warned("materialized_views"); len(rs) > 0 {
		var sql strings.Builder
		for _, x := range rs {
			fmt.Fprintf(&sql, "-- in %s\n", x.Database)
			for _, n := range names(x) {
				fmt.Fprintf(&sql, "REFRESH MATERIALIZED VIEW %s;\n", n)
			}
		}
		out = append(out, ChecklistItem{Title: "Refresh the materialized views on the target", Detail: "They were created on the target but hold no data until refreshed.", Databases: dbsOf(rs), SQL: sql.String()})
	}
	simple := []struct{ id, title, detail string }{
		{"event_triggers", "Re-create the event triggers on the target", "The engine does not copy event triggers. Re-create them (a superuser is needed) or confirm they are not wanted"},
		{"foreign_servers", "Re-create the foreign servers' user mappings on the target", "User mappings and their credentials are not copied"},
		{"pg_cron", "Review the pg_cron jobs", "Re-create or disable the scheduled jobs on the target, and stop them on the source"},
		{"pg_partman", "Run partition maintenance on one side only", "Pause pg_partman maintenance on the source now that the target is in use"},
		{"unlogged_tables", "Check the unlogged tables", "They were copied once and not streamed: changes made to them after the base copy are not on the target"},
		{"large_objects", "Check the large objects", "They were copied once during the base copy; verification compared them"},
	}
	for _, s := range simple {
		rs := warned(s.id)
		if len(rs) == 0 {
			continue
		}
		d := s.detail + "."
		var all []string
		for _, x := range rs {
			for _, n := range names(x) {
				all = append(all, x.Database+": "+n)
			}
		}
		if len(all) > 0 {
			d = s.detail + ": " + strings.Join(all, ", ") + "."
		}
		out = append(out, ChecklistItem{Title: s.title, Detail: d, Databases: dbsOf(rs)})
	}
	if !r.Migration.Flags.CleanedUp {
		out = append(out, ChecklistItem{Title: "Clean up the migration", Detail: "Clean up drops the replication slots (which keep WAL on the source), publications, origins and heartbeat tables, and deletes the stored credentials."})
	}
	return out
}
