# Upwell overnight build: progress

Status legend: **done**, **partial** (what is missing is stated), **needs droplet** (implemented to spec and tested as far as a container allows; needs systemd as PID 1, a real DigitalOcean cluster or a DO account), **stub** (labelled "coming soon" in the UI, no dead buttons).

## Summary

_Written at the end of the night; see the bottom of this file for sweep results._

## Checklist

### Spec and data safety
- [x] Spec amendments PC1 to PC14 applied to `docs/spec.md` (done)
- [x] `target_apply_privileges` hard blocker, re-checked inside each target database at launch (done)
- [x] Heartbeat on by default; final heartbeat per database after writers stop and before end positions; GO requires it on the target (done, D10)
- [x] Target origin must reach the final-heartbeat position after the drain, else NO-GO (done, D3)
- [x] Verification compares data: exact counts under the threshold, checksums within the time budget, schema, sequences; never trusts the engine (done, D4)
- [x] Automated S02 tests (missing SET privilege; privilege revoked mid-stream) end NO-GO or refuse to start (done)
- [x] Automated S09 test (last batch missing) ends NO-GO (done)
- [x] Checksum mismatch test ends NO-GO (done)

### Backend
- [x] Go backend, SQLite store, embedded migrations (done)
- [x] Secrets encryption (AES-256-GCM, master key file, rotation command) (done)
- [x] Local users, three roles, Argon2id, lockout, sessions, CSRF (done); TOTP (stub)
- [x] Hash-chained audit log with verification (done)
- [x] Live log streaming (SSE) and search/download (done)
- [x] pgcopydb engine adapter: plan, start, observe, end position plus nudge, stop (SIGTERM, SIGKILL after 10 s, slot-release wait), cleanup by origin name (done)
- [x] Orchestrator: state machine, intents, attempts, retries with backoff, F10 base-copy failure detection, slot-loss classification by `wal_status` and log line (done)
- [x] Full preflight catalog plus new checks; blocker acceptance with reason; lapse on evidence change (done)
- [x] Multi-database runs with scheduler (`max_concurrent_base_copies`) (done)
- [x] Per-database stop, resume, restart from zero (done)
- [x] Cutover with manual stop-writes, write detection, admin override (done); write freeze (stub)
- [x] HTML and Markdown report (done); PDF (stub)
- [x] Cleanup: slots, publications, origins by name, heartbeat schema, work dirs, credentials; handles runs that never reached CDC (done)
- [x] Reconciliation after app restart and reboot (boot ID) (done; reboot path needs droplet)
- [x] Settings catalog API and screen (done)

### UI
- [x] Every screen in the spec (done; see screenshots)
- [ ] Playwright visual checks (in progress)

### Install
- [ ] install.sh, systemd units, polkit rule, watchdog (in progress)

### Out of scope, stubbed with "coming soon"
DigitalOcean API discovery, notifications, Prometheus endpoint, PDF reports, support bundle, SSO and TOTP, write freeze, queue of migrations, offline bundle, self-serve mode.

## Bug sweeps

_Recorded below as they run._
