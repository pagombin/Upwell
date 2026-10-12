//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/check"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/pgpack"
)

// superExec runs SQL on the source cluster as its superuser (role setup the
// admin user cannot do).
func superExec(t *testing.T, db, sql string) {
	t.Helper()
	out, err := exec.Command("psql", "-X", "-v", "ON_ERROR_STOP=1", "-h", "/var/tmp/upwell-spike", "-p", fmt.Sprint(srcPort), "-U", "postgres", "-d", db, "-c", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("psql: %v: %s", err, out)
	}
}

func preflightResults(t *testing.T, env pgpack.Env) map[string]check.Result {
	t.Helper()
	got := map[string]check.Result{}
	(&pgpack.Runner{Env: env, Emit: func(r check.Result) { got[r.CheckID] = r }}).Run(context.Background())
	return got
}

// TestReplicaIdentityMatrix (preflight findings 1 to 5 and 9, enhancements
// E3 and E4).
func TestReplicaIdentityMatrix(t *testing.T) {
	db := uniq("it_rimx")
	seedDB(t, db, 100)
	exec1(t, srcConn, db, `
		CREATE TABLE nokey_d(a int, b text);
		CREATE TABLE nokey_n(a int PRIMARY KEY, b text); ALTER TABLE nokey_n REPLICA IDENTITY NOTHING;
		CREATE TABLE ti_dropped(a int NOT NULL, b text); CREATE UNIQUE INDEX ti_dropped_a ON ti_dropped(a);
		ALTER TABLE ti_dropped REPLICA IDENTITY USING INDEX ti_dropped_a; DROP INDEX ti_dropped_a;
		CREATE TABLE "Mixed Case"(a int, b text);
		CREATE TABLE pp(a int NOT NULL, b text) PARTITION BY RANGE(a);
		CREATE TABLE pp1 PARTITION OF pp FOR VALUES FROM (0) TO (100); ALTER TABLE pp1 ADD PRIMARY KEY (a);
		CREATE TABLE pn(a int NOT NULL, b text) PARTITION BY RANGE(a);
		CREATE TABLE pn1 PARTITION OF pn FOR VALUES FROM (0) TO (100);
		INSERT INTO nokey_d SELECT g, 'x' FROM generate_series(1, 50) g;
		UPDATE nokey_d SET b = 'y' WHERE a <= 20; DELETE FROM nokey_d WHERE a > 45;
		CREATE MATERIALIZED VIEW "Daily Sales" AS SELECT customer, sum(amount) FROM orders GROUP BY 1;`)
	superExec(t, db, `DO $$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='ri_app_owner') THEN CREATE ROLE ri_app_owner; END IF; END$$;
		CREATE TABLE app_owned(a int, b text); ALTER TABLE app_owned OWNER TO ri_app_owner; GRANT SELECT ON app_owned TO doadmin;`)
	t.Cleanup(func() { dropDB(t, db); superExec(t, "defaultdb", `DROP ROLE IF EXISTS ri_app_owner`) })
	// Let the statistics collector publish the updates and deletes.
	exec1(t, srcConn, db, `SELECT pg_stat_force_next_flush()`)

	want := []string{`public.nokey_d`, `public.nokey_n`, `public.ti_dropped`, `public."Mixed Case"`, `public.pn1`, `public.app_owned`}
	notWant := []string{`public.pp`, `public.pn`, `public.pp1`}
	for _, plugin := range []string{"pgoutput", "test_decoding"} {
		t.Run(plugin, func(t *testing.T) {
			env := failClosedEnv(t, db)
			env.Settings["decoding_plugin"] = plugin
			all := preflightResults(t, env)
			if mv := all["materialized_views"]; fmt.Sprint(mv.Evidence["names"]) != `[public."Daily Sales"]` {
				t.Errorf("materialized_views names (for the post-cutover checklist): %v", mv.Evidence["names"])
			}
			r := all["replica_identity"]
			t.Logf("%s: %s\nremediation: %s", r.Level, r.Message, r.Remediation)
			raw, _ := json.Marshal(r.Evidence)
			t.Logf("evidence: %.1500s", raw)
			var ev struct {
				Tables  []string `json:"tables"`
				Count   int      `json:"count"`
				Plugin  string   `json:"plugin"`
				FixSQL  string   `json:"fix_sql"`
				Details []struct {
					Table      string `json:"table"`
					Owner      string `json:"owner"`
					OwnedByYou bool   `json:"owned_by_you"`
					Updates    int64  `json:"updates"`
					Deletes    int64  `json:"deletes"`
					SizeBytes  int64  `json:"size_bytes"`
					Cost       string `json:"full_cost"`
				} `json:"details"`
			}
			if err := json.Unmarshal(raw, &ev); err != nil {
				t.Fatal(err)
			}
			if r.Level != check.Blocker || !r.Hard {
				t.Fatalf("not a hard blocker: %s", r.Level)
			}
			// Finding 9: 'i' with its index dropped is covered; partitioned
			// parents are not flagged (PostgreSQL checks the leaf).
			for _, w := range want {
				if !contains(ev.Tables, w) {
					t.Errorf("%s is not flagged", w)
				}
			}
			for _, w := range notWant {
				if contains(ev.Tables, w) {
					t.Errorf("%s is flagged although PostgreSQL never checks it", w)
				}
			}
			if ev.Count != len(want) {
				t.Errorf("count %d, want %d", ev.Count, len(want))
			}
			// Finding 1: the message matches the plugin.
			if plugin == "pgoutput" && !strings.Contains(r.Message, "fail with an error on the source") {
				t.Errorf("pgoutput message does not say the source refuses the statement: %s", r.Message)
			}
			if plugin == "test_decoding" {
				if strings.Contains(r.Message, "fail with an error on the source") {
					t.Errorf("test_decoding message claims the source refuses the statement: %s", r.Message)
				}
				if !strings.Contains(r.Message, "succeed on the source") || !strings.Contains(r.Message, "cannot be applied on the target") {
					t.Errorf("test_decoding message does not describe the real failure: %s", r.Message)
				}
			}
			// Findings 2 to 5: cost, lock queue, PK rewrite, ownership.
			for _, s := range []string{"lock_timeout = '5s'", "rewrites", "owner", "every column"} {
				if !strings.Contains(r.Remediation, s) {
					t.Errorf("remediation does not mention %q", s)
				}
			}
			if !strings.Contains(r.Message, "owned by another role") {
				t.Errorf("message does not say a table is owned by another role: %s", r.Message)
			}
			// E3: copy-ready, quoted fix SQL with the lock timeout.
			if !strings.Contains(ev.FixSQL, `SET lock_timeout = '5s';`) || !strings.Contains(ev.FixSQL, `ALTER TABLE public."Mixed Case" REPLICA IDENTITY FULL;`) {
				t.Errorf("fix_sql is not copy-ready: %s", ev.FixSQL)
			}
			// E4 and finding 2: per-table updates, deletes, size and cost.
			found := map[string]bool{}
			for _, d := range ev.Details {
				found[d.Table] = true
				switch d.Table {
				case "public.nokey_d":
					if d.Updates < 20 || d.Deletes < 5 || d.SizeBytes <= 0 || !strings.Contains(d.Cost, "costly") {
						t.Errorf("nokey_d evidence: %+v", d)
					}
				case "public.app_owned":
					if d.OwnedByYou || d.Owner != "ri_app_owner" {
						t.Errorf("app_owned ownership: %+v", d)
					}
				case "public.pn1":
					if !strings.Contains(d.Cost, "cheap") {
						t.Errorf("an insert-only table is not called cheap: %+v", d)
					}
				}
			}
			for _, w := range want {
				if !found[w] {
					t.Errorf("no details for %s", w)
				}
			}
		})
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func superExecDst(t *testing.T, db, sql string) {
	t.Helper()
	out, err := exec.Command("psql", "-X", "-v", "ON_ERROR_STOP=1", "-h", "/var/tmp/upwell-spike", "-p", fmt.Sprint(dstPort), "-U", "postgres", "-d", db, "-c", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("psql: %v: %s", err, out)
	}
}

// TestApplyPrivilegeGuidance (preflight findings 6 to 8, enhancement E3): a
// target user without the apply privileges gets the version wording, the
// template1/template0 guidance, the security trade-off, and grant and
// revoke scripts that work as shipped.
func TestApplyPrivilegeGuidance(t *testing.T) {
	db := uniq("it_apriv")
	seedDB(t, db, 50)
	superExecDst(t, "defaultdb", `DROP ROLE IF EXISTS ri_noapply; CREATE ROLE ri_noapply LOGIN PASSWORD 'pw-noapply' CREATEDB`)
	t.Cleanup(func() {
		superExecDst(t, "defaultdb", `REVOKE SET ON PARAMETER session_replication_role FROM ri_noapply; DROP OWNED BY ri_noapply; DROP ROLE IF EXISTS ri_noapply`)
	})
	env := failClosedEnv(t, db)
	env.Target.User, env.Target.Password = "ri_noapply", "pw-noapply"
	run := func() check.Result {
		r := preflightResults(t, env)["target_apply_privileges"]
		t.Logf("%s: %s", r.Level, r.Message)
		return r
	}
	r := run()
	t.Logf("remediation: %s", r.Remediation)
	if r.Level != check.Blocker || !r.Hard {
		t.Fatalf("not a hard blocker: %s", r.Level)
	}
	for _, s := range []string{"template1", "template0", "foreign-key", "pg_<oid>", "revoke script", "superuser"} {
		if !strings.Contains(r.Remediation, s) {
			t.Errorf("remediation does not mention %q", s)
		}
	}
	grant, _ := r.Evidence["grant_script"].(string)
	revoke, _ := r.Evidence["revoke_script"].(string)
	t.Logf("grant script:\n%s\nrevoke script:\n%s", grant, revoke)
	if !strings.Contains(grant, "GRANT SET ON PARAMETER session_replication_role TO \"ri_noapply\";") || !strings.Contains(grant, "pg_catalog.pg_replication_origin_create(text)") {
		t.Fatalf("grant script incomplete")
	}
	if !strings.Contains(revoke, "REVOKE SET ON PARAMETER session_replication_role FROM \"ri_noapply\";") {
		t.Fatalf("revoke script incomplete")
	}
	// The scripts work as shipped.
	superExecDst(t, "defaultdb", grant)
	if r := run(); r.Level != check.OK {
		t.Fatalf("after the grant script: %s %s", r.Level, r.Message)
	}
	superExecDst(t, "defaultdb", revoke)
	if r := run(); r.Level != check.Blocker {
		t.Fatalf("after the revoke script: %s %s", r.Level, r.Message)
	}

	// Finding 7: the wording for targets before PostgreSQL 15.
	ctx := context.Background()
	c, err := pg.Connect(ctx, env.Target, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	ap, err := pgpack.TargetApplyPrivileges(ctx, c, 140000)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(ap.Missing, "; ")
	t.Logf("PostgreSQL 14 target: %s", joined)
	if !strings.Contains(joined, "before PostgreSQL 15") || strings.Contains(joined, "PostgreSQL 14 allows") {
		t.Errorf("pre-15 wording: %s", joined)
	}
}
