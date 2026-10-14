package pgpack

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/pg"
)

// SnapshotBlocker is a session whose open transaction a logical slot's
// creation waits for: the base copy cannot start until it ends.
type SnapshotBlocker struct {
	PID         int    `json:"pid"`
	Database    string `json:"database"`
	User        string `json:"user"`
	App         string `json:"application"`
	Client      string `json:"client"`
	State       string `json:"state"`
	OpenSeconds int64  `json:"open_seconds"`
}

// SlotCreationBlockers reports whether the engine's CREATE_REPLICATION_SLOT
// for slot is waiting, and for which sessions. Creating a logical slot waits
// for every transaction running on the cluster when it starts (in any
// database), so one long transaction holds the base copy back silently.
func SlotCreationBlockers(ctx context.Context, c *pgx.Conn, slot string) (bool, []SnapshotBlocker, error) {
	rows, err := pg.Query(ctx, c, `SELECT w.pid, b.pid, COALESCE(b.datname, ''), COALESCE(b.usename, ''), COALESCE(b.application_name, ''),
			COALESCE(host(b.client_addr), ''), COALESCE(b.state, ''), COALESCE(extract(epoch FROM now() - b.xact_start)::bigint, 0)
		FROM pg_stat_activity w
		LEFT JOIN LATERAL unnest(pg_blocking_pids(w.pid)) bp(pid) ON true
		LEFT JOIN pg_stat_activity b ON b.pid = bp.pid
		WHERE w.backend_type = 'walsender' AND w.query LIKE 'CREATE_REPLICATION_SLOT %' AND split_part(w.query, '"', 2) = $1
		ORDER BY 8 DESC`, slot)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	waiting := false
	var out []SnapshotBlocker
	for rows.Next() {
		var w int
		var bpid *int
		var b SnapshotBlocker
		if err := rows.Scan(&w, &bpid, &b.Database, &b.User, &b.App, &b.Client, &b.State, &b.OpenSeconds); err != nil {
			return false, nil, err
		}
		if bpid != nil {
			waiting = true
			b.PID = *bpid
			out = append(out, b)
		}
	}
	return waiting, out, rows.Err()
}
