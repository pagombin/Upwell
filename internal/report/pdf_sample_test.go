package report

import (
	"os"
	"testing"

	"github.com/pagombin/upwell/internal/check"
	"github.com/pagombin/upwell/internal/orch"
)

func TestWriteSamplePDF(t *testing.T) {
	out := os.Getenv("UPWELL_PDF_SAMPLE")
	if out == "" {
		t.Skip()
	}
	r := &Report{Migration: orch.Migration{Name: "Example Customer: shop to Advanced", ShortID: "k3m9q", State: "completed", Flags: orch.Flags{Verdict: "GO", Cutover: &orch.CutoverState{WritePauseMS: 64000}}},
		Permission: &orch.Permission{Customer: "Example Customer", AccountID: "acct-0001", Ticket: "SUP-1234", GrantedBy: "Jamie Customer", GrantedAt: "2026-10-07", Scope: "Copy shop to the Advanced cluster and cut over."},
		Source:     &orch.Connection{Host: "db-standard-nyc3-01.example", Port: 25060, User: "doadmin", SSLMode: "require"},
		Target:     &orch.Connection{Host: "db-advanced-nyc3-02.example", Port: 25060, User: "doadmin", SSLMode: "require"},
		Databases:  []orch.Database{{SourceName: "shop", TargetName: "shop", SizeBytes: 1 << 30, State: "verified", Plugin: "pgoutput", Verdict: "GO", Include: true}},
		Preflight: []check.Result{{CheckID: "materialized_views", Database: "shop", Level: check.Warning, Title: "Materialized views", Message: "2 materialized view(s) are created but must be refreshed on the target after cutover.", Evidence: map[string]any{"names": []any{"public.daily_sales", "public.top_items"}}},
			{CheckID: "target_apply_privileges", Database: "shop", Level: check.OK, Evidence: map[string]any{"superuser": false, "revoke_script": "REVOKE EXECUTE ON FUNCTION\n  pg_catalog.pg_replication_origin_create(text)\nFROM \"doadmin\";\nREVOKE SET ON PARAMETER session_replication_role FROM \"doadmin\";\n"}}},
		Verification:  []orch.VerificationRow{{Database: "shop", Title: "Row counts", Result: "ok", Detail: []byte(`{"message":"Exact counts match for 12 table(s)."}`)}, {Database: "shop", Title: "Checksums", Result: "ok", Detail: []byte(`{"message":"Checksums match for all 12 table(s)."}`)}},
		Events:        []map[string]any{{"ts": int64(1791600000000), "database": "shop", "message": "Base copy started for shop"}, {"ts": int64(1791600600000), "database": "", "message": "Cutover verdict GO: every database verified (write pause 1m4s)"}},
		EngineVersion: "0.18", AppVersion: "dev"}
	r.Checklist = Checklist(r)
	b, err := r.PDF()
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(out, b, 0o644)
}
