//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"
)

// Audit findings 2 and 4: the writers-stopped step must see writers it could
// not see before, fail closed, and verification must end NO-GO if the source
// changed after the writers step.

func streamingMigration(t *testing.T, e *Env, name, db string) string {
	t.Helper()
	seedDB(t, db, 3000)
	id := e.defineMigration(name, []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	return id
}

// cutoverOp starts a cutover once readiness allows it and returns the
// finished operation (without failing the test on a failed operation).
func (e *Env) cutoverOp(id string, body map[string]any) *orch.Operation {
	e.t.Helper()
	var rd struct {
		Ready bool `json:"ready"`
	}
	e.waitFor(15*time.Minute, "the readiness gate", func() bool {
		e.do("GET", "/api/v1/migrations/"+id+"/cutover/readiness", nil, &rd, 200)
		return rd.Ready
	})
	var r struct {
		OperationID string `json:"operation_id"`
	}
	e.do("POST", "/api/v1/migrations/"+id+"/cutover", body, &r, 202)
	return e.waitOp(r.OperationID, 25*time.Minute)
}

func blockerWith(t *testing.T, e *Env, id, checkID, contains string) bool {
	t.Helper()
	for c, r := range verifyResults(t, e, id) {
		if c == checkID && strings.HasPrefix(r, "blocker") && strings.Contains(r, contains) {
			return true
		}
	}
	return false
}

func TestWritersOpenTransactionRefused(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_wopen")
	id := streamingMigration(t, e, "Writers open txn", db)
	ctx := context.Background()
	// Connect as an application would (pg.Connect names its sessions
	// "upwell", which the writers check rightly ignores).
	w, err := pgx.Connect(ctx, fmt.Sprintf("postgres://doadmin:%s@127.0.0.1:%d/%s?sslmode=disable&application_name=shop-api", password, srcPort, db))
	if err != nil {
		t.Fatal(err)
	}
	// A writer inside an open transaction: its insert is not in the
	// statistics until it commits, which could be after the end position.
	if _, err := w.Exec(ctx, `BEGIN; INSERT INTO orders(customer, amount, note) VALUES (1, 1, 'open-txn')`); err != nil {
		t.Fatal(err)
	}
	op := e.cutoverOp(id, map[string]any{"confirm_writers_stopped": true})
	_, _ = w.Exec(ctx, `COMMIT`)
	w.Close(ctx)
	v := e.migration(id).Migration.Flags.Verdict
	t.Logf("operation %s: %s; verdict %q", op.State, op.Error, v)
	if v == "GO" {
		t.Fatal("GO although a writer held an open transaction through the writers step (its row is now missing on the target)")
	}
	if op.State != "failed" || !strings.Contains(op.Error, "open write transaction") {
		t.Fatalf("the writers step did not name the open write transaction: %s %q", op.State, op.Error)
	}
	e.abortAndClean(id, "Writers open txn")
}

func TestWritesAfterVerificationNoGo(t *testing.T) {
	var db string
	hook := func(ctx context.Context, d orch.Database) {
		// A writer that resumes after the table was verified: verification saw
		// equal data, but this row will be missing when applications switch.
		exec1(t, srcConn, d.SourceName, `INSERT INTO orders(customer, amount, note) VALUES (2, 2, 'after-verify')`)
	}
	e := newEnv(t, "", orch.TestHooks{AfterVerify: hook})
	db = uniq("it_wafter")
	id := streamingMigration(t, e, "Writes after verify", db)
	time.Sleep(3 * time.Second)
	e.startCutover(id)
	if v := e.migration(id).Migration.Flags.Verdict; v != "NO-GO" {
		t.Fatalf("verdict %s although the source changed after verification", v)
	}
	if !blockerWith(t, e, id, "writes_stopped", "changed") {
		t.Fatal("no writes_stopped blocker naming the change")
	}
	e.abortAndClean(id, "Writes after verify")
}

func TestWritersCheckFailsClosed(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_wfail")
	id := streamingMigration(t, e, "Writers fail closed", db)
	restore := pg.SetFaultHook(func(sql string) error {
		if strings.Contains(sql, "n_tup_ins") {
			return errInjected
		}
		return nil
	})
	op := e.cutoverOp(id, map[string]any{"confirm_writers_stopped": true})
	restore()
	t.Logf("operation %s: %s", op.State, op.Error)
	if op.State != "failed" || !strings.Contains(op.Error, "injected failure") {
		t.Fatalf("the writers step passed although its sample failed: %s %q (verdict %q)", op.State, op.Error, e.migration(id).Migration.Flags.Verdict)
	}
	e.abortAndClean(id, "Writers fail closed")
}

func TestOverrideWithWritesRunningNoGo(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_wover")
	name := "Override writes"
	id := streamingMigration(t, e, name, db)
	w := startWriter(t, db)
	defer w.Stop()
	op := e.cutoverOp(id, map[string]any{"confirm_writers_stopped": true, "override_confirm": name})
	w.Stop()
	v := e.migration(id).Migration.Flags.Verdict
	t.Logf("operation %s: %s; verdict %q", op.State, op.Error, v)
	if v != "NO-GO" {
		t.Fatalf("verdict %q with an Admin override while writes kept running", v)
	}
	t.Logf("writes_stopped: %s", verifyResults(t, e, id)["writes_stopped"])
	if !blockerWith(t, e, id, "writes_stopped", "Admin overrode") {
		t.Fatal("NO-GO did not name the overridden writes")
	}
	e.abortAndClean(id, name)
}
