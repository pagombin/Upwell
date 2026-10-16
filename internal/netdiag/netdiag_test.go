package netdiag

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const ssOut = "ESTAB 0      0      127.0.0.1:33890 127.0.0.1:55432 timer:(keepalive,119min,0)\n" +
	"\t ts sack bbr wscale:10,10 rto:208 rtt:5.732/11.211 ato:40 mss:32768 pmtu:65535 cwnd:15 bytes_sent:259 bytes_acked:260 bytes_received:621 lastsnd:960 lastrcv:65000 lastack:916 snd_wnd:0 unacked:3 retrans:0/2\n" +
	"CLOSE-WAIT 1 0 10.0.0.5:40000 159.89.220.21:5432\n" +
	"\t rtt:30/2 lastrcv:100\n"

func TestParseSS(t *testing.T) {
	s := parseSS(ssOut)
	if len(s) != 2 {
		t.Fatalf("parsed %d sockets: %+v", len(s), s)
	}
	if s[0].State != "ESTAB" || s[0].Peer != "127.0.0.1:55432" || s[0].Fields["snd_wnd"] != "0" || s[0].Fields["unacked"] != "3" || s[0].Timer != "(keepalive,119min,0)" {
		t.Fatalf("first socket: %+v", s[0])
	}
	if _, ok := s[0].Fields["ts"]; ok {
		t.Fatal("kept a flag that is not a measurement")
	}
	if s[1].State != "CLOSE-WAIT" || s[1].RecvQ != 1 {
		t.Fatalf("second socket: %+v", s[1])
	}
	v := strings.Join(sockVerdict(s), " ")
	for _, want := range []string{"CLOSE-WAIT", "zero send window"} {
		if !strings.Contains(v, want) {
			t.Errorf("verdict %q lacks %q", v, want)
		}
	}
	// A busy connection (data outstanding) receiving nothing is a COPY, not
	// silence; an idle one receiving nothing is.
	if strings.Contains(v, "received nothing") {
		t.Errorf("silence reported on a busy connection: %s", v)
	}
	idle := parseSS("ESTAB 0 0 10.0.0.5:40001 159.89.220.21:5432\n\t rtt:30/2 lastrcv:65000 lastsnd:65000\n")
	if v := strings.Join(sockVerdict(idle), " "); !strings.Contains(v, "received nothing for 1m5s") {
		t.Errorf("idle silence not reported: %s", v)
	}
}

// server-close.txt: two psql sessions to 127.0.0.1:55432; the server ended
// one (pg_terminate_backend) and so sent its FIN first; the other client
// closed its own session first.
func TestCloseVerdictFromCapture(t *testing.T) {
	b, err := os.ReadFile("testdata/server-close.txt")
	if err != nil {
		t.Fatal(err)
	}
	pk := parseTcpdump(string(b))
	if len(pk) != 8 || pk[0].TTL != 64 || pk[0].Flags != "S" || pk[0].Src != "127.0.0.1.37078" {
		t.Fatalf("parsed %d packets, first %+v", len(pk), pk[0])
	}
	v := strings.Join(closeVerdict(pk, map[string]bool{"127.0.0.1.55432": true}, "target"), " ")
	for _, want := range []string{"closed 1 connection(s) first", "a FIN at 22:26:59.336", "127.0.0.1.37078 -> 127.0.0.1.55432", "This droplet closed 1 first", "TTL (64) matches"} {
		if !strings.Contains(v, want) {
			t.Errorf("verdict %q lacks %q", v, want)
		}
	}
	// A FIN with another hop count than the handshake: injected on the path.
	for i := range pk {
		if pk[i].Src == "127.0.0.1.55432" && strings.Contains(pk[i].Flags, "F") {
			pk[i].TTL = 52
		}
	}
	if v := strings.Join(closeVerdict(pk, map[string]bool{"127.0.0.1.55432": true}, "target"), " "); !strings.Contains(v, "TTL (52) differs") {
		t.Errorf("no path verdict: %s", v)
	}
	if v := strings.Join(closeVerdict(nil, map[string]bool{"127.0.0.1.55432": true}, "target"), " "); !strings.Contains(v, "No FIN or RST") {
		t.Errorf("empty capture: %s", v)
	}
}

func TestCounters(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "net/stat"), 0o755)
	os.MkdirAll(filepath.Join(root, "sys/net/netfilter"), 0o755)
	os.WriteFile(filepath.Join(root, "net/snmp"), []byte("Tcp: RtoAlgorithm EstabResets RetransSegs\nTcp: 1 7 100\n"), 0o644)
	os.WriteFile(filepath.Join(root, "net/netstat"), []byte("TcpExt: SyncookiesSent TCPAbortOnTimeout\nTcpExt: 0 2\n"), 0o644)
	os.WriteFile(filepath.Join(root, "net/stat/nf_conntrack"), []byte("entries clashres found new invalid\n00000010 0 0 0 0000000a\n00000010 0 0 0 00000001\n"), 0o644)
	os.WriteFile(filepath.Join(root, "sys/net/netfilter/nf_conntrack_count"), []byte("42\n"), 0o644)
	c := Counters(root)
	if c["Tcp.EstabResets"] != 7 || c["TcpExt.TCPAbortOnTimeout"] != 2 || c["conntrack.invalid"] != 11 || c["conntrack.count"] != 42 {
		t.Fatalf("counters %+v", c)
	}
	if _, ok := c["Tcp.RtoAlgorithm"]; ok {
		t.Fatal("kept an uninteresting counter")
	}
	d := Deltas([]Sample{{Counters: c}, {Counters: map[string]int64{"Tcp.EstabResets": 9, "TcpExt.TCPAbortOnTimeout": 3, "conntrack.invalid": 11, "conntrack.count": 50}}})
	if d["Tcp.EstabResets"] != 2 || d["TcpExt.TCPAbortOnTimeout"] != 1 || len(d) != 2 {
		t.Fatalf("deltas %+v", d)
	}
	if v := strings.Join(counterVerdict(d), " "); !strings.Contains(v, "gave up on 1 connection") || !strings.Contains(v, "2 established connection(s)") {
		t.Fatalf("counter verdict %s", v)
	}
}

// The whole report from a real capture file, as the orchestrator writes it.
func TestIncidentReport(t *testing.T) {
	if _, err := exec.LookPath("tcpdump"); err != nil {
		t.Skip("tcpdump not installed")
	}
	root := t.TempDir()
	r := New(filepath.Join(root, "net"))
	os.MkdirAll(r.CaptureDir(), 0o755)
	b, _ := os.ReadFile("testdata/server-close.pcap")
	os.WriteFile(filepath.Join(r.CaptureDir(), "upwell-20261014T222600Z.pcap"), b, 0o644)
	// Not a capture file: ignored.
	os.WriteFile(filepath.Join(r.CaptureDir(), "tcpdump.log"), []byte("listening"), 0o644)
	at, _ := time.Parse("2006-01-02 15:04:05", "2026-10-14 22:27:00")
	path, verdict := r.Incident(context.Background(), IncidentInput{Migration: "m1", Database: "shop", Side: "target",
		Peers: []Peer{{IP: "127.0.0.1", Port: 55432}}, FailedAt: at, AttemptStart: at.Add(-time.Hour), Reason: "test",
		ServerSessions: []string{"pid 4242 pgcopydb[1] from 10.0.0.9:51000 since 22:20:00, state active"}})
	rep, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "incident-20261014T222700Z-m1-shop.txt") {
		t.Fatalf("path %s", path)
	}
	for _, want := range []string{"closed 1 connection(s) first", "still lists 1 session(s)"} {
		if !strings.Contains(verdict, want) {
			t.Errorf("verdict lacks %q: %s", want, verdict)
		}
	}
	if !strings.Contains(string(rep), "Flags [F.]") || !strings.Contains(string(rep), "== Verdict ==") {
		t.Fatalf("report:\n%s", rep)
	}
}

// A file written by deploy/netcapture/upwell-netcapture (tcpdump -i any: the
// interface and direction precede the IP header in -v output).
func TestHelperCaptureFormat(t *testing.T) {
	if _, err := exec.LookPath("tcpdump"); err != nil {
		t.Skip("tcpdump not installed")
	}
	out, err := exec.Command("tcpdump", "-nn", "-tttt", "-v", "-r", "testdata/upwell-20261014T223030Z.pcap").Output()
	if err != nil {
		t.Fatal(err)
	}
	pk := parseTcpdump(string(out))
	if len(pk) != 8 || pk[0].TTL != 64 || pk[0].Src != "127.0.0.1.38964" || pk[0].Flags != "S" {
		t.Fatalf("parsed %d packets, first %+v", len(pk), pk[0])
	}
	if v := strings.Join(closeVerdict(pk, map[string]bool{"127.0.0.1.55432": true}, "target"), " "); !strings.Contains(v, "closed 1 connection(s) first") {
		t.Fatalf("verdict %s", v)
	}
}

// The droplet's samples (2026-10-15, addresses replaced): a COPY connection
// to the target stopped being acknowledged at 14:19:28 and disappeared 120 s
// later, when the kernel's TCP_USER_TIMEOUT ended it; two others stalled for
// 10 to 25 s and recovered. The verdict must say the droplet ended it, not
// that the target closed it, and name the bursts of loss.
func TestStallVerdictFromDroplet(t *testing.T) {
	b, err := os.ReadFile("testdata/droplet-stall.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var samples []Sample
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var s Sample
		if err := json.Unmarshal([]byte(l), &s); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, s)
	}
	failed, _ := time.Parse(time.RFC3339, "2026-10-15T14:21:30Z")
	v := strings.Join(stallVerdict(samples, map[string]bool{"198.51.100.20:5432": true}, failed, 120*time.Second), " ")
	t.Log(v)
	for _, want := range []string{"192.0.2.10:51326 -> 198.51.100.20:5432 stopped being acknowledged at 14:19:2", "when it disappeared", "ended here, not closed by the other end", "recovered by themselves"} {
		if !strings.Contains(v, want) {
			t.Errorf("verdict lacks %q", want)
		}
	}
	d := Deltas(samples)
	cv := strings.Join(counterVerdict(d), " ")
	t.Log(cv)
	if !strings.Contains(cv, "retransmitted") || !strings.Contains(cv, "SYN") {
		t.Errorf("counter verdict lacks the loss: %s", cv)
	}
}

// The droplet's 240 GB copy (2026-10-16, addresses replaced): two CREATE
// INDEX connections to the target sat idle 9 m 40 s (no keepalive: the
// kernel's 2 hours), then sent their statements at 01:59:57. Nothing was
// acknowledged; every retransmission came back as an RST from the target's
// address carrying this droplet's own sequence number and payload, 2 hops
// nearer than the handshake (TTL 63 against 61), which the kernel rightly
// ignored; it gave up 600 s later. The verdict must say the connections
// were idle first, and that the RSTs were reflected by a device on the path.
func TestIdleThenReflectedRST(t *testing.T) {
	b, err := os.ReadFile("testdata/droplet-idle-rst.txt")
	if err != nil {
		t.Fatal(err)
	}
	pk := parseTcpdump(string(b))
	peers := map[string]bool{"198.51.100.30.5432": true}
	cv := strings.Join(closeVerdict(pk, peers, "target"), " ")
	t.Log(cv)
	for _, want := range []string{"TTL (63) differs", "this droplet's own sequence number", "not from the target"} {
		if !strings.Contains(cv, want) {
			t.Errorf("close verdict lacks %q", want)
		}
	}
	raw, err := os.ReadFile("testdata/droplet-idle-rst.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var samples []Sample
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var s Sample
		if err := json.Unmarshal([]byte(l), &s); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, s)
	}
	failed, _ := time.Parse(time.RFC3339, "2026-10-16T02:09:57Z")
	sv := strings.Join(stallVerdict(samples, map[string]bool{"198.51.100.30:5432": true}, failed, 600*time.Second), " ")
	t.Log(sv)
	for _, want := range []string{"stopped being acknowledged at 01:59:5", "idle for 9m", "ended here"} {
		if !strings.Contains(sv, want) {
			t.Errorf("stall verdict lacks %q", want)
		}
	}
}
