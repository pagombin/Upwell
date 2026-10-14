package orch

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/pg"
)

var failSlotBusy sync.Map // database ID -> true while a release runs

// releaseFailedCopySlot drops the replication slot (and its publication and
// origin) of a database whose base copy failed (D81). A base copy cannot
// resume, and a restart from zero cleans these up and creates a new slot at a
// new snapshot, so until then the slot only makes the source keep WAL: on the
// droplet, 74 GiB and growing while the database waited in Restart required.
func (o *Orchestrator) releaseFailedCopySlot(ctx context.Context, m Migration, d Database) {
	if _, busy := failSlotBusy.LoadOrStore(d.ID, true); busy {
		return
	}
	defer failSlotBusy.Delete(d.ID)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	spec, err := o.dbSpec(ctx, m, d)
	if err != nil {
		return
	}
	var held float64
	if c, err := pg.Connect(ctx, spec.SourceConn.WithDB(d.SourceName), 20*time.Second); err == nil {
		var n int
		err = pg.QueryRow(ctx, c, `SELECT count(*), COALESCE(sum(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)), 0)::float8 FROM pg_replication_slots WHERE slot_name = $1`, d.SlotName).Scan(&n, &held)
		c.Close(ctx)
		if err != nil || n == 0 {
			return
		}
	} else {
		return
	}
	// Under the migration's lock, which a restart from zero also takes, and
	// only if the database still waits for one (retried later when busy).
	// The supervision tick that saw the failure holds it for a moment.
	unlock, ok := o.tryLock(m.ID)
	for wait := time.Now().Add(time.Minute); !ok && time.Now().Before(wait); {
		time.Sleep(2 * time.Second)
		unlock, ok = o.tryLock(m.ID)
	}
	if !ok {
		return
	}
	defer unlock()
	dbs, err := o.ListDatabases(ctx, m.ID)
	if err != nil {
		return
	}
	still := false
	for _, x := range dbs {
		if x.ID == d.ID && x.State == DRestartRequired && !x.BaseCopyDone {
			still = true
		}
	}
	if !still {
		return
	}
	did, err := o.eng.Cleanup(ctx, spec, engine.CleanupOptions{})
	if err != nil {
		o.logf("warn", "orchestrator", m.ID, d.SourceName, "", "dropping the failed base copy's slot: %v", err)
		return
	}
	o.Event(ctx, m.ID, d.SourceName, "slot_released", "info", fmt.Sprintf("Dropped %s's replication slot %s, which held %s of source WAL: a failed base copy cannot resume, and Restart from zero creates a new slot (%v)", d.SourceName, d.SlotName, humanSize(int64(held)), did), nil)
}
