package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pagombin/upwell/internal/netdiag"
	"github.com/pagombin/upwell/internal/orch"
)

// D101: the support bundle's network section is limited to what explains
// this migration's failures. The capture and the samples cover every cluster
// of every migration for up to 48 hours, which made bundles of 40 MB and
// more; by default only windows around this migration's failures are kept
// (15 minutes before each to 5 minutes after, and the last 15 minutes),
// only its own clusters' packets and sockets. ?network=full keeps the old
// selection, ?network=none leaves captures and samples out.

const (
	netBefore     = 15 * time.Minute
	netAfter      = 5 * time.Minute
	netCapMax     = 15 << 20
	netSamplesMax = 4 << 20
)

type netWindow struct{ from, to time.Time }

func (w netWindow) overlaps(a, b time.Time) bool { return !b.Before(w.from) && !a.After(w.to) }

// netWindows: around each failure of the migration in the last 48 hours
// (events of severity critical or error, and network incidents), and the
// last 15 minutes; overlapping windows merged.
func netWindows(events []map[string]any, now time.Time) []netWindow {
	ws := []netWindow{{now.Add(-netBefore), now}}
	for _, e := range events {
		ts, _ := e["ts"].(int64)
		sev, _ := e["severity"].(string)
		typ, _ := e["type"].(string)
		t := time.UnixMilli(ts)
		if now.Sub(t) > 48*time.Hour || !(sev == "critical" || sev == "error" || typ == "network_incident") {
			continue
		}
		ws = append(ws, netWindow{t.Add(-netBefore), t.Add(netAfter)})
	}
	sort.Slice(ws, func(i, j int) bool { return ws[i].from.Before(ws[j].from) })
	var out []netWindow
	for _, w := range ws {
		if n := len(out); n > 0 && !w.from.After(out[n-1].to) {
			if w.to.After(out[n-1].to) {
				out[n-1].to = w.to
			}
			continue
		}
		out = append(out, w)
	}
	return out
}

func inAny(ws []netWindow, a, b time.Time) bool {
	for _, w := range ws {
		if w.overlaps(a, b) {
			return true
		}
	}
	return false
}

// captureStart reads the start time from a capture file's name
// (upwell-20261016T015447Z.pcap).
func captureStart(name string) (time.Time, bool) {
	t, err := time.Parse("20060102T150405Z", strings.TrimSuffix(strings.TrimPrefix(name, "upwell-"), ".pcap"))
	return t, err == nil
}

type capFile struct {
	name       string
	start, end time.Time
}

// selectCaptures lists the capture files that overlap a window, newest
// first (a file runs from the time in its name to its last write).
func selectCaptures(dir string, ws []netWindow) []capFile {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []capFile
	for _, e := range ents {
		start, ok := captureStart(e.Name())
		if !ok || !strings.HasSuffix(e.Name(), ".pcap") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if ws == nil || inAny(ws, start, fi.ModTime()) {
			out = append(out, capFile{e.Name(), start, fi.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start.After(out[j].start) })
	return out
}

// peerFilter is a BPF filter for the migration's clusters (and ICMP errors
// from anywhere, which can concern them), without this droplet's own
// zero-window packets to them, which captures made before D101 hold by the
// hundred thousand.
func peerFilter(peers []netdiag.Peer) string {
	var tcp, dst []string
	seen := map[string]bool{}
	for _, p := range peers {
		if strings.Contains(p.IP, ":") { // the capture is IPv4
			continue
		}
		k := fmt.Sprintf("(host %s and port %d)", p.IP, p.Port)
		if !seen[k] {
			seen[k] = true
			tcp = append(tcp, k)
		}
		if d := "dst host " + p.IP; !seen[d] {
			seen[d] = true
			dst = append(dst, d)
		}
	}
	if len(tcp) == 0 {
		return ""
	}
	sort.Strings(tcp)
	sort.Strings(dst)
	return "(tcp and (" + strings.Join(tcp, " or ") + ") and not (tcp[tcpflags] & (tcp-syn|tcp-fin|tcp-rst) = 0 and tcp[14:2] = 0 and (" + strings.Join(dst, " or ") + "))) or icmp"
}

// filterCapture keeps the packets that match filter (tcpdump reads and
// writes capture files without privileges). Without tcpdump or a filter the
// file is kept as it is.
func filterCapture(ctx context.Context, path, filter string) ([]byte, error) {
	if filter == "" {
		return os.ReadFile(path)
	}
	if _, err := exec.LookPath("tcpdump"); err != nil {
		return os.ReadFile(path)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(cctx, "tcpdump", "-r", path, "-w", "-", filter)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tcpdump: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// selectSamples reads the socket sample files (oldest first) and keeps the
// samples inside a window, with only the sockets to the migration's
// clusters; the newest are kept when they exceed max bytes.
func selectSamples(dir string, ws []netWindow, peers []netdiag.Peer, max int) []byte {
	want := map[string]bool{}
	for _, p := range peers {
		want[p.String()] = true
	}
	var lines [][]byte
	for _, name := range []string{"sockets.2.jsonl", "sockets.1.jsonl", "sockets.jsonl"} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var s netdiag.Sample
			if json.Unmarshal(sc.Bytes(), &s) != nil || !inAny(ws, s.At, s.At) {
				continue
			}
			if len(want) > 0 {
				keep := s.Socks[:0]
				for _, k := range s.Socks {
					if want[k.Peer] {
						keep = append(keep, k)
					}
				}
				s.Socks = keep
			}
			if b, err := json.Marshal(s); err == nil {
				lines = append(lines, b)
			}
		}
		f.Close()
	}
	var out []byte
	for i := len(lines) - 1; i >= 0 && len(out)+len(lines[i])+1 <= max; i-- {
		out = append(append(append([]byte{}, lines[i]...), '\n'), out...)
	}
	return out
}

// bundleNetwork returns the network/ files for the bundle.
func (s *Server) bundleNetwork(ctx context.Context, m orch.Migration, events []map[string]any, mode string) []bundleFile {
	var files []bundleFile
	add := func(name string, data []byte, bin bool) {
		files = append(files, bundleFile{Name: "network/" + name, data: data, Binary: bin})
	}
	diag := s.Orch.NetDiagDir()
	if ents, err := os.ReadDir(diag); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "incident-") && strings.Contains(e.Name(), "-"+m.ShortID+"-") {
				if b, err := os.ReadFile(filepath.Join(diag, e.Name())); err == nil {
					add(e.Name(), b, false)
				}
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(diag, "capture", "status")); err == nil {
		add("capture-status.txt", b, false)
	} else {
		add("capture-status.txt", []byte("no status: the root unit upwell-netcapture is not installed or has not run ("+err.Error()+")\n"), false)
	}
	if mode == "none" {
		add("README.txt", []byte("Packet captures and socket samples were left out (network=none). The incident reports above summarise them.\n"), false)
		return files
	}
	now := time.Now()
	var ws []netWindow
	var peers []netdiag.Peer
	capMax := netCapMax
	if mode == "full" {
		ws = []netWindow{{now.Add(-48 * time.Hour), now}}
		capMax = 40 << 20
	} else {
		ws = netWindows(events, now)
		peers = s.Orch.MigrationPeers(ctx, m)
	}
	var readme strings.Builder
	fmt.Fprintf(&readme, "Network diagnostics for migration %s (selection: %s).\n\n", m.ShortID, map[bool]string{true: "full, every cluster, 48 hours", false: "this migration's failures"}[mode == "full"])
	if mode != "full" {
		readme.WriteString("Windows (UTC), 15 minutes before each failure to 5 minutes after, and the last 15 minutes:\n")
		for _, w := range ws {
			fmt.Fprintf(&readme, "  %s to %s\n", w.from.UTC().Format("2006-01-02 15:04:05"), w.to.UTC().Format("15:04:05"))
		}
		readme.WriteString("Addresses kept:")
		if len(peers) == 0 {
			readme.WriteString(" all (the migration's hosts did not resolve)")
		}
		for _, p := range peers {
			readme.WriteString(" " + p.String())
		}
		readme.WriteString("\nDownload with ?network=full for every capture of the last 48 hours.\n")
	}
	if b := selectSamples(filepath.Join(diag, "net"), ws, peers, netSamplesMax); len(b) > 0 {
		add("sockets.jsonl", b, false)
	}
	filter := peerFilter(peers)
	total, skipped := 0, 0
	for _, c := range selectCaptures(filepath.Join(diag, "capture"), ws) {
		b, err := filterCapture(ctx, filepath.Join(diag, "capture", c.name), filter)
		if err != nil {
			fmt.Fprintf(&readme, "%s: %v\n", c.name, err)
			continue
		}
		if len(b) <= 24 { // a pcap header and no packet
			continue
		}
		if total+len(b) > capMax {
			skipped++
			continue
		}
		total += len(b)
		add(c.name, b, true)
	}
	if skipped > 0 {
		fmt.Fprintf(&readme, "%d older capture file(s) left out to keep the bundle under %d MB.\n", skipped, capMax>>20)
	}
	add("README.txt", []byte(readme.String()), false)
	return files
}
