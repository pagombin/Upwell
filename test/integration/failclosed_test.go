//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"path/filepath"

	"github.com/pagombin/upwell/internal/check"
	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/engine/pgcopydb"
	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/pgpack"
	"github.com/pagombin/upwell/internal/runner"
	"github.com/pagombin/upwell/internal/settings"
)

var errInjected = errors.New("injected failure (test)")

func failClosedEnv(t *testing.T, db string) pgpack.Env {
	v := settings.Defaults()
	v["wal_sample_seconds"] = 1
	v["create_target_databases"] = true
	return pgpack.Env{Source: srcConn, Target: dstConn, Settings: v, EngineVersion: "0.18", WorkDir: t.TempDir(),
		Databases:  []pgpack.DB{{Source: db, Target: db, SlotName: "upwell_fc_slot", Origin: "upwell_fc_slot"}},
		Permission: pgpack.Permission{Present: true, Customer: "Test", Ticket: "T-1"}}
}

// TestPreflightFailsClosed runs preflight once per query, failing only that
// query, and checks the rule "no safety check may pass when it could not
// run": the result emitted right after a failed query (the check that ran
// it) must be a blocker, never OK, a warning or info.
func TestPreflightFailsClosed(t *testing.T) {
	db := uniq("it_fc")
	seedDB(t, db, 200)
	env := failClosedEnv(t, db)
	ctx := context.Background()

	var total int
	restore := pg.SetFaultHook(func(string) error { total++; return nil })
	(&pgpack.Runner{Env: env, Emit: func(check.Result) {}}).Run(ctx)
	restore()
	t.Logf("preflight runs %d queries", total)
	if total < 30 {
		t.Fatalf("only %d queries went through the fault hook", total)
	}

	var violations []string
	for i := 1; i <= total; i++ {
		n, pending, failedSQL := 0, false, ""
		restore := pg.SetFaultHook(func(sql string) error {
			n++
			if n == i {
				pending, failedSQL = true, sql
				return errInjected
			}
			return nil
		})
		(&pgpack.Runner{Env: env, Emit: func(r check.Result) {
			if !pending {
				return
			}
			pending = false
			if r.Level != check.Blocker || !strings.Contains(r.Message, "injected failure") {
				q := strings.Join(strings.Fields(failedSQL), " ")
				if len(q) > 90 {
					q = q[:90] + "…"
				}
				violations = append(violations, fmt.Sprintf("query %d (%s) failed, then %s reported %s: %.120s", i, q, r.CheckID, r.Level, r.Message))
			}
		}}).Run(ctx)
		restore()
	}
	for _, v := range violations {
		t.Error(v)
	}
	t.Logf("%d of %d injected failures were reported as a blocker", total-len(violations), total)
}

// Part 1, general rule: no check may pass when it could not run. When the
// store could not be read from the drain on, the cutover listed no database,
// called the drain done, verified nothing and ended GO.
func TestCutoverStoreReadFailureNeverGO(t *testing.T) {
	var failing atomic.Bool
	e := newEnv(t, "", orch.TestHooks{CutoverReadErr: func() error {
		if failing.Load() {
			return errors.New("injected: database is locked")
		}
		return nil
	}})
	db := uniq("it_storefail")
	id := streamingMigration(t, e, "Store read failure", db)
	exec1(t, srcConn, db, `INSERT INTO orders(customer, amount, note) VALUES (1, 1, 'before cutover')`)
	time.Sleep(10 * time.Second)
	go func() {
		for i := 0; i < 600; i++ {
			m := e.migration(id).Migration
			if c := m.Flags.Cutover; c != nil && (c.Phase == "endpos" || c.Phase == "drain") {
				failing.Store(true)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	e.startCutover(id)
	v := e.migration(id).Migration.Flags.Verdict
	t.Logf("verdict with the store unreadable from the drain on: %q", v)
	if v == "GO" {
		t.Fatal("GO although the cutover could not read its databases")
	}
	failing.Store(false)
	e.abortAndClean(id, "Store read failure")
}

// TestEngineCleanupCheckFailureNotSuccess: cleanup checked whether the slot,
// publication and origin still existed and ignored a failed check, so it
// read as "already gone" and cleanup reported success with the slot still
// keeping WAL on the source.
func TestEngineCleanupCheckFailureNotSuccess(t *testing.T) {
	db := uniq("it_cleanfail")
	seedDB(t, db, 10)
	slot := uniq("upwell_cf_slot")
	exec1(t, srcConn, db, fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLE orders`, slot))
	exec1(t, srcConn, db, fmt.Sprintf(`SELECT pg_create_physical_replication_slot('%s', true)`, slot))
	exec1(t, dstConn, "defaultdb", fmt.Sprintf(`SELECT pg_replication_origin_create('%s')`, slot))
	t.Cleanup(func() {
		exec1(t, srcConn, db, fmt.Sprintf(`SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = '%s'`, slot))
		exec1(t, dstConn, "defaultdb", fmt.Sprintf(`SELECT pg_replication_origin_drop(roname) FROM pg_replication_origin WHERE roname = '%s'`, slot))
	})
	r, err := runner.New("local", filepath.Join(t.TempDir(), "units"))
	if err != nil {
		t.Fatal(err)
	}
	eng := pgcopydb.New(envStr("UPWELL_IT_PGCOPYDB", "pgcopydb"), r)
	spec := engine.DatabaseSpec{MigrationID: "cf", Source: db, Target: db, SourceConn: srcConn, TargetConn: dstConn,
		Instance: "cf-" + slot, SlotName: slot, OriginName: slot, RunDir: t.TempDir()}
	ctx := context.Background()
	for _, what := range []string{"pg_replication_slots", "pg_publication", "pg_replication_origin"} {
		restore := pg.SetFaultHook(func(sql string) error {
			if strings.Contains(sql, "count(*) FROM "+what+" WHERE") {
				return errInjected
			}
			return nil
		})
		done, err := eng.Cleanup(ctx, spec, engine.CleanupOptions{})
		restore()
		if err == nil {
			t.Fatalf("cleanup reported success although the check on %s failed (did: %v)", what, done)
		}
		t.Logf("check on %s failed: cleanup error %q after %v", what, err, done)
	}
	if _, err := eng.Cleanup(ctx, spec, engine.CleanupOptions{}); err != nil {
		t.Fatalf("clean cleanup: %v", err)
	}
	if n := q(t, srcConn, db, fmt.Sprintf(`SELECT count(*)::text FROM pg_replication_slots WHERE slot_name = '%s'`, slot)); n != "0" {
		t.Fatal("slot still present after a successful cleanup")
	}
}
