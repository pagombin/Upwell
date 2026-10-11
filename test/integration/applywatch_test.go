//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/orch"
)

func (e *Env) firing(rule, scope string) *orch.Alert {
	var out []orch.Alert
	e.do("GET", "/api/v1/alerts?state=firing", nil, &out, 200)
	for i := range out {
		if out[i].Rule == rule && out[i].Scope == scope {
			return &out[i]
		}
	}
	return nil
}

func (e *Env) condition(id, key, db string) *orch.Condition {
	var rd struct {
		Conditions []orch.Condition `json:"conditions"`
	}
	e.do("GET", "/api/v1/migrations/"+id+"/cutover/readiness", nil, &rd, 200)
	for i := range rd.Conditions {
		if rd.Conditions[i].Key == key && rd.Conditions[i].Database == db {
			return &rd.Conditions[i]
		}
	}
	return nil
}

// TestApplyStallDetected (preflight finding 10, E2): the target stops
// applying (a lock it waits on, so pgcopydb logs nothing) while the source
// keeps changing. The watchdog must alert and hold the cutover, then clear
// when apply resumes.
func TestApplyStallDetected(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	setSetting(t, e, "apply_stall_seconds", 20)
	db := uniq("it_stall")
	id := streamingMigration(t, e, "Apply stall", db)
	e.waitFor(2*time.Minute, "the apply condition to be measured", func() bool {
		c := e.condition(id, "apply", db)
		return c != nil && c.OK
	})
	ctx := context.Background()
	lock, err := pgx.Connect(ctx, fmt.Sprintf("postgres://doadmin:%s@127.0.0.1:%d/%s?sslmode=disable&application_name=dba-maintenance", password, dstPort, db))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close(ctx)
	if _, err := lock.Exec(ctx, `BEGIN; LOCK TABLE orders IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	w := startWriter(t, db)
	defer w.Stop()
	e.waitFor(3*time.Minute, "the apply_stalled alert", func() bool { return e.firing("apply_stalled", db) != nil })
	a := e.firing("apply_stalled", db)
	t.Logf("alert %s: %s", a.Severity, a.Message)
	c := e.condition(id, "apply", db)
	t.Logf("condition ok=%v: %s", c.OK, c.Detail)
	if c.OK || a.Severity != "critical" {
		t.Fatalf("the cutover is not held: condition %v, severity %s", c.OK, a.Severity)
	}
	if _, err := lock.Exec(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	w.Stop()
	e.waitFor(3*time.Minute, "the alert to resolve", func() bool { return e.firing("apply_stalled", db) == nil })
	e.waitFor(time.Minute, "the apply condition to recover", func() bool { return e.condition(id, "apply", db).OK })
	t.Logf("recovered: %s", e.condition(id, "apply", db).Detail)
	e.abortAndClean(id, "Apply stall")
}
