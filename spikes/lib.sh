# Shared helpers for Phase 0 spikes. Source this file; do not execute it.
# Connection details default to the local pair from env/local-clusters.sh and can be
# pointed at real clusters with UPWELL_SRC_URI / UPWELL_DST_URI (admin user URIs
# WITHOUT the password) plus a PGPASSFILE.
set -euo pipefail

SPIKE_ROOT="${UPWELL_SPIKE_ROOT:-/var/tmp/upwell-spike}"
SRC_BASE="${UPWELL_SRC_URI:-postgres://doadmin@127.0.0.1:55432}"
DST_BASE="${UPWELL_DST_URI:-postgres://doadmin@127.0.0.1:55433}"
SSL_QS="${UPWELL_SSL_QS:-sslmode=disable}"
PGCOPYDB="${PGCOPYDB:-pgcopydb}"
EVIDENCE_DIR="${EVIDENCE_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/docs/phase0/evidence}"

if [[ -z "${PGPASSFILE:-}" && -z "${UPWELL_SRC_URI:-}" ]]; then
  export PGPASSFILE="$SPIKE_ROOT/pgpass"
  printf '127.0.0.1:*:*:doadmin:%s\n' "${UPWELL_SPIKE_PASSWORD:-spike-password-not-secret}" >"$PGPASSFILE"
  chmod 600 "$PGPASSFILE"
fi

src_uri() { echo "$SRC_BASE/$1?$SSL_QS"; }
dst_uri() { echo "$DST_BASE/$1?$SSL_QS"; }
src() { local db=$1; shift; psql -X -q -At -v ON_ERROR_STOP=1 "$(src_uri "$db")" "$@"; }
dst() { local db=$1; shift; psql -X -q -At -v ON_ERROR_STOP=1 "$(dst_uri "$db")" "$@"; }

now_ms() { date +%s%3N; }
log()  { printf '%s %s\n' "$(date -u +%H:%M:%S.%3N)" "$*" | tee -a "${SPIKE_LOG:-/dev/null}" >&2; }
fail() { log "FAIL: $*"; exit 1; }

# Wait until cmd succeeds, polling every 0.2 s, up to $1 seconds. Prints elapsed ms.
wait_for() {
  local timeout=$1; shift
  local start; start=$(now_ms)
  while ! "$@" >/dev/null 2>&1; do
    if (( $(now_ms) - start > timeout * 1000 )); then return 1; fi
    sleep 0.2
  done
  echo $(( $(now_ms) - start ))
}

slot_active()   { [[ "$(src "$1" -c "select active from pg_replication_slots where slot_name='$2'")" == t ]]; }
slot_inactive() { [[ "$(src "$1" -c "select active from pg_replication_slots where slot_name='$2'")" == f ]]; }
slot_gone()     { [[ -z "$(src "$1" -c "select 1 from pg_replication_slots where slot_name='$2'")" ]]; }

# Create database $1 on the source with N rows (default 200k) across two tables,
# owned by doadmin, plus a sequence.
seed_db() {
  local db=$1 rows=${2:-200000}
  src defaultdb -c "drop database if exists \"$db\" with (force)"
  dst defaultdb -c "drop database if exists \"$db\" with (force)"
  src defaultdb -c "create database \"$db\""
  src "$db" <<SQL
create table orders(id bigserial primary key, customer int not null, amount numeric(12,2), note text, created_at timestamptz default now());
create table items(order_id bigint references orders(id), line int, sku text, qty int, primary key(order_id,line));
insert into orders(customer,amount,note) select g%1000, (g%9999)/100.0, md5(g::text) from generate_series(1,$rows) g;
insert into items select id, l, 'sku-'||(id*l)%5000, l from orders, generate_series(1,2) l;
create index on orders(customer);
analyze orders, items;
SQL
}

# Background writer: one small transaction every ~50 ms until stop_writer. Prints its PID.
start_writer() {
  local db=$1 stopf; stopf=$(mktemp -u "$SPIKE_ROOT/writer-stop.XXXXXX")
  ( while [[ ! -e $stopf ]]; do
      src "$db" -c "insert into orders(customer,amount,note) values ((random()*1000)::int, 1.00, 'w'); update orders set amount=amount+1 where id=(select max(id)-5 from orders)" >/dev/null 2>&1 || true
      sleep 0.05
    done ) >/dev/null 2>&1 </dev/null &
  local pid=$!
  echo "$stopf" >"$SPIKE_ROOT/writer-$pid.stop"
  echo $pid
}
# Stop a writer and wait until its last transaction has finished (no in-flight commit
# can land after an end position taken right afterwards).
stop_writer() {
  local pid=$1 f="$SPIKE_ROOT/writer-$1.stop"
  [[ -f $f ]] && touch "$(cat "$f")"
  while kill -0 "$pid" 2>/dev/null; do sleep 0.05; done
  [[ -f $f ]] && rm -f "$(cat "$f")" "$f"
  return 0
}

# Plain-text sentinel (the --json output is unreliable in 0.15; see spec).
sentinel() { "$PGCOPYDB" stream sentinel get --source "$(src_uri "$1")" --dir "$2" 2>/dev/null; }
sentinel_field() { sentinel "$1" "$2" | awk -v k="$3" '$1==k {print $2}'; }

pg_lsn_diff() { src defaultdb -c "select pg_wal_lsn_diff('$1','$2')::bigint"; }

# Drop everything a run named $2 left in database $1 (slot, publication, origin) and the
# target database copy. Used between spike scenarios.
reset_run() {
  local db=$1 name=$2
  src "$db" -c "select pg_terminate_backend(active_pid) from pg_replication_slots where slot_name='$name' and active_pid is not null" >/dev/null || true
  sleep 0.5
  src "$db" -c "select pg_drop_replication_slot('$name') from pg_replication_slots where slot_name='$name'" >/dev/null || true
  src "$db" -c "drop publication if exists \"$name\"" >/dev/null || true
  # Origins live in a shared catalog: dropping the target database does not remove them.
  dst defaultdb -c "select pg_replication_origin_drop('$name') from pg_replication_origin where roname='$name'" >/dev/null || true
  dst defaultdb -c "drop database if exists \"$db\" with (force)" >/dev/null
  dst defaultdb -c "create database \"$db\"" >/dev/null
}

# Drop the slot and publication a spike created (leaves the target untouched). Every spike
# calls this on exit so no slot keeps retaining WAL on the source between spikes.
drop_slot() { # db name
  src "$1" -c "select pg_terminate_backend(active_pid) from pg_replication_slots where slot_name='$2' and active_pid is not null" >/dev/null 2>&1 || true
  sleep 0.3
  src "$1" -c "select pg_drop_replication_slot('$2') from pg_replication_slots where slot_name='$2'" >/dev/null 2>&1 || true
  src "$1" -c "drop publication if exists \"$2\"" >/dev/null 2>&1 || true
}
