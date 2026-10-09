# Engine-unit runner used by the spikes. Source after lib.sh.
#
# RUNNER=systemd (default on a droplet): the exact launch shape from the spec, via
#   systemd-run with KillMode=control-group and TimeoutStopSec.
# RUNNER=cgroup  (fallback for containers without systemd as PID 1): a cgroup v2 group
#   per unit, stopped the way systemd stops a KillMode=control-group unit: SIGTERM to
#   every process in the group, wait TimeoutStopSec, then cgroup.kill (SIGKILL to all).
#   This proves the cgroup mechanics, NOT systemd's own behavior; results from this mode
#   are labelled "cgroup-emulated" in the evidence.
# RUNNER=template: the proposed design from the findings: instances of the pre-installed
#   template spikes/s07-polkit/upwell-eng@.service (install it first). The engine binary
#   is fixed by the template; its arguments go to /var/lib/upwell/runs/<instance>/engine.env.
RUNNER="${RUNNER:-$( [[ "$(ps -p 1 -o comm=)" == systemd ]] && echo systemd || echo cgroup )}"
STOP_TIMEOUT="${STOP_TIMEOUT:-30}"
CGROOT="${CGROOT:-$( [[ -d /sys/fs/cgroup/unified ]] && echo /sys/fs/cgroup/unified || echo /sys/fs/cgroup )}/upwell-spike"
UNIT_STATE="${SPIKE_ROOT}/units"; mkdir -p "$UNIT_STATE"

svc() { # unit -> systemd service name
  if [[ $RUNNER == template ]]; then echo "upwell-eng@${1#upwell-eng-}.service"; else echo "$1.service"; fi
}

unit_start() { # unit logfile cmd...
  local unit=$1 logf=$2; shift 2
  if [[ $RUNNER == template ]]; then
    local inst=${unit#upwell-eng-} dir=/var/lib/upwell/runs/${unit#upwell-eng-}
    install -d -o upwell -g upwell "$dir"
    install -m 0600 -o upwell -g upwell "$PGPASSFILE" "$dir/pgpass"
    { printf 'UPWELL_ENGINE_ARGS=%s\n' "${*:2}"; printf 'PGPASSFILE=%s\n' "$dir/pgpass"; } >"$dir/engine.env"
    chown upwell:upwell "$dir/engine.env"; chmod 0600 "$dir/engine.env"
    # Work directories passed in the arguments must be writable by the upwell user.
    chown -R upwell:upwell "$SPIKE_ROOT/runs" 2>/dev/null || true
    touch "$dir/engine.log"; chown upwell:upwell "$dir/engine.log"; ln -sf "$dir/engine.log" "$logf"
    systemctl start "upwell-eng@$inst.service"
  elif [[ $RUNNER == systemd ]]; then
    systemd-run --quiet --unit="$unit" --collect \
      --property=KillMode=control-group --property=TimeoutStopSec="$STOP_TIMEOUT" \
      --property=StandardOutput=append:"$logf" --property=StandardError=append:"$logf" \
      --setenv=PGPASSFILE="$PGPASSFILE" "$@"
  else
    mkdir -p "$CGROOT/$unit"
    # The launcher moves itself into the group, then execs the engine so the engine is
    # the group's first (main) process, as it is the unit's main process under systemd.
    setsid bash -c 'echo $$ > "$1/cgroup.procs"; shift; exec "$@"' _ "$CGROOT/$unit" "$@" \
      >>"$logf" 2>&1 </dev/null &
    echo $! >"$UNIT_STATE/$unit.mainpid"
    sleep 0.2
  fi
}

unit_pids() { # unit -> pids in the unit's cgroup
  local unit=$1
  if [[ $RUNNER != cgroup ]]; then
    local cg; cg=$(systemctl show -P ControlGroup "$(svc "$unit")" 2>/dev/null)
    [[ -n "$cg" && -f "/sys/fs/cgroup$cg/cgroup.procs" ]] && cat "/sys/fs/cgroup$cg/cgroup.procs"
  else
    [[ -f "$CGROOT/$unit/cgroup.procs" ]] && cat "$CGROOT/$unit/cgroup.procs"
  fi
  return 0
}

# Live (non-zombie) processes in the unit.
unit_live_count() { local n=0 p; for p in $(unit_pids "$1"); do
    [[ "$(awk '{print $3}' /proc/$p/stat 2>/dev/null)" =~ ^[^ZX]$ ]] && n=$((n+1)); done; echo $n; }
unit_empty() { [[ "$(unit_live_count "$1")" == 0 ]]; }

unit_mainpid() {
  if [[ $RUNNER != cgroup ]]; then systemctl show -P MainPID "$(svc "$1")"; else cat "$UNIT_STATE/$1.mainpid"; fi
}

unit_state() {
  if [[ $RUNNER != cgroup ]]; then systemctl show -P ActiveState,SubState,ExecMainStatus "$(svc "$1")" | paste -sd' '
  else unit_empty "$1" && echo "inactive" || echo "active"; fi
}

# Stop the unit; prints "<ms to empty> <how>" where how is term (SIGTERM was enough)
# or kill (SIGKILL after the stop timeout was needed).
unit_stop() {
  local unit=$1 t0; t0=$(now_ms)
  if [[ $RUNNER != cgroup ]]; then
    systemctl stop "$(svc "$unit")"
    local how=term
    journalctl -u "$(svc "$unit")" --since "@$((t0/1000))" 2>/dev/null | grep -q "Killing process" && how=kill
    echo "$(( $(now_ms) - t0 )) $how"
  else
    local p; for p in $(unit_pids "$unit"); do kill -TERM "$p" 2>/dev/null || true; done
    if wait_for "$STOP_TIMEOUT" unit_empty "$unit" >/dev/null; then echo "$(( $(now_ms) - t0 )) term"; return; fi
    echo 1 >"$CGROOT/$unit/cgroup.kill"
    wait_for 10 unit_empty "$unit" >/dev/null || true
    echo "$(( $(now_ms) - t0 )) kill"
  fi
}

unit_reset() { # forget a stopped unit
  if [[ $RUNNER != cgroup ]]; then systemctl reset-failed "$(svc "$1")" 2>/dev/null || true
  else [[ -d "$CGROOT/$1" ]] && { echo 1 >"$CGROOT/$1/cgroup.kill" 2>/dev/null || true; sleep 0.3; rmdir "$CGROOT/$1" 2>/dev/null || true; }
       rm -f "$UNIT_STATE/$1.mainpid"; fi
}
