package orch

import "time"

// headroomSample is one reading of a slot's safe_wal_size: how much more
// WAL the source may write before PostgreSQL invalidates the slot
// (max_slot_wal_keep_size).
type headroomSample struct {
	at    time.Time
	bytes float64
}

// headroomRunsOut estimates, from readings at least two minutes apart, in
// how many hours the headroom reaches zero at its current rate. It answers
// only while the headroom shrinks (D113).
func headroomRunsOut(s []headroomSample) (float64, bool) {
	if len(s) < 2 {
		return 0, false
	}
	last := s[len(s)-1]
	first := s[0]
	span := last.at.Sub(first.at).Seconds()
	if span < 120 {
		return 0, false
	}
	rate := (first.bytes - last.bytes) / span
	if rate <= 0 {
		return 0, false
	}
	if last.bytes <= 0 {
		return 0, true
	}
	return last.bytes / rate / 3600, true
}
