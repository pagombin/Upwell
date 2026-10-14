package orch

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pagombin/upwell/internal/pgpack"
)

// D88: long, fast streams through the droplet's firewall (ufw) were cut when
// conntrack marked out-of-window packets INVALID, which ufw drops. The
// installer keeps net.netfilter.nf_conntrack_tcp_be_liberal = 1, and a root
// unit (upwell-firewall.path) allows each cluster address Upwell lists here.
// Upwell itself runs without privileges, so it only writes the list.

func (o *Orchestrator) firewallDir() string { return filepath.Join(o.cfg.DataDir, "firewall") }

// clusterAddrs resolves every source and target cluster of the migrations
// that are not cleaned up, as "ip port" lines.
func (o *Orchestrator) clusterAddrs(ctx context.Context) ([]string, []string) {
	return o.clusterAddrList(ctx, false)
}

// clusterAddrList is clusterAddrs, optionally with loopback addresses (the
// firewall never needs them; network diagnostics watch every cluster).
func (o *Orchestrator) clusterAddrList(ctx context.Context, loopback bool) ([]string, []string) {
	ms, err := o.ListMigrations(ctx)
	if err != nil {
		return nil, []string{err.Error()}
	}
	seen := map[string]bool{}
	var out, errs []string
	for _, m := range ms {
		if m.Fixture || m.Flags.CleanedUp {
			continue
		}
		src, dst, err := o.Conns(ctx, m)
		if err != nil {
			continue // not defined yet
		}
		for _, c := range []struct {
			host string
			port int
		}{{src.Host, src.Port}, {dst.Host, dst.Port}} {
			if c.host == "" || c.port == 0 {
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			ips, err := net.DefaultResolver.LookupIPAddr(rctx, c.host)
			cancel()
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", c.host, err))
				continue
			}
			for _, ip := range ips {
				if ip.IP.IsLoopback() && !loopback {
					continue
				}
				l := ip.IP.String() + " " + strconv.Itoa(c.port)
				if !seen[l] {
					seen[l] = true
					out = append(out, l)
				}
			}
		}
	}
	sort.Strings(out)
	return out, errs
}

// SyncFirewall writes the cluster address list when the firewall unit is
// installed (its directory exists) and the list changed. Called at preflight,
// at each launch and every few minutes (a cluster's address can change).
func (o *Orchestrator) SyncFirewall(ctx context.Context) (want []string, changed bool, err error) {
	dir := o.firewallDir()
	if _, err := os.Stat(dir); err != nil {
		return nil, false, nil // not installed (development)
	}
	want, errs := o.clusterAddrs(ctx)
	body := "# Written by Upwell: the source and target cluster addresses (D88).\n" + strings.Join(want, "\n") + "\n"
	path := filepath.Join(dir, "clusters")
	if cur, err := os.ReadFile(path); err == nil && string(cur) == body {
		return want, false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o640); err != nil {
		return want, false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return want, false, err
	}
	if len(errs) > 0 {
		o.logf("warn", "orchestrator", "", "", "", "cluster firewall: could not resolve %s", strings.Join(errs, "; "))
	}
	o.logf("info", "orchestrator", "", "", "", "cluster firewall: %d cluster address(es) listed for the firewall unit", len(want))
	return want, true, nil
}

// NetworkPath reads the firewall unit's result for the preflight check,
// after syncing the list and giving the unit a moment to apply it.
func (o *Orchestrator) NetworkPath(ctx context.Context) *pgpack.NetworkPath {
	np := &pgpack.NetworkPath{}
	if b, err := os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_tcp_be_liberal"); err == nil {
		np.Liberal = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/etc/ufw/ufw.conf"); err == nil {
		np.UfwEnabled = strings.Contains(string(b), "ENABLED=yes")
	}
	want, changed, err := o.SyncFirewall(ctx)
	if err != nil {
		np.Error = err.Error()
		return np
	}
	if _, err := os.Stat(o.firewallDir()); err != nil {
		return np
	}
	np.Managed = true
	np.Wanted = want
	status := filepath.Join(o.firewallDir(), "status")
	deadline := time.Now().Add(20 * time.Second)
	for {
		allowed, at := readFirewallStatus(status)
		missing := missingAddrs(want, allowed)
		fresh := !changed || at >= time.Now().Add(-30*time.Second).Unix()
		if (len(missing) == 0 && fresh) || time.Now().After(deadline) || ctx.Err() != nil {
			np.Missing = missing
			np.StatusAt = at
			break
		}
		time.Sleep(time.Second)
	}
	return np
}

func readFirewallStatus(path string) (map[string]bool, int64) {
	allowed := map[string]bool{}
	var at int64
	b, err := os.ReadFile(path)
	if err != nil {
		return allowed, 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(l, "at "):
			at, _ = strconv.ParseInt(strings.TrimPrefix(l, "at "), 10, 64)
		case strings.HasPrefix(l, "allowed "):
			allowed[strings.TrimPrefix(l, "allowed ")] = true
		}
	}
	return allowed, at
}

func missingAddrs(want []string, allowed map[string]bool) []string {
	var out []string
	for _, w := range want {
		if !allowed[w] {
			out = append(out, w)
		}
	}
	return out
}
