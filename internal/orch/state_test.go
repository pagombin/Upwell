package orch

import "testing"

func db(state string) Database { return Database{Include: true, State: state} }

func TestDeriveState(t *testing.T) {
	cases := []struct {
		name string
		f    Flags
		dbs  []Database
		pf   bool
		perm bool
		want string
	}{
		{"new", Flags{}, nil, false, false, MDraft},
		{"preflight passed", Flags{}, []Database{db(DPending)}, true, true, MReady},
		{"no permission yet", Flags{}, []Database{db(DPending)}, true, false, MDraft},
		{"copying", Flags{Started: true}, []Database{db(DBaseCopy), db(DPending)}, true, true, MRunning},
		{"all in sync", Flags{Started: true}, []Database{db(DInSync), db(DInSync)}, true, true, MStreaming},
		{"one failed", Flags{Started: true}, []Database{db(DInSync), db(DFailed)}, true, true, MNeedsAttention},
		{"restart required", Flags{Started: true}, []Database{db(DRestartRequired)}, true, true, MNeedsAttention},
		{"cutover running", Flags{Started: true, Cutover: &CutoverState{Phase: "drain"}}, []Database{db(DDraining)}, true, true, MCutover},
		{"verifying", Flags{Started: true, Cutover: &CutoverState{Phase: "verify"}}, []Database{db(DDrained)}, true, true, MVerifying},
		{"GO", Flags{Started: true, Verdict: "GO", Cutover: &CutoverState{EndedAt: 1}}, []Database{db(DVerified)}, true, true, MCompleted},
		{"NO-GO", Flags{Started: true, Verdict: "NO-GO", Cutover: &CutoverState{EndedAt: 1}}, []Database{db(DDrained)}, true, true, MNeedsAttention},
		{"aborted", Flags{Started: true, Aborted: true}, []Database{db(DStopped)}, true, true, MAborted},
		{"cleaned up", Flags{Started: true, Aborted: true, CleanedUp: true}, nil, true, true, MCleanedUp},
		{"paused", Flags{Started: true, Paused: true}, []Database{db(DStopped), db(DStopped)}, true, true, MPaused},
	}
	for _, c := range cases {
		if got := DeriveState(Migration{Flags: c.f}, c.dbs, c.pf, c.perm); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestNames(t *testing.T) {
	s := SlotNameFor("m42abc", "orders")
	if len(s) > 63 || s != "upwell_m42abc_orders" {
		t.Fatalf("slot name %q", s)
	}
	long := SlotNameFor("m42abc", "db49_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	if len(long) > 63 {
		t.Fatalf("slot name for a 63-character database is %d characters", len(long))
	}
	if SlotNameFor("m42abc", "db49_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx1") == SlotNameFor("m42abc", "db49_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx2") {
		t.Fatal("two long database names map to the same slot")
	}
}
