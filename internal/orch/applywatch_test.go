package orch

import (
	"errors"
	"testing"
	"time"
)

func TestApplyWatchStall(t *testing.T) {
	t0 := time.Unix(1000, 0)
	after := 60 * time.Second
	var w applyWatch
	w = w.observe(t0, "0/100", 10, "0/100", after)
	if w.Stalled {
		t.Fatal("stalled on the first sample")
	}
	// Idle source: the origin does not move and nothing changed: not a stall.
	w = w.observe(t0.Add(5*time.Minute), "0/100", 10, "0/120", after)
	if w.Stalled {
		t.Fatal("an idle source reads as a stall")
	}
	// The source changes and the origin keeps up: not a stall.
	w = w.observe(t0.Add(6*time.Minute), "0/200", 50, "0/200", after)
	if w.Stalled || w.CounterAtChange != 50 {
		t.Fatalf("origin moved: %+v", w)
	}
	// The source changes, the origin does not, for less than the window.
	w = w.observe(t0.Add(6*time.Minute+30*time.Second), "0/200", 80, "0/300", after)
	if w.Stalled {
		t.Fatal("stalled before the window")
	}
	// ... and for the whole window: stalled, with the gap measured.
	w = w.observe(t0.Add(7*time.Minute), "0/200", 90, "0/300", after)
	if !w.Stalled || w.ChangesSince() != 40 || w.GapBytes != 0x100 {
		t.Fatalf("not stalled: %+v", w)
	}
	// A failed sample keeps the stall and records the error.
	w = w.failed(t0.Add(7*time.Minute+10*time.Second), errors.New("boom"))
	if !w.Stalled || w.Err == nil {
		t.Fatalf("failed sample: %+v", w)
	}
	// The origin moves again: recovered.
	w = w.observe(t0.Add(8*time.Minute), "0/300", 95, "0/300", after)
	if w.Stalled || w.Err != nil {
		t.Fatalf("not recovered: %+v", w)
	}
	// Nothing ever applied (F2): "" never moves while the source changes.
	var f applyWatch
	f = f.observe(t0, "", 0, "0/10", after)
	f = f.observe(t0.Add(2*time.Minute), "", 25, "0/90", after)
	if !f.Stalled {
		t.Fatal("an origin that never advanced is not a stall")
	}
}
