//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pagombin/upwell/internal/orch"
)

// TestModeOfflineNeverIgnored (audit finding 5): mode=offline was accepted and
// ignored, so a migration set to offline silently ran online. Either the
// setting is refused, or the engine command it produces is not online.
func TestModeOfflineNeverIgnored(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_mode")
	seedDB(t, db, 100)
	var gout any
	global := e.do("PATCH", "/api/v1/settings", map[string]any{"mode": "offline"}, &gout)
	t.Logf("global mode=offline: %d", global)
	var out map[string]any
	id := e.defineMigration("Mode offline", []string{db}, nil)
	per := e.do("PATCH", "/api/v1/migrations/"+id, map[string]any{"settings": map[string]any{"mode": "offline"}}, &out)
	t.Logf("per-migration mode=offline: %d %v", per, out["message"])
	if global >= 400 && per >= 400 {
		return // refused: never ignored
	}
	var plan any
	e.do("GET", "/api/v1/migrations/"+id+"/plan", nil, &plan, 200)
	b, _ := json.Marshal(plan)
	if strings.Contains(string(b), "--follow") {
		t.Fatalf("mode=offline was accepted (global %d, per migration %d) but the engine command still streams changes (--follow)", global, per)
	}
}

// TestPreflightCountsAndQuoting (audit finding 7): counts came from a sample
// with LIMIT 100 (or 50), so 150 tables showed as 100, and names were built
// with nspname||'.'||relname, so a copied ALTER TABLE failed on mixed case.
func TestPreflightCountsAndQuoting(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_counts")
	seedDB(t, db, 50)
	exec1(t, srcConn, db, `DO $$ BEGIN FOR i IN 1..150 LOOP EXECUTE format('CREATE TABLE %I(a int)', 'NoKey_'||i); END LOOP; END $$;`)
	superSQL(t, srcPort, db, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='other_owner') THEN CREATE ROLE other_owner; END IF;
		FOR i IN 1..60 LOOP EXECUTE format('CREATE TABLE %I(a int PRIMARY KEY)', 'Hidden_'||i); EXECUTE format('ALTER TABLE %I OWNER TO other_owner', 'Hidden_'||i); EXECUTE format('REVOKE ALL ON %I FROM PUBLIC', 'Hidden_'||i); END LOOP; END $$;`)
	id := e.defineMigration("Counts", []string{db}, nil)
	pv := e.preflight(id)
	got := map[string]string{}
	for _, r := range pv.Results {
		if r.CheckID == "replica_identity" || r.CheckID == "read_privs" {
			got[r.CheckID] = r.Message
			t.Logf("%s: %s", r.CheckID, r.Message)
		}
	}
	if !strings.Contains(got["replica_identity"], "150 table") {
		t.Errorf("replica_identity does not count 150 tables: %q", got["replica_identity"])
	}
	if !strings.Contains(got["replica_identity"], `public."NoKey_`) {
		t.Errorf("replica_identity does not quote mixed-case names: %q", got["replica_identity"])
	}
	if !strings.Contains(got["read_privs"], "60 table") {
		t.Errorf("read_privs does not count 60 tables: %q", got["read_privs"])
	}
	if !strings.Contains(got["read_privs"], `public."Hidden_`) {
		t.Errorf("read_privs does not quote mixed-case names: %q", got["read_privs"])
	}
}
