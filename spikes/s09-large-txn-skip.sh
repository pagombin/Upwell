#!/usr/bin/env bash
# Phase 0 spike S09: reproduce the S08 data loss (three ~100 MB transactions never applied,
# sentinel replay_lsn moved past them, no error) and find the trigger.
#   MODE=quiet   large transactions with idle gaps, nothing else touches the run
#   MODE=poll    same, plus coordinator `stream prune --dry-run` every 0.2 s (what S08's
#                wait loop did from the moment the missing transactions were written)
# No prune ever deletes anything here, so a missing batch cannot be blamed on prune.
# Output: docs/phase0/evidence/s09/results.jsonl
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
source "$HERE/lib.sh"; source "$HERE/runner.sh"
MODE=${MODE:-quiet}; DB=${DB:-skip}; NAME="upwell_s09_$MODE"; UNIT="upwell-eng-s09-$MODE"
RUN="$SPIKE_ROOT/runs/s09-$MODE"; OUT="$EVIDENCE_DIR/s09"; mkdir -p "$OUT"; EVID="$OUT/results.jsonl"
export SPIKE_LOG="$OUT/spike-$MODE${RUNIDX:+-$RUNIDX}.log"; : >"$SPIKE_LOG"
record() { printf '%s\n' "$1" >>"$EVID"; log "RESULT $1"; }
BATCHES=${BATCHES:-8}; ROWS=${ROWS:-60000}; GAP=${GAP:-45}; PORT=${PORT:-57009}
POLLER=""; SAMPLER=""
cleanup() { [[ -n "$POLLER" ]] && kill "$POLLER" 2>/dev/null || true; [[ -n "$SAMPLER" ]] && kill "$SAMPLER" 2>/dev/null || true; unit_stop "$UNIT" >/dev/null 2>&1 || true; unit_reset "$UNIT"; drop_slot "$DB" "$NAME"; }
trap cleanup EXIT

seed_db "$DB" 1000 >/dev/null 2>&1
src "$DB" -c "create table big(id bigint primary key, batch int, pad text)"
unit_reset "$UNIT"; reset_run "$DB" "$NAME"; rm -rf "$RUN"; mkdir -p "$RUN"
unit_start "$UNIT" "$RUN/engine.log" "$PGCOPYDB" clone --follow --source "$(src_uri "$DB")" \
  --target "$(dst_uri "$DB")" --dir "$RUN/clone" --slot-name "$NAME" --origin "$NAME" \
  --skip-extensions --no-owner --host 127.0.0.1 --port "$PORT" \
  ${JOBS:+--table-jobs "$JOBS" --index-jobs "$JOBS"}
wait_for 300 bash -c "[[ \$($PGCOPYDB stream sentinel get --source '$(src_uri "$DB")' --dir '$RUN/clone' 2>/dev/null | awk '\$1==\"apply\"{print \$2}') == enabled ]]" >/dev/null || fail "no CDC"
bash "$HERE/sample-cdc-rate.sh" "$DB" "$RUN/clone" "$NAME" "$OUT/cdc-rate-$MODE${RUNIDX:+-$RUNIDX}.csv" 5 >/dev/null 2>&1 </dev/null &
SAMPLER=$!
if [[ $MODE == poll ]]; then
  ( while true; do "$PGCOPYDB" stream prune --dir "$RUN/clone" --dry-run --host 127.0.0.1 --port "$PORT" >/dev/null 2>&1; sleep 0.2; done ) &
  POLLER=$!
fi
for b in $(seq 1 "$BATCHES"); do
  src "$DB" -c "insert into big select g, $b, repeat(md5(g::text), 30) from generate_series(($b-1)*$ROWS+1, $b*$ROWS) g"
  log "batch $b committed at $(src "$DB" -c 'select pg_current_wal_lsn()'); sentinel: $(sentinel "$DB" "$RUN/clone" | awk '/lsn/{printf "%s=%s ", $1, $2}') origin=$(dst defaultdb -c "select pg_replication_origin_progress('$NAME', false)")"
  sleep "$GAP"
done
sleep 60
[[ -n "$POLLER" ]] && { kill "$POLLER"; POLLER=""; }
"$PGCOPYDB" stream sentinel set endpos --current --source "$(src_uri "$DB")" --dir "$RUN/clone" >>"$RUN/engine.log" 2>&1
src "$DB" -c "select pg_logical_emit_message(true, 'upwell', 'drain nudge')" >/dev/null
drain_ms=$(wait_for 1800 unit_empty "$UNIT" || echo -1)
per_batch=$(dst "$DB" -c "select string_agg(batch||':'||n, ' ' order by batch) from (select batch, count(*) n from big group by batch) s")
s=$(src "$DB" -c "select count(*) from big"); d=$(dst "$DB" -c "select count(*) from big")
record "{\"mode\":\"$MODE\",\"run\":\"${RUNIDX:-1}\",\"jobs\":\"${JOBS:-default}\",\"batches\":$BATCHES,\"rows_per_batch\":$ROWS,\"gap_s\":$GAP,\"drain_ms\":$drain_ms,\"source_rows\":$s,\"target_rows\":$d,\"match\":$( [[ $s == "$d" ]] && echo true || echo false),\"target_per_batch\":\"$per_batch\",\"claims_endpos_reached\":$(grep -c 'Follow mode is now done, reached endpos' "$RUN/engine.log" || true),\"engine_log_errors\":$(grep -c ' ERROR ' "$RUN/engine.log" || true)}"
grep -v "dry_run=1" "$RUN/engine.log" >"$OUT/engine-$MODE${RUNIDX:+-$RUNIDX}.log" || true
log done
