// Package netdiag records the state of the droplet's TCP connections to the
// clusters, so that when one closes under the engine the support bundle can
// say what closed it (D97). Upwell runs without privileges: it samples the
// kernel's socket table (ss) and counters (/proc) itself; packet headers are
// captured by a separate root unit (upwell-netcapture) into a directory this
// package reads.
package netdiag

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Peer is one cluster address.
type Peer struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

func (p Peer) String() string {
	if strings.Contains(p.IP, ":") {
		return "[" + p.IP + "]:" + strconv.Itoa(p.Port)
	}
	return p.IP + ":" + strconv.Itoa(p.Port)
}

// ParsePeers reads "ip port" lines (the firewall list format).
func ParsePeers(lines []string) []Peer {
	var out []Peer
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		if p, err := strconv.Atoi(f[1]); err == nil {
			out = append(out, Peer{IP: f[0], Port: p})
		}
	}
	return out
}

// Sock is one TCP connection to a peer, as the kernel sees it.
type Sock struct {
	State  string            `json:"state"`
	Local  string            `json:"local"`
	Peer   string            `json:"peer"`
	SendQ  int64             `json:"send_q"`
	RecvQ  int64             `json:"recv_q"`
	Timer  string            `json:"timer,omitempty"`
	Fields map[string]string `json:"info,omitempty"` // rtt, retrans, unacked, lastsnd, lastrcv, lastack, bytes_*, snd_wnd...
}

// Sample is one reading.
type Sample struct {
	At       time.Time        `json:"at"`
	Socks    []Sock           `json:"sockets"`
	Counters map[string]int64 `json:"counters"`
	Err      string           `json:"error,omitempty"`
}

// keep is what a sample's socket line keeps (the rest is noise).
var keep = map[string]bool{"rtt": true, "rto": true, "retrans": true, "unacked": true, "lost": true, "lastsnd": true, "lastrcv": true, "lastack": true,
	"bytes_sent": true, "bytes_acked": true, "bytes_received": true, "bytes_retrans": true, "snd_wnd": true, "rcv_space": true, "cwnd": true,
	"pmtu": true, "mss": true, "backoff": true, "probes": true, "rcv_wnd": true, "notsent": true, "busy": true, "rwnd_limited": true, "sndbuf_limited": true}

// parseSS parses `ss -tinoHn` output: a socket line, then an indented info line.
func parseSS(out string) []Sock {
	var socks []Sock
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] == '\t' || line[0] == ' ' {
			if len(socks) == 0 {
				continue
			}
			s := &socks[len(socks)-1]
			for _, f := range strings.Fields(line) {
				k, v, ok := strings.Cut(f, ":")
				if ok && keep[k] {
					s.Fields[k] = v
				}
			}
			continue
		}
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		s := Sock{State: f[0], Local: f[3], Peer: f[4], Fields: map[string]string{}}
		s.RecvQ, _ = strconv.ParseInt(f[1], 10, 64)
		s.SendQ, _ = strconv.ParseInt(f[2], 10, 64)
		for _, x := range f[5:] {
			if strings.HasPrefix(x, "timer:") {
				s.Timer = strings.TrimPrefix(x, "timer:")
			}
		}
		socks = append(socks, s)
	}
	return socks
}

// Sockets lists the TCP connections to the peers. It needs `ss` (iproute2).
func Sockets(ctx context.Context, peers []Peer) ([]Sock, error) {
	if len(peers) == 0 {
		return nil, nil
	}
	args := []string{"-tinoHn", "state", "all", "("}
	for i, p := range peers {
		if i > 0 {
			args = append(args, "or")
		}
		args = append(args, "dst", p.String())
	}
	args = append(args, ")")
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "ss", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("ss: %w", err)
	}
	return parseSS(string(out)), nil
}

// interesting are the kernel counters worth watching around a closed
// connection (all others are dropped to keep the record small).
var interesting = map[string]bool{
	"Tcp.ActiveOpens": true, "Tcp.AttemptFails": true, "Tcp.EstabResets": true, "Tcp.RetransSegs": true, "Tcp.InErrs": true, "Tcp.OutRsts": true, "Tcp.InCsumErrors": true,
	"TcpExt.TCPAbortOnData": true, "TcpExt.TCPAbortOnClose": true, "TcpExt.TCPAbortOnMemory": true, "TcpExt.TCPAbortOnTimeout": true, "TcpExt.TCPAbortOnLinger": true, "TcpExt.TCPAbortFailed": true,
	"TcpExt.TCPTimeouts": true, "TcpExt.TCPLossProbes": true, "TcpExt.TCPSynRetrans": true, "TcpExt.TCPRcvQDrop": true, "TcpExt.TCPZeroWindowDrop": true,
	"TcpExt.TCPToZeroWindowAdv": true, "TcpExt.TCPFromZeroWindowAdv": true, "TcpExt.TCPWantZeroWindowAdv": true, "TcpExt.TCPBacklogDrop": true, "TcpExt.ListenDrops": true,
	"TcpExt.TCPRetransFail": true, "TcpExt.TCPMemoryPressures": true, "TcpExt.PruneCalled": true, "TcpExt.TCPOFODrop": true, "TcpExt.TCPMTUPFail": true,
	"IpExt.InNoRoutes": true, "Ip.InDiscards": true, "Ip.OutDiscards": true, "Icmp.InDestUnreachs": true, "Icmp.InTimeExcds": true, "Icmp.InErrors": true,
	"conntrack.invalid": true, "conntrack.drop": true, "conntrack.early_drop": true, "conntrack.insert_failed": true, "conntrack.icmp_error": true, "conntrack.count": true, "conntrack.max": true,
}

// Counters reads the kernel's TCP, IP, ICMP and conntrack counters.
func Counters(root string) map[string]int64 {
	out := map[string]int64{}
	for _, f := range []string{"net/snmp", "net/netstat"} {
		readPairs(filepath.Join(root, f), out)
	}
	// /proc/net/stat/nf_conntrack: one hex row per CPU, summed.
	if b, err := os.ReadFile(filepath.Join(root, "net/stat/nf_conntrack")); err == nil {
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) > 1 {
			head := strings.Fields(lines[0])
			for _, l := range lines[1:] {
				for i, v := range strings.Fields(l) {
					if i < len(head) && head[i] != "entries" {
						n, _ := strconv.ParseInt(v, 16, 64)
						out["conntrack."+head[i]] += n
					}
				}
			}
		}
	}
	for k, f := range map[string]string{"conntrack.count": "sys/net/netfilter/nf_conntrack_count", "conntrack.max": "sys/net/netfilter/nf_conntrack_max"} {
		if b, err := os.ReadFile(filepath.Join(root, f)); err == nil {
			out[k], _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		}
	}
	for k := range out {
		if !interesting[k] {
			delete(out, k)
		}
	}
	return out
}

// readPairs reads the "Name: a b c / Name: 1 2 3" pairs of /proc/net/snmp and netstat.
func readPairs(path string, out map[string]int64) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var head []string
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		prefix := strings.TrimSuffix(f[0], ":")
		if head == nil || head[0] != f[0] {
			head = f
			continue
		}
		for i := 1; i < len(f) && i < len(head); i++ {
			if n, err := strconv.ParseInt(f[i], 10, 64); err == nil {
				out[prefix+"."+head[i]] = n
			}
		}
		head = nil
	}
}

// Recorder keeps the last samples in memory and appends every sample to a
// size-capped JSON-lines file in Dir.
type Recorder struct {
	Dir      string
	ProcRoot string // "/proc" (tests point it elsewhere)
	Keep     time.Duration
	MaxFile  int64 // bytes per file before it rotates (3 files kept)

	mu    sync.Mutex
	ring  []Sample
	peers []Peer
}

// New returns a recorder writing under dir.
func New(dir string) *Recorder {
	return &Recorder{Dir: dir, ProcRoot: "/proc", Keep: 30 * time.Minute, MaxFile: 20 << 20}
}

// SetPeers replaces the cluster addresses to watch.
func (r *Recorder) SetPeers(p []Peer) {
	r.mu.Lock()
	r.peers = p
	r.mu.Unlock()
}

// Peers returns the watched addresses.
func (r *Recorder) Peers() []Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Peer(nil), r.peers...)
}

// Tick takes one sample (nothing when no peer is watched).
func (r *Recorder) Tick(ctx context.Context) {
	peers := r.Peers()
	if len(peers) == 0 {
		return
	}
	s := Sample{At: time.Now().UTC(), Counters: Counters(r.ProcRoot)}
	socks, err := Sockets(ctx, peers)
	if err != nil {
		s.Err = err.Error()
	}
	s.Socks = socks
	r.mu.Lock()
	r.ring = append(r.ring, s)
	cut := time.Now().Add(-r.Keep)
	i := 0
	for i < len(r.ring) && r.ring[i].At.Before(cut) {
		i++
	}
	r.ring = append([]Sample(nil), r.ring[i:]...)
	r.mu.Unlock()
	r.append(s)
}

func (r *Recorder) append(s Sample) {
	if r.Dir == "" {
		return
	}
	if err := os.MkdirAll(r.Dir, 0o750); err != nil {
		return
	}
	path := filepath.Join(r.Dir, "sockets.jsonl")
	if fi, err := os.Stat(path); err == nil && fi.Size() > r.MaxFile {
		// Best effort: a failed rotation only means a longer file.
		_ = os.Rename(filepath.Join(r.Dir, "sockets.1.jsonl"), filepath.Join(r.Dir, "sockets.2.jsonl"))
		_ = os.Rename(path, filepath.Join(r.Dir, "sockets.1.jsonl"))
	}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n')) // the in-memory ring keeps the sample either way
	f.Close()
}

// Since returns the samples taken at or after t.
func (r *Recorder) Since(t time.Time) []Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Sample
	for _, s := range r.ring {
		if !s.At.Before(t) {
			out = append(out, s)
		}
	}
	return out
}

// Deltas are the counters that changed between the first and last sample.
func Deltas(samples []Sample) map[string]int64 {
	out := map[string]int64{}
	if len(samples) < 2 {
		return out
	}
	a, b := samples[0].Counters, samples[len(samples)-1].Counters
	for k, v := range b {
		if k == "conntrack.count" || k == "conntrack.max" {
			continue
		}
		if d := v - a[k]; d != 0 {
			out[k] = d
		}
	}
	return out
}

func sortedKeys(m map[string]int64) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
