//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pgpack"
)

func verificationRows(e *Env, id string) []orch.VerificationRow {
	var ver struct {
		Results []orch.VerificationRow `json:"results"`
	}
	e.do("GET", "/api/v1/migrations/"+id+"/verification", nil, &ver, 200)
	return ver.Results
}

func findCheck(rows []orch.VerificationRow, db, id string) *orch.VerificationRow {
	for i := range rows {
		if rows[i].Database == db && rows[i].CheckID == id {
			return &rows[i]
		}
	}
	return nil
}

// Part 1, scenario 15 (D91): one changed byte in one row of a table larger
// than exact_count_max_mb and outside the checksum budget.
func TestVerifyLargeTableOneByte(t *testing.T) {
	for _, mode := range []string{"budget", "full"} {
		t.Run(mode, func(t *testing.T) {
			db := uniq("it_onebyte_" + mode)
			var changed string
			e := newEnv(t, "", orch.TestHooks{BeforeVerify: func(ctx context.Context, d orch.Database) {
				// One byte of one row's note, same length (the size check cannot see it).
				exec1(t, dstConn, db, `UPDATE orders SET note = overlay(note placing 'Z' from 1 for 1) WHERE id = 777777`)
				changed = q(t, dstConn, db, `SELECT left(note, 3) FROM orders WHERE id = 777777`)
			}})
			seedDB(t, db, 1500000)
			id := e.defineMigration("One byte "+mode, []string{db}, map[string]any{
				"exact_count_max_mb": 0, "verify_checksum_budget_seconds": 0, "verify_large_tables": mode, "verify_chunk_mb": 16})
			e.preflight(id)
			e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
			e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
			e.startCutover(id)
			t.Logf("changed on the target: %s", changed)
			v := e.migration(id).Migration.Flags.Verdict
			rows := verificationRows(e, id)
			cov := findCheck(rows, db, "coverage")
			if cov == nil {
				logVerification(t, e, id)
				t.Fatal("no coverage result")
			}
			switch mode {
			case "budget":
				// Not detectable without comparing rows: the verdict must say the
				// table was not compared row by row.
				if cov.Result != "warning" || !strings.Contains(string(cov.Detail), "public.orders") {
					logVerification(t, e, id)
					t.Fatalf("budget mode: coverage %s, want a warning naming public.orders (verdict %s)", cov.Result, v)
				}
				f := e.migration(id).Migration.Flags
				if v == "GO" && (f.UnverifiedCount == 0 || !strings.Contains(strings.Join(f.Unverified, ","), db+": public.orders")) {
					t.Fatalf("a GO resting on an uncompared table is not marked: %d %v", f.UnverifiedCount, f.Unverified)
				}
				// "Verify every table fully" then finds the byte.
				var op struct {
					ID string `json:"operation_id"`
				}
				e.do("POST", "/api/v1/migrations/"+id+"/verify", map[string]any{"full": true}, &op, 202)
				e.waitOp(op.ID, 10*time.Minute)
				if v2 := e.migration(id).Migration.Flags.Verdict; v2 != "NO-GO" {
					logVerification(t, e, id)
					t.Fatalf("full verification after a partial GO ended %s, want NO-GO", v2)
				}
				fc := findCheck(verificationRows(e, id), db, "full_compare")
				if fc == nil || !strings.Contains(string(fc.Detail), "different contents") {
					t.Fatalf("full verification did not name the range: %+v", fc)
				}
			case "full":
				fc := findCheck(rows, db, "full_compare")
				if v != "NO-GO" || fc == nil || fc.Result != "blocker" || !strings.Contains(string(fc.Detail), "different contents") {
					logVerification(t, e, id)
					t.Fatalf("full mode: verdict %s, full_compare %+v; want NO-GO naming the range", v, fc)
				}
				t.Logf("full_compare: %.300s", string(fc.Detail))
			}
			e.cleanup(id, "One byte "+mode)
		})
	}
}

// The same, without a change: full mode compares every range and ends GO
// with full coverage; a second pass with every range recorded is skipped
// (resume).
func TestVerifyLargeTableFullGO(t *testing.T) {
	db := uniq("it_fullgo")
	e := newEnv(t, "", orch.TestHooks{})
	seedDB(t, db, 1500000)
	id := e.defineMigration("Full GO", []string{db}, map[string]any{
		"exact_count_max_mb": 0, "verify_checksum_budget_seconds": 0, "verify_large_tables": "full", "verify_chunk_mb": 16})
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	e.startCutover(id)
	rows := verificationRows(e, id)
	if v := e.migration(id).Migration.Flags.Verdict; v != "GO" {
		logVerification(t, e, id)
		t.Fatalf("verdict %s", v)
	}
	if c := findCheck(rows, db, "coverage"); c == nil || c.Result != "ok" {
		logVerification(t, e, id)
		t.Fatalf("coverage %+v, want ok", c)
	}
	// Ranges and resume, directly.
	mem := &memTracker{done: map[string]bool{}}
	tables := []pgpack.LargeTable{{Name: "public.orders", Bytes: 130 << 20, Est: 1500000}}
	r1 := pgpack.FullCompare(context.Background(), srcConn.WithDB(db), dstConn.WithDB(db), tables, 16<<20, 4, mem, nil)
	if r1[0].Err != nil || len(r1[0].Mismatches) > 0 || r1[0].Chunks < 4 || r1[0].Rows < 1500000 {
		t.Fatalf("first pass: %+v", r1[0])
	}
	queried := 0
	r2 := pgpack.FullCompare(context.Background(), srcConn.WithDB(db), dstConn.WithDB(db), tables, 16<<20, 4, mem, func(done, total int, table string) { queried = total })
	if r2[0].Rows != 0 || queried != r1[0].Chunks {
		t.Fatalf("second pass compared %d rows (want 0, every range recorded): %+v", r2[0].Rows, r2[0])
	}
	t.Logf("%d ranges, %d rows; resumed pass compared none", r1[0].Chunks, r1[0].Rows)
	e.cleanup(id, "Full GO")
}

type memTracker struct{ done map[string]bool }

func (m *memTracker) Done(table, lo, hi string) bool { return m.done[table+"|"+lo+"|"+hi] }
func (m *memTracker) Record(table, lo, hi string, n int64) {
	m.done[table+"|"+lo+"|"+hi] = true
}
