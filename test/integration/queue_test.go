//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

type queueView struct {
	Running *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"running"`
	Entries []orch.QueueEntry `json:"entries"`
}

func (e *Env) queue() queueView {
	var q queueView
	e.do("GET", "/api/v1/queue", nil, &q, 200)
	return q
}

// TestQueueRunLock (overnight item 7, bug bash 10): one runner at a time; a
// second start is refused and can be queued; the queue survives a restart and
// can be reordered while a migration runs; when the lock frees only an entry
// with auto-start starts by itself.
func TestQueueRunLock(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	a, b, c := uniq("it_qa"), uniq("it_qb"), uniq("it_qc")
	for _, db := range []string{a, b, c} {
		seedDB(t, db, 200)
	}
	idA := e.defineMigration("Queue A", []string{a}, nil)
	idB := e.defineMigration("Queue B", []string{b}, nil)
	idC := e.defineMigration("Queue C", []string{c}, nil)
	for _, id := range []string{idA, idB, idC} {
		e.preflight(id)
	}
	e.do("POST", "/api/v1/migrations/"+idA+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	var out map[string]any
	if code := e.do("POST", "/api/v1/migrations/"+idB+"/start", map[string]any{"warnings_reviewed": true}, &out); code != 409 || !strings.Contains(out["message"].(string), "Queue A") {
		t.Fatalf("a second migration started while A runs: %d %v", code, out)
	}
	e.do("POST", "/api/v1/migrations/"+idB+"/queue", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.do("POST", "/api/v1/migrations/"+idC+"/queue", map[string]any{"warnings_reviewed": true, "auto_start": true}, nil, 200)
	if s := e.migration(idB).Migration.State; s != "queued" {
		t.Fatalf("B's state: %s", s)
	}
	e.do("PUT", "/api/v1/queue/order", map[string]any{"ids": []string{idC, idB}}, nil, 200)

	// Restart Upwell with A running and B, C queued.
	dir := e.Dir
	e.Close()
	e2 := newEnv(t, dir, orch.TestHooks{})
	q := e2.queue()
	if q.Running == nil || q.Running.ID != idA || len(q.Entries) != 2 || q.Entries[0].MigrationID != idC || q.Entries[1].MigrationID != idB || !q.Entries[0].AutoStart {
		t.Fatalf("queue after the restart: %+v", q)
	}
	e2.waitDBState(idA, a, 3*time.Minute, orch.DInSync)

	// A ends: C (auto-start) starts by itself; B (no auto-start) waits.
	e2.do("POST", "/api/v1/migrations/"+idA+"/abort", map[string]any{"confirm": "Queue A"}, nil, 200)
	e2.waitFor(2*time.Minute, "C to start from the queue", func() bool { return e2.migration(idC).Migration.Flags.Started })
	time.Sleep(3 * time.Second)
	if e2.migration(idB).Migration.Flags.Started {
		t.Fatal("B started without an operator although it has no auto-start")
	}
	if q := e2.queue(); q.Running == nil || q.Running.ID != idC || len(q.Entries) != 1 {
		t.Fatalf("queue after C started: %+v", q)
	}
	// C ends: B still waits for an operator, who starts it.
	e2.do("POST", "/api/v1/migrations/"+idC+"/abort", map[string]any{"confirm": "Queue C"}, nil, 200)
	time.Sleep(8 * time.Second)
	if e2.migration(idB).Migration.Flags.Started {
		t.Fatal("B started by itself")
	}
	e2.do("POST", "/api/v1/migrations/"+idB+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	if q := e2.queue(); q.Running == nil || q.Running.ID != idB || len(q.Entries) != 0 {
		t.Fatalf("queue after B started: %+v", q)
	}
	for _, x := range []struct{ id, name string }{{idA, "Queue A"}, {idC, "Queue C"}} {
		e2.cleanup(x.id, x.name)
	}
	e2.abortAndClean(idB, "Queue B")
}
