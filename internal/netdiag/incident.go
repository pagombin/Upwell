package netdiag

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IncidentInput describes a connection that closed under the engine.
type IncidentInput struct {
	Migration, Database string
	Side                string // "source" or "target"
	Peers               []Peer // that side's addresses
	FailedAt            time.Time
	AttemptStart        time.Time
	Reason              string
	// ServerSessions are the engine's sessions the cluster still lists after
	// the failure (from pg_stat_activity), one line each; ServerErr when that
	// could not be read.
	ServerSessions []string
	ServerErr      string
}

// Packet is one captured TCP control packet (or ICMP message).
type Packet struct {
	At    time.Time
	Src   string
	Dst   string
	Flags string // tcpdump's: S, S., F., R, R., P. ...
	TTL   int
	Line  string
}

var (
	reHead  = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d+) (?:\S+ +\S+ +)?IP6? \(.*?(?:ttl|hlim) (\d+)`)
	reTCP   = regexp.MustCompile(`^\s*(\S+) > (\S+): Flags \[([^\]]+)\]`)
	reShort = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d+) (?:\S+ +\S+ +)?IP6? (\S+) > (\S+): (?:Flags \[([^\]]+)\]|(ICMP.*))`)
)

// parseTcpdump reads `tcpdump -nn -tttt -v -r` output (two lines a packet
// with -v: the IP header with the TTL, then the TCP line).
func parseTcpdump(out string) []Packet {
	var pk []Packet
	lines := strings.Split(out, "\n")
	for i := 0; i < len(lines); i++ {
		m := reHead.FindStringSubmatch(lines[i])
		if m == nil {
			if s := reShort.FindStringSubmatch(lines[i]); s != nil {
				at, _ := time.Parse("2006-01-02 15:04:05.000000", s[1])
				p := Packet{At: at, Src: s[2], Dst: strings.TrimSuffix(s[3], ":"), Flags: s[4], Line: strings.TrimSpace(lines[i])}
				if s[5] != "" {
					p.Flags = "ICMP"
				}
				pk = append(pk, p)
			}
			continue
		}
		at, _ := time.Parse("2006-01-02 15:04:05.000000", m[1])
		ttl, _ := strconv.Atoi(m[2])
		p := Packet{At: at, TTL: ttl, Line: strings.TrimSpace(lines[i])}
		if i+1 < len(lines) {
			if t := reTCP.FindStringSubmatch(lines[i+1]); t != nil {
				p.Src, p.Dst, p.Flags = t[1], t[2], t[3]
				p.Line += " | " + strings.TrimSpace(lines[i+1])
				i++
			} else if strings.Contains(lines[i+1], "ICMP") {
				p.Flags = "ICMP"
				p.Line += " | " + strings.TrimSpace(lines[i+1])
				i++
			}
		}
		pk = append(pk, p)
	}
	return pk
}

// captureFiles lists the capture files written since t (newest last).
func captureFiles(dir string, t time.Time) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type f struct {
		p string
		m time.Time
	}
	var fs []f
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "upwell-") || !strings.HasSuffix(e.Name(), ".pcap") {
			continue
		}
		if fi, err := e.Info(); err == nil && !fi.ModTime().Before(t) {
			fs = append(fs, f{filepath.Join(dir, e.Name()), fi.ModTime()})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].m.Before(fs[j].m) })
	out := make([]string, len(fs))
	for i, x := range fs {
		out[i] = x.p
	}
	return out
}

// CaptureDir is where the root unit writes packet headers.
func (r *Recorder) CaptureDir() string { return filepath.Join(filepath.Dir(r.Dir), "capture") }

// Incident writes a report of what the droplet saw of a closed connection
// and returns its path and a one-paragraph verdict.
func (r *Recorder) Incident(ctx context.Context, in IncidentInput) (string, string) {
	var b strings.Builder
	var verdict []string
	// Three minutes before the failure, whatever the attempt (the samples and
	// packets just before a quick failure come from before its start), and
	// one sample interval after it, so the state right after is in too.
	from := in.FailedAt.Add(-3 * time.Minute)
	if wait := time.Until(in.FailedAt.Add(6 * time.Second)); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	fmt.Fprintf(&b, "Upwell network incident\nmigration %s, database %s, %s connection\nfailed at %s UTC (attempt started %s UTC)\nreason: %s\n\n",
		in.Migration, in.Database, in.Side, in.FailedAt.UTC().Format("2006-01-02 15:04:05.000"), in.AttemptStart.UTC().Format("2006-01-02 15:04:05"), in.Reason)
	isPeer := map[string]bool{}   // by IP, for the kernel log
	peerAddr := map[string]bool{} // by tcpdump address, for packets
	var peerList []string
	for _, p := range in.Peers {
		isPeer[p.IP] = true
		peerAddr[tcpdumpAddr(p)] = true
		peerList = append(peerList, p.String())
	}
	fmt.Fprintf(&b, "%s addresses: %s\n\n", in.Side, strings.Join(peerList, ", "))

	// 1. Packet headers (root unit upwell-netcapture).
	b.WriteString("== Captured TCP control packets (SYN, FIN, RST, zero window) and ICMP, from 3 minutes before ==\n")
	files := captureFiles(r.CaptureDir(), from)
	var pk []Packet
	if len(files) == 0 {
		b.WriteString("no capture: upwell-netcapture is not running or wrote nothing in this window (see capture/status)\n")
		verdict = append(verdict, "No packet capture covers the failure (is upwell-netcapture running?).")
	} else if _, err := exec.LookPath("tcpdump"); err != nil {
		b.WriteString("tcpdump is not installed, so the capture files could not be read here; they are in the bundle\n")
	} else {
		var filt []string
		for _, p := range in.Peers {
			filt = append(filt, fmt.Sprintf("(host %s and port %d)", p.IP, p.Port))
		}
		for _, f := range files {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			out, _ := exec.CommandContext(cctx, "tcpdump", "-nn", "-tttt", "-v", "-r", f, strings.Join(filt, " or ")).Output()
			cancel()
			for _, p := range parseTcpdump(string(out)) {
				if !p.At.Before(from) && !p.At.After(in.FailedAt.Add(time.Minute)) {
					pk = append(pk, p)
				}
			}
		}
		sort.Slice(pk, func(i, j int) bool { return pk[i].At.Before(pk[j].At) })
		n := 0
		for _, p := range pk {
			if n++; n > 400 {
				b.WriteString("... (more in the capture files)\n")
				break
			}
			b.WriteString(p.Line + "\n")
		}
		if len(pk) == 0 {
			b.WriteString("no control packet to or from these addresses in the window\n")
		}
		verdict = append(verdict, closeVerdict(pk, peerAddr, in.Side)...)
	}

	// 2. The kernel's view of each connection (ss), every 5 s.
	b.WriteString("\n== Connections to these addresses, as the kernel saw them (ss, every 5 s) ==\n")
	samples := r.Since(from)
	peerSet := map[string]bool{}
	for _, p := range in.Peers {
		peerSet[p.String()] = true
	}
	var last []Sock
	for _, s := range samples {
		var mine []Sock
		for _, k := range s.Socks {
			if peerSet[k.Peer] {
				mine = append(mine, k)
			}
		}
		if s.At.After(in.FailedAt) && len(last) > 0 {
			// keep "last" as the view just before the failure
		} else if len(mine) > 0 {
			last = mine
		}
		fmt.Fprintf(&b, "%s  %d connection(s)%s\n", s.At.Format("15:04:05"), len(mine), map[bool]string{true: "  error: " + s.Err, false: ""}[s.Err != ""])
		for _, k := range mine {
			fmt.Fprintf(&b, "    %-11s %s -> %s send-q %d recv-q %d %s\n", k.State, k.Local, k.Peer, k.SendQ, k.RecvQ, fieldLine(k))
		}
	}
	if len(samples) == 0 {
		b.WriteString("no samples (the sampler starts with the first migration that has clusters)\n")
	}
	verdict = append(verdict, sockVerdict(last)...)

	// 3. Kernel counters that changed.
	b.WriteString("\n== Kernel counters that changed in the window ==\n")
	d := Deltas(samples)
	for _, k := range sortedKeys(d) {
		fmt.Fprintf(&b, "%-32s %+d\n", k, d[k])
	}
	if len(d) == 0 {
		b.WriteString("none\n")
	}
	verdict = append(verdict, counterVerdict(d)...)

	// 4. Kernel log: firewall drops and conntrack trouble.
	b.WriteString("\n== Kernel log (ufw blocks for these addresses, conntrack, TCP) ==\n")
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	out, err := exec.CommandContext(cctx, "journalctl", "-k", "-q", "--no-pager", "-o", "short-iso-precise",
		"--since", "@"+strconv.FormatInt(from.Unix(), 10), "--until", "@"+strconv.FormatInt(in.FailedAt.Add(time.Minute).Unix(), 10)).Output()
	cancel()
	kn := 0
	if err != nil {
		fmt.Fprintf(&b, "could not read the kernel log: %v\n", err)
	}
	for _, l := range strings.Split(string(out), "\n") {
		hit := strings.Contains(l, "nf_conntrack") || strings.Contains(l, "TCP:")
		for ip := range isPeer {
			if strings.Contains(l, "SRC="+ip+" ") || strings.Contains(l, "DST="+ip+" ") {
				hit = true
			}
		}
		if hit {
			kn++
			b.WriteString(l + "\n")
		}
	}
	if kn == 0 && err == nil {
		b.WriteString("nothing\n")
	} else if kn > 0 {
		verdict = append(verdict, fmt.Sprintf("The kernel log has %d firewall or conntrack line(s) about these addresses around the failure.", kn))
	}

	// 5. The cluster's own view.
	fmt.Fprintf(&b, "\n== Engine sessions the %s still lists after the failure (pg_stat_activity) ==\n", in.Side)
	switch {
	case in.ServerErr != "":
		fmt.Fprintf(&b, "could not ask: %s\n", in.ServerErr)
	case len(in.ServerSessions) == 0:
		b.WriteString("none\n")
	default:
		for _, s := range in.ServerSessions {
			b.WriteString(s + "\n")
		}
		verdict = append(verdict, fmt.Sprintf("The %s still lists %d session(s) of the failed engine after this droplet saw the connection close: PostgreSQL did not end them, so the close came from something in between (a proxy or load balancer).", in.Side, len(in.ServerSessions)))
	}

	b.WriteString("\n== Verdict ==\n" + strings.Join(verdict, "\n") + "\n")
	name := fmt.Sprintf("incident-%s-%s-%s.txt", in.FailedAt.UTC().Format("20060102T150405Z"), safe(in.Migration), safe(in.Database))
	path := filepath.Join(filepath.Dir(r.Dir), name)
	err = os.MkdirAll(filepath.Dir(path), 0o750)
	if err == nil {
		err = os.WriteFile(path, []byte(b.String()), 0o640)
	}
	if err != nil {
		verdict = append(verdict, "The report could not be saved: "+err.Error()+".")
	}
	return path, strings.Join(verdict, " ")
}

func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

func fieldLine(k Sock) string {
	var parts []string
	for _, f := range []string{"rtt", "retrans", "unacked", "lastsnd", "lastrcv", "lastack", "snd_wnd", "rcv_space", "bytes_acked", "bytes_received", "backoff", "probes"} {
		if v, ok := k.Fields[f]; ok {
			parts = append(parts, f+":"+v)
		}
	}
	if k.Timer != "" {
		parts = append(parts, "timer:"+k.Timer)
	}
	return strings.Join(parts, " ")
}

// tcpdumpAddr is how tcpdump prints an address and port ("1.2.3.4.5432").
func tcpdumpAddr(p Peer) string { return p.IP + "." + strconv.Itoa(p.Port) }

type connClose struct {
	key            string
	synAt, closeAt time.Time
	first          Packet
	byPeer         bool
}

// closeVerdict reads, for each connection to the peer, who sent the first
// FIN or RST and how long it had been open, and whether a FIN or RST from
// the peer's address has the TTL of its handshake (a different hop count
// points at a device on the path injecting it). Upwell's own short checks
// close from this droplet's side; the engine's long connections are the ones
// that matter.
func closeVerdict(pk []Packet, peers map[string]bool, side string) []string {
	var out []string
	synTTL := map[int]bool{}
	conns := map[string]*connClose{}
	icmp := 0
	for _, p := range pk {
		if p.Flags == "ICMP" {
			icmp++
			continue
		}
		fromPeer := peers[p.Src]
		if !fromPeer && !peers[p.Dst] {
			continue
		}
		key := p.Src + "|" + p.Dst
		if fromPeer {
			key = p.Dst + "|" + p.Src
		}
		c := conns[key]
		if c == nil {
			c = &connClose{key: key}
			conns[key] = c
		}
		if strings.HasPrefix(p.Flags, "S") {
			if fromPeer && p.TTL > 0 {
				synTTL[p.TTL] = true
			}
			if !fromPeer && c.synAt.IsZero() {
				c.synAt = p.At
			}
			continue
		}
		if strings.ContainsAny(p.Flags, "FR") && c.closeAt.IsZero() {
			c.closeAt, c.first, c.byPeer = p.At, p, fromPeer
		}
	}
	var peerFirst, ownFirst []*connClose
	for _, c := range conns {
		if c.closeAt.IsZero() {
			continue
		}
		if c.byPeer {
			peerFirst = append(peerFirst, c)
		} else {
			ownFirst = append(ownFirst, c)
		}
	}
	// The engine's connections are the long ones: report the earliest of
	// those first (short ones are checks and probes).
	long := func(c *connClose) bool { return c.synAt.IsZero() || c.closeAt.Sub(c.synAt) >= 10*time.Second }
	sort.Slice(peerFirst, func(i, j int) bool {
		if long(peerFirst[i]) != long(peerFirst[j]) {
			return long(peerFirst[i])
		}
		return peerFirst[i].closeAt.Before(peerFirst[j].closeAt)
	})
	nLong := 0
	for _, c := range peerFirst {
		if long(c) {
			nLong++
		}
	}
	open := func(c *connClose) string {
		if c.synAt.IsZero() {
			return "opened before the capture window"
		}
		return "open " + c.closeAt.Sub(c.synAt).Round(time.Millisecond).String()
	}
	switch {
	case len(peerFirst) == 0 && len(ownFirst) == 0:
		out = append(out, "No FIN or RST to or from the "+side+" was captured around the failure: the connection did not end with a TCP packet that reached this droplet (a silent drop on the path, or the capture was not running).")
	case len(peerFirst) == 0:
		out = append(out, fmt.Sprintf("Every connection that closed in the window (%d) was closed first by this droplet: nothing from the %s's address ended a connection.", len(ownFirst), side))
	default:
		c := peerFirst[0]
		kind := "FIN"
		if strings.Contains(c.first.Flags, "R") {
			kind = "RST"
		}
		which := "the earliest"
		if nLong > 0 {
			which = "the earliest of those open 10 s or more"
		}
		s := fmt.Sprintf("The %s's address closed %d connection(s) first (%d open 10 s or more); %s was a %s at %s UTC on %s (%s). This droplet closed %d first (Upwell's own short checks end that way).",
			side, len(peerFirst), nLong, which, kind, c.closeAt.Format("15:04:05.000"), strings.Replace(c.key, "|", " -> ", 1), open(c), len(ownFirst))
		if c.first.TTL > 0 && len(synTTL) > 0 {
			if synTTL[c.first.TTL] {
				s += fmt.Sprintf(" Its TTL (%d) matches the %s's handshake packets: it came from the host that accepted the connection (the server, or a proxy that terminates TCP in front of it).", c.first.TTL, side)
			} else {
				var t []string
				for x := range synTTL {
					t = append(t, strconv.Itoa(x))
				}
				sort.Strings(t)
				s += fmt.Sprintf(" Its TTL (%d) differs from the %s's handshake packets (%s): it was likely sent by another device on the path, such as a firewall or load balancer.", c.first.TTL, side, strings.Join(t, ", "))
			}
		}
		out = append(out, s)
	}
	if icmp > 0 {
		out = append(out, fmt.Sprintf("%d ICMP message(s) to or from the %s were captured (unreachable or fragmentation needed points at the network path).", icmp, side))
	}
	return out
}

func sockVerdict(last []Sock) []string {
	var out []string
	cw, zero, maxRcv, maxUnacked := 0, 0, 0, 0
	for _, k := range last {
		if k.State == "CLOSE-WAIT" {
			cw++
		}
		if k.Fields["snd_wnd"] == "0" {
			zero++
		}
		if v, err := strconv.Atoi(k.Fields["lastrcv"]); err == nil && k.State == "ESTAB" && v > maxRcv {
			maxRcv = v
		}
		if v, err := strconv.Atoi(k.Fields["unacked"]); err == nil && v > maxUnacked {
			maxUnacked = v
		}
	}
	if len(last) == 0 {
		return nil
	}
	if cw > 0 {
		out = append(out, fmt.Sprintf("%d connection(s) were in CLOSE-WAIT just before the failure: the other end had already closed them.", cw))
	}
	if zero > 0 {
		out = append(out, fmt.Sprintf("%d connection(s) had a zero send window: the other end had stopped reading.", zero))
	}
	if maxRcv > 60000 {
		out = append(out, fmt.Sprintf("One connection had received nothing for %s before the failure.", (time.Duration(maxRcv)*time.Millisecond).Round(time.Second)))
	}
	if maxUnacked > 0 {
		out = append(out, fmt.Sprintf("Up to %d segment(s) were waiting for an acknowledgement.", maxUnacked))
	}
	return out
}

func counterVerdict(d map[string]int64) []string {
	var out []string
	if d["TcpExt.TCPAbortOnTimeout"] > 0 {
		out = append(out, fmt.Sprintf("This droplet's kernel gave up on %d connection(s) after retransmitting without an answer (TCPAbortOnTimeout, or tcp_user_timeout).", d["TcpExt.TCPAbortOnTimeout"]))
	}
	if n := d["conntrack.invalid"] + d["conntrack.drop"] + d["conntrack.insert_failed"] + d["conntrack.early_drop"]; n > 0 {
		out = append(out, fmt.Sprintf("conntrack marked or dropped %d packet(s) in the window (invalid %d, drop %d, insert_failed %d, early_drop %d).", n, d["conntrack.invalid"], d["conntrack.drop"], d["conntrack.insert_failed"], d["conntrack.early_drop"]))
	}
	if d["Tcp.EstabResets"] > 0 {
		out = append(out, fmt.Sprintf("%d established connection(s) on this droplet were reset in the window (Tcp.EstabResets).", d["Tcp.EstabResets"]))
	}
	return out
}
