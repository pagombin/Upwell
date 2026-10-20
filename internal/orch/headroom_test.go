package orch

import (
	"testing"
	"time"
)

// D113: the slot's WAL headroom (safe_wal_size) shrinking towards zero is
// the warning before an invalidated slot, which forces a restart from zero.
func TestHeadroomRunsOut(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	at := func(min int, gib float64) headroomSample {
		return headroomSample{at: t0.Add(time.Duration(min) * time.Minute), bytes: gib * (1 << 30)}
	}
	cases := []struct {
		name    string
		samples []headroomSample
		hours   float64
		ok      bool
	}{
		{"shrinking 1 GiB a minute, 30 GiB left", []headroomSample{at(0, 35), at(1, 34), at(5, 30)}, 0.5, true},
		{"steady", []headroomSample{at(0, 30), at(5, 30)}, 0, false},
		{"growing (the apply catches up)", []headroomSample{at(0, 20), at(5, 30)}, 0, false},
		{"too short a window to judge", []headroomSample{at(0, 30), at(1, 10)}, 0, false},
		{"one sample", []headroomSample{at(0, 30)}, 0, false},
	}
	for _, c := range cases {
		h, ok := headroomRunsOut(c.samples)
		if ok != c.ok || (ok && (h < c.hours*0.99 || h > c.hours*1.01)) {
			t.Errorf("%s: %.3f h, %v; want %.3f h, %v", c.name, h, ok, c.hours, c.ok)
		}
	}
}
