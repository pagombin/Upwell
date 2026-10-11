//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pagombin/upwell/internal/check"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/pgpack"
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
