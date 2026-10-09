# shellcheck shell=bash
# Source me: small curl helpers for the dev server.
# Usage: source dev/api.sh; up_login admin 'password...'; up GET /api/v1/migrations
UP_BASE="${UP_BASE:-https://127.0.0.1:8443}"
UP_JAR="${UP_JAR:-/tmp/upwell-dev-cookies}"
up_login() {
  local body; body=$(printf '{"username":"%s","password":"%s"}' "$1" "$2")
  UP_CSRF=$(curl -sk -c "$UP_JAR" -H 'Content-Type: application/json' -d "$body" "$UP_BASE/api/v1/auth/login" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("csrf_token",""))')
  export UP_CSRF
}
up_csrf() { curl -sk -b "$UP_JAR" "$UP_BASE/api/v1/auth/me" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("csrf_token",""))'; }
up() { # METHOD PATH [JSON]
  local m=$1 p=$2 d=${3:-}
  [[ -n "${UP_CSRF:-}" ]] || UP_CSRF=$(up_csrf)
  if [[ $m == GET ]]; then curl -sk -b "$UP_JAR" "$UP_BASE$p"; return; fi
  curl -sk -b "$UP_JAR" -X "$m" -H 'Content-Type: application/json' -H "X-CSRF-Token: ${UP_CSRF:-}" -H "Idempotency-Key: ${UP_KEY:-$(cat /proc/sys/kernel/random/uuid)}" ${d:+-d "$d"} "$UP_BASE$p"
}
