package orch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/pg"
)

// D100: a migration's source and target connections can change after it
// started, within limits. A different cluster under a running migration would
// leave the slot on the old source, or the copied data on the old target, so:
//   - before the start: any change (as before);
//   - a new password alone (same host, port, user, database, SSL mode and CA):
//     at any time, once it is shown to work, since running engines keep their
//     sessions and the next connection uses it;
//   - the target's storage size alone: at any time (it is only used to check
//     the space needed);
//   - anything else (host, port, user, database, SSL mode, CA): only while no
//     engine runs for the migration, no cutover is in progress and there is
//     no verdict yet, and only to an endpoint proven to be the same cluster
//     (a private hostname for the public one, a pooler or a new address).
// Never after an abort, a clean up or a verdict.

// connChange classifies an edit of a started migration's connection.
type connChange int

const (
	changeNone connChange = iota
	changePassword
	changeStorage
	changeEndpoint
)

func classifyConnChange(old Connection, in ConnInput) connChange {
	endpoint := old.Host != in.Host || old.Port != in.Port || old.User != in.User || old.DBName != in.DBName || old.SSLMode != in.SSLMode ||
		strings.TrimSpace(old.CACert) != strings.TrimSpace(in.CACert)
	switch {
	case endpoint:
		return changeEndpoint
	case in.Password != "":
		return changePassword
	case in.StorageGB != old.StorageGB:
		return changeStorage
	}
	return changeNone
}

// checkStartedConnChange applies the rules above to a started migration and
// returns the reason a change is refused, or nil.
func (o *Orchestrator) checkStartedConnChange(ctx context.Context, m Migration, kind string, old Connection, oldPG pg.Conn, in ConnInput) error {
	if m.Flags.CleanedUp || m.Flags.Aborted {
		return uerr("conn_frozen", "This migration was aborted or cleaned up; its connections can no longer change.", "Create a new migration.")
	}
	if m.Flags.Verdict != "" {
		return uerr("conn_frozen", "This migration has a verdict ("+m.Flags.Verdict+"); its connections can no longer change.", "Create a new migration to migrate again.")
	}
	newPG := in.PG()
	if in.Password == "" {
		newPG.Password = oldPG.Password
	}
	newPG.CAFile = oldPG.CAFile
	switch classifyConnChange(old, in) {
	case changeNone, changeStorage:
		return nil
	case changePassword:
		c, err := pg.Connect(ctx, newPG, 15*time.Second)
		if err != nil {
			return uerr("conn_test_failed", "The new "+kind+" password does not work, so it was not saved: "+pg.Explain(err).Error()+".", "Check the password; the old one is still in use.")
		}
		c.Close(ctx)
		return nil
	}
	// An endpoint change: nothing may be running.
	if c := m.Flags.Cutover; c != nil && c.EndedAt == 0 {
		return uerr("conn_busy", "A cutover is in progress; the "+kind+" connection cannot change now.", "Let the cutover finish, or abort it first.")
	}
	dbs, err := o.ListDatabases(ctx, m.ID)
	if err != nil {
		return err
	}
	var running []string
	for _, d := range included(dbs) {
		active := activeDB[d.State]
		if st, err := o.run.Status(ctx, d.Instance); err != nil {
			return fmt.Errorf("could not confirm that %s's engine is stopped: %w", d.SourceName, err)
		} else if st.Active {
			active = true
		}
		if active {
			running = append(running, d.SourceName+" ("+Label(d.State)+")")
		}
	}
	if len(running) > 0 {
		return uerr("conn_busy", "The "+kind+" connection cannot change while an engine runs: "+strings.Join(running, ", ")+". Changing where it connects under a live migration could leave the slot or the copied data behind.",
			"Stop the running databases (or pause the migration), change the connection, then resume them. A password alone can change at any time.")
	}
	// Prove it is the same cluster.
	same, how, err := o.sameCluster(ctx, m, kind, oldPG, newPG)
	if err != nil {
		return uerr("conn_test_failed", "Upwell could not check the new "+kind+" endpoint: "+err.Error()+".", "Check the host, port, user and password, then try again.")
	}
	if !same {
		return uerr("conn_other_cluster", "The new "+kind+" endpoint is not the same cluster ("+how+"). A started migration can only move to another address of the same cluster.",
			"Abort and clean up this migration, then create a new one for the other cluster.")
	}
	o.logf("info", "orchestrator", m.ID, "", "", "%s connection: the new endpoint %s:%d is the same cluster (%s)", kind, in.Host, in.Port, how)
	return nil
}

// clusterFingerprint identifies a PostgreSQL cluster through any address of
// it (a proxy included): when its server started, its version, and the oid
// and name of every database.
func clusterFingerprint(ctx context.Context, c pg.Conn) (string, error) {
	conn, err := pg.Connect(ctx, c, 15*time.Second)
	if err != nil {
		return "", pg.Explain(err)
	}
	defer conn.Close(ctx)
	var fp string
	err = pg.QueryRow(ctx, conn, `SELECT pg_postmaster_start_time()::text || ' | ' || current_setting('server_version_num') || ' | ' ||
		(SELECT string_agg(oid::text || ':' || datname, ',' ORDER BY oid) FROM pg_database)`).Scan(&fp)
	return fp, err
}

// sameCluster reports whether newPG reaches the same cluster as oldPG. When
// the old endpoint cannot be reached, the migration's own replication slot
// (source) or origin (target) on the new endpoint is the proof.
func (o *Orchestrator) sameCluster(ctx context.Context, m Migration, kind string, oldPG, newPG pg.Conn) (bool, string, error) {
	nf, err := clusterFingerprint(ctx, newPG)
	if err != nil {
		return false, "", err
	}
	of, oerr := clusterFingerprint(ctx, oldPG)
	if oerr == nil {
		if of == nf {
			return true, "same server start time, version and databases", nil
		}
		return false, "its server start time, version or databases differ from the current endpoint's", nil
	}
	// The old endpoint is gone: look for this migration's slot or origin.
	conn, err := pg.Connect(ctx, newPG, 15*time.Second)
	if err != nil {
		return false, "", pg.Explain(err)
	}
	defer conn.Close(ctx)
	dbs, err := o.ListDatabases(ctx, m.ID)
	if err != nil {
		return false, "", err
	}
	for _, d := range included(dbs) {
		var n int
		q := `SELECT count(*) FROM pg_replication_slots WHERE slot_name = $1`
		arg := d.SlotName
		if kind == "target" {
			q, arg = `SELECT count(*) FROM pg_replication_origin WHERE roname = $1`, d.OriginName
		}
		if err := pg.QueryRow(ctx, conn, q, arg).Scan(&n); err != nil {
			return false, "", err
		}
		if n > 0 {
			return true, "the current endpoint cannot be reached (" + oerr.Error() + "), but the new one holds this migration's " + map[string]string{"source": "replication slot " + d.SlotName, "target": "replication origin " + d.OriginName}[kind], nil
		}
	}
	return false, "", errors.New("the current endpoint cannot be reached (" + oerr.Error() + ") and the new one holds none of this migration's " + map[string]string{"source": "replication slots", "target": "replication origins"}[kind] + ", so Upwell cannot prove it is the same cluster")
}

// retargetEngines tells the engine of every included database that has run
// about the migration's current connections (pgcopydb refuses to resume on
// addresses other than those it recorded). Called with the migration locked
// and no engine running.
func (o *Orchestrator) retargetEngines(ctx context.Context, m Migration) error {
	rt, ok := o.eng.(engine.Retargeter)
	if !ok {
		return nil
	}
	m, err := o.GetMigration(ctx, m.ID) // the connection ids just stored
	if err != nil {
		return err
	}
	dbs, err := o.ListDatabases(ctx, m.ID)
	if err != nil {
		return err
	}
	for _, d := range included(dbs) {
		if d.Instance == "" {
			continue
		}
		spec, err := o.dbSpec(ctx, m, d)
		if err != nil {
			return err
		}
		changed, err := rt.Retarget(ctx, spec)
		if err != nil {
			return fmt.Errorf("%s: %w", d.SourceName, err)
		}
		if changed {
			o.logf("info", "orchestrator", m.ID, d.SourceName, "", "the engine's catalog now records %s:%d (source) and %s:%d (target)", spec.SourceConn.Host, spec.SourceConn.Port, spec.TargetConn.Host, spec.TargetConn.Port)
		}
	}
	return nil
}
