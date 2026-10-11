//go:build integration

package integration

import (
	"strconv"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

// TestWatchedTables (overnight item 9): an operator picks a table; Upwell
// counts it exactly on both sides while streaming, and the counts meet once
// the target has caught up.
func TestWatchedTables(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_watch")
	id := streamingMigration(t, e, "Watched", db)
	var tables []map[string]any
	e.do("GET", "/api/v1/migrations/"+id+"/databases/"+db+"/tables", nil, &tables, 200)
	found := false
	for _, x := range tables {
		if x["schema"] == "public" && x["table"] == "orders" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the table picker does not list public.orders: %v", tables)
	}
	if c := e.do("POST", "/api/v1/migrations/"+id+"/watched", map[string]any{"database": db, "schema": "public", "table": "nope"}, nil); c < 400 {
		t.Fatalf("watching a missing table was accepted: %d", c)
	}
	var wt orch.WatchedTable
	e.do("POST", "/api/v1/migrations/"+id+"/watched", map[string]any{"database": db, "schema": "public", "table": "orders"}, &wt, 201)
	// One transaction of 500 rows: the engine applies a few transactions a
	// second (F11), so a continuous writer would keep the target minutes behind.
	exec1(t, srcConn, db, `INSERT INTO orders(customer, amount, note) SELECT g, 1, 'watched' FROM generate_series(1, 500) g`)
	var ws []orch.WatchedTable
	e.waitFor(3*time.Minute, "the first exact counts", func() bool {
		e.do("GET", "/api/v1/migrations/"+id+"/watched", nil, &ws, 200)
		return len(ws) == 1 && ws[0].CountedAt != nil && ws[0].SourceRows != nil && ws[0].TargetRows != nil
	})
	t.Logf("first count: source %d, target %d (%s)", *ws[0].SourceRows, *ws[0].TargetRows, ws[0].CountError)
	src := q(t, srcConn, db, "SELECT count(*)::text FROM orders")
	e.waitFor(4*time.Minute, "the counts to meet", func() bool {
		e.do("GET", "/api/v1/migrations/"+id+"/watched", nil, &ws, 200)
		return ws[0].SourceRows != nil && ws[0].TargetRows != nil && *ws[0].SourceRows == *ws[0].TargetRows && formatInt(*ws[0].SourceRows) == src
	})
	t.Logf("caught up: %d rows on both sides", *ws[0].SourceRows)
	e.do("DELETE", "/api/v1/migrations/"+id+"/watched/"+wt.ID, nil, nil, 200)
	e.abortAndClean(id, "Watched")
}

func formatInt(n int64) string { return strconv.FormatInt(n, 10) }
