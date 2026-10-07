#!/usr/bin/env bash
# Phase 0 spike S03: can the cluster admin (not superuser) apply and undo the cutover
# write freeze? Safe to run against a real Standard cluster: everything happens in a
# throwaway database and role that the script creates and drops.
#
# Checks, as the admin user from UPWELL_SRC_URI (default: local doadmin):
#   1  ALTER DATABASE ... SET default_transaction_read_only = on on a database the admin owns
#   2  the same on a database owned by ANOTHER role (customer databases may be)
#   3  a new session of an application role cannot write after the freeze
#   4  a session opened BEFORE the freeze can still write (so sessions must be terminated)
#   5  the admin can terminate the application role's session (pg_signal_backend)
#   6  a frozen session can turn read-only off for itself (freeze is a guard, not a lock)
#   7  ALTER DATABASE ... RESET undoes it for new sessions
#   8  pg_logical_emit_message is allowed for the admin (used by the drain nudge)
# Output: docs/phase0/evidence/s03/results.jsonl
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
source "$HERE/lib.sh"
OUT="$EVIDENCE_DIR/s03"; mkdir -p "$OUT"; EVID="$OUT/results.jsonl"; : >"$EVID"
export SPIKE_LOG="$OUT/spike.log"; : >"$SPIKE_LOG"
record() { printf '%s\n' "$1" >>"$EVID"; log "RESULT $1"; }
P=upwell_freeze_probe; P2=upwell_freeze_probe_other; APP=upwell_probe_app
APP_PASS="probe-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
HOSTPORT=$(sed -E 's|^postgres(ql)?://[^@]*@||' <<<"$SRC_BASE")
app_uri() { echo "postgres://$APP:$APP_PASS@$HOSTPORT/$1?$SSL_QS"; }
app() { local db=$1; shift; PGPASSFILE=/dev/null psql -X -q -At -v ON_ERROR_STOP=1 "$(app_uri "$db")" "$@"; }
try() { local out; if out=$("$@" 2>&1); then echo "{\"ok\":true}"; else echo "{\"ok\":false,\"error\":\"$(tr -d '"\\\n' <<<"$out" | head -c 200)\"}"; fi; }

cleanup() {
  src defaultdb -c "drop database if exists $P with (force)" >/dev/null 2>&1 || true
  # P2 is owned by the probe role, so the admin needs membership to drop it.
  src defaultdb -c "grant $APP to current_user" >/dev/null 2>&1 || true
  src defaultdb -c "drop database if exists $P2 with (force)" >/dev/null 2>&1 || true
  src defaultdb -c "revoke $APP from current_user" >/dev/null 2>&1 || true
  src defaultdb -c "drop role if exists $APP" >/dev/null 2>&1 || true
  [[ -n "${HOLD:-}" ]] && kill "$HOLD" 2>/dev/null || true
}
trap cleanup EXIT
cleanup

record "{\"check\":\"0_context\",\"result\":$(src defaultdb -c "select json_build_object('user',current_user,'server_version',current_setting('server_version'),'superuser',(select rolsuper from pg_roles where rolname=current_user),'createrole',(select rolcreaterole from pg_roles where rolname=current_user),'member_pg_signal_backend',pg_has_role('pg_signal_backend','member'),'member_pg_read_all_stats',pg_has_role('pg_read_all_stats','member'))")}"

src defaultdb -c "create role $APP login password '$APP_PASS'"
src defaultdb -c "create database $P"
src $P -c "create table t(i int); grant all on t to $APP; grant usage on schema public to $APP"
# A second database owned by the application role (the admin is NOT its owner).
src defaultdb -c "grant $APP to current_user" >/dev/null 2>&1 || true
src defaultdb -c "create database $P2 owner $APP" >/dev/null 2>&1 || src defaultdb -c "create database $P2"
src defaultdb -c "revoke $APP from current_user" >/dev/null 2>&1 || true

# Session opened before the freeze, held open: writes every second while alive.
( PGPASSFILE=/dev/null psql -X -q "$(app_uri $P)" >/dev/null 2>"$OUT/held-session.err" <<'SQL'
select pg_sleep(0);
\o /dev/null
select 1;
do $$ begin for i in 1..60 loop insert into t values (i); commit; perform pg_sleep(1); end loop; end $$;
SQL
) & HOLD=$!
sleep 2
held_pid=$(src $P -c "select pid from pg_stat_activity where usename='$APP' and datname='$P' limit 1")

record "{\"check\":\"1_freeze_owned_db\",\"result\":$(try src defaultdb -c "alter database $P set default_transaction_read_only = on")}"
record "{\"check\":\"2_freeze_db_owned_by_other_role\",\"owner\":\"$(src defaultdb -c "select pg_get_userbyid(datdba) from pg_database where datname='$P2'")\",\"result\":$(try src defaultdb -c "alter database $P2 set default_transaction_read_only = on")}"
record "{\"check\":\"3_new_app_session_write_blocked\",\"result\":$(try app $P -c "insert into t values (-1)")}"
before=$(src $P -c "select count(*) from t"); sleep 3; after=$(src $P -c "select count(*) from t")
record "{\"check\":\"4_old_session_still_writes\",\"rows_before\":$before,\"rows_3s_later\":$after,\"still_writing\":$( (( after > before )) && echo true || echo false)}"
record "{\"check\":\"5_terminate_app_session\",\"pid\":\"$held_pid\",\"returned\":\"$(src $P -c "select pg_terminate_backend($held_pid)" 2>&1 | tr -d '"')\"}"
record "{\"check\":\"6_session_can_unfreeze_itself\",\"result\":$(try app $P -c "set default_transaction_read_only = off" -c "insert into t values (-2)")}"
record "{\"check\":\"7_reset_undoes\",\"reset\":$(try src defaultdb -c "alter database $P reset default_transaction_read_only"),\"new_session_write\":$(try app $P -c "insert into t values (-3)")}"
record "{\"check\":\"8_emit_logical_message\",\"result\":$(try src $P -c "select pg_logical_emit_message(true, 'upwell', 'probe')")}"
log "done"
