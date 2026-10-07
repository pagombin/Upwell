#!/usr/bin/env bash
# Phase 0 spike S01: pgcopydb as the main process of an engine unit.
# Proves or disproves, during CDC (base copy finished, streaming):
#   A. Stopping the unit leaves no process behind and frees the slot; measures timings,
#      and whether SIGTERM alone was enough or the SIGKILL after TimeoutStopSec was needed.
#   B. SIGKILL of pgcopydb's main process: do sub-processes keep running and keep the slot
#      active (harness finding)? Under systemd, does the unit clean the cgroup on its own?
#   C. Resume (--resume --not-consistent) after B reuses slot and origin, skips the base
#      copy, and the target converges to the source.
#   D. A stale walsender (client frozen, TCP open) keeps the slot active; the same
#      non-superuser role can terminate it; measures time to release.
#   E. SIGTERM to the main process only: does pgcopydb exit, and its children?
# Output: a JSON-lines evidence file plus the engine logs, under docs/phase0/evidence/s01/.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
source "$HERE/lib.sh"; source "$HERE/runner.sh"

DB=${DB:-orders}; ROWS=${ROWS:-300000}
NAME="upwell_s01_${DB}"; UNIT="upwell-eng-s01-${DB}"
RUN="$SPIKE_ROOT/runs/s01/$DB"; OUT="$EVIDENCE_DIR/s01"; mkdir -p "$OUT"
EVID="$OUT/results.jsonl"; : >"$EVID"
export SPIKE_LOG="$OUT/spike.log"; : >"$SPIKE_LOG"
record() { printf '%s\n' "$1" >>"$EVID"; log "RESULT $1"; }

clone_cmd() { # extra args... -> sets CMD array
  CMD=("$PGCOPYDB" clone --follow --source "$(src_uri "$DB")" --target "$(dst_uri "$DB")" \
    --dir "$RUN/clone" --slot-name "$NAME" --origin "$NAME" \
    --table-jobs 2 --index-jobs 2 --skip-extensions --no-owner "$@")
}
in_cdc() { [[ "$(sentinel_field "$DB" "$RUN/clone" apply)" == enabled ]]; }
procs_named() { pgrep -f "^pgcopydb: $1" | wc -l; }
cleanup() { [[ -n "${WRITER:-}" ]] && stop_writer "$WRITER" 2>/dev/null || true; unit_stop "$UNIT" >/dev/null 2>&1 || true; unit_reset "$UNIT"; }
trap cleanup EXIT

log "runner=$RUNNER pgcopydb=$($PGCOPYDB --version 2>/dev/null | sed -n 1p) server=$(src defaultdb -c 'show server_version')"
record "{\"meta\":{\"runner\":\"$RUNNER\",\"pgcopydb\":\"$($PGCOPYDB --version 2>/dev/null | sed -n 1p | awk '{print $3}')\",\"server\":\"$(src defaultdb -c 'show server_version')\",\"stop_timeout_s\":$STOP_TIMEOUT}}"

start_fresh() {
  unit_reset "$UNIT"; reset_run "$DB" "$NAME"; rm -rf "$RUN"; mkdir -p "$RUN"
  clone_cmd; unit_start "$UNIT" "$RUN/engine.log" "${CMD[@]}"
  local ms; ms=$(wait_for 600 in_cdc) || fail "never reached CDC"
  log "reached CDC after ${ms} ms"; sleep 5
  slot_active "$DB" "$NAME" || fail "slot not active in CDC"
}

log "seeding $DB with $ROWS rows"; seed_db "$DB" "$ROWS" >/dev/null 2>&1
WRITER=$(start_writer "$DB")

# ---------- A: unit stop during CDC
start_fresh
before=$(unit_live_count "$UNIT")
read -r ms how < <(unit_stop "$UNIT")
inactive_ms=$(wait_for 120 slot_inactive "$DB" "$NAME" || echo -1)
record "{\"scenario\":\"A_unit_stop_cdc\",\"procs_before\":$before,\"ms_to_empty\":$ms,\"stopped_by\":\"$how\",\"procs_after\":$(unit_live_count "$UNIT"),\"ms_slot_inactive_after_empty\":$inactive_ms}"
unit_reset "$UNIT"

# ---------- B: SIGKILL main during CDC (reuses the slot: this is a resume start)
clone_cmd --resume --not-consistent; unit_start "$UNIT" "$RUN/engine.log" "${CMD[@]}"
wait_for 120 slot_active "$DB" "$NAME" >/dev/null || fail "B: slot never active after resume"
sleep 5
main=$(unit_mainpid "$UNIT"); pidfile_main=$(head -1 "$RUN/clone/pgcopydb.pid")
before=$(unit_live_count "$UNIT")
kill -KILL "$main"; sleep 10
survivors=$(unit_live_count "$UNIT"); still_active=$(slot_active "$DB" "$NAME" && echo true || echo false)
recv=$(procs_named "follow receive"); appl=$(procs_named "follow apply")
if [[ $RUNNER == systemd ]]; then
  auto_ms=$(wait_for 60 unit_empty "$UNIT" || echo -1); manual='null'
  state=$(unit_state "$UNIT")
else
  auto_ms='null'; read -r m how < <(unit_stop "$UNIT"); manual="{\"ms_to_empty\":$m,\"stopped_by\":\"$how\"}"; state=n/a
fi
inactive_ms=$(wait_for 120 slot_inactive "$DB" "$NAME" || echo -1)
record "{\"scenario\":\"B_sigkill_main_cdc\",\"unit_mainpid\":$main,\"pidfile_mainpid\":$pidfile_main,\"procs_before\":$before,\"live_procs_10s_after_kill\":$survivors,\"receive_alive\":$recv,\"apply_alive\":$appl,\"slot_active_10s_after_kill\":$still_active,\"systemd_auto_cleanup_ms\":$auto_ms,\"unit_state\":\"$state\",\"emulated_stop\":$manual,\"ms_slot_inactive_after\":$inactive_ms}"
unit_reset "$UNIT"

# ---------- C: resume after B, then converge
start_lsn=$(sentinel_field "$DB" "$RUN/clone" startpos)
log_lines_before=$(wc -l <"$RUN/engine.log")
clone_cmd --resume --not-consistent; unit_start "$UNIT" "$RUN/engine.log" "${CMD[@]}"
wait_for 120 slot_active "$DB" "$NAME" >/dev/null || fail "C: slot never active"
sleep 10
stop_writer "$WRITER"; WRITER=""
sleep 2
"$PGCOPYDB" stream sentinel set endpos --current --source "$(src_uri "$DB")" --dir "$RUN/clone" >>"$RUN/engine.log" 2>&1
drain_ms=$(wait_for 600 unit_empty "$UNIT" || echo -1)
s_cnt=$(src "$DB" -c "select count(*)||'/'||sum(amount) from orders"); d_cnt=$(dst "$DB" -c "select count(*)||'/'||sum(amount) from orders")
recopied=$(tail -n +"$((log_lines_before+1))" "$RUN/engine.log" | grep -c "STEP 4: starting" || true)
record "{\"scenario\":\"C_resume_after_kill\",\"startpos_kept\":\"$start_lsn\",\"slot_reused\":$( [[ "$(sentinel_field "$DB" "$RUN/clone" startpos)" == "$start_lsn" ]] && echo true || echo false),\"base_copy_starts_in_log\":$recopied,\"drain_ms\":$drain_ms,\"exit_state\":\"$(unit_state "$UNIT")\",\"source_orders\":\"$s_cnt\",\"target_orders\":\"$d_cnt\",\"match\":$( [[ "$s_cnt" == "$d_cnt" ]] && echo true || echo false)}"
unit_reset "$UNIT"

# ---------- D: stale walsender (fresh run: after C the run reached its end position)
cp "$RUN/engine.log" "$OUT/engine-ABC.log"
WRITER=$(start_writer "$DB")
start_fresh
# Freeze every engine process: TCP stays open and nothing is read, which is what a
# client on a vanished or partitioned host looks like to the server.
for p in $(unit_pids "$UNIT"); do kill -STOP "$p" 2>/dev/null || true; done
sleep 2
active_pid=$(src "$DB" -c "select active_pid from pg_replication_slots where slot_name='$NAME'")
held_ms=$(wait_for 20 slot_inactive "$DB" "$NAME" || echo -1)   # -1 means still held after 20 s
t0=$(now_ms)
term_ok=$(src "$DB" -c "select pg_terminate_backend($active_pid)")
rel_ms=$(wait_for 30 slot_inactive "$DB" "$NAME" || echo -1)
record "{\"scenario\":\"D_stale_walsender\",\"walsender_pid\":$active_pid,\"released_on_its_own_within_20s\":$( [[ $held_ms == -1 ]] && echo false || echo true),\"terminate_as_same_nonsuper_role\":\"$term_ok\",\"ms_release_after_terminate\":$rel_ms,\"wal_sender_timeout\":\"$(src defaultdb -c 'show wal_sender_timeout')\"}"
for p in $(unit_pids "$UNIT"); do kill -CONT "$p" 2>/dev/null || true; done
unit_stop "$UNIT" >/dev/null; unit_reset "$UNIT"

# ---------- E: SIGTERM to main only (resume of the run D froze; it is still in CDC)
clone_cmd --resume --not-consistent; unit_start "$UNIT" "$RUN/engine.log" "${CMD[@]}"
wait_for 120 slot_active "$DB" "$NAME" >/dev/null || fail "E: slot never active"
sleep 5
main=$(unit_mainpid "$UNIT"); kill -TERM "$main"
main_exit_ms=$(wait_for 30 bash -c "! kill -0 $main 2>/dev/null || [[ \$(awk '{print \$3}' /proc/$main/stat) == Z ]]" || echo -1)
sleep 2
record "{\"scenario\":\"E_sigterm_main_only\",\"main_exit_ms\":$main_exit_ms,\"live_procs_after\":$(unit_live_count "$UNIT"),\"slot_active_after\":$(slot_active "$DB" "$NAME" && echo true || echo false)}"
unit_stop "$UNIT" >/dev/null; unit_reset "$UNIT"

# ---------- F: SIGINT to every process in the unit (pgcopydb's "fast stop"), which is
# what systemd sends with KillSignal=SIGINT. Compare with A (SIGTERM).
clone_cmd --resume --not-consistent; unit_start "$UNIT" "$RUN/engine.log" "${CMD[@]}"
wait_for 120 slot_active "$DB" "$NAME" >/dev/null || fail "F: slot never active"
sleep 5
t0=$(now_ms); for p in $(unit_pids "$UNIT"); do kill -INT "$p" 2>/dev/null || true; done
int_ms=$(wait_for 30 unit_empty "$UNIT" || echo -1)
inactive_ms=$(wait_for 60 slot_inactive "$DB" "$NAME" || echo -1)
record "{\"scenario\":\"F_sigint_all\",\"ms_to_empty\":$int_ms,\"ms_slot_inactive_after\":$inactive_ms}"
unit_stop "$UNIT" >/dev/null; unit_reset "$UNIT"

# ---------- H: SIGTERM to every process AND terminate the slot's walsender on the
# source (same non-superuser role). D showed the receive process exits at once when the
# server ends the replication stream.
clone_cmd --resume --not-consistent; unit_start "$UNIT" "$RUN/engine.log" "${CMD[@]}"
wait_for 120 slot_active "$DB" "$NAME" >/dev/null || fail "H: slot never active"
sleep 5
t0=$(now_ms); for p in $(unit_pids "$UNIT"); do kill -TERM "$p" 2>/dev/null || true; done
src "$DB" -c "select pg_terminate_backend(active_pid) from pg_replication_slots where slot_name='$NAME' and active_pid is not null" >/dev/null
h_ms=$(wait_for 30 unit_empty "$UNIT" || echo -1)
inactive_ms=$(wait_for 60 slot_inactive "$DB" "$NAME" || echo -1)
record "{\"scenario\":\"H_sigterm_plus_walsender_terminate\",\"ms_to_empty\":$h_ms,\"ms_slot_inactive_after\":$inactive_ms,\"clean_exit_logged\":$(tail -40 "$RUN/engine.log" | grep -c 'asked to terminate' || true)}"
unit_stop "$UNIT" >/dev/null; unit_reset "$UNIT"

# ---------- G: resume after F and H still converges
clone_cmd --resume --not-consistent; unit_start "$UNIT" "$RUN/engine.log" "${CMD[@]}"
wait_for 120 slot_active "$DB" "$NAME" >/dev/null || fail "G: slot never active"
sleep 5; stop_writer "$WRITER"; WRITER=""; sleep 2
"$PGCOPYDB" stream sentinel set endpos --current --source "$(src_uri "$DB")" --dir "$RUN/clone" >>"$RUN/engine.log" 2>&1
drain_ms=$(wait_for 600 unit_empty "$UNIT" || echo -1)
s_cnt=$(src "$DB" -c "select count(*)||'/'||sum(amount) from orders"); d_cnt=$(dst "$DB" -c "select count(*)||'/'||sum(amount) from orders")
record "{\"scenario\":\"G_resume_after_D_E_F\",\"drain_ms\":$drain_ms,\"source_orders\":\"$s_cnt\",\"target_orders\":\"$d_cnt\",\"match\":$( [[ "$s_cnt" == "$d_cnt" ]] && echo true || echo false)}"
unit_reset "$UNIT"

cp "$RUN/engine.log" "$OUT/engine-DEFG.log"
log "done; evidence in $EVID"
