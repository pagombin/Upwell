#!/usr/bin/env bash
# Start, stop or restart the dev server (dev/config.dev.yaml). Logs: /var/tmp/upwell-dev/serve.log
set -u
BIN=${BIN:-/tmp/claude-0/upwell}
CFG=${CFG:-$(dirname "$0")/config.dev.yaml}
PIDF=/var/tmp/upwell-dev/serve.pid
stop() { [[ -f $PIDF ]] && kill "$(cat $PIDF)" 2>/dev/null; for _ in $(seq 1 50); do [[ -f $PIDF ]] && kill -0 "$(cat $PIDF)" 2>/dev/null || break; sleep 0.2; done; rm -f $PIDF; }
start() { mkdir -p /var/tmp/upwell-dev; UPWELL_LOG_STDERR=1 setsid "$BIN" serve --config "$CFG" >>/var/tmp/upwell-dev/serve.log 2>&1 < /dev/null & echo $! > $PIDF; for _ in $(seq 1 50); do curl -sk https://127.0.0.1:8443/readyz | grep -q '"ok":true' && break; sleep 0.2; done; }
case "${1:-restart}" in start) start ;; stop) stop ;; restart) stop; start ;; esac
