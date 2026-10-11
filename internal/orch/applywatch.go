package orch

import (
	"time"

	"github.com/pagombin/upwell/internal/pg"
)

// applyWatch is the independent apply-progress watchdog for one database
// (preflight finding 10, E2). pgcopydb's exit status and log are not proof
// of apply: Phase 0 showed it applying nothing, logging nothing and
// reporting success (F2). The watchdog compares the target's replication
// origin with the source: when the source's user tables keep changing and
// the origin does not move for apply_stall_seconds, apply has stalled.
type applyWatch struct {
	Origin          string    // target origin progress ("" before the first applied transaction)
	OriginChangedAt time.Time // when Origin last moved (or was first seen)
	CounterAtChange int64     // source row-change counter when Origin last moved
	Counter         int64     // source row-change counter now
	ConfirmedFlush  string    // the source slot's confirmed_flush_lsn
	GapBytes        int64     // confirmed_flush_lsn minus the origin
	At              time.Time // last complete sample
	Err             error     // the latest sample's error, if it failed
	ErrAt           time.Time
	Stalled         bool
}

// observe folds one complete sample into the watch.
func (w applyWatch) observe(now time.Time, origin string, counter int64, flush string, stallAfter time.Duration) applyWatch {
	if w.At.IsZero() || origin != w.Origin || counter < w.CounterAtChange {
		// First sample, the origin moved, or the source's statistics were
		// reset: start a new window.
		w.Origin, w.OriginChangedAt, w.CounterAtChange = origin, now, counter
	}
	w.Counter, w.ConfirmedFlush, w.At, w.Err = counter, flush, now, nil
	w.GapBytes = 0
	if origin != "" && flush != "" {
		w.GapBytes = pg.LSNDiff(flush, origin)
	}
	w.Stalled = w.Counter > w.CounterAtChange && now.Sub(w.OriginChangedAt) >= stallAfter
	return w
}

// failed records a sample that could not be taken.
func (w applyWatch) failed(now time.Time, err error) applyWatch {
	w.Err, w.ErrAt = err, now
	return w
}

// ChangesSince is the number of source row changes the origin has not moved for.
func (w applyWatch) ChangesSince() int64 { return w.Counter - w.CounterAtChange }

// ApplyWatch returns the watchdog's state for a database.
func (c *collector) ApplyWatch(dbID string) (applyWatch, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w, ok := c.apply[dbID]
	return w, ok
}

func (c *collector) setApplyWatch(dbID string, f func(applyWatch) applyWatch) applyWatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := f(c.apply[dbID])
	c.apply[dbID] = w
	return w
}
