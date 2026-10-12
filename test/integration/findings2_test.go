//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"
)

// TestSequenceSyncFailureShown (audit finding 9): a failed sequence sync was
// logged as a warning and the step still read "done".
func TestSequenceSyncFailureShown(t *testing.T) {
	var broken atomic.Bool
	restore := pg.SetFaultHook(func(sql string) error {
		if broken.Load() && strings.Contains(sql, "FROM pg_sequences") {
			return errInjected
		}
		return nil
	})
	defer restore()
	// The sync step fails; verification's own comparison runs normally.
	e := newEnv(t, "", orch.TestHooks{BeforeVerify: func(context.Context, orch.Database) { broken.Store(false) }})
	db := uniq("it_seqfail")
	id := streamingMigration(t, e, "Sequence sync failure", db)
	broken.Store(true)
	op := e.cutoverOp(id, map[string]any{"confirm_writers_stopped": true})
	broken.Store(false)
	var seq orch.Step
	for _, s := range op.Steps {
		t.Logf("step %-10s %-8s %s", s.Key, s.State, s.Detail)
		if s.Key == "sequences" {
			seq = s
		}
	}
	if seq.State != "failed" || !strings.Contains(seq.Detail, "injected") {
		t.Fatalf("the sequences step reads %q (%s) although the sync failed", seq.State, seq.Detail)
	}
	if r := verifyResults(t, e, id)["sequences"]; !strings.HasPrefix(r, "ok") {
		t.Fatalf("verification's sequence comparison did not decide: %s", r)
	}
	e.abortAndClean(id, "Sequence sync failure")
}

// TestVerificationStoreFailureNoGo (audit finding 10): verification rows
// that could not be stored were dropped, and the verdict could still be GO
// with nothing to show for it.
func TestVerificationStoreFailureNoGo(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{FailVerificationStore: true})
	db := uniq("it_vstore")
	id := streamingMigration(t, e, "Verification store", db)
	op := e.cutoverOp(id, map[string]any{"confirm_writers_stopped": true})
	mv := e.migration(id)
	t.Logf("operation %s: %s; verdict %q", op.State, op.Error, mv.Migration.Flags.Verdict)
	if mv.Migration.Flags.Verdict == "GO" {
		t.Fatal("GO although no verification result could be recorded")
	}
	d := e.db(id, db)
	if !strings.Contains(d.LastError, "could not be recorded") {
		t.Fatalf("the NO-GO does not say the results could not be recorded: %q", d.LastError)
	}
	e.abortAndClean(id, "Verification store")
}

// TestAuditFailureRefusesOverrides (audit finding 11): an accepted risk and
// an Admin's override of detected writes went ahead when their audit entry
// could not be written. Both must be refused, with nothing changed.
func TestAuditFailureRefusesOverrides(t *testing.T) {
	var failing atomic.Value
	failing.Store("")
	e := newEnv(t, "", orch.TestHooks{})
	e.Audit.FailWith = func(action string) error {
		if action == failing.Load().(string) {
			return fmt.Errorf("audit store unavailable (test)")
		}
		return nil
	}

	// An acceptable blocker: preflight could not check prepared transactions.
	t.Run("accept", func(t *testing.T) {
		db := uniq("it_aacc")
		seedDB(t, db, 100)
		id := e.defineMigration("Audit accept", []string{db}, nil)
		restore := pg.SetFaultHook(func(sql string) error {
			if strings.Contains(sql, "pg_prepared_xacts") {
				return errInjected
			}
			return nil
		})
		pv := e.preflight(id)
		restore()
		var rid string
		for _, r := range pv.Results {
			if r.CheckID == "prepared_xacts" && r.Level == "blocker" && !r.Hard {
				rid = r.ID
			}
		}
		if rid == "" {
			t.Fatal("no acceptable prepared_xacts blocker")
		}
		failing.Store("preflight.accept")
		code := e.do("POST", "/api/v1/migrations/"+id+"/acceptances", map[string]any{"result_id": rid, "reason": "testing the audit failure path"}, nil)
		failing.Store("")
		if code < 400 {
			t.Fatalf("the risk was accepted (%d) although its audit entry could not be written", code)
		}
		var n int
		if err := e.Store.DB.QueryRow(`SELECT count(*) FROM acceptances WHERE migration_id=?`, id).Scan(&n); err != nil || n != 0 {
			t.Fatalf("an acceptance was stored (%d, %v) although it was refused", n, err)
		}
		if c := e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil); c < 400 {
			t.Fatalf("the migration started (%d) with an unaccepted blocker", c)
		}
	})

	t.Run("override", func(t *testing.T) {
		db := uniq("it_aover")
		name := "Audit override"
		id := streamingMigration(t, e, name, db)
		w := startWriter(t, db)
		defer w.Stop()
		failing.Store("cutover.override_writes")
		op := e.cutoverOp(id, map[string]any{"confirm_writers_stopped": true, "override_confirm": name})
		failing.Store("")
		w.Stop()
		t.Logf("operation %s: %s", op.State, op.Error)
		if op.State != "failed" || !strings.Contains(op.Error, "audit") {
			t.Fatalf("the override went ahead without its audit entry: %s %q", op.State, op.Error)
		}
		for _, d := range e.migration(id).Databases {
			if d.EndPos != "" {
				t.Fatalf("an end position was set (%s) after a refused override", d.EndPos)
			}
		}
		e.abortAndClean(id, name)
	})
}
