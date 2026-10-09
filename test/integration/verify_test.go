//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"
)

// Objects the verification gate once missed (bug bash 4): a column named t
// shadowed the row alias in the checksum, descending sequences could never be
// "behind", and indexes and constraints were not compared at all.
const hardeningSchema = `
	CREATE TABLE tcol(id int PRIMARY KEY, t text, v text);
	INSERT INTO tcol SELECT g, 'same', 'v'||g FROM generate_series(1, 500) g;
	CREATE SEQUENCE dseq INCREMENT -1 START -1 MAXVALUE -1;
	SELECT nextval('dseq') FROM generate_series(1, 100);
	CREATE INDEX tcol_v_idx ON tcol(v);
	ALTER TABLE orders ADD CONSTRAINT amount_positive CHECK (amount >= 0);`

func verifyResults(t *testing.T, e *Env, id string) map[string]string {
	t.Helper()
	var ver struct {
		Results []orch.VerificationRow `json:"results"`
	}
	e.do("GET", "/api/v1/migrations/"+id+"/verification", nil, &ver, 200)
	out := map[string]string{}
	for _, r := range ver.Results {
		out[r.CheckID] = r.Result + " " + string(r.Detail)
		t.Logf("verification %-16s %-8s %.200s", r.CheckID, r.Result, string(r.Detail))
	}
	return out
}

func TestVerifyCatchesWhatItMissed(t *testing.T) {
	db := uniq("it_vhard")
	hook := func(ctx context.Context, d orch.Database) {
		// Only the non-t column differs, the descending sequence is reset,
		// and an index and a constraint are gone.
		exec1(t, dstConn, d.TargetName, `UPDATE tcol SET v = 'tampered' WHERE id = 7;
			SELECT setval('dseq', -1, false);
			DROP INDEX tcol_v_idx;
			ALTER TABLE orders DROP CONSTRAINT amount_positive;`)
	}
	e := newEnv(t, "", orch.TestHooks{BeforeVerify: hook})
	seedDB(t, db, 3000)
	exec1(t, srcConn, db, hardeningSchema)
	id := e.defineMigration("Verify hardening", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	e.startCutover(id)
	if v := e.migration(id).Migration.Flags.Verdict; v != "NO-GO" {
		t.Fatalf("verdict %s, want NO-GO", v)
	}
	res := verifyResults(t, e, id)
	for check, want := range map[string]string{"checksums": "public.tcol", "sequences": "public.dseq", "schema_objects": "tcol_v_idx"} {
		if r := res[check]; !strings.HasPrefix(r, "blocker") || !strings.Contains(r, want) {
			t.Errorf("%s: %q, want a blocker naming %s", check, r, want)
		}
	}
	if r := res["schema_objects"]; !strings.Contains(r, "amount_positive") {
		t.Errorf("schema_objects does not name the dropped constraint: %q", r)
	}
	e.abortAndClean(id, "Verify hardening")
}

func TestVerifyGOWithHardSchema(t *testing.T) {
	db := uniq("it_vgo")
	var holder func()
	hook := func(ctx context.Context, d orch.Database) {
		// Another session holds a temporary table on the source during
		// verification: it is not the customer's data and must not count.
		c, err := pg.Connect(ctx, srcConn.WithDB(d.SourceName), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `CREATE TEMP TABLE scratch_tmp(a int); INSERT INTO scratch_tmp VALUES (1)`); err != nil {
			t.Fatal(err)
		}
		holder = func() { c.Close(context.Background()) }
	}
	e := newEnv(t, "", orch.TestHooks{BeforeVerify: hook})
	seedDB(t, db, 3000)
	exec1(t, srcConn, db, hardeningSchema)
	id := e.defineMigration("Verify GO hard schema", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	exec1(t, srcConn, db, `UPDATE tcol SET v = 'changed' WHERE id <= 10; SELECT nextval('dseq') FROM generate_series(1, 50);`)
	v := outcome(t, e, id, db)
	if holder != nil {
		holder()
	}
	res := verifyResults(t, e, id)
	if v != "GO" {
		t.Fatalf("verdict %s, want GO", v)
	}
	if got, want := q(t, dstConn, db, `SELECT last_value FROM dseq`), q(t, srcConn, db, `SELECT last_value FROM dseq`); got != want {
		t.Errorf("descending sequence on the target at %s, source at %s", got, want)
	}
	if r := res["schema_objects"]; !strings.HasPrefix(r, "ok") {
		t.Errorf("schema_objects: %s", r)
	}
	e.cleanup(id, "Verify GO hard schema")
}
