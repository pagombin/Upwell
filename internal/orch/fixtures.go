package orch

import (
	"context"
	"fmt"
	"strings"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/store"
)

// SeedFixtures creates demonstration migrations used by the visual tests:
// a 50-database stress migration with extreme values, a completed GO, and a
// NO-GO. They are marked as fixtures: the orchestrator never supervises them
// and they cannot be started. Development mode only.
func SeedFixtures(ctx context.Context, o *Orchestrator, a audit.Actor) ([]string, error) {
	var ids []string
	now := store.Now()
	day := int64(24 * 3600 * 1000)
	mk := func(name, state string, flags Flags, startedAgo int64) (Migration, error) {
		m := Migration{ID: store.NewID(), ShortID: newShortID(), Name: name, State: state, Mode: "online", Engine: "pgcopydb", Settings: map[string]any{}, Flags: flags, CreatedAt: now - startedAgo - day, UpdatedAt: now}
		started := now - startedAgo
		_, err := o.st.DB.ExecContext(ctx, `INSERT INTO migrations(id,short_id,name,state,mode,engine,settings,flags,fixture,created_by,created_at,updated_at,started_at) VALUES (?,?,?,?,?,?,?,?,1,?,?,?,?)`,
			m.ID, m.ShortID, m.Name, m.State, m.Mode, m.Engine, "{}", toJSON(flags), a.Username, m.CreatedAt, m.UpdatedAt, started)
		if err != nil {
			return m, err
		}
		o.st.DB.ExecContext(ctx, `INSERT INTO permission_records(id,migration_id,customer,account_id,ticket,granted_by,granted_at,scope,locked_at,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			store.NewID(), m.ID, "Example Customer Holdings International (fixture)", "team-0000-fixture", "FIXTURE-000000", "fixture.owner@example.com", "2026-10-01", "Every database on the Standard cluster, fixture data only", now, now, now)
		ids = append(ids, m.ID)
		return m, nil
	}
	stress, err := mk("Stress fixture: 50 databases, 10+ TiB, 7+ days", MStreaming, Flags{Started: true}, 8*day+3*3600*1000)
	if err != nil {
		return nil, err
	}
	states := []string{DInSync, DCatchUp, DBaseCopy, DInSync, DRestartRequired, DInSync, DFailed, DPending, DStopped, DDegraded}
	longErr := "ERROR: could not receive data from WAL stream: server closed the connection unexpectedly. This probably means the server terminated abnormally before or while processing the request; the replication connection was lost after 41 retries across 7 days of streaming and the last 20 attempts failed identically"
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("db%02d_", i)
		name += strings.Repeat("x", 63-len(name))
		if i%7 == 3 {
			name = fmt.Sprintf("orders_%02d", i)
		}
		st := states[i%len(states)]
		backlog := int64(i) * 3 << 20
		if i%5 == 0 {
			backlog = 2_700_000_000_000
		}
		lastErr := ""
		if st == DFailed || st == DRestartRequired || st == DDegraded {
			lastErr = longErr
		}
		bc := 0
		if st == DInSync || st == DCatchUp || st == DStopped {
			bc = 1
		}
		since := now - int64(i)*61000
		o.st.DB.ExecContext(ctx, `INSERT INTO migration_databases(id,migration_id,source_name,target_name,include,state,slot_name,origin_name,instance,size_bytes,rows_estimate,table_count,last_error,in_sync_since,backlog_bytes,replay_lsn,write_lsn,base_copy_done,retry_count,created_at,updated_at)
			VALUES (?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, store.NewID(), stress.ID, name, name, st, SlotNameFor(stress.ShortID, name), SlotNameFor(stress.ShortID, name), InstanceFor(stress.ShortID, name),
			int64(11_500_000_000_000)+int64(i)*7_000_000_000, int64(10_400_000_000)+int64(i)*1_000_003, 4000+i, store.NullString(lastErr), since, backlog, "1A2/3F00B8C8", "1A2/40000000", bc, i%4, now, now)
	}
	series := []string{"backlog_bytes", "replay_rate", "slot_retained_bytes", "target_size_bytes"}
	for _, s := range series {
		for i := 0; i < 5; i++ {
			name := fmt.Sprintf("db%02d_", i)
			name += strings.Repeat("x", 63-len(name))
			for k := 0; k < 120; k++ {
				ts := now - int64(120-k)*30000
				v := float64(1<<30) * float64(1+(k*7+i*13)%17)
				if s == "replay_rate" {
					v = float64(80<<20) + float64((k*31+i)%23)*float64(1<<20)
				}
				o.metrics.put(ctx, stress.ID, name, s, v, ts)
			}
		}
	}
	for k := 0; k < 120; k++ {
		o.metrics.put(ctx, stress.ID, "", "src_wal_rate", float64(30<<20)+float64(k%13)*float64(1<<20), now-int64(120-k)*30000)
	}
	for i := 0; i < 30; i++ {
		o.Event(ctx, stress.ID, "", "fixture", []string{"info", "warning", "critical"}[i%3], fmt.Sprintf("Fixture event %d: %s", i, longErr[:40+i*5]), nil)
	}
	o.alerts.fire(ctx, stress.ID, "backlog_growing", "db00", "warning", "db00: the change backlog grew for 5 consecutive samples (fixture)")
	o.alerts.fire(ctx, stress.ID, "slot_at_risk", "db04", "critical", "db04: the slot's WAL is no longer reserved and may be removed (fixture)")

	goM, err := mk("Completed fixture: GO", MCompleted, Flags{Started: true, Verdict: "GO", VerdictAt: now - 3600000, Cutover: &CutoverState{Phase: "done", StartedAt: now - 3700000, WritesStoppedAt: now - 3650000, EndedAt: now - 3600000, WritePauseMS: 50000, Verdict: "GO"}}, 2*day)
	if err != nil {
		return nil, err
	}
	noGo, err := mk("Failed fixture: NO-GO", MNeedsAttention, Flags{Started: true, Verdict: "NO-GO", VerdictAt: now - 1800000, Cutover: &CutoverState{Phase: "done", StartedAt: now - 1900000, WritesStoppedAt: now - 1850000, EndedAt: now - 1800000, WritePauseMS: 50000, Verdict: "NO-GO"}}, day)
	if err != nil {
		return nil, err
	}
	for _, fx := range []struct {
		m      Migration
		verdict string
	}{{goM, "GO"}, {noGo, "NO-GO"}} {
		for _, db := range []string{"orders", "inventory"} {
			st, verdict := DVerified, "GO"
			lastErr := ""
			if fx.verdict == "NO-GO" && db == "orders" {
				st, verdict, lastErr = DFailed, "NO-GO", "NO-GO: Final heartbeat on the target; Row counts match"
			}
			o.st.DB.ExecContext(ctx, `INSERT INTO migration_databases(id,migration_id,source_name,target_name,include,state,slot_name,origin_name,instance,size_bytes,rows_estimate,table_count,verdict,last_error,hb_before_lsn,hb_token,endpos,origin_lsn,base_copy_done,created_at,updated_at)
				VALUES (?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)`, store.NewID(), fx.m.ID, db, db, st, SlotNameFor(fx.m.ShortID, db), SlotNameFor(fx.m.ShortID, db), InstanceFor(fx.m.ShortID, db),
				int64(5_368_709_120), int64(42_000_000), 18, verdict, store.NullString(lastErr), "0/5A0001C8", "final-fixture-"+db, "0/5A0002F0", map[bool]string{true: "0/5A000210", false: "0/59FF0000"}[verdict == "GO"], now, now)
			checks := []struct{ id, title, result, msg string }{
				{"final_heartbeat", "Final heartbeat on the target", "ok", "The final heartbeat, the last write before the end position, is on the target."},
				{"origin_progress", "Target origin reached the final heartbeat", "ok", "The target applied changes through 0/5A000210, past the final heartbeat at 0/5A0001C8."},
				{"schema", "Schema matches", "ok", "All 18 tables exist on the target with the same columns."},
				{"row_counts", "Row counts match", "ok", "Exact counts match for 18 table(s)."},
				{"checksums", "Row checksums match", "ok", "Checksums match for all 18 table(s)."},
				{"sequences", "Sequences synced", "ok", "All 6 sequence(s) are at or past the source's values."},
			}
			if verdict == "NO-GO" {
				checks[0] = struct{ id, title, result, msg string }{"final_heartbeat", "Final heartbeat on the target", "blocker", "The final heartbeat written after writes stopped never reached the target. Changes made before it may be missing too."}
				checks[1] = struct{ id, title, result, msg string }{"origin_progress", "Target origin reached the final heartbeat", "blocker", "The target applied changes only up to 0/59FF0000, short of 0/5A0001C8 where the final heartbeat was written."}
				checks[3] = struct{ id, title, result, msg string }{"row_counts", "Row counts match", "blocker", "1 table(s) have different row counts: public.orders (source 420000, target 360000)."}
			}
			for _, c := range checks {
				o.st.DB.ExecContext(ctx, `INSERT INTO verifications(id,migration_id,database,run_id,check_id,result,title,detail,ts) VALUES (?,?,?,?,?,?,?,?,?)`,
					store.NewID(), fx.m.ID, db, "fixture-"+fx.m.ID, c.id, c.result, c.title, toJSON(map[string]any{"message": c.msg}), now)
			}
		}
		o.updateFlags(ctx, fx.m.ID, func(f *Flags) { f.VerifyRunID = "fixture-" + fx.m.ID })
	}
	return ids, nil
}
