//go:build integration

package integration

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/orch"
)

// TestRestartWithLeftoverEngineSession reproduces the droplet failure
// "dropping public.stock6 on the target: canceling statement due to
// statement timeout": after the engine died, its server session on the
// target (an index build or COPY keeps running without its client) held a
// lock, and the reset's DROP TABLE waited until its 60 s statement timeout.
// The target database also holds an object of its own (on Advanced, the
// doadmin.aries_* table), so the reset drops table by table instead of
// dropping the database.
func TestRestartWithLeftoverEngineSession(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_restart")
	seedDB(t, db, 2000)
	id := e.defineMigration("Restart leftover", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	exec1(t, dstConn, db, `CREATE TABLE target_only_health(id int)`)
	ctx := context.Background()
	held, err := pgx.Connect(ctx, fmt.Sprintf("postgres://doadmin:%s@127.0.0.1:%d/%s?sslmode=disable&application_name=pgcopydb%%5B4242%%5D%%20create%%20index", password, dstPort, db))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close(ctx)
	if _, err := held.Exec(ctx, `BEGIN; LOCK TABLE public.orders IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var out map[string]any
	code := e.do("POST", "/api/v1/migrations/"+id+"/databases/"+db+"/restart", map[string]any{"confirm": db}, &out)
	t.Logf("restart answered %d after %s: %v", code, time.Since(start).Round(time.Second), out)
	if code != 200 {
		t.Fatalf("restart from zero failed (%d) while a leftover engine session held a lock: %v", code, out)
	}
	if n := q(t, dstConn, db, `SELECT count(*) FROM pg_stat_activity WHERE application_name LIKE 'pgcopydb[4242]%'`); n != "0" {
		t.Fatalf("the leftover engine session is still there (%s)", n)
	}
	if n := q(t, dstConn, db, `SELECT count(*) FROM pg_class WHERE relname='target_only_health'`); n != "1" {
		t.Fatal("the reset dropped an object that did not come from the source")
	}
	defer func() {
		if t.Failed() {
			out, _ := exec.Command("bash", "-c", "grep -hE 'ERROR|FATAL|WARN' "+e.Cfg.RunsDir()+"/*/engine.log | tail -25").CombinedOutput()
			t.Logf("engine log:\n%s", out)
		}
	}()
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	e.abortAndClean(id, "Restart leftover")
}
