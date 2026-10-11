package orch

import "testing"

func TestParseNetDev(t *testing.T) {
	in := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 4078364093 1776123    0    0    0     0          0         0 4078364093 1776123    0    0    0     0       0          0
  eth0: 1000 10 0 0 0 0 0 0 2500 20 0 0 0 0 0 0
  eth1:  200  2 0 0 0 0 0 0  300  3 0 0 0 0 0 0
`
	rx, tx, ok := parseNetDev([]byte(in))
	if !ok || rx != 1200 || tx != 2800 {
		t.Fatalf("rx %d tx %d ok %v", rx, tx, ok)
	}
	if _, _, ok := parseNetDev([]byte("garbage")); ok {
		t.Fatal("garbage parsed")
	}
}
