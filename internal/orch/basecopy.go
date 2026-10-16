package orch

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pagombin/upwell/internal/store"
)

// D102: how long each database's base copy took, from the launch that
// started it (a base copy cannot resume, only restart) to the moment the
// engine enabled the apply.

// baseCopyStartedAt is when the latest launch of the database started, in
// milliseconds, or 0.
func (o *Orchestrator) baseCopyStartedAt(ctx context.Context, dbID string) int64 {
	var ts int64
	o.st.DB.QueryRowContext(ctx, `SELECT started_at FROM attempts WHERE database_id=? AND kind='start' ORDER BY number DESC LIMIT 1`, dbID).Scan(&ts)
	return ts
}

func baseCopyDoneMessage(name string, took time.Duration, bytes int64) string {
	if took <= 0 {
		return "Base copy of " + name + " finished; streaming changes"
	}
	s := fmt.Sprintf("Base copy of %s finished in %s", name, took.Round(time.Second))
	if bytes > 0 {
		s += fmt.Sprintf(" (%.1f GiB on the source, about %.1f MiB/s)", float64(bytes)/(1<<30), float64(bytes)/(1<<20)/took.Seconds())
	}
	return s + "; streaming changes"
}

// baseCopyDone records the end of a database's base copy.
func (o *Orchestrator) baseCopyDone(ctx context.Context, m Migration, d Database) {
	var took time.Duration
	if st := o.baseCopyStartedAt(ctx, d.ID); st > 0 {
		took = time.Duration(store.Now()-st) * time.Millisecond
	}
	o.Event(ctx, m.ID, d.SourceName, "base_copy_done", "info", baseCopyDoneMessage(d.SourceName, took, d.SizeBytes),
		map[string]any{"duration_ms": took.Milliseconds(), "source_bytes": d.SizeBytes})
}

// BaseCopyDurations returns, for each database of the migration, how long
// its latest finished base copy took (milliseconds), from its event.
func (o *Orchestrator) BaseCopyDurations(ctx context.Context, migID string) map[string]int64 {
	out := map[string]int64{}
	rows, err := o.st.DB.QueryContext(ctx, `SELECT COALESCE(database,''), COALESCE(data,'{}') FROM events WHERE migration_id=? AND type='base_copy_done' ORDER BY seq`, migID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var db, data string
		if rows.Scan(&db, &data) != nil {
			continue
		}
		var v struct {
			Ms int64 `json:"duration_ms"`
		}
		if json.Unmarshal([]byte(data), &v) == nil && v.Ms > 0 {
			out[db] = v.Ms
		}
	}
	return out
}
