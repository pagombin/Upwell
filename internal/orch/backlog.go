package orch

import "time"

// backlogSample is one reading of a database's change backlog.
type backlogSample struct {
	at    time.Time
	bytes float64
}

// Backlog growth alert thresholds (D115).
const (
	backlogWindow     = 10 * time.Minute
	backlogMinSpan    = 5 * time.Minute
	backlogMinGrowth  = 64 << 20 // bytes over the window
	backlogInSyncSize = 16 << 20 // the in-sync threshold (D109)
)

// backlogGrowth returns how much the backlog grew from the oldest reading in
// s to the newest, and over how long. ok is false until the readings span
// backlogMinSpan.
func backlogGrowth(s []backlogSample) (growth float64, span time.Duration, ok bool) {
	if len(s) < 2 {
		return 0, 0, false
	}
	first, last := s[0], s[len(s)-1]
	span = last.at.Sub(first.at)
	if span < backlogMinSpan {
		return 0, span, false
	}
	return last.bytes - first.bytes, span, true
}

// backlogGrowing decides the backlog_growing alert with hysteresis. It fires
// when the backlog grew by backlogMinGrowth and by a tenth over the window,
// and, once firing, stays on until the backlog no longer grows over the
// window or falls under the in-sync size. Under a steady load the backlog
// rises and dips from one sample to the next; the earlier rule (five rising
// samples in a row) fired and cleared every minute or so.
func backlogGrowing(s []backlogSample, firing bool) bool {
	growth, _, ok := backlogGrowth(s)
	if !ok {
		return firing
	}
	last := s[len(s)-1].bytes
	if firing {
		return growth > 0 && last >= backlogInSyncSize
	}
	return growth >= backlogMinGrowth && growth >= s[0].bytes/10
}
