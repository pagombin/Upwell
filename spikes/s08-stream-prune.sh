#!/usr/bin/env bash
# Phase 0 spike S08: can `pgcopydb stream prune` free applied CDC files while a
# `clone --follow` run is streaming, without breaking the run?
#
# Facts read from pgcopydb 0.18 source before running (see findings):
#   - CDC files rotate at maxReplayDBSize; `clone` exposes no option for it, so the
#     clone default of 1 GiB applies (cli_clone_follow.c). Prune can only remove CLOSED
#     file pairs whose end LSN is below the sentinel replay_lsn.
#   - Prune has a direct mode (--dir, opens source.db) and a coordinator mode
#     (--host/--port, asks the running follow process over TCP).
# The spike writes > 1 GiB of changes so at least one rotation happens, then:
#   P1 prune --dry-run in coordinator mode, P2 prune in coordinator mode,
#   P3 prune in direct mode while running, and finally drains and compares.
# Output: docs/phase0/evidence/s08/results.jsonl
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
source "$HERE/lib.sh"; source "$HERE/runner.sh"
OUT="$EVIDENCE_DIR/s08"; mkdir -p "$OUT"; EVID="$OUT/results.jsonl"; : >"$EVID"
export SPIKE_LOG="$OUT/spike.log"; : >"$SPIKE_LOG"
record() { printf '%s\n' "$1" >>"$EVID"; log "RESULT $1"; }
DB=${DB:-prune}; NAME=upwell_s08_prune; UNIT=upwell-eng-s08-prune; RUN="$SPIKE_ROOT/runs/s08"
COORD_PORT=${COORD_PORT:-57001}; BATCHES=${BATCHES:-14}
cleanup() { unit_stop "$UNIT" >/dev/null 2>&1 || true; unit_reset "$UNIT"; }
trap cleanup EXIT

seed_db "$DB" 1000 >/dev/null 2>&1
src "$DB" -c "create table big(id bigint primary key, pad text)"
unit_reset "$UNIT"; reset_run "$DB" "$NAME"; rm -rf "$RUN"; mkdir -p "$RUN"
unit_start "$UNIT" "$RUN/engine.log" "$PGCOPYDB" clone --follow --source "$(src_uri "$DB")" \
  --target "$(dst_uri "$DB")" --dir "$RUN/clone" --slot-name "$NAME" --origin "$NAME" \
  --table-jobs 2 --index-jobs 2 --skip-extensions --no-owner --host 127.0.0.1 --port "$COORD_PORT"
wait_for 300 bash -c "[[ \$($PGCOPYDB stream sentinel get --source '$(src_uri "$DB")' --dir '$RUN/clone' 2>/dev/null | awk '\$1==\"apply\"{print \$2}') == enabled ]]" >/dev/null || fail "no CDC"

cdc_bytes() { du -sb "$RUN/clone/cdc" | cut -f1; }
cdc_files() { find "$RUN/clone/cdc" -name '*-output.db' | wc -l; }
log "writing $BATCHES batches of ~100 MB"
for b in $(seq 1 "$BATCHES"); do
  src "$DB" -c "insert into big select g, repeat(md5(g::text), 30) from generate_series(($b-1)*100000+1, $b*100000) g"
  log "batch $b: cdc=$(cdc_bytes) bytes, files=$(cdc_files)"
done
rot_ms=$(wait_for 900 bash -c "(( \$(find '$RUN/clone/cdc' -name '*-output.db' | wc -l) >= 2 ))" || echo -1)
record "{\"step\":\"rotation\",\"output_files\":$(cdc_files),\"ms_waited\":$rot_ms,\"rotated_log_lines\":$(grep -c 'rotating' "$RUN/engine.log" || true)}"
# Wait for apply to pass the first file's end so something is prunable.
prunable() { "$PGCOPYDB" stream prune --dir "$RUN/clone" --dry-run --host 127.0.0.1 --port "$COORD_PORT" 2>&1 | grep -q "would remove [1-9]"; }
caught_ms=$(wait_for 1800 prunable || echo -1)
record "{\"step\":\"wait_until_prunable\",\"ms\":$caught_ms}"

before=$(cdc_bytes)
p1=$("$PGCOPYDB" stream prune --dir "$RUN/clone" --dry-run --host 127.0.0.1 --port "$COORD_PORT" 2>&1 | grep -E "DRY RUN|ERROR" | tail -1 | cut -c60- | tr -d '"')
record "{\"step\":\"P1_coordinator_dry_run\",\"out\":\"$p1\",\"bytes_unchanged\":$( [[ $(cdc_bytes) == "$before" ]] && echo true || echo false)}"
p2=$("$PGCOPYDB" stream prune --dir "$RUN/clone" --host 127.0.0.1 --port "$COORD_PORT" 2>&1 | grep -E "Removed|ERROR" | tail -1 | cut -c60- | tr -d '"')
after=$(cdc_bytes)
record "{\"step\":\"P2_coordinator_prune\",\"out\":\"$p2\",\"bytes_before\":$before,\"bytes_after\":$after,\"unit_still_running\":$(unit_empty "$UNIT" && echo false || echo true)}"
# More changes, another rotation, then direct mode while the run is live.
for b in $(seq $((BATCHES+1)) $((BATCHES*2))); do
  src "$DB" -c "insert into big select g, repeat(md5(g::text), 30) from generate_series(($b-1)*100000+1, $b*100000) g"
done
sleep 60
before=$(cdc_bytes)
p3=$("$PGCOPYDB" stream prune --dir "$RUN/clone" 2>&1 | grep -E "Removed|ERROR|locked|busy" | tail -1 | cut -c60- | tr -d '"')
record "{\"step\":\"P3_direct_prune_while_running\",\"out\":\"$p3\",\"bytes_before\":$before,\"bytes_after\":$(cdc_bytes),\"unit_still_running\":$(unit_empty "$UNIT" && echo false || echo true)}"
"$PGCOPYDB" stream sentinel set endpos --current --source "$(src_uri "$DB")" --dir "$RUN/clone" >>"$RUN/engine.log" 2>&1
drain_ms=$(wait_for 3600 unit_empty "$UNIT" || echo -1)
s=$(src "$DB" -c "select count(*)||'/'||sum(length(pad)) from big"); d=$(dst "$DB" -c "select count(*)||'/'||sum(length(pad)) from big")
record "{\"step\":\"drain_and_compare\",\"drain_ms\":$drain_ms,\"source\":\"$s\",\"target\":\"$d\",\"match\":$( [[ "$s" == "$d" ]] && echo true || echo false),\"engine_log_errors\":$(grep -c ' ERROR ' "$RUN/engine.log" || true)}"
cp "$RUN/engine.log" "$OUT/engine.log"
log done
