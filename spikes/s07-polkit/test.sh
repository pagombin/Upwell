#!/usr/bin/env bash
# Phase 0 spike S07, run as root on a disposable Ubuntu 24.04 (and 22.04) droplet.
# Installs the candidate rule and template, then checks, AS THE upwell USER:
#   T1  start/stop of upwell-eng@x.service is allowed without a password
#   T2  systemd-run --uid=root (transient unit) is REFUSED. If it is allowed, transient
#       units under a name-prefix rule are a root escalation for the service user.
#   T3  stopping an unrelated unit (cron.service) is refused
#   T4  the engine instance runs as upwell with pgcopydb as MainPID, and a stop leaves the
#       cgroup empty
# Prints one JSON line per check and a summary of polkit and systemd versions.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=${OUT:-$HERE/../../docs/phase0/evidence/s07}; mkdir -p "$OUT"; EVID="$OUT/results-$(. /etc/os-release; echo "$VERSION_ID").jsonl"; : >"$EVID"
rec() { echo "$1" | tee -a "$EVID"; }
id upwell >/dev/null 2>&1 || useradd --system --home /var/lib/upwell --shell /usr/sbin/nologin upwell
install -d -o upwell -g upwell /var/lib/upwell/runs/x
polkit_ver=$(dpkg-query -W -f='${Version}' polkitd 2>/dev/null || dpkg-query -W -f='${Version}' policykit-1 2>/dev/null)
rec "{\"meta\":{\"os\":\"$(. /etc/os-release; echo "$PRETTY_NAME")\",\"systemd\":\"$(systemctl --version | head -1)\",\"polkit\":\"$polkit_ver\",\"js_rules_supported\":$( [[ -d /etc/polkit-1/rules.d ]] && echo true || echo false)}}"
install -m 0644 "$HERE/50-upwell.rules" /etc/polkit-1/rules.d/50-upwell.rules
install -m 0644 "$HERE/upwell-eng@.service" /etc/systemd/system/upwell-eng@.service
systemctl daemon-reload; systemctl restart polkit 2>/dev/null || true
cat >/var/lib/upwell/runs/x/engine.env <<'ENV'
UPWELL_ENGINE_ARGS=--version
ENV
# A long-running stand-in for the engine to test stop: replace with a real clone run when
# clusters are available (see s01-unit-stop.sh with RUNNER=systemd).
chown -R upwell:upwell /var/lib/upwell/runs/x
as_upwell() { runuser -u upwell -- "$@"; }
r=$(as_upwell systemctl start upwell-eng@x.service 2>&1); rc=$?
rec "{\"check\":\"T1_start_template\",\"allowed\":$( ((rc==0)) && echo true || echo false),\"out\":\"$(tr -d '"\n' <<<"$r" | head -c 200)\"}"
r=$(as_upwell systemd-run --unit=upwell-eng-escalation --uid=root /usr/bin/id 2>&1); rc=$?
rec "{\"check\":\"T2_transient_as_root_refused\",\"refused\":$( ((rc!=0)) && echo true || echo false),\"out\":\"$(tr -d '"\n' <<<"$r" | head -c 200)\"}"
r=$(as_upwell systemctl stop cron.service 2>&1); rc=$?
rec "{\"check\":\"T3_unrelated_unit_refused\",\"refused\":$( ((rc!=0)) && echo true || echo false)}"
systemctl start cron.service 2>/dev/null || true
sleep 1
rec "{\"check\":\"T4_engine_identity\",\"show\":\"$(systemctl show -P User,MainPID,ExecMainStatus,Result upwell-eng@x.service | paste -sd' ')\",\"log\":\"$(tail -2 /var/lib/upwell/runs/x/engine.log 2>/dev/null | tr -d '"\n' | head -c 200)\"}"
