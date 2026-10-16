package api

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/netdiag"
)

// D101: the droplet's bundle held 111 capture files (41 MB) and 4 MB of
// samples, for every cluster of every migration over 48 hours. Only the
// windows around this migration's failures, and only its clusters, are kept.
func TestNetWindows(t *testing.T) {
	now := time.Date(2026, 10, 16, 2, 46, 0, 0, time.UTC)
	ms := func(t time.Time) int64 { return t.UnixMilli() }
	ev := []map[string]any{
		{"ts": ms(now.Add(-36 * time.Minute)), "severity": "critical", "type": "base_copy_failed"},
		{"ts": ms(now.Add(-35 * time.Minute)), "severity": "critical", "type": "state"},
		{"ts": ms(now.Add(-3 * time.Hour)), "severity": "info", "type": "db_state"},
		{"ts": ms(now.Add(-50 * time.Hour)), "severity": "critical", "type": "base_copy_failed"}, // too old
		{"ts": ms(now.Add(-20 * time.Hour)), "severity": "warning", "type": "network_incident"},
	}
	ws := netWindows(ev, now)
	if len(ws) != 3 {
		t.Fatalf("windows %+v", ws)
	}
	if !ws[0].from.Equal(now.Add(-20*time.Hour-netBefore)) || !ws[1].from.Equal(now.Add(-36*time.Minute-netBefore)) || !ws[1].to.Equal(now.Add(-35*time.Minute+netAfter)) || !ws[2].to.Equal(now) {
		t.Fatalf("windows %+v", ws)
	}
}

func TestSelectAndFilterCaptures(t *testing.T) {
	if _, err := exec.LookPath("tcpdump"); err != nil {
		t.Skip("tcpdump not installed")
	}
	dir := t.TempDir()
	pc, err := os.ReadFile("../netdiag/testdata/server-close.pcap") // 127.0.0.1:55432
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ { // an hour of 5-minute files
		st := base.Add(time.Duration(i) * 5 * time.Minute)
		p := filepath.Join(dir, "upwell-"+st.Format("20060102T150405Z")+".pcap")
		os.WriteFile(p, pc, 0o644)
		os.Chtimes(p, st.Add(5*time.Minute), st.Add(5*time.Minute))
	}
	os.WriteFile(filepath.Join(dir, "tcpdump.log"), []byte("x"), 0o644)
	ws := []netWindow{{base.Add(22 * time.Minute), base.Add(31 * time.Minute)}}
	got := selectCaptures(dir, ws)
	var names []string
	for _, c := range got {
		names = append(names, c.name)
	}
	if strings.Join(names, ",") != "upwell-20261016T013000Z.pcap,upwell-20261016T012500Z.pcap,upwell-20261016T012000Z.pcap" {
		t.Fatalf("selected %v", names)
	}
	mine, err := filterCapture(context.Background(), filepath.Join(dir, got[0].name), peerFilter([]netdiag.Peer{{IP: "127.0.0.1", Port: 55432}}))
	if err != nil || len(mine) != len(pc) {
		t.Fatalf("own cluster's packets: %d bytes of %d, %v", len(mine), len(pc), err)
	}
	other, err := filterCapture(context.Background(), filepath.Join(dir, got[0].name), peerFilter([]netdiag.Peer{{IP: "127.0.0.1", Port: 55433}}))
	if err != nil || len(other) > 24 {
		t.Fatalf("another cluster's packets were kept: %d bytes, %v", len(other), err)
	}
}

func TestSelectSamples(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile("../netdiag/testdata/droplet-idle-rst.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "sockets.1.jsonl"), b, 0o644)
	from := time.Date(2026, 10, 16, 2, 0, 0, 0, time.UTC)
	ws := []netWindow{{from, from.Add(time.Minute)}}
	out := selectSamples(dir, ws, []netdiag.Peer{{IP: "198.51.100.30", Port: 5432}}, 1<<20)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 10 || len(lines) > 14 {
		t.Fatalf("%d samples in a minute", len(lines))
	}
	var s netdiag.Sample
	json.Unmarshal([]byte(lines[0]), &s)
	if len(s.Socks) != 2 || s.At.Before(from) {
		t.Fatalf("first sample %v with %d sockets", s.At, len(s.Socks))
	}
	if out := selectSamples(dir, ws, []netdiag.Peer{{IP: "198.51.100.99", Port: 5432}}, 1<<20); strings.Contains(string(out), "198.51.100.30") {
		t.Fatal("kept another cluster's sockets")
	}
	if small := selectSamples(dir, ws, nil, 8000); len(small) > 8000 || !strings.Contains(string(small), "02:00:5") {
		t.Fatalf("cap: %d bytes, newest kept: %v", len(small), strings.Contains(string(small), "02:00:5"))
	}
}
