package orch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/pgpack"
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
	if spec, err := o.dbSpec(ctx, m, d); err == nil {
		o.tidyTarget(ctx, m, d, spec)
	}
}

// tidyTarget fixes what the schema copy got wrong on the target (D112): it
// drops the engine's own publication, which older builds restored there, and
// gives each table the source's replica identity, which pgcopydb leaves out
// when it builds the indexes itself. Both are needed before the target can
// publish changes of its own, and the first lets it UPDATE and DELETE rows
// of tables whose identity was lost.
func (o *Orchestrator) tidyTarget(ctx context.Context, m Migration, d Database, spec engine.DatabaseSpec) {
	tc, err := pg.Connect(ctx, spec.TargetConn.WithDB(d.TargetName), 30*time.Second)
	if err != nil {
		o.logf("warn", "orchestrator", m.ID, d.SourceName, "", "tidying the target after the copy: %v", err)
		return
	}
	defer tc.Close(ctx)
	var had bool
	if err := pg.QueryRow(ctx, tc, `SELECT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = $1)`, d.SlotName).Scan(&had); err == nil && had {
		if _, err := tc.Exec(ctx, `DROP PUBLICATION IF EXISTS `+pg.QuoteIdent(d.SlotName)); err != nil {
			o.logf("warn", "orchestrator", m.ID, d.SourceName, "", "dropping the engine's publication %s on the target: %v", d.SlotName, err)
		} else {
			o.Event(ctx, m.ID, d.SourceName, "target_tidied", "info", fmt.Sprintf("%s: dropped the engine's own publication %s, which the schema copy had restored on the target", d.SourceName, d.SlotName), nil)
		}
	}
	sc, err := pg.Connect(ctx, spec.SourceConn.WithDB(d.SourceName), 30*time.Second)
	if err != nil {
		o.logf("warn", "orchestrator", m.ID, d.SourceName, "", "reading the source's replica identities: %v", err)
		return
	}
	defer sc.Close(ctx)
	done, err := pgpack.CopyReplicaIdentities(ctx, sc, tc)
	if err != nil {
		o.logf("warn", "orchestrator", m.ID, d.SourceName, "", "copying replica identities to the target: %v", err)
	}
	if len(done) > 0 {
		o.Event(ctx, m.ID, d.SourceName, "target_tidied", "info", fmt.Sprintf("%s: gave %d target table(s) the source's replica identity: %s", d.SourceName, len(done), strings.Join(limitStrings(done, 5), "; ")), map[string]any{"replica_identities": done})
	}
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
