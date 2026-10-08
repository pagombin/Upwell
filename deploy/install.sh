#!/usr/bin/env bash
# Upwell installer for Ubuntu 24.04 (22.04 should work).
#
# Installs or updates everything Upwell needs on a droplet. Every later run
# changes only what differs, so running it twice in a row changes nothing.
#
#   sudo bash deploy/install.sh                  # install or update from this checkout
#   sudo bash deploy/install.sh --check          # print what would change, change nothing
#   sudo bash deploy/install.sh --binary ./upwell  # install a prebuilt binary instead of building
#   sudo bash deploy/install.sh --bundle FILE.tar.gz  # offline: binary + manifest (+ .deb files)
#   sudo bash deploy/install.sh --rollback       # go back to the previous binary
#   sudo bash deploy/install.sh --uninstall [--purge-data]
#
# Everything is also written to /var/log/upwell/install.log.
set -Eeuo pipefail

PGCOPYDB_PIN="0.18"
PGCOPYDB_TAG="v0.18"
GO_VERSION="1.24.4"
PORT_DEFAULT=443

SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$SELF_DIR/.." && pwd)
BIN=/usr/local/bin/upwell
ETC=/etc/upwell
DATA=/var/lib/upwell
LOGD=/var/log/upwell
CFG=$ETC/config.yaml
STATE=$DATA/installed.json
UNIT_DIR=/etc/systemd/system
POLKIT=/etc/polkit-1/rules.d/50-upwell.rules
LOGROTATE=/etc/logrotate.d/upwell
BUILD_CACHE=/var/cache/upwell-build

MODE=install
BINARY=""
BUNDLE=""
PURGE=0
FORCE=0
CHANGED=0
RESTART=0
RELOAD=0

usage() { sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; }
while [[ $# -gt 0 ]]; do
  case "$1" in
    --check) MODE=check ;;
    --binary) BINARY=$(realpath "$2"); shift ;;
    --bundle) BUNDLE=$(realpath "$2"); shift ;;
    --rollback) MODE=rollback ;;
    --uninstall) MODE=uninstall ;;
    --purge-data) PURGE=1 ;;
    --force) FORCE=1 ;;
    --version) echo "Release downloads are not available for this build. Install from a checkout of the repository, with --binary, or with --bundle." >&2; exit 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1 (see --help)" >&2; exit 2 ;;
  esac
  shift
done

# ---------- output

mkdir -p "$LOGD" 2>/dev/null || true
LOGFILE=$LOGD/install.log
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" >>"$LOGFILE" 2>/dev/null || true; }
say() { printf '%-28s %s\n' "$1" "$2"; log "$1 $2"; }
fail() { printf '\nInstall failed: %s\n' "$*" >&2; log "FAILED: $*"; exit 1; }
trap 'fail "unexpected error on line $LINENO (see $LOGFILE)"' ERR
would() { CHANGED=1; say "$1" "would $2"; }
check() { [[ $MODE == check ]]; }

[[ $EUID -eq 0 ]] || fail "run as root: sudo bash $0"
log "---- install.sh $MODE $*"

if [[ -r /etc/os-release ]]; then
  . /etc/os-release
  if [[ "${ID:-}" != ubuntu ]]; then say "os" "warning: $PRETTY_NAME is not Ubuntu; continuing"; fi
  if [[ "${VERSION_ID:-}" != 24.04 && "${VERSION_ID:-}" != 22.04 ]]; then say "os" "warning: Ubuntu ${VERSION_ID:-?} is untested (24.04 is supported)"; fi
fi

engines_active() { systemctl list-units --no-legend --state=active 'upwell-eng@*' 2>/dev/null | awk '{print $1}' | grep -c . || true; }
sha() { sha256sum "$1" 2>/dev/null | awk '{print $1}'; }

# ---------- uninstall and rollback

if [[ $MODE == uninstall ]]; then
  n=$(engines_active)
  if [[ $n -gt 0 && $FORCE -eq 0 ]]; then fail "$n engine unit(s) are running. Finish or abort the migrations first, or pass --force."; fi
  systemctl disable --now upwell.service upwell-health.timer >/dev/null 2>&1 || true
  systemctl stop 'upwell-eng@*' >/dev/null 2>&1 || true
  rm -f "$UNIT_DIR/upwell.service" "$UNIT_DIR/upwell-eng@.service" "$UNIT_DIR/upwell-health.service" "$UNIT_DIR/upwell-health.timer" "$POLKIT" "$LOGROTATE" "$BIN" "$BIN.prev"
  systemctl daemon-reload
  say "uninstall" "removed the service, units, polkit rule and binary"
  if [[ $PURGE -eq 1 ]]; then
    rm -rf "$DATA" "$LOGD" "$ETC" "$BUILD_CACHE"
    userdel upwell >/dev/null 2>&1 || true
    say "purge" "removed $DATA, $LOGD, $ETC and the upwell user"
  else
    say "data" "kept $DATA, $ETC and $LOGD (use --purge-data to remove)"
  fi
  exit 0
fi

if [[ $MODE == rollback ]]; then
  [[ -x $BIN.prev ]] || fail "no previous binary at $BIN.prev"
  "$BIN.prev" selftest --config "$CFG" >/dev/null || fail "the previous binary fails its selftest; not rolling back"
  cp -p "$BIN" "$BIN.rollback-from" 2>/dev/null || true
  mv -f "$BIN.prev" "$BIN"
  [[ -f $BIN.rollback-from ]] && mv -f "$BIN.rollback-from" "$BIN.prev"
  systemctl restart upwell.service
  say "rollback" "now running $($BIN version)"
  exit 0
fi

# ---------- packages

PKGS=(curl ca-certificates gnupg ufw chrony jq openssl)
# polkit lets the upwell user start and stop its engine units (the rule below).
if apt-cache show polkitd >/dev/null 2>&1; then PKGS+=(polkitd); else PKGS+=(policykit-1); fi
missing=()
for p in "${PKGS[@]}"; do dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q "install ok installed" || missing+=("$p"); done
if [[ ${#missing[@]} -eq 0 ]]; then say "os packages" "unchanged"
elif check; then would "os packages" "install ${missing[*]}"
else
  DEBIAN_FRONTEND=noninteractive apt-get update -qq >>"$LOGFILE" 2>&1
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${missing[@]}" >>"$LOGFILE" 2>&1 || fail "apt-get install ${missing[*]} failed"
  say "os packages" "installed ${missing[*]}"; CHANGED=1
fi

# ---------- PostgreSQL clients (newest major from PGDG)

client_major() { for v in 18 17 16 15 14; do dpkg-query -W -f='${Status}' "postgresql-client-$v" 2>/dev/null | grep -q "install ok installed" && { echo "$v"; return; }; done; }
have=$(client_major || true)
if [[ -n $have ]]; then say "postgresql client" "unchanged ($have)"
elif check; then would "postgresql client" "add the PGDG repository and install the newest client"
else
  if ! apt-cache policy postgresql-client-17 2>/dev/null | grep -q 'Candidate: [0-9]'; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postgresql-common >>"$LOGFILE" 2>&1
    yes | /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y >>"$LOGFILE" 2>&1 || fail "could not add the PGDG apt repository"
  fi
  for v in 18 17 16; do
    if apt-cache policy "postgresql-client-$v" 2>/dev/null | grep -q 'Candidate: [0-9]'; then
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "postgresql-client-$v" >>"$LOGFILE" 2>&1 || fail "installing postgresql-client-$v failed"
      have=$v; break
    fi
  done
  [[ -n $have ]] || fail "no PostgreSQL client package is available"
  say "postgresql client" "installed $have"; CHANGED=1
fi

# ---------- pgcopydb (pinned)

pgcopydb_version() {
  local b
  b=$(command -v /usr/local/bin/pgcopydb || command -v pgcopydb || true)
  [[ -n $b ]] || return 0
  "$b" --version 2>&1 | grep -oE 'pgcopydb version [0-9.]+' | awk '{print $3}' | head -1
}
vge() { [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" == "$2" ]]; }
cur=$(pgcopydb_version || true)
if [[ -n $cur ]] && vge "$cur" "$PGCOPYDB_PIN"; then say "pgcopydb" "unchanged ($cur)"
elif [[ $(engines_active) -gt 0 ]]; then fail "pgcopydb needs an update but engine units are running; finish or pause the migrations first (the engine is never swapped under a running copy)"
elif check; then would "pgcopydb" "install $PGCOPYDB_PIN (found ${cur:-none})"
else
  cand=$(apt-cache policy pgcopydb 2>/dev/null | awk '/Candidate:/ {print $2}' | grep -oE '^[0-9]+\.[0-9]+' || true)
  if [[ -n $cand ]] && vge "$cand" "$PGCOPYDB_PIN"; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq pgcopydb >>"$LOGFILE" 2>&1 || fail "installing the pgcopydb package failed"
    ln -sf "$(command -v pgcopydb)" /usr/local/bin/pgcopydb 2>/dev/null || true
    say "pgcopydb" "installed package $cand"
  else
    say "pgcopydb" "building $PGCOPYDB_TAG from source (the package is ${cand:-unavailable})"
    dev=$(client_major); dev=${dev:-16}
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git build-essential "postgresql-server-dev-$dev" libpq-dev libgc-dev libncurses-dev libedit-dev libssl-dev libkrb5-dev zlib1g-dev liblz4-dev libzstd-dev libxslt1-dev libselinux1-dev libpam0g-dev libsqlite3-dev >>"$LOGFILE" 2>&1 || fail "installing build dependencies for pgcopydb failed"
    src=/usr/local/src/pgcopydb-$PGCOPYDB_PIN
    [[ -d $src ]] || git clone -q --depth 1 --branch "$PGCOPYDB_TAG" https://github.com/dimitri/pgcopydb "$src" >>"$LOGFILE" 2>&1 || fail "cloning pgcopydb failed"
    make -C "$src" -j"$(nproc)" >>"$LOGFILE" 2>&1 || fail "building pgcopydb failed (see $LOGFILE)"
    install -m 0755 "$src/src/bin/pgcopydb/pgcopydb" /usr/local/bin/pgcopydb
    say "pgcopydb" "installed $(pgcopydb_version) from source"
  fi
  CHANGED=1
fi

# ---------- user and directories

if id upwell >/dev/null 2>&1; then say "service user" "unchanged"
elif check; then would "service user" "create upwell"
else useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin upwell; say "service user" "created upwell"; CHANGED=1
fi
dirs_ok=1
for d in "$DATA" "$DATA/runs" "$LOGD"; do [[ -d $d && $(stat -c %U "$d" 2>/dev/null) == upwell ]] || dirs_ok=0; done
[[ -d $ETC ]] || dirs_ok=0
if [[ $dirs_ok -eq 1 ]]; then say "directories" "unchanged"
elif check; then would "directories" "create $DATA, $LOGD, $ETC"
else
  install -d -o upwell -g upwell -m 0750 "$DATA" "$DATA/runs" "$LOGD"
  install -d -o root -g upwell -m 0750 "$ETC"
  say "directories" "created"; CHANGED=1
fi

# ---------- config (never overwritten)

if [[ -f $CFG ]]; then say "config" "unchanged ($CFG)"
elif check; then would "config" "write $CFG"
else
  cat >"$CFG" <<EOF
# Upwell configuration. install.sh writes this once and never overwrites it.
listen_address: 0.0.0.0
https_port: $PORT_DEFAULT
http_redirect: true
http_port: 80
data_dir: $DATA
log_dir: $LOGD
tls_cert: $ETC/tls/cert.pem
tls_key: $ETC/tls/key.pem
master_key: $ETC/master.key
setup_token_file: $DATA/setup-token
engine:
  runner: systemd
  pgcopydb: /usr/local/bin/pgcopydb
EOF
  chown root:upwell "$CFG"; chmod 0640 "$CFG"
  say "config" "written ($CFG)"; CHANGED=1
fi
PORT=$(awk '/^https_port:/ {print $2}' "$CFG" 2>/dev/null || echo $PORT_DEFAULT); PORT=${PORT:-$PORT_DEFAULT}

# ---------- binary

NEW=""
if [[ -n $BUNDLE ]]; then
  tmpd=$(mktemp -d); tar -xzf "$BUNDLE" -C "$tmpd"
  [[ -x $tmpd/upwell ]] || fail "the bundle has no upwell binary"
  if [[ -f $tmpd/manifest.json ]]; then
    want=$(jq -r '.components[] | select(.name=="upwell") | .sha256' "$tmpd/manifest.json")
    [[ -z $want || $want == "$(sha "$tmpd/upwell")" ]] || fail "the bundle's binary does not match its manifest checksum"
  fi
  ls "$tmpd"/*.deb >/dev/null 2>&1 && ! check && dpkg -i "$tmpd"/*.deb >>"$LOGFILE" 2>&1
  NEW=$tmpd/upwell
elif [[ -n $BINARY ]]; then
  NEW=$BINARY
else
  # Build from this checkout. The UI is committed prebuilt (internal/webui/dist).
  [[ -f $REPO/go.mod && -d $REPO/cmd/upwell ]] || fail "no --binary given and $REPO is not an Upwell checkout"
  GO=$(command -v go || true)
  [[ -z $GO && -x /usr/local/go/bin/go ]] && GO=/usr/local/go/bin/go
  if [[ -z $GO ]]; then
    if check; then would "go toolchain" "install Go $GO_VERSION to build Upwell"; GO=""
    else
      arch=$(dpkg --print-architecture)
      file="go$GO_VERSION.linux-$arch.tar.gz"
      # The checksum comes from go.dev's release list over TLS.
      sum=$(curl -fsSL "https://go.dev/dl/?mode=json&include=all" | jq -r --arg f "$file" '.[].files[] | select(.filename==$f) | .sha256' | head -1)
      [[ -n $sum ]] || fail "could not read the checksum for $file from go.dev"
      curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/$file" || fail "downloading Go failed"
      echo "$sum  /tmp/go.tgz" | sha256sum -c --quiet - || fail "the Go download does not match its checksum"
      rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
      GO=/usr/local/go/bin/go
      say "go toolchain" "installed $GO_VERSION"
    fi
  fi
  if [[ -n $GO ]]; then
    mkdir -p "$BUILD_CACHE"
    ver=$(cd "$REPO" && git describe --tags --always --dirty 2>/dev/null || echo dev)
    (cd "$REPO" && GOTOOLCHAIN=local GOFLAGS=-mod=mod GOCACHE=$BUILD_CACHE/cache GOMODCACHE=$BUILD_CACHE/mod CGO_ENABLED=0 \
      "$GO" build -trimpath -buildvcs=false -ldflags "-s -w -X main.Version=$ver" -o "$BUILD_CACHE/upwell" ./cmd/upwell) >>"$LOGFILE" 2>&1 || fail "building Upwell failed (see $LOGFILE)"
    NEW=$BUILD_CACHE/upwell
  fi
fi
if [[ -n $NEW ]]; then
  if [[ "$(sha "$NEW")" == "$(sha "$BIN")" ]]; then say "upwell binary" "unchanged ($($BIN version 2>/dev/null | awk '{print $2}'))"
  elif check; then would "upwell binary" "install $("$NEW" version | awk '{print $2}')"
  else
    install -m 0755 "$NEW" "$BIN.new"
    [[ -x $BIN ]] && cp -p "$BIN" "$BIN.prev"
    # Store schema changes are forward-only: keep a copy of the store first.
    [[ -f $DATA/store.db ]] && cp -p "$DATA/store.db" "$DATA/store.db.bak-$($BIN version 2>/dev/null | awk '{print $2}' || echo prev)" 2>/dev/null || true
    mv -f "$BIN.new" "$BIN"
    say "upwell binary" "installed $($BIN version | awk '{print $2}')"; CHANGED=1; RESTART=1
  fi
fi

# ---------- systemd units, polkit rule, log rotation

place() { # src dst mode label
  if cmp -s "$1" "$2"; then say "$4" "unchanged"; return; fi
  if check; then would "$4" "install $2"; return; fi
  install -m "$3" "$1" "$2"; say "$4" "installed $2"; CHANGED=1; RELOAD=1
}
place "$SELF_DIR/systemd/upwell.service" "$UNIT_DIR/upwell.service" 0644 "unit upwell.service"
place "$SELF_DIR/systemd/upwell-eng@.service" "$UNIT_DIR/upwell-eng@.service" 0644 "unit upwell-eng@.service"
place "$SELF_DIR/systemd/upwell-health.service" "$UNIT_DIR/upwell-health.service" 0644 "unit upwell-health.service"
place "$SELF_DIR/systemd/upwell-health.timer" "$UNIT_DIR/upwell-health.timer" 0644 "unit upwell-health.timer"
mkdir -p "$(dirname "$POLKIT")"
place "$SELF_DIR/polkit/50-upwell.rules" "$POLKIT" 0644 "polkit rule"
place "$SELF_DIR/logrotate/upwell" "$LOGROTATE" 0644 "log rotation"
if [[ $RELOAD -eq 1 ]]; then systemctl daemon-reload; RESTART=1; fi
health_env="UPWELL_PORT=$PORT"
if [[ "$(cat $ETC/health.env 2>/dev/null)" != "$health_env" ]]; then
  if check; then would "health check" "write $ETC/health.env"; else echo "$health_env" >"$ETC/health.env"; say "health check" "port $PORT"; fi
fi

# ---------- key, certificate, setup token (never replaced)

if check; then
  [[ -f $ETC/master.key ]] && say "master key" "unchanged" || would "master key" "create $ETC/master.key"
  [[ -f $ETC/tls/cert.pem ]] && say "tls certificate" "unchanged" || would "tls certificate" "create a self-signed certificate"
else
  had_key=0; [[ -f $ETC/master.key ]] && had_key=1
  had_cert=0; [[ -f $ETC/tls/cert.pem ]] && had_cert=1
  out=$("$BIN" init --config "$CFG" 2>&1) || fail "upwell init: $out"
  log "$out"
  chown upwell:upwell "$ETC/master.key" && chmod 0600 "$ETC/master.key"
  install -d -o root -g upwell -m 0750 "$ETC/tls"
  chown upwell:upwell "$ETC/tls/key.pem" "$ETC/tls/cert.pem" && chmod 0600 "$ETC/tls/key.pem" && chmod 0644 "$ETC/tls/cert.pem"
  chown -R upwell:upwell "$DATA" "$LOGD"
  [[ $had_key -eq 1 ]] && say "master key" "unchanged" || { say "master key" "created"; CHANGED=1; }
  [[ $had_cert -eq 1 ]] && say "tls certificate" "unchanged" || { say "tls certificate" "created (self-signed)"; CHANGED=1; }
fi

# ---------- firewall: SSH and the HTTPS port only

need=()
st=$(ufw status 2>/dev/null || true)
grep -qE '^(22|OpenSSH)(/tcp)? +ALLOW' <<<"$st" || need+=("OpenSSH")
grep -qE "^$PORT/tcp +ALLOW" <<<"$st" || need+=("$PORT/tcp")
if grep -q 'http_redirect: true' "$CFG" 2>/dev/null; then grep -qE '^80/tcp +ALLOW' <<<"$st" || need+=("80/tcp"); fi
active=1; grep -q 'Status: active' <<<"$st" || active=0
if [[ ${#need[@]} -eq 0 && $active -eq 1 ]]; then say "firewall" "unchanged"
elif check; then would "firewall" "allow ${need[*]:-nothing new}$([[ $active -eq 0 ]] && echo ' and enable ufw')"
else
  for r in "${need[@]}"; do ufw allow "$r" >>"$LOGFILE" 2>&1; done
  [[ $active -eq 1 ]] || ufw --force enable >>"$LOGFILE" 2>&1
  say "firewall" "allowed ${need[*]:-existing rules}; ufw active"; CHANGED=1
fi

# ---------- service

if check; then
  systemctl is-enabled upwell.service >/dev/null 2>&1 && say "service" "unchanged" || would "service" "enable and start upwell.service"
  echo
  [[ $CHANGED -eq 0 ]] && echo "Nothing would change." || echo "Run without --check to apply."
  exit 0
fi
if ! systemctl is-enabled upwell.service >/dev/null 2>&1 || ! systemctl is-enabled upwell-health.timer >/dev/null 2>&1; then
  systemctl enable upwell.service upwell-health.timer >>"$LOGFILE" 2>&1; CHANGED=1
fi
if ! systemctl is-active upwell.service >/dev/null 2>&1; then
  "$BIN" selftest --config "$CFG" >>"$LOGFILE" 2>&1 || fail "upwell selftest failed (see $LOGFILE)"
  systemctl start upwell.service upwell-health.timer || fail "upwell.service did not start: journalctl -u upwell -n 50"
  say "service" "started"; CHANGED=1
elif [[ $RESTART -eq 1 ]]; then
  "$BIN" selftest --config "$CFG" >>"$LOGFILE" 2>&1 || { [[ -x $BIN.prev ]] && mv -f "$BIN.prev" "$BIN"; fail "the new binary failed its selftest; the previous binary was restored"; }
  systemctl restart upwell.service || fail "upwell.service did not restart: journalctl -u upwell -n 50"
  say "service" "restarted (engine units keep running)"
else
  say "service" "unchanged (running)"
fi
systemctl is-active upwell-health.timer >/dev/null 2>&1 || systemctl start upwell-health.timer

# Wait for readiness.
for _ in $(seq 1 60); do curl -fsk --max-time 2 "https://127.0.0.1:$PORT/readyz" >/dev/null 2>&1 && break; sleep 1; done

jq -n --arg upwell "$($BIN version | awk '{print $2}')" --arg sha "$(sha "$BIN")" --arg pgcopydb "$(pgcopydb_version)" --arg pg "$(client_major)" --arg at "$(date -u +%FT%TZ)" \
  '{installed_at: $at, components: [{name: "upwell", version: $upwell, sha256: $sha}, {name: "pgcopydb", version: $pgcopydb}, {name: "postgresql-client", version: $pg}]}' >"$STATE.tmp"
if ! cmp -s <(jq 'del(.installed_at)' "$STATE.tmp") <(jq 'del(.installed_at)' "$STATE" 2>/dev/null); then mv -f "$STATE.tmp" "$STATE"; chown upwell:upwell "$STATE"; else rm -f "$STATE.tmp"; fi

# ---------- summary

ip=$(curl -fs --max-time 2 http://169.254.169.254/metadata/v1/interfaces/public/0/ipv4/address 2>/dev/null || hostname -I | awk '{print $1}')
fp=$(openssl x509 -in "$ETC/tls/cert.pem" -noout -fingerprint -sha256 2>/dev/null | cut -d= -f2)
echo
[[ $CHANGED -eq 0 ]] && echo "Everything is up to date; nothing changed."
echo "Upwell:       https://$ip$([[ $PORT != 443 ]] && echo ":$PORT")/"
echo "Certificate:  SHA-256 $fp"
if [[ -f $DATA/setup-token ]]; then
  echo "First sign-in: open the URL, check the fingerprint above, and use this setup token:"
  echo "               $(cat "$DATA/setup-token")"
fi
echo "Logs:         journalctl -u upwell -f   and   $LOGD/"
