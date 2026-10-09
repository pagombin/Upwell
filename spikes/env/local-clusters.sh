#!/usr/bin/env bash
# Creates two local PostgreSQL clusters that mimic the Phase 0 test pair:
#   source (port 55432): Standard-like. wal_level=logical, small max_slot_wal_keep_size,
#                        admin user "doadmin" with REPLICATION and CREATEDB but NOT superuser.
#   target (port 55433): Advanced-like. "doadmin" non-superuser, plus a pre-existing
#                        doadmin.aries_* table in every database (as Advanced has).
# Usage: sudo bash local-clusters.sh [up|down|status]
# Data lives in ${UPWELL_SPIKE_ROOT:-/var/tmp/upwell-spike}.
set -euo pipefail

PGVER="${PGVER:-16}"
BIN="${PGBIN:-/usr/lib/postgresql/${PGVER}/bin}"
ROOT="${UPWELL_SPIKE_ROOT:-/var/tmp/upwell-spike}"
SRC_PORT="${SRC_PORT:-55432}"
DST_PORT="${DST_PORT:-55433}"
# TARGET_ONLY=1 creates only the target, for example a newer major version:
#   TARGET_ONLY=1 PGBIN=/usr/local/pg18/bin UPWELL_SPIKE_ROOT=/var/tmp/upwell-pg18 DST_PORT=55434 bash local-clusters.sh up
TARGET_ONLY="${TARGET_ONLY:-0}"
PASS="${UPWELL_SPIKE_PASSWORD:-spike-password-not-secret}"

as_pg() { runuser -u postgres -- "$@"; }

init_cluster() { # name port
  local port=$2 dir="$ROOT/$1"
  install -d -o postgres -g postgres "$ROOT"
  if [[ -f "$dir/PG_VERSION" ]]; then return; fi
  install -d -o postgres -g postgres "$dir"
  as_pg "$BIN/initdb" -D "$dir" -U postgres --auth=trust --locale=C.UTF-8 --encoding=UTF8 >/dev/null
  cat >>"$dir/postgresql.conf" <<CONF
port = $port
listen_addresses = '127.0.0.1'
unix_socket_directories = '$ROOT'
wal_level = logical
max_replication_slots = 20
max_wal_senders = 20
max_slot_wal_keep_size = 1GB
wal_sender_timeout = 60s
log_line_prefix = '%m [%p] %u@%d '
logging_collector = off
CONF
  cat >"$dir/pg_hba.conf" <<HBA
local all postgres trust
local all all scram-sha-256
host  all all 127.0.0.1/32 scram-sha-256
host  replication all 127.0.0.1/32 scram-sha-256
HBA
}

start_cluster() { # name
  local dir="$ROOT/$1"
  if as_pg "$BIN/pg_ctl" -D "$dir" status >/dev/null 2>&1; then return; fi
  as_pg "$BIN/pg_ctl" -D "$dir" -l "$ROOT/$1.log" -w start >/dev/null
}

psql_super() { # port sql...
  local port=$1; shift
  as_pg psql -X -q -v ON_ERROR_STOP=1 -h "$ROOT" -p "$port" -U postgres "$@"
}

seed() {
  # doadmin mirrors the Standard admin: REPLICATION, CREATEDB, CREATEROLE, not superuser.
  ports="$SRC_PORT $DST_PORT"
  [[ $TARGET_ONLY == 1 ]] && ports=$DST_PORT
  for port in $ports; do
    psql_super "$port" -d postgres <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='doadmin') THEN
    CREATE ROLE doadmin LOGIN REPLICATION CREATEDB CREATEROLE PASSWORD '$PASS';
  END IF;
END \$\$;
GRANT pg_signal_backend, pg_read_all_stats, pg_monitor TO doadmin;
SELECT 'CREATE DATABASE defaultdb OWNER doadmin'
 WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname='defaultdb') \gexec
SQL
  done
  # Advanced-like target: doadmin schema with an aries_* table in defaultdb.
  psql_super $DST_PORT -d defaultdb <<'SQL'
CREATE SCHEMA IF NOT EXISTS doadmin AUTHORIZATION doadmin;
CREATE TABLE IF NOT EXISTS doadmin.aries_health (id int primary key, ts timestamptz default now());
SQL
  # pgcopydb's CDC apply needs the replication-origin functions and SET on
  # session_replication_role on the target. Both are superuser-only by default (Phase 0
  # findings F-ORIGIN, F-SRR; spike S02). Whether Advanced's doadmin has them
  # is unverified, so the grant is simulated here (set ORIGIN_GRANTS=0 to reproduce the failure).
  # Function grants are per database, so grant in template1 for databases created later.
  if [[ "${ORIGIN_GRANTS:-1}" == 1 ]]; then
    for db in template1 postgres defaultdb; do
      psql_super $DST_PORT -d "$db" -c "GRANT EXECUTE ON FUNCTION
        pg_replication_origin_create(text), pg_replication_origin_drop(text),
        pg_replication_origin_oid(text), pg_replication_origin_session_setup(text),
        pg_replication_origin_session_reset(), pg_replication_origin_session_is_setup(),
        pg_replication_origin_session_progress(boolean),
        pg_replication_origin_xact_setup(pg_lsn, timestamptz), pg_replication_origin_xact_reset(),
        pg_replication_origin_advance(text, pg_lsn), pg_replication_origin_progress(text, boolean)
        TO doadmin"
    done
    # The apply step also SETs session_replication_role = replica (finding F-SRR).
    psql_super $DST_PORT -d postgres -c "GRANT SET ON PARAMETER session_replication_role TO doadmin"
  fi
}

case "${1:-up}" in
  up)
    if [[ $TARGET_ONLY != 1 ]]; then init_cluster source "$SRC_PORT"; start_cluster source; fi
    init_cluster target "$DST_PORT"; start_cluster target
    seed
    [[ $TARGET_ONLY == 1 ]] || echo "source: postgres://doadmin@127.0.0.1:$SRC_PORT/defaultdb"
    echo "target: postgres://doadmin@127.0.0.1:$DST_PORT/defaultdb"
    ;;
  down)
    for n in source target; do as_pg "$BIN/pg_ctl" -D "$ROOT/$n" -m fast stop || true; done ;;
  status)
    for n in source target; do as_pg "$BIN/pg_ctl" -D "$ROOT/$n" status || true; done ;;
  *) echo "usage: $0 [up|down|status]" >&2; exit 2 ;;
esac
