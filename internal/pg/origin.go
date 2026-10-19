package pg

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var originActiveRe = regexp.MustCompile(`already active for PID (\d+)`)

// ReleaseOrigin makes sure no session on the target holds the replication
// origin, so that a new apply can set it up. Called only when no engine runs
// for the database. Behind a pooler in session mode the apply's server
// session outlives the engine and keeps the origin (DISCARD ALL does not
// release one; the droplet, 2026-10-17), and its application name is gone,
// so EndStaleEngineSessions cannot recognise it. Each attempt takes a new
// connection through c, as the engine would: the pooler may hand back the
// session that holds the origin ("already setup": reset it there) or another
// one ("already active for PID p": end p, a session of this user in this
// database). It returns what it did.
func ReleaseOrigin(ctx context.Context, c Conn, origin string) ([]string, error) {
	var did []string
	for attempt := 0; attempt < 10; attempt++ {
		conn, err := Connect(ctx, c, 30*time.Second)
		if err != nil {
			return did, err
		}
		free, step, err := releaseOriginOnce(ctx, conn, origin)
		conn.Close(ctx)
		if step != "" {
			did = append(did, step)
		}
		if err != nil || free {
			return did, err
		}
	}
	return did, fmt.Errorf("replication origin %s is still held after %d attempts: %s", origin, 10, strings.Join(did, "; "))
}

func releaseOriginOnce(ctx context.Context, conn *pgx.Conn, origin string) (free bool, step string, err error) {
	for reset := false; ; reset = true {
		_, err := conn.Exec(ctx, `SELECT pg_replication_origin_session_setup($1)`, origin)
		switch {
		case err == nil:
			_, err = conn.Exec(ctx, `SELECT pg_replication_origin_session_reset()`)
			return err == nil, step, err
		case strings.Contains(err.Error(), "does not exist"):
			return true, step, nil
		case strings.Contains(err.Error(), "when one is already setup") && !reset:
			if _, err := conn.Exec(ctx, `SELECT pg_replication_origin_session_reset()`); err != nil {
				return false, step, err
			}
			var pid int32
			conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
			step = fmt.Sprintf("released it in pooled server session %d", pid)
		case originActiveRe.MatchString(err.Error()):
			pid := originActiveRe.FindStringSubmatch(err.Error())[1]
			var who string
			if err := conn.QueryRow(ctx, `SELECT format('server session %s (%s, %s, since %s)', pid, COALESCE(NULLIF(application_name,''),'no application name'), COALESCE(state,''), backend_start)
				FROM pg_stat_activity WHERE pid = $1::int AND datname = current_database() AND usename = current_user`, pid).Scan(&who); err != nil {
				return false, step, fmt.Errorf("replication origin %s is held by server session %s, which is not this user's in this database: %w", origin, pid, err)
			}
			var ok bool
			if err := conn.QueryRow(ctx, `SELECT pg_terminate_backend($1::int, 10000)`, pid).Scan(&ok); err != nil {
				return false, step, err
			}
			return false, "ended " + who, nil
		default:
			return false, step, err
		}
	}
}
