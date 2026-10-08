//go:build integration

package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"
)

// TestApplyThroughput measures how fast the engine applies streamed changes on
// this machine: one large transaction, then many small ones. It records the
// numbers (finding F8 covers receive; this covers apply) and fails only if a
// batch never arrives.
func TestApplyThroughput(t *testing.T) {
	if os.Getenv("UPWELL_MEASURE") == "" {
		t.Skip("a measurement, not a check: run with UPWELL_MEASURE=1 (about 15 minutes)")
	}
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_apply_rate")
	seedDB(t, db, 1000)
	exec1(t, srcConn, db, `CREATE TABLE sink(id bigserial PRIMARY KEY, a int, b text, c timestamptz DEFAULT now())`)
	id := e.defineMigration("Apply throughput", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	time.Sleep(5 * time.Second)

	measure := func(label string, rows int, load func()) {
		start := time.Now()
		load()
		loaded := time.Since(start)
		want := q(t, srcConn, db, "SELECT count(*)::text FROM sink")
		lastLog := time.Now()
		e.waitFor(20*time.Minute, label+" to reach the target", func() bool {
			got := q(t, dstConn, db, "SELECT count(*)::text FROM sink")
			if time.Since(lastLog) > time.Minute {
				t.Logf("  %s: %s of %s rows on the target after %s", label, got, want, time.Since(start).Round(time.Second))
				lastLog = time.Now()
			}
			return got == want
		})
		took := time.Since(start)
		t.Logf("%s: %d rows written on the source in %s, all on the target after %s (%.0f rows/s applied)", label, rows, loaded.Round(time.Millisecond), took.Round(time.Second), float64(rows)/took.Seconds())
	}
	measure("one transaction of 20000 rows", 20000, func() {
		exec1(t, srcConn, db, `INSERT INTO sink(a, b) SELECT g, md5(g::text) FROM generate_series(1, 20000) g`)
	})
	measure("3000 single-row transactions", 3000, func() {
		ctx := context.Background()
		c, err := pg.Connect(ctx, srcConn.WithDB(db), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(ctx)
		for i := 0; i < 3000; i++ {
			if _, err := c.Exec(ctx, `INSERT INTO sink(a, b) VALUES ($1, 'single')`, i); err != nil {
				t.Fatal(err)
			}
		}
	})
	e.abortAndClean(id, "Apply throughput")
}
