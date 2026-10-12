//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

// installExtension writes an extension's control and script into the local
// PostgreSQL's extension directory (source and target share one install) and
// returns a function that removes them, which makes the extension unavailable
// on the target while it stays installed in a source database.
func installExtension(t *testing.T, name, script string, trusted bool) (remove func()) {
	t.Helper()
	share := superSQL(t, srcPort, "postgres", `SELECT setting FROM pg_config WHERE name='SHAREDIR'`)
	dir := filepath.Join(share, "extension")
	control := "default_version = '1.0'\nschema = " + name + "\nrelocatable = false\nsuperuser = true\n"
	if trusted {
		control += "trusted = true\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name+".control"), []byte(control), 0o644); err != nil {
		t.Skipf("cannot install a test extension in %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+"--1.0.sql"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	remove = func() {
		os.Remove(filepath.Join(dir, name+".control"))
		os.Remove(filepath.Join(dir, name+"--1.0.sql"))
	}
	t.Cleanup(remove)
	return remove
}

// The droplet (2026-10-12): the source database gained DigitalOcean Standard's
// aiven_extras extension after the migration started, with grants on its
// functions WITH GRANT OPTION to doadmin_group and pg_database_owner. Restart
// from zero then failed in pg_restore: the engine runs with --skip-extensions,
// so the GRANTs on the extension's functions had nothing to apply to (and
// doadmin_group does not exist on the target).
const aivenExtrasLike = `
CREATE FUNCTION aiven_extras.session_replication_role(arg_parameter text) RETURNS text LANGUAGE sql AS $$ SELECT arg_parameter $$;
CREATE FUNCTION aiven_extras.claim_public_schema_ownership() RETURNS void LANGUAGE sql AS $$ SELECT $$;
CREATE VIEW aiven_extras.pg_stat_replication AS SELECT 1 AS x;
`

func addAivenExtras(t *testing.T, db string) {
	superSQL(t, srcPort, db, `DO $$BEGIN CREATE ROLE it_doadmin_group; EXCEPTION WHEN duplicate_object THEN NULL; END$$`)
	for _, s := range []string{
		`CREATE EXTENSION aiven_extras`,
		`GRANT USAGE ON SCHEMA aiven_extras TO it_doadmin_group WITH GRANT OPTION`,
		`GRANT ALL ON FUNCTION aiven_extras.session_replication_role(text) TO it_doadmin_group WITH GRANT OPTION`,
		`GRANT ALL ON FUNCTION aiven_extras.session_replication_role(text) TO pg_database_owner WITH GRANT OPTION`,
		`GRANT ALL ON FUNCTION aiven_extras.claim_public_schema_ownership() TO pg_database_owner WITH GRANT OPTION`,
		`GRANT SELECT ON aiven_extras.pg_stat_replication TO pg_database_owner WITH GRANT OPTION`,
	} {
		superSQL(t, srcPort, db, s)
	}
}

func TestProviderExtensionLeftOut(t *testing.T) {
	remove := installExtension(t, "aiven_extras", aivenExtrasLike, false)
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_aivenext")
	seedDB(t, db, 2000)
	id := e.defineMigration("Provider extension", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)

	// The platform adds aiven_extras to the source database; the target
	// cannot provide it.
	addAivenExtras(t, db)
	remove()

	var out map[string]any
	if code := e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/restart", map[string]any{"confirm": db}, &out); code != 200 {
		t.Fatalf("restart from zero answered %d: %v", code, out)
	}
	d := e.waitDBState(id, db, 3*time.Minute, orch.DInSync, orch.DRestartRequired, orch.DFailed)
	if d.State != orch.DInSync {
		t.Fatalf("restart from zero with aiven_extras on the source ended %s: %s", d.State, d.LastError)
	}
	if n := q(t, dstConn, db, `SELECT count(*) FROM pg_namespace WHERE nspname='aiven_extras'`); n != "0" {
		t.Fatalf("the provider extension's schema was copied to the target")
	}
	// A resume passes the same filters (pgcopydb keeps them in its catalog).
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/stop", nil, nil, 200)
	e.waitDBState(id, db, 2*time.Minute, orch.DStopped)
	e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/resume", nil, nil, 200)
	if d := e.waitDBState(id, db, 3*time.Minute, orch.DInSync, orch.DRestartRequired, orch.DFailed); d.State != orch.DInSync {
		t.Fatalf("resume after leaving aiven_extras out ended %s: %s", d.State, d.LastError)
	}
	exec1(t, srcConn, db, `INSERT INTO orders(customer, amount, note) VALUES (1, 1, 'after resume')`)
	found := false
	for _, ev := range eventsOf(t, e, id) {
		msg, _ := ev["message"].(string)
		if ev["type"] == "extensions" && strings.Contains(msg, "aiven_extras") && strings.Contains(msg, "left out") {
			found = true
			t.Logf("event: %s", msg)
		}
	}
	if !found {
		t.Fatal("no event says aiven_extras was left out of the copy")
	}
	op := e.startCutover(id)
	if v := e.migration(id).Migration.Flags.Verdict; v != "GO" {
		for _, st := range op.Steps {
			t.Logf("step %-10s %-8s %s", st.Key, st.State, st.Detail)
		}
		t.Fatalf("verdict %s with only the provider extension left out", v)
	}
	e.cleanup(id, "Provider extension")
}

// An extension the target cannot provide that is not a provider's own is
// refused at the launch with a reason that names it, as preflight refuses it
// at the start, instead of failing inside pg_restore.
func TestUnavailableExtensionRefusedAtLaunch(t *testing.T) {
	remove := installExtension(t, "it_userext", `CREATE FUNCTION it_userext.f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;`, false)
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_userext")
	seedDB(t, db, 500)
	id := e.defineMigration("Unavailable extension", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	superSQL(t, srcPort, db, `CREATE EXTENSION it_userext`)
	remove()
	var out map[string]any
	if code := e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/restart", map[string]any{"confirm": db}, &out); code != 200 {
		t.Fatalf("restart from zero answered %d: %v", code, out)
	}
	d := e.waitDBState(id, db, 3*time.Minute, orch.DRestartRequired, orch.DFailed)
	t.Logf("%s: %s", d.State, d.LastError)
	if !strings.Contains(d.LastError, "it_userext") || !strings.Contains(d.LastError, "not available on the target") {
		t.Fatalf("the reason does not name the unavailable extension: %s", d.LastError)
	}
	if n := q(t, dstConn, db, `SELECT count(*) FROM pg_namespace WHERE nspname='it_userext'`); n != "0" {
		t.Fatal("the engine ran although the extension cannot be created on the target")
	}
	e.abortAndClean(id, "Unavailable extension")
}

// An extension the target can provide, in a schema of its own that the
// target does not have yet: Upwell creates the schema and the extension
// (it used to skip it, and pg_restore then created an empty schema).
func TestExtensionInOwnSchemaCreated(t *testing.T) {
	installExtension(t, "it_ownext", `CREATE FUNCTION it_ownext.f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;`, true)
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_ownext")
	seedDB(t, db, 500)
	id := e.defineMigration("Own-schema extension", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	superSQL(t, srcPort, db, `CREATE EXTENSION it_ownext`)
	var out map[string]any
	if code := e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/restart", map[string]any{"confirm": db}, &out); code != 200 {
		t.Fatalf("restart from zero answered %d: %v", code, out)
	}
	if d := e.waitDBState(id, db, 3*time.Minute, orch.DInSync, orch.DRestartRequired, orch.DFailed); d.State != orch.DInSync {
		t.Fatalf("restart from zero ended %s: %s", d.State, d.LastError)
	}
	if n := q(t, dstConn, db, `SELECT count(*) FROM pg_extension WHERE extname='it_ownext'`); n != "1" {
		t.Fatal("the extension was not created on the target")
	}
	e.abortAndClean(id, "Own-schema extension")
}
