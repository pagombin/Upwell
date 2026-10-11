package report

import (
	"strings"
	"testing"

	"github.com/pagombin/upwell/internal/check"
	"github.com/pagombin/upwell/internal/orch"
)

// TestPostCutoverChecklist (E5): the report gathers what is left to do after
// cutover from preflight's evidence and the role copy.
func TestPostCutoverChecklist(t *testing.T) {
	r := &Report{
		Migration: orch.Migration{Name: "Shop", Flags: orch.Flags{Verdict: "GO"}},
		Preflight: []check.Result{
			{CheckID: "target_apply_privileges", Database: "shop", Level: check.OK, Evidence: map[string]any{"superuser": false,
				"revoke_script": "REVOKE EXECUTE ON FUNCTION pg_catalog.pg_replication_origin_create(text) FROM \"doadmin\";\nREVOKE SET ON PARAMETER session_replication_role FROM \"doadmin\";\n"}},
			{CheckID: "target_apply_privileges", Database: "crm", Level: check.OK, Evidence: map[string]any{"superuser": false, "revoke_script": "REVOKE SET ON PARAMETER session_replication_role FROM \"doadmin\";\n"}},
			{CheckID: "materialized_views", Database: "shop", Level: check.Warning, Evidence: map[string]any{"count": 2, "names": []any{"public.daily_sales", `public."Top Items"`}}},
			{CheckID: "event_triggers", Database: "shop", Level: check.Warning, Evidence: map[string]any{"count": 1, "names": []any{"audit_ddl"}}},
			{CheckID: "foreign_servers", Database: "crm", Level: check.Warning, Evidence: map[string]any{"count": 1, "names": []any{"legacy_erp"}}},
			{CheckID: "pg_cron", Database: "shop", Level: check.Warning, Evidence: map[string]any{"count": 1}},
			{CheckID: "large_objects", Database: "shop", Level: check.OK, Evidence: map[string]any{"count": 0}},
		},
		Events: []map[string]any{{"ts": int64(1), "type": "roles", "severity": "info", "message": "Copied roles to the target without passwords: 2 created, 0 already existed. Set passwords for app_rw, report_ro on the target before switching applications."}},
	}
	items := Checklist(r)
	r.Checklist = items
	var titles []string
	for _, it := range items {
		titles = append(titles, it.Title)
	}
	t.Logf("checklist: %s", strings.Join(titles, " | "))
	html, err := r.HTML()
	if err != nil {
		t.Fatal(err)
	}
	md := string(r.Markdown())
	for _, want := range []string{
		"After cutover",
		"REVOKE SET ON PARAMETER session_replication_role FROM &#34;doadmin&#34;", // HTML-escaped
		`REFRESH MATERIALIZED VIEW public.&#34;Top Items&#34;;`,
		"audit_ddl", "legacy_erp", "app_rw, report_ro", "pg_cron", "Clean up",
	} {
		if !strings.Contains(string(html), want) {
			t.Errorf("HTML report lacks %q", want)
		}
	}
	for _, want := range []string{"## After cutover", `REVOKE SET ON PARAMETER session_replication_role FROM "doadmin";`, `REFRESH MATERIALIZED VIEW public."Top Items";`, "audit_ddl", "legacy_erp", "app_rw, report_ro"} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown report lacks %q", want)
		}
	}
	// One revoke item, not one per database: the parameter grant is cluster-wide.
	if n := strings.Count(md, "REVOKE SET ON PARAMETER"); n != 1 {
		t.Errorf("the revoke script appears %d times", n)
	}
	if strings.Contains(md, "Large objects") {
		t.Error("a check that found nothing is on the checklist")
	}
}
