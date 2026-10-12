package orch

import (
	"context"
	"fmt"
	"time"

	"github.com/pagombin/upwell/internal/pg"
)

// failureReason explains a failed attempt from its log (D78). For a
// connection that closed under the engine it also names the table being
// copied (with its size on the source) and asks the cluster on the other
// end whether its PostgreSQL server restarted during the attempt, which
// tells a crash, an out-of-memory kill or a failover apart from a proxy or
// load balancer closing one connection.
func (o *Orchestrator) failureReason(ctx context.Context, m Migration, d Database, c classes) string {
	if c.connLost == "" {
		return c.rootCause()
	}
	src, dst, err := o.Conns(ctx, m)
	if err != nil {
		return c.rootCause()
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	table := ""
	if c.failedOID != "" {
		if sc, err := pg.Connect(cctx, src.WithDB(d.SourceName), 5*time.Second); err == nil {
			var name, size string
			if pg.QueryRow(cctx, sc, `SELECT format('%I.%I', n.nspname, c.relname), pg_size_pretty(pg_table_size(c.oid)) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = $1::oid`, c.failedOID).Scan(&name, &size) == nil {
				table = fmt.Sprintf("%s (%s on the source)", name, size)
			}
			sc.Close(cctx)
		}
	}
	side := dst
	if c.connLost == "source" {
		side = src
	}
	var started int64
	o.st.DB.QueryRowContext(ctx, `SELECT started_at FROM attempts WHERE database_id=? ORDER BY number DESC LIMIT 1`, d.ID).Scan(&started)
	conn, err := pg.Connect(cctx, side, 10*time.Second)
	if err != nil {
		return fmt.Sprintf("%s. Upwell could not connect to the %s right afterwards (%v): it may be restarting or failing over; check the cluster's status, metrics and logs. pgcopydb does not retry a table, so the copy must restart", c.connLostSummary(table), c.connLost, err)
	}
	defer conn.Close(cctx)
	var up time.Time
	if err := pg.QueryRow(cctx, conn, `SELECT pg_postmaster_start_time()`).Scan(&up); err != nil || started == 0 {
		return c.connLostSummary(table) + ". Something between this droplet and the " + c.connLost + " (a proxy or load balancer, a failover or a restart) ended it; pgcopydb does not retry a table, so the copy must restart"
	}
	att := time.UnixMilli(started).UTC()
	if up.After(att) {
		into := up.Sub(att).Round(time.Minute)
		if into < time.Minute {
			into = up.Sub(att).Round(time.Second)
		}
		return fmt.Sprintf("%s. The %s's PostgreSQL server restarted at %s UTC, %s into this attempt, which ended every connection: a server restarts like this when it runs out of memory or disk, crashes, or fails over. Check the %s cluster's memory and disk metrics and its logs around that time before restarting the copy; fewer table and index jobs reduce the load on it",
			c.connLostSummary(table), c.connLost, up.UTC().Format("2006-01-02 15:04:05"), into, c.connLost)
	}
	return fmt.Sprintf("%s. The %s's PostgreSQL server did not restart (up since %s UTC), so this one connection was closed by something between this droplet and the %s, such as a proxy, a load balancer or a connection limit, or the server ended that one session. pgcopydb does not retry a table, so the copy must restart",
		c.connLostSummary(table), c.connLost, up.UTC().Format("2006-01-02 15:04:05"), c.connLost)
}
