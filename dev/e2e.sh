#!/usr/bin/env bash
# End-to-end UI suite: builds the UI and binary, starts a fresh Upwell on port
# 8543 with its own data directory, creates the first Admin, defines and starts
# a real migration against the local spike clusters (spikes/env/local-clusters.sh),
# keeps a writer running on the source, seeds the demonstration fixtures, then
# runs Playwright. Screenshots land in docs/screenshots.
#
#   dev/e2e.sh                 # full suite
#   dev/e2e.sh --update        # also refresh the visual regression baseline
#   E2E_KEEP=1 dev/e2e.sh      # leave the server and migration running afterwards
set -euo pipefail
cd "$(dirname "$0")/.."

DATA=/var/tmp/upwell-e2e
PORT=8543
BASE=https://127.0.0.1:$PORT
BIN=$DATA/upwell
JAR=$DATA/cookies
PW='correct-horse-battery-staple'
SOCK=/var/tmp/upwell-spike
SRC_PORT=55432
DST_PORT=55433
DB=e2e_shop
UPDATE=""
REUSE=0
PWARGS=()
for a in "$@"; do
  case "$a" in
    --update) UPDATE="--update-snapshots" ;;
    --reuse) REUSE=1 ;;
    *) PWARGS+=("$a") ;;
  esac
done

log() { printf '\n== %s\n' "$*"; }
psql_su() { local port=$1 db=$2; shift 2; runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1 -h "$SOCK" -p "$port" -d "$db" "$@"; }
psql_do() { local port=$1 db=$2; shift 2; PGPASSWORD=spike-password-not-secret psql -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -p "$port" -U doadmin -d "$db" "$@"; }

stop_server() {
  if [[ -f $DATA/serve.pid ]]; then kill "$(cat $DATA/serve.pid)" 2>/dev/null || true; sleep 1; rm -f $DATA/serve.pid; fi
}
cleanup_all() {
  [[ -n "${WRITER:-}" ]] && kill "$WRITER" 2>/dev/null || true
  if [[ -z "${E2E_KEEP:-}" ]]; then
    # Abort and clean up the migration so no slot or origin is left behind.
    if [[ -n "${MIG:-}" ]]; then
      api POST /api/v1/migrations/$MIG/abort "{\"confirm\":\"$MIG_NAME\"}" >/dev/null 2>&1 || true
      sleep 12
      api POST /api/v1/migrations/$MIG/cleanup "{\"confirm\":\"$MIG_NAME\"}" >/dev/null 2>&1 || true
      sleep 8
    fi
    stop_server
    pgrep -x pgcopydb >/dev/null && pgrep -x pgcopydb | xargs kill -9 2>/dev/null || true
  fi
}
trap cleanup_all EXIT

api() { # METHOD PATH [JSON]
  local m=$1 p=$2 d=${3:-}
  if [[ $m == GET ]]; then curl -sfk -b "$JAR" "$BASE$p"; return; fi
  curl -sk -b "$JAR" -c "$JAR" -X "$m" -H 'Content-Type: application/json' -H "X-CSRF-Token: ${CSRF:-}" \
    -H "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)" ${d:+-d "$d"} "$BASE$p"
}
json() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)"; }

# --reuse: keep a server from an earlier E2E_KEEP=1 run and only rerun Playwright.
if [[ $REUSE -eq 1 && -f $DATA/e2e.env ]] && curl -sk $BASE/readyz | grep -q '"ok":true'; then
  E2E_KEEP=1
  # shellcheck disable=SC1091
  . $DATA/e2e.env
  (cd web && npm run build --silent >/dev/null)
  log "Reusing the running server (migration $E2E_MIG)"
  cd web
  set +e
  npx playwright test $UPDATE "${PWARGS[@]}"
  RC=$?
  echo "playwright exit $RC"
  exit $RC
fi

log "Build UI and binary"
(cd web && npm run build --silent >/dev/null)
mkdir -p $DATA
go build -o $BIN ./cmd/upwell

log "Local clusters"
spikes/env/local-clusters.sh up >/dev/null 2>&1 || true
psql_su $SRC_PORT postgres -c "SELECT 1" >/dev/null

log "Fresh server on $PORT"
stop_server
pgrep -x pgcopydb >/dev/null && pgrep -x pgcopydb | xargs kill -9 2>/dev/null || true
rm -rf $DATA/data $DATA/logs $DATA/setup-token $DATA/master.key $DATA/tls
cat > $DATA/config.yaml <<EOF
listen_address: 127.0.0.1
https_port: $PORT
http_redirect: false
data_dir: $DATA/data
log_dir: $DATA/logs
tls_cert: $DATA/tls/cert.pem
tls_key: $DATA/tls/key.pem
master_key: $DATA/master.key
setup_token_file: $DATA/setup-token
dev: true
engine:
  runner: local
  pgcopydb: /usr/local/bin/pgcopydb
  pg_bin_dir: /usr/lib/postgresql/16/bin
EOF
UPWELL_LOG_STDERR=1 setsid $BIN serve --config $DATA/config.yaml >>$DATA/serve.log 2>&1 < /dev/null &
echo $! > $DATA/serve.pid
for _ in $(seq 1 100); do curl -sk $BASE/readyz | grep -q '"ok":true' && break; sleep 0.2; done

log "First Admin"
TOKEN=$(cat $DATA/setup-token)
curl -sk -H 'Content-Type: application/json' -H "Idempotency-Key: setup-1" -d "{\"setup_token\":\"$TOKEN\",\"username\":\"admin\",\"password\":\"$PW\",\"time_zone\":\"UTC\"}" $BASE/api/v1/setup >/dev/null
CSRF=$(curl -sk -c "$JAR" -H 'Content-Type: application/json' -H "Idempotency-Key: login-1" -d "{\"username\":\"admin\",\"password\":\"$PW\"}" $BASE/api/v1/auth/login | json 'd["csrf_token"]')
api POST /api/v1/users "{\"username\":\"viewer\",\"password\":\"$PW\",\"role\":\"viewer\"}" >/dev/null
api POST /api/v1/users "{\"username\":\"operator\",\"password\":\"$PW\",\"role\":\"operator\"}" >/dev/null
# Fast cadence so charts and live updates move during the suite. The container's
# disk is small, so raise the disk thresholds (the droplet uses the defaults).
api PATCH /api/v1/settings '{"sample_interval_seconds":2,"wal_sample_seconds":10,"alert_disk_warn_pct":98,"alert_disk_crit_pct":99,"disk_guard_stop_pct":99}' >/dev/null || true

log "Source database $DB with a writer"
psql_su $SRC_PORT postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" -c "CREATE DATABASE $DB OWNER doadmin"
psql_su $DST_PORT postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)"
psql_do $SRC_PORT $DB <<'SQL'
CREATE TABLE orders(id bigserial PRIMARY KEY, customer int NOT NULL, amount numeric(12,2), note text, created_at timestamptz DEFAULT now());
CREATE TABLE items(order_id bigint REFERENCES orders(id), line int, sku text, qty int, PRIMARY KEY(order_id, line));
INSERT INTO orders(customer, amount, note) SELECT g%1000, (g%9999)/100.0, md5(g::text) FROM generate_series(1,200000) g;
INSERT INTO items SELECT id, l, 'sku-'||(id*l)%5000, l FROM orders, generate_series(1,2) l;
CREATE INDEX ON orders(customer);
ANALYZE orders, items;
SQL

conn() { echo "{\"host\":\"127.0.0.1\",\"port\":$1,\"user\":\"doadmin\",\"password\":\"spike-password-not-secret\",\"dbname\":\"defaultdb\",\"sslmode\":\"disable\",\"storage_gb\":20}"; }
perm='{"customer":"Example Customer","account_id":"acct-0001","ticket":"SUP-1234","granted_by":"Jamie Customer","granted_at":"2026-10-07","scope":"Copy e2e_shop to the target and cut over."}'

define() { # name -> short id; sets connections, permission and databases
  local name=$1 id
  id=$(api POST /api/v1/migrations "{\"name\":\"$name\"}" | json 'd["id"]')
  api PUT /api/v1/migrations/$id/permission "$perm" >/dev/null
  api PUT /api/v1/migrations/$id/connections/source "$(conn $SRC_PORT)" >/dev/null
  api PUT /api/v1/migrations/$id/connections/target "$(conn $DST_PORT)" >/dev/null
  # Include only $DB; discovery includes every database by default.
  sel=$(api POST /api/v1/migrations/$id/discover | python3 -c "import sys,json; print(json.dumps([{'source_name':d['source_name'],'target_name':d['target_name'],'include':d['source_name']=='$DB'} for d in json.load(sys.stdin) if not d.get('skip_reason') or d['source_name']=='$DB']))")
  api PUT /api/v1/migrations/$id/databases "$sel" >/dev/null
  echo "$id"
}
wait_preflight() {
  for _ in $(seq 1 120); do
    st=$(api GET /api/v1/migrations/$1/preflight/latest 2>/dev/null | json '(d["run"] or {}).get("state","")' 2>/dev/null || true)
    [[ $st != running && -n $st ]] && return 0
    sleep 1
  done
  return 1
}

log "Running migration"
MIG_NAME="Example Customer: shop to Advanced"
MIG=$(define "$MIG_NAME")
api POST /api/v1/migrations/$MIG/preflight >/dev/null
wait_preflight $MIG
api GET /api/v1/migrations/$MIG/preflight/latest | json 'd["run"]["summary"]'
api POST /api/v1/migrations/$MIG/start '{"warnings_reviewed":true}' | json 'd.get("state", d)'
for _ in $(seq 1 180); do
  st=$(api GET /api/v1/migrations/$MIG | json '[x["state"] for x in d["databases"] if x["include"]][0]')
  [[ $st == in_sync ]] && break
  sleep 2
done
echo "database state: $st"
[[ $st == in_sync ]] || { echo "the e2e migration did not reach in_sync; see $DATA/serve.log" >&2; exit 1; }
( while true; do psql_do $SRC_PORT $DB -c "INSERT INTO orders(customer, amount, note) SELECT g%1000, 1.5, 'live' FROM generate_series(1,200) g" >/dev/null 2>&1 || true; sleep 1; done ) &
WRITER=$!

log "Draft migration for the wizard"
DRAFT=$(define "Draft: wizard walkthrough")
api POST /api/v1/migrations/$DRAFT/preflight >/dev/null
wait_preflight $DRAFT

log "Fixtures"
api POST /api/v1/dev/fixtures >/dev/null

SHORT=$(api GET /api/v1/migrations/$MIG | json 'd["migration"]["short_id"]')
DSHORT=$(api GET /api/v1/migrations/$DRAFT | json 'd["migration"]["short_id"]')
export E2E_BASE=$BASE E2E_PASSWORD=$PW E2E_MIG=$SHORT E2E_DRAFT=$DSHORT E2E_DB=$DB
printf 'export E2E_BASE=%s E2E_PASSWORD=%s E2E_MIG=%s E2E_DRAFT=%s E2E_DB=%s\n' "$BASE" "$PW" "$SHORT" "$DSHORT" "$DB" > $DATA/e2e.env
log "Playwright (running migration $SHORT, draft $DSHORT)"
cd web
set +e
npx playwright test $UPDATE "${PWARGS[@]}"
RC=$?
set -e
echo "playwright exit $RC"
exit $RC
