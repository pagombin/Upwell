#!/usr/bin/env bash
# Phase 0 spike S05: does a stream reach its end position when nothing more is written,
# and does a transactional logical decoding message ("nudge") get it past a stall?
#
# For each decoding plugin (pgoutput, test_decoding):
#   N1 idle:  writes stop, endpos = current; time until pgcopydb exits on its own.
#   N2 busy-elsewhere: writes stop in this database but continue in another database of
#             the same cluster (WAL moves, nothing decodable for this slot); same timing.
#   If a run has not exited after NUDGE_AFTER seconds, emit
#   pg_logical_emit_message(true, 'upwell', 'drain nudge') in the database (as the
#   non-superuser admin) and time the exit after it.
# Output: docs/phase0/evidence/s05/results.jsonl
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
source "$HERE/lib.sh"; source "$HERE/runner.sh"

DB=${DB:-nudge}; BUSY_DB=${BUSY_DB:-busy}; ROWS=${ROWS:-50000}
NUDGE_AFTER=${NUDGE_AFTER:-30}; GIVE_UP=${GIVE_UP:-90}
OUT="$EVIDENCE_DIR/s05"; mkdir -p "$OUT"; EVID="$OUT/results.jsonl"; : >"$EVID"
export SPIKE_LOG="$OUT/spike.log"; : >"$SPIKE_LOG"
record() { printf '%s\n' "$1" >>"$EVID"; log "RESULT $1"; }
WRITER=""; BUSY=""
cleanup() { for p in $WRITER $BUSY; do stop_writer "$p"; done; [[ -n "${UNIT:-}" ]] && { unit_stop "$UNIT" >/dev/null 2>&1 || true; unit_reset "$UNIT"; }; }
trap cleanup EXIT

seed_db "$DB" "$ROWS" >/dev/null 2>&1
seed_db "$BUSY_DB" 1000 >/dev/null 2>&1

one() { # plugin scenario
  local plugin=$1 scen=$2
  NAME="upwell_s05_${plugin}_${scen}"; UNIT="upwell-eng-s05-${plugin}-${scen}"
  local run="$SPIKE_ROOT/runs/s05/$plugin-$scen"
  unit_reset "$UNIT"; reset_run "$DB" "$NAME"; rm -rf "$run"; mkdir -p "$run"
  WRITER=$(start_writer "$DB")
  local cmd=("$PGCOPYDB" clone --follow --source "$(src_uri "$DB")" --target "$(dst_uri "$DB")"
    --dir "$run/clone" --slot-name "$NAME" --origin "$NAME" --plugin "$plugin"
    --table-jobs 2 --index-jobs 2 --skip-extensions --no-owner)
  unit_start "$UNIT" "$run/engine.log" "${cmd[@]}"
  wait_for 300 bash -c "[[ \$($PGCOPYDB stream sentinel get --source '$(src_uri "$DB")' --dir '$run/clone' 2>/dev/null | awk '\$1==\"apply\"{print \$2}') == enabled ]]" >/dev/null || fail "$plugin/$scen: no CDC"
  sleep 8
  stop_writer "$WRITER"; WRITER=""
  [[ $scen == busy ]] && BUSY=$(start_writer "$BUSY_DB")
  sleep 3
  local t0; t0=$(now_ms)
  "$PGCOPYDB" stream sentinel set endpos --current --source "$(src_uri "$DB")" --dir "$run/clone" >>"$run/engine.log" 2>&1
  local endpos; endpos=$(sentinel_field "$DB" "$run/clone" endpos)
  local exit_ms nudged=false nudge_ok=null after_nudge_ms=null
  if exit_ms=$(wait_for "$NUDGE_AFTER" unit_empty "$UNIT"); then :; else
    exit_ms=-1; nudged=true
    local n0; n0=$(now_ms)
    if src "$DB" -c "select pg_logical_emit_message(true, 'upwell', 'drain nudge')" >>"$run/nudge.out" 2>&1; then nudge_ok=true; else nudge_ok=false; fi
    after_nudge_ms=$(wait_for "$GIVE_UP" unit_empty "$UNIT" || echo -1)
  fi
  [[ -n "$BUSY" ]] && { stop_writer "$BUSY"; BUSY=""; }
  local replay; replay=$(sentinel_field "$DB" "$run/clone" replay_lsn)
  local s d; s=$(src "$DB" -c "select count(*)||'/'||sum(amount) from orders"); d=$(dst "$DB" -c "select count(*)||'/'||sum(amount) from orders")
  record "{\"plugin\":\"$plugin\",\"scenario\":\"$scen\",\"endpos\":\"$endpos\",\"exited_without_nudge_ms\":$exit_ms,\"nudged\":$nudged,\"nudge_sql_ok\":$nudge_ok,\"exit_ms_after_nudge\":$after_nudge_ms,\"replay_lsn\":\"$replay\",\"match\":$( [[ "$s" == "$d" ]] && echo true || echo false),\"source\":\"$s\",\"target\":\"$d\",\"nudge_output\":\"$(tr -d '"\n' <"$run/nudge.out" 2>/dev/null | head -c 200)\"}"
  unit_stop "$UNIT" >/dev/null; unit_reset "$UNIT"
  cp "$run/engine.log" "$OUT/engine-$plugin-$scen.log"
}

for plugin in ${PLUGINS:-pgoutput test_decoding}; do
  for scen in idle busy; do one "$plugin" "$scen"; done
done
log "done; evidence in $EVID"
