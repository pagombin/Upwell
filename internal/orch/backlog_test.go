package orch

import (
	"testing"
	"time"
)

func samples(step time.Duration, mib ...float64) []backlogSample {
	t0 := time.Unix(0, 0)
	var s []backlogSample
	for i, v := range mib {
		s = append(s, backlogSample{at: t0.Add(time.Duration(i) * step), bytes: v * (1 << 20)})
	}
	return s
}

func TestBacklogGrowing(t *testing.T) {
	min := time.Minute
	// A steady load: the backlog rises and dips around 1 GiB. The old rule
	// fired on every five rising samples; this one stays quiet.
	saw := samples(30*time.Second, 1000, 1010, 1020, 1030, 1040, 1050, 990, 1000, 1010, 1020, 1030, 1040, 980, 995)
	if backlogGrowing(saw, false) {
		t.Error("fired on a steady sawtooth")
	}
	growing := samples(min, 500, 560, 620, 700, 780, 850)
	if !backlogGrowing(growing, false) {
		t.Error("did not fire on 350 MiB of growth in 5 minutes")
	}
	if backlogGrowing(samples(min, 10, 20, 30), false) {
		t.Error("fired on less than five minutes of readings")
	}
	if backlogGrowing(samples(min, 40000, 40010, 40020, 40030, 40040, 40070), false) {
		t.Error("fired on 70 MiB of growth on a 40 GiB backlog (under a tenth)")
	}
	// Hysteresis: once firing it stays on while the backlog still grows,
	// even slowly, and clears once it shrinks over the window.
	if !backlogGrowing(samples(min, 900, 905, 910, 915, 920, 925), true) {
		t.Error("cleared while the backlog still grew")
	}
	if backlogGrowing(samples(min, 900, 950, 920, 890, 880, 870), true) {
		t.Error("stayed on while the backlog shrank over the window")
	}
	if backlogGrowing(samples(min, 10, 11, 12, 13, 14, 15), true) {
		t.Error("stayed on under the in-sync size")
	}
}
