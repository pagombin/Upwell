package orch

import (
	"context"

	"github.com/pagombin/upwell/internal/store"
)

// chunkTracker records the key ranges a full verification found equal
// (D91), per verification run, so one interrupted by a restart resumes.
type chunkTracker struct {
	o   *Orchestrator
	key string
}

func (t chunkTracker) Done(table, lo, hi string) bool {
	var n int
	err := t.o.st.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM verify_chunks WHERE run_key=? AND table_name=? AND lo=? AND hi=?`, t.key, table, lo, hi).Scan(&n)
	// An unreadable record only means the range is compared again.
	return err == nil && n > 0
}

func (t chunkTracker) Record(table, lo, hi string, n int64) {
	t.o.st.DB.ExecContext(context.Background(), `INSERT OR REPLACE INTO verify_chunks(run_key, table_name, lo, hi, rows, created_at) VALUES (?,?,?,?,?,?)`, t.key, table, lo, hi, n, store.Now())
}
