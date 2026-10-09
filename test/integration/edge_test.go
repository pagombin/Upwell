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

// Edge cases where data could be lost without anyone noticing. Each must end
// with identical data, or with NO-GO; never with GO and a difference.

func edgeMigration(t *testing.T, e *Env, name, db string, setup string) string {
	t.Helper()
	seedDB(t, db, 5000)
	if setup != "" {
		exec1(t, srcConn, db, setup)
	}
	id := e.defineMigration(name, []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	return id
}

// outcome runs the cutover and checks the rule: GO only with identical data.
func outcome(t *testing.T, e *Env, id, db string) string {
	t.Helper()
	time.Sleep(3 * time.Second)
	e.startCutover(id)
	verdict := e.migration(id).Migration.Flags.Verdict
	_, mm := compareTables(t, db)
	switch {
	case verdict == "GO" && len(mm) > 0:
		t.Fatalf("GO although the data differs:\n%s", strings.Join(mm, "\n"))
	case verdict == "GO":
		t.Logf("GO, and the independent comparison finds identical data")
	default:
		t.Logf("NO-GO; the independent comparison finds: %s", strings.Join(mm, "; "))
	}
	return verdict
}

// compareTables is compareDatabase without the fidelity schema's extra checks.
func compareTables(t *testing.T, db string) (int, []string) {
	src, dst := tableFingerprints(t, srcConn, db), tableFingerprints(t, dstConn, db)
	var mm []string
	for n, v := range src {
		if dst[n] != v {
			mm = append(mm, db+"."+n+": source "+v+", target "+dst[n])
		}
	}
	return len(src), mm
}

func TestEdgeTruncateDuringCDC(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_edge_trunc")
	id := edgeMigration(t, e, "Edge truncate", db, `CREATE TABLE scratch(id int PRIMARY KEY, v text); INSERT INTO scratch SELECT g, 'old' FROM generate_series(1, 1000) g;`)
	exec1(t, srcConn, db, `TRUNCATE scratch; INSERT INTO scratch SELECT g, 'new' FROM generate_series(1, 10) g;`)
	v := outcome(t, e, id, db)
	t.Logf("TRUNCATE during streaming: %s", v)
	e.cleanup(id, "Edge truncate")
}

func TestEdgeDDLDuringCDC(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_edge_ddl")
	id := edgeMigration(t, e, "Edge DDL", db, "")
	// Logical decoding does not carry DDL: the new column exists only on the source.
	exec1(t, srcConn, db, `ALTER TABLE orders ADD COLUMN coupon text; UPDATE orders SET coupon = 'X' WHERE id <= 10;`)
	if v := outcome(t, e, id, db); v == "GO" {
		t.Fatal("GO although the schema changed on the source during streaming")
	}
	e.cleanup(id, "Edge DDL")
}

// TestEdgeNoReplicaIdentity: a table without a primary key or replica
// identity. With pgoutput's publication FOR ALL TABLES, PostgreSQL refuses
// UPDATE and DELETE on such a table on the source. This records what the
// customer's application would see and what the migration does.
func TestEdgeNoReplicaIdentity(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_edge_noident")
	seedDB(t, db, 2000)
	exec1(t, srcConn, db, `CREATE TABLE nokey(a int, b text); INSERT INTO nokey SELECT g, 'x' FROM generate_series(1, 100) g;`)
	id := e.defineMigration("Edge no identity", []string{db}, nil)
	pv := e.preflight(id)
	var resultID string
	for _, r := range pv.Results {
		if r.CheckID == "replica_identity" && r.Level == "blocker" {
			resultID = r.ID
			t.Logf("preflight: %s (hard %v)", r.Message, r.Hard)
		}
	}
	if resultID == "" {
		t.Fatal("preflight did not flag the table without a replica identity")
	}
	code := e.do("POST", "/api/v1/migrations/"+id+"/acceptances", map[string]any{"result_id": resultID, "reason": "testing the consequences of accepting this"}, nil)
	if code >= 400 {
		t.Logf("the risk cannot be accepted (%d)", code)
		if c := e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil); c < 400 {
			t.Fatalf("start was accepted (%d) with a table that would break the application's updates", c)
		}
		// The customer's fix: REPLICA IDENTITY FULL, then the migration runs and updates work.
		exec1(t, srcConn, db, `ALTER TABLE nokey REPLICA IDENTITY FULL`)
		e.preflight(id)
	}
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	ctx := context.Background()
	conn, err := pg.Connect(ctx, srcConn.WithDB(db), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, upErr := conn.Exec(ctx, `UPDATE nokey SET b = 'y' WHERE a = 1`)
	conn.Close(ctx)
	t.Logf("UPDATE on the source table while migrating: %v", upErr)
	if upErr != nil {
		t.Fatalf("the application's UPDATE failed on the source: %v", upErr)
	}
	exec1(t, srcConn, db, `INSERT INTO nokey VALUES (1000, 'inserted')`)
	outcome(t, e, id, db)
	e.cleanup(id, "Edge no identity")
}
