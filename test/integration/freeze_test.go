//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/orch"
)

// appWriter writes as an application would (its own application name, so
// the writers check and the freeze treat it as a client), reconnecting after
// its session is ended. rogue sessions turn read-only off for themselves.
type appWriter struct {
	ok, failed atomic.Int64
	stop       chan struct{}
	done       chan struct{}
	once       sync.Once
	mu         sync.Mutex
	lastErr    string
}

func startAppWriter(t *testing.T, db string, rogue bool, waitFrozen bool) *appWriter {
	w := &appWriter{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		ctx := context.Background()
		var c *pgx.Conn
		defer func() {
			if c != nil {
				c.Close(ctx)
			}
		}()
		for {
			select {
			case <-w.stop:
				return
			default:
			}
			if waitFrozen && frozenSetting(t, db) != "on" {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			if c == nil {
				var err error
				c, err = pgx.Connect(ctx, fmt.Sprintf("postgres://doadmin:%s@127.0.0.1:%d/%s?sslmode=disable&application_name=shop-api", password, srcPort, db))
				if err != nil {
					time.Sleep(200 * time.Millisecond)
					continue
				}
				if rogue {
					c.Exec(ctx, `SET default_transaction_read_only = off`)
				}
			}
			if _, err := c.Exec(ctx, `INSERT INTO orders(customer, amount, note) VALUES (7, 7, 'app')`); err != nil {
				w.failed.Add(1)
				w.mu.Lock()
				w.lastErr = err.Error()
				w.mu.Unlock()
				if c.IsClosed() || strings.Contains(err.Error(), "terminat") || strings.Contains(err.Error(), "closed") {
					c.Close(ctx)
					c = nil
				}
			} else {
				w.ok.Add(1)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return w
}

func (w *appWriter) Stop() { w.once.Do(func() { close(w.stop); <-w.done }) }

// frozenSetting is the database's own default_transaction_read_only ("" if unset).
func frozenSetting(t *testing.T, db string) string {
	return q(t, srcConn, "defaultdb", fmt.Sprintf(`SELECT COALESCE(max(split_part(s, '=', 2)), '') FROM pg_db_role_setting r, unnest(r.setconfig) s
		WHERE r.setdatabase = (SELECT oid FROM pg_database WHERE datname = '%s') AND r.setrole = 0 AND s LIKE 'default_transaction_read_only=%%'`, db))
}

func newSessionCanWrite(t *testing.T, db string) error {
	ctx := context.Background()
	c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://doadmin:%s@127.0.0.1:%d/%s?sslmode=disable&application_name=shop-api", password, srcPort, db))
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	_, err = c.Exec(ctx, `INSERT INTO orders(customer, amount, note) VALUES (8, 8, 'probe')`)
	return err
}

func (e *Env) auditActions(target string) []string {
	var out []string
	rows, err := e.Store.DB.Query(`SELECT action FROM audit_log WHERE target=? ORDER BY seq`, target)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		rows.Scan(&a)
		out = append(out, a)
	}
	return out
}

func (e *Env) freezeCutover(id string) *orch.Operation {
	e.t.Helper()
	var pv orch.FreezePreview
	e.do("GET", "/api/v1/migrations/"+id+"/cutover/freeze", nil, &pv, 200)
	return e.cutoverOp(id, map[string]any{"write_freeze": true, "freeze_confirm": pv.Confirm})
}

// TestWriteFreezeGO (overnight item 1): preview, a refused confirmation,
// the freeze ending a running application's writes, GO with the freeze kept,
// the undo button, and the audit trail.
func TestWriteFreezeGO(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_freeze")
	id := streamingMigration(t, e, "Write freeze", db)
	app := startAppWriter(t, db, false, false)
	defer app.Stop()
	time.Sleep(time.Second)

	var pv orch.FreezePreview
	e.do("GET", "/api/v1/migrations/"+id+"/cutover/freeze", nil, &pv, 200)
	t.Logf("preview: confirm %q, available %v, %d database(s)", pv.Confirm, pv.Available, len(pv.Databases))
	if !pv.Available || len(pv.Databases) != 1 || !strings.Contains(pv.Note, "not a lock") {
		t.Fatalf("preview: %+v", pv)
	}
	d0 := pv.Databases[0]
	t.Logf("apply: %s; undo: %s; sessions: %+v", strings.Join(d0.ApplySQL, " "), strings.Join(d0.UndoSQL, " "), d0.Sessions)
	found := false
	for _, s := range d0.Sessions {
		if s.App == "shop-api" {
			found = true
		}
		if strings.HasPrefix(s.App, "upwell") || strings.HasPrefix(s.App, "pgcopydb") {
			t.Errorf("the preview would end Upwell's or the engine's session: %+v", s)
		}
	}
	if !found || !strings.Contains(d0.ApplySQL[0], "SET default_transaction_read_only = on") {
		t.Fatalf("the preview does not show the application session or the statement")
	}
	if frozenSetting(t, db) != "" {
		t.Fatal("the preview changed something")
	}
	// A wrong confirmation is refused before anything happens.
	if c := e.do("POST", "/api/v1/migrations/"+id+"/cutover", map[string]any{"write_freeze": true, "freeze_confirm": "wrong"}, nil); c != 409 {
		t.Fatalf("wrong confirmation: %d", c)
	}

	op := e.freezeCutover(id)
	for _, s := range op.Steps {
		t.Logf("step %-10s %-8s %s", s.Key, s.State, s.Detail)
	}
	m := e.migration(id).Migration
	if op.State != "done" || m.Flags.Verdict != "GO" {
		t.Fatalf("expected GO with the freeze: %s %q", m.Flags.Verdict, op.Error)
	}
	app.Stop()
	t.Logf("application: %d writes ok, %d refused (last: %s)", app.ok.Load(), app.failed.Load(), app.lastErr)
	if app.failed.Load() == 0 || !strings.Contains(app.lastErr, "read-only") {
		t.Errorf("the application's writes were not stopped by the freeze")
	}
	if f := m.Flags.Freeze; f == nil || f.State != "applied" || f.Databases[0].Terminated == 0 {
		t.Fatalf("freeze state after GO: %+v", f)
	}
	if frozenSetting(t, db) != "on" {
		t.Fatal("the freeze is not applied after GO")
	}
	// The undo button.
	e.do("POST", "/api/v1/migrations/"+id+"/cutover/unfreeze", nil, nil, 200)
	if f := e.migration(id).Migration.Flags.Freeze; f.State != "undone" {
		t.Fatalf("undo: %+v", f)
	}
	if s := frozenSetting(t, db); s != "" {
		t.Fatalf("setting after undo: %q", s)
	}
	if err := newSessionCanWrite(t, db); err != nil {
		t.Fatalf("the source is not writable after undo: %v", err)
	}
	acts := strings.Join(e.auditActions(id), ",")
	if !strings.Contains(acts, "cutover.write_freeze,") || !strings.Contains(acts, "cutover.write_freeze_undo") {
		t.Fatalf("audit trail: %s", acts)
	}
	e.cleanup(id, "Write freeze")
}

// TestWriteFreezeOverrideDetected (bug bash 4): a client turns read-only off
// for itself and writes: the writers check catches it, the cutover stops
// and the freeze is undone automatically.
func TestWriteFreezeOverrideDetected(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_frzov")
	id := streamingMigration(t, e, "Freeze override", db)
	rogue := startAppWriter(t, db, true, true)
	defer rogue.Stop()
	op := e.freezeCutover(id)
	rogue.Stop()
	t.Logf("operation %s: %s; rogue wrote %d rows", op.State, op.Error, rogue.ok.Load())
	if op.State != "failed" || !strings.Contains(op.Error, "writes have not stopped") {
		t.Fatalf("the overriding writer was not detected: %s %q", op.State, op.Error)
	}
	m := e.migration(id).Migration
	if m.Flags.Verdict == "GO" {
		t.Fatal("GO with a writer that overrode the freeze")
	}
	if f := m.Flags.Freeze; f == nil || f.State != "undone" {
		t.Fatalf("the freeze was not undone after the cutover stopped: %+v", f)
	}
	if s := frozenSetting(t, db); s != "" {
		t.Fatalf("setting after the automatic undo: %q", s)
	}
	e.abortAndClean(id, "Freeze override")
}

// TestWriteFreezeAbortAndRestart (bug bash 4): the freeze is undone when the
// cutover is aborted, and when Upwell is killed with the freeze applied.
func TestWriteFreezeAbortAndRestart(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	setSetting(t, e, "cutover_write_sample_seconds", 60)
	db := uniq("it_frzab")
	id := streamingMigration(t, e, "Freeze abort", db)
	start := func(env *Env) {
		var pv orch.FreezePreview
		env.do("GET", "/api/v1/migrations/"+id+"/cutover/freeze", nil, &pv, 200)
		var rd struct {
			Ready bool `json:"ready"`
		}
		env.waitFor(5*time.Minute, "the readiness gate", func() bool {
			env.do("GET", "/api/v1/migrations/"+id+"/cutover/readiness", nil, &rd, 200)
			return rd.Ready
		})
		env.do("POST", "/api/v1/migrations/"+id+"/cutover", map[string]any{"write_freeze": true, "freeze_confirm": pv.Confirm}, nil, 202)
		env.waitFor(time.Minute, "the freeze to be applied", func() bool { return frozenSetting(t, db) == "on" })
	}
	// Abort during the writers step.
	start(e)
	e.do("POST", "/api/v1/migrations/"+id+"/cutover/abort", nil, nil, 200)
	if f := e.migration(id).Migration.Flags.Freeze; f == nil || f.State != "undone" || !strings.Contains(f.UndoReason, "aborted") {
		t.Fatalf("abort did not undo the freeze: %+v", f)
	}
	if s := frozenSetting(t, db); s != "" {
		t.Fatalf("setting after abort: %q", s)
	}
	// Kill Upwell with the freeze applied.
	time.Sleep(2 * time.Second)
	start(e)
	dir := e.Dir
	e.Close()
	if frozenSetting(t, db) != "on" {
		t.Fatal("the freeze went away with the process; the restart test proves nothing")
	}
	e2 := newEnv(t, dir, orch.TestHooks{})
	e2.waitFor(time.Minute, "the restart to undo the freeze", func() bool { return frozenSetting(t, db) == "" })
	f := e2.migration(id).Migration.Flags.Freeze
	t.Logf("after restart: %+v", f)
	if f == nil || f.State != "undone" || !strings.Contains(f.UndoReason, "restarted") {
		t.Fatalf("freeze state after restart: %+v", f)
	}
	if err := newSessionCanWrite(t, db); err != nil {
		t.Fatalf("the source is not writable after the restart: %v", err)
	}
	e2.do("POST", "/api/v1/migrations/"+id+"/cutover/abort", nil, nil)
	e2.abortAndClean(id, "Freeze abort")
}
