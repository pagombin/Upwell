#!/usr/bin/env bash
# Phase 0 spike S02: what pgcopydb's CDC apply needs on the target, and what happens
# when the target admin (not superuser) lacks it.
#
# Part 1, probe (safe on a real Advanced cluster, read-only): reports whether the target
#   user can execute the replication-origin functions and SET session_replication_role.
# Part 2, matrix (local clusters only, needs local superuser via runuser postgres): runs a
#   short CDC with writes under three privilege sets and records, for each, whether the
#   engine log shows any error, whether pgcopydb claims the end position was reached, and
#   whether the target actually matches the source.
#     none      no extra grants
#     origin    EXECUTE on pg_replication_origin_* only
#     full      origin + GRANT SET ON PARAMETER session_replication_role (PostgreSQL 15+)
# Output: docs/phase0/evidence/s02/results.jsonl
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
source "$HERE/lib.sh"; source "$HERE/runner.sh"
OUT="$EVIDENCE_DIR/s02"; mkdir -p "$OUT"; EVID="$OUT/results.jsonl"; : >"$EVID"
export SPIKE_LOG="$OUT/spike.log"; : >"$SPIKE_LOG"
record() { printf '%s\n' "$1" >>"$EVID"; log "RESULT $1"; }
DB=${DB:-privs}

probe() { # db -> JSON
  dst "$1" -c "select json_build_object(
    'server_version', current_setting('server_version'),
    'user', current_user,
    'superuser', (select rolsuper from pg_roles where rolname=current_user),
    'origin_create', has_function_privilege('pg_replication_origin_create(text)','execute'),
    'origin_drop', has_function_privilege('pg_replication_origin_drop(text)','execute'),
    'origin_session_setup', has_function_privilege('pg_replication_origin_session_setup(text)','execute'),
    'origin_xact_setup', has_function_privilege('pg_replication_origin_xact_setup(pg_lsn,timestamptz)','execute'),
    'origin_progress', has_function_privilege('pg_replication_origin_progress(text,boolean)','execute'),
    'set_session_replication_role', case when current_setting('server_version_num')::int >= 150000
         then has_parameter_privilege('session_replication_role','SET') end)"
}
record "{\"part\":\"probe\",\"target_db\":\"defaultdb\",\"result\":$(probe defaultdb)}"
[[ "${PROBE_ONLY:-0}" == 1 || -n "${UPWELL_DST_URI:-}" ]] && { log "probe only"; exit 0; }

superdst() { runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1 -h "$SPIKE_ROOT" -p 55433 -d "$1" -c "$2" >/dev/null; }
ORIGIN_FUNCS="pg_replication_origin_create(text), pg_replication_origin_drop(text),
  pg_replication_origin_oid(text), pg_replication_origin_session_setup(text),
  pg_replication_origin_session_reset(), pg_replication_origin_session_is_setup(),
  pg_replication_origin_session_progress(boolean), pg_replication_origin_xact_setup(pg_lsn, timestamptz),
  pg_replication_origin_xact_reset(), pg_replication_origin_advance(text, pg_lsn),
  pg_replication_origin_progress(text, boolean)"
set_privs() { # level; applied in template1 so the freshly created target db inherits it
  local level=$1
  for d in template1 "$DB"; do
    superdst "$d" "revoke execute on function $ORIGIN_FUNCS from doadmin" || true
    [[ $level != none ]] && superdst "$d" "grant execute on function $ORIGIN_FUNCS to doadmin"
  done
  superdst postgres "revoke set on parameter session_replication_role from doadmin" || true
  [[ $level == full ]] && superdst postgres "grant set on parameter session_replication_role to doadmin"
  return 0
}

seed_db "$DB" 20000 >/dev/null 2>&1
for level in none origin full; do
  NAME="upwell_s02_${level}"; UNIT="upwell-eng-s02-$level"; RUN="$SPIKE_ROOT/runs/s02/$level"
  unit_reset "$UNIT"; reset_run "$DB" "$NAME"; rm -rf "$RUN"; mkdir -p "$RUN"
  set_privs "$level"
  log_before=$(grep -c "permission denied" "$SPIKE_ROOT/target.log" || true)
  WRITER=$(start_writer "$DB")
  unit_start "$UNIT" "$RUN/engine.log" "$PGCOPYDB" clone --follow --source "$(src_uri "$DB")" \
    --target "$(dst_uri "$DB")" --dir "$RUN/clone" --slot-name "$NAME" --origin "$NAME" \
    --table-jobs 2 --index-jobs 2 --skip-extensions --no-owner
  sleep 20
  stop_writer "$WRITER"; sleep 2
  "$PGCOPYDB" stream sentinel set endpos --current --source "$(src_uri "$DB")" --dir "$RUN/clone" >>"$RUN/engine.log" 2>&1 || true
  exited_ms=$(wait_for 120 unit_empty "$UNIT" || echo -1)
  claimed=$(grep -c "Follow mode is now done, reached endpos" "$RUN/engine.log" || true)
  errs=$(grep -c " ERROR " "$RUN/engine.log" || true)
  first_err=$(grep -m1 " ERROR " "$RUN/engine.log" | cut -c60- | tr -d '"\\' | head -c 160 || true)
  s=$(src "$DB" -c "select count(*)||'/'||sum(amount) from orders")
  d=$(dst "$DB" -c "select count(*)||'/'||coalesce(sum(amount),0) from orders" 2>/dev/null || echo "no table")
  origin=$(runuser -u postgres -- psql -X -At -h "$SPIKE_ROOT" -p 55433 -d postgres -c "select remote_lsn from pg_replication_origin_status s join pg_replication_origin o on o.roident=s.local_id where o.roname='$NAME'" 2>/dev/null || true)
  record "{\"part\":\"matrix\",\"privileges\":\"$level\",\"exited_ms\":$exited_ms,\"engine_log_errors\":$errs,\"first_error\":\"$first_err\",\"claims_endpos_reached\":$claimed,\"sentinel_replay_lsn\":\"$(sentinel_field "$DB" "$RUN/clone" replay_lsn)\",\"target_origin_lsn\":\"$origin\",\"source\":\"$s\",\"target\":\"$d\",\"match\":$( [[ "$s" == "$d" ]] && echo true || echo false),\"target_server_permission_errors\":$(( $(grep -c "permission denied" "$SPIKE_ROOT/target.log" || true) - log_before ))}"
  unit_stop "$UNIT" >/dev/null 2>&1 || true; unit_reset "$UNIT"
  cp "$RUN/engine.log" "$OUT/engine-$level.log"
done
set_privs full
for level in none origin full; do drop_slot "$DB" "upwell_s02_${level}"; done
log "done"
