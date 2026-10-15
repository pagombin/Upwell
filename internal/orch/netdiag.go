package orch

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pagombin/upwell/internal/netdiag"
	"github.com/pagombin/upwell/internal/pg"
)

// D97: when a connection to a cluster closes under the engine, nothing on the
// droplet said what closed it. The recorder samples the kernel's view of
// every connection to the clusters every 5 seconds; at a failure Upwell
// writes an incident report from those samples, the kernel counters and log,
// the packet headers the root unit upwell-netcapture captured, and the
// cluster's own list of the engine's sessions.

// netLoop samples the connections to the clusters until ctx ends.
func (o *Orchestrator) netLoop(ctx context.Context) {
	var peersAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		if !o.globalSettings(ctx).Bool("network_diagnostics") {
			continue
		}
		if time.Since(peersAt) > 30*time.Second || len(o.net.Peers()) == 0 {
			addrs, _ := o.clusterAddrList(ctx, true)
			o.net.SetPeers(netdiag.ParsePeers(addrs))
			peersAt = time.Now()
		}
		o.net.Tick(ctx)
	}
}

// NetDiagDir is where samples, incidents and captures are kept.
func (o *Orchestrator) NetDiagDir() string { return filepath.Join(o.cfg.DataDir, "diag") }

// netIncident writes the incident report for a connection that closed under
// the engine and records an event with its verdict. It runs in the
// background: the failure is handled without waiting for it.
func (o *Orchestrator) netIncident(m Migration, d Database, side string, conn pg.Conn, reason string, attemptStart, failedAt time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if !o.globalSettings(ctx).Bool("network_diagnostics") || !o.metrics.due(d.ID+"|netincident", time.Minute) {
		return // off, or this failure already has its report
	}
	in := netdiag.IncidentInput{Migration: m.ShortID, Database: d.SourceName, Side: side, Reason: reason, AttemptStart: attemptStart, FailedAt: failedAt,
		UserTimeout: time.Duration(o.Settings(ctx, m).Int("engine_tcp_user_timeout_seconds")) * time.Second}
	if ips, err := net.DefaultResolver.LookupIPAddr(ctx, conn.Host); err == nil {
		for _, ip := range ips {
			in.Peers = append(in.Peers, netdiag.Peer{IP: ip.IP.String(), Port: conn.Port})
		}
	}
	// The cluster's own view: sessions of an engine (pgcopydb names them
	// pgcopydb[pid]) that it still lists, where it sees them coming from.
	if c, err := pg.Connect(ctx, conn, 15*time.Second); err != nil {
		in.ServerErr = err.Error()
	} else {
		rows, err := pg.Query(ctx, c, `SELECT pid, COALESCE(datname, ''), application_name, COALESCE(host(client_addr), ''), COALESCE(client_port, 0),
				COALESCE(to_char(backend_start AT TIME ZONE 'UTC', 'HH24:MI:SS'), ''), COALESCE(state, ''), COALESCE(wait_event_type || '/' || wait_event, ''), COALESCE(left(query, 80), '')
			FROM pg_stat_activity WHERE application_name LIKE 'pgcopydb%' AND usename = current_user ORDER BY backend_start`)
		if err != nil {
			in.ServerErr = err.Error()
		} else {
			for rows.Next() {
				var pid, port int
				var db, app, addr, start, state, wait, q string
				if err := rows.Scan(&pid, &db, &app, &addr, &port, &start, &state, &wait, &q); err != nil {
					in.ServerErr = err.Error()
					break
				}
				in.ServerSessions = append(in.ServerSessions, fmt.Sprintf("pid %d %s in %s from %s:%d since %s UTC, %s %s: %s", pid, app, db, addr, port, start, state, wait, q))
			}
			if err := rows.Err(); err != nil && in.ServerErr == "" {
				in.ServerErr = err.Error()
			}
			rows.Close()
		}
		c.Close(ctx)
	}
	path, verdict := o.net.Incident(ctx, in)
	o.Event(ctx, m.ID, d.SourceName, "network_incident", "warning",
		fmt.Sprintf("%s: network diagnostics for the closed %s connection (%s, in the support bundle): %s", d.SourceName, side, filepath.Base(path), verdict),
		map[string]any{"file": filepath.Base(path), "peers": len(in.Peers), "server_sessions": len(in.ServerSessions), "attempt_started": strconv.FormatInt(attemptStart.UnixMilli(), 10)})
}
