# Upwell overnight build: progress

Branch `overnight-build`. Status legend: **done**; **partial** (what is missing is stated); **needs droplet** (implemented to spec and tested as far as a container allows, but it needs systemd as PID 1, a real DigitalOcean cluster or a DO account to prove); **stub** (labelled "Coming soon" in the UI, never a dead button).

## Summary

Upwell is demo-ready for the end-to-end test. Everything below was verified in the build container against two local PostgreSQL 16 clusters that mimic the pair (a Standard-like source with a non-superuser `doadmin` with REPLICATION, an Advanced-like target with a non-superuser `doadmin` holding the F1 privileges and a `doadmin.aries_health` table). No DigitalOcean cluster or real credential was used.

**What works**

- **The whole online migration from the browser**, automated as a Playwright test (`web/e2e/flow.spec.ts`, about 2 minutes): define it in the wizard with a live connection test, preflight, start, watch it reach In sync, stop and resume a database, cut over to **GO**, read the verification and the report, clean up.
- **The data-safety gate.** GO needs, per database, the final heartbeat on the target, the target origin at or past the heartbeat position, and a data comparison (schema, exact counts, checksums, sequences). S02 (missing privilege at start, privilege revoked mid-stream), S09 (last batch missing) and a checksum mismatch each end NO-GO or cannot start. A heartbeat that never arrives is an explicit NO-GO with a retry allowed.
- **Recovery and faults** (integration tests): the engine killed during CDC resumes by itself; killed during the base copy goes to Restart required, then restart from zero reaches GO; the app killed during CDC reattaches to the running engines; a stale walsender is terminated after 15 s; double-submitted commands act once; an unlogged table and a verification mismatch end NO-GO; a name conflict on the target is a hard blocker; a simulated reboot resumes streaming databases and flags the one caught mid-copy.
- **The UI**: every screen in the spec at 1440 and 1280 pixels in dark and light (140 screenshots in `docs/screenshots/`), with no overlapping text, no clipping without an ellipsis and tooltip, no horizontal scroll, no axe violations, loading/empty/error/stale states on every screen, CLS under 0.1 while metrics stream, no console errors or failed requests, every control reachable by keyboard with a visible focus ring, and actions hidden from roles that cannot use them.
- **Three bug sweeps** (below): vet, staticcheck, golangci-lint, the race detector, tsc, ESLint, shellcheck, the integration suite (also under `-race`) and the e2e suite; every finding fixed.

**Partial**

- Write freeze, offline mode, the migration queue, DigitalOcean API discovery, notifications, the Prometheus endpoint, PDF reports, the support bundle, SSO and TOTP are stubs labelled "Coming soon".
- Watched tables and saved log views are labelled "Coming soon".
- Known engine limitations F2, F8 and F9 are guarded against, not fixed (README, "Known limitations").

**Needs a droplet** (implemented to spec; the container has no systemd as PID 1 and no DigitalOcean access)

- `deploy/install.sh` for real, and its idempotency (`--check` was verified here; the Go build it uses is reproducible, so a second run sees the binary unchanged).
- `upwell.service` with the watchdog, `upwell-eng@.service`, the polkit rule, `upwell-health.timer`, Restart Upwell from the System screen, and systemd's reason in engine errors (D21).
- A real reboot (simulated in-process here).
- A real Advanced target: whether its `doadmin` has the F1 privileges is unverified; preflight and the connection test say so before anything is copied.
- Receive throughput on a droplet (F8).

**Exact commands**

Install on a fresh Ubuntu 24.04 droplet:

```bash
sudo apt-get update && sudo apt-get install -y git
git clone https://github.com/pagombin/Upwell.git && cd Upwell && git checkout overnight-build
sudo bash deploy/install.sh          # prints the URL, the certificate fingerprint and the setup token
sudo bash deploy/install.sh          # second run: "Everything is up to date; nothing changed."
```

Optional local test clusters on the droplet, and test data:

```bash
sudo apt-get install -y postgresql-16 && sudo bash spikes/env/local-clusters.sh up
dev/demo-data.sh create 'postgresql://doadmin:spike-password-not-secret@127.0.0.1:55432/defaultdb' 200000
dev/demo-data.sh write  'postgresql://doadmin:spike-password-not-secret@127.0.0.1:55432/defaultdb'   # Ctrl+C before the cutover
```

Then follow "Test migration, step by step" in the README.

Developer test commands (as root, with the local clusters up):

```bash
go test ./...                                                  # unit and API tests
go test -tags integration -timeout 60m ./test/integration/     # data-safety gate and fault scenarios
dev/e2e.sh                                                     # Playwright: screens, states, keyboard, CLS, visual baseline, walkthrough
```

## Checklist

### Spec and data-safety gate (STEP 1 and STEP 2)

| Item | Status | Evidence |
| --- | --- | --- |
| Proposed spec changes 1 to 14 applied to `docs/spec.md` (PC1 to PC14) | done | `docs/spec.md` "Phase 0 amendments"; D2 |
| Template unit `upwell-eng@.service`; polkit allows only that template | done; needs droplet | `deploy/systemd/upwell-eng@.service`, `deploy/polkit/50-upwell.rules` |
| Stop = SIGTERM, then SIGKILL of the whole cgroup after 10 s, then the slot-release wait | done | unit template (`TimeoutStopSec=10`, `SendSIGKILL=yes`); local runner uses `cgroup.kill`; TestHappyPath stops in about 10 s |
| Nudge with a logical decoding message right after each end position | done | `pgcopydb.Nudge`, cutover step "Set end positions and nudge" |
| Never the coordinator (`--host`/`--port`), never `stream prune` | done | `Plan` refuses `--host`; unit test `TestPlan` |
| Base-copy failure detected from the engine log; Upwell stops the unit (F10) | done | supervisor F10 path; TestFaultSIGKILLDuringBaseCopy |
| Slot loss classified by `wal_status = 'lost'` and the log line, never the exit code | done | `Classify`, collector `slot_wal_status`; unit test `TestClassify` |
| Compact metrics schema | done | `metric_series` + `metric_samples` + rollups (migration 0001) |
| Cleanup drops origins by name; handles runs that never reached CDC | done | `pgcopydb.Cleanup`; every integration test ends with `noLeftovers` |
| `target_apply_privileges` hard blocker (origin functions + SET on `session_replication_role`), re-checked in each target database at launch | done | TestS02MissingSetPrivilege (refused) |
| Heartbeat on by default; final heartbeat per database after writers stop and before end positions; GO requires it on the target | done | D5, D10; TestHappyPath |
| After the drain the target origin must reach the final-heartbeat position, else NO-GO | done | D3; TestS02PrivilegeLostMidStream |
| Verification compares data: exact counts under the threshold, checksums within the budget, schema, sequences; never trusts pgcopydb | done | D4, D12; TestVerificationChecksumMismatch |
| Automated S02 reproduction ends NO-GO (or cannot start) | done | TestS02MissingSetPrivilege, TestS02PrivilegeLostMidStream |
| Automated S09 reproduction ends NO-GO | done | TestS09LastBatchMissing |

### Backend

| Item | Status | Notes |
| --- | --- | --- |
| Go backend, embedded React UI, SQLite store with embedded migrations | done | |
| Secrets encryption (AES-256-GCM, name-bound, master key file, `upwell key rotate`) | done | unit test `TestBox` |
| Local users, three roles, Argon2id, lockout, idle and maximum session age, CSRF | done | `internal/api/api_test.go`, TestRBAC |
| TOTP | stub | |
| Audit log, hash-chained and append-only, with chain verification | done | unit test `TestChain` |
| Live log streaming (SSE), search, download; redaction of secrets | done | unit test `TestRedact` |
| pgcopydb engine adapter (plan, start, observe, end position, nudge, stop, release slot, cleanup, log parsing, classification) | done | |
| Orchestrator: state machine, intents, attempts, retries with backoff and an hourly cap | done | unit test `TestDeriveState` |
| Full preflight catalog plus the new checks; acceptance of blockers with a reason; acceptances lapse when evidence changes | done | 55+ checks per run; TestFaultNameConflictOnTarget (hard blockers cannot be accepted) |
| Multi-database runs with the scheduler | done | TestHappyPath (two databases) |
| Per-database stop, resume and restart from zero | done | TestHappyPath, TestFaultSIGKILLDuringBaseCopy |
| Pause, resume and abort of a whole migration (typed confirmation for abort) | done | |
| Cutover console with manual stop-writes and write detection (Admin override by typing the name) | done | |
| Write freeze | stub | |
| HTML and Markdown report | done | |
| PDF report | stub | |
| Cleanup (slots, publications, origins by name, heartbeat schema, work directories, stored credentials) | done | |
| Reconciliation after an app restart | done | TestFaultAppKilledDuringCDC (engines are reattached, not restarted) |
| Reconciliation after a reboot (boot ID) | done; needs droplet | TestFaultReboot simulates it in-process; a real reboot needs a droplet |
| Settings catalog and screen; per-migration overrides frozen at start | done | unit test `TestCatalogDefaultsAreValid` |
| Disk guard, alerts, events | done | |
| `upwell init`, `selftest`, `user create`, `migrate list/show`, `key rotate` | done | |
| Restart Upwell from the System screen | done; needs droplet | refuses with a clear message without systemd (D16) |

### UI

| Item | Status | Notes |
| --- | --- | --- |
| Every screen in the spec (first-run setup, sign-in, home, 7-step wizard, overview, database detail, replication, source health, preflight, cutover console, verification, report, logs, events and audit, alerts, settings, host, system) plus the command palette and operation panels | done | `docs/screenshots/` |
| Roles hide buttons (not disable) | done | e2e "viewer: action buttons are hidden" |
| Dark and light themes, density toggle, UTC or local time | done | |
| Coming-soon labels for every stubbed feature | done | |
| Playwright checks: screenshots at 1440x900 and 1280x800 in both themes; overlap, clipping, horizontal scroll, axe (WCAG 2.1 AA and the other rules); stress content; loading, empty, error and stale states; CLS under 0.1 while metrics stream; no console errors or failed requests; keyboard reachability with visible focus; visual regression baseline | done | `dev/e2e.sh`, 229 tests; baseline in `web/e2e/__visual__` |
| The tester's walkthrough through the browser (wizard to cleanup, GO) | done | `web/e2e/flow.spec.ts` |

### Install and operations

| Item | Status | Notes |
| --- | --- | --- |
| `deploy/install.sh`: packages, PGDG client, pgcopydb 0.18 (package or source build), service user, directories, config, binary with `.prev` and store backup, units, polkit, logrotate, key, certificate, setup token, ufw, selftest before restart, `installed.json`; `--check`, `--binary`, `--bundle`, `--rollback`, `--uninstall [--purge-data]`; refuses to change pgcopydb while engine units run | done; needs droplet | `--check` verified in the container; a real install needs systemd as PID 1 |
| Idempotent (a second run changes nothing) | done; needs droplet | every step compares before changing; services restart only on change |
| `upwell.service` with `Type=notify` and `WatchdogSec=30`; WATCHDOG=1 only while every loop is healthy | done; needs droplet | |
| `upwell-health.timer` | done; needs droplet | |

### Out of scope, stubbed with "Coming soon"

DigitalOcean API discovery, notifications, Prometheus endpoint, PDF reports, support bundle, SSO and TOTP, write freeze, a queue of migrations, offline mode and bundles, self-serve mode.

## Bug sweeps

### Sweep 1 (2026-10-08, about 03:00 to 04:00 UTC)

| Tool | Result | Fixed |
| --- | --- | --- |
| `go vet ./...` (and `-tags integration`) | clean | |
| `staticcheck ./...` (2025.1.1) | 3 findings | an unused helper exposed that the catalog's `client_version` check never ran: now implemented (hard blocker when pg_dump/pg_restore are older than the source); a dead store in discovery |
| `golangci-lint run` (v2.1.6) | 425 errcheck, 1 ineffassign, 7 style | errcheck policy in `.golangci.yml` (D18); six real ones handled (stopping an engine the supervisor decided to stop, the disk guard, the drain nudge, the cleanup password file, the automatic restart's target reset, a resume's stop); state writes log their failures; now 0 issues |
| `go test -race ./internal/...` | pass | |
| TypeScript (`tsc --noEmit`, `verbatimModuleSyntax`) | 1 class of error | type-only imports compiled as runtime imports broke Playwright; fixed across the UI and enforced |
| ESLint (`--max-warnings 0`) | clean | |
| Integration suite (`go test -tags integration`) | 12 of 14 passed | **bug:** the engine's main process dying during the base copy took the transient path and retried a resume that can never work; now Restart required. **bug:** a failed automatic resume retried every 30 s forever; it now follows the backoff and hourly cap. The stale-walsender test held the slot as a superuser (doadmin correctly cannot terminate it) and `pg_recvlogical` reconnected by itself; the test now behaves like a real stale walsender. The slot-holder error printed a pointer. |
| Playwright e2e (`dev/e2e.sh`) | 147 of 229 passed on the first full run | missing landmark on sign-in and setup, empty table headers, dialog `<header>` counted as a second banner, the stress table scrolled horizontally at 1280 (tables now hide optional columns first), state tests matched short IDs while the app used full IDs, a spurious heartbeat alert on idle databases (D19); then 229 of 229 |
| Fault scenarios | SIGKILL main during CDC: pass. SIGKILL during base copy: fixed, pass. App killed during CDC: pass (engines reattached, not restarted). Stale walsender: pass (released after 15 s). Double-submitted commands: pass (one effect, identical replays; a second cutover refused). Verification mismatch: NO-GO. Unlogged table with the default plugin: preflight warns, cutover NO-GO. Name conflict on the target: hard blocker, start refused. Simulated reboot: pass. | |

### Sweep 2 (about 04:00 to 05:00 UTC)

| Tool | Result | Fixed |
| --- | --- | --- |
| `go vet`, `staticcheck`, `golangci-lint` | clean, 0 issues | |
| `go test -race ./internal/...` (now including the API tests) | pass | |
| `tsc --noEmit`, ESLint | clean | |
| Integration suite under the race detector (`go test -race -tags integration`, 15 tests, 21 minutes) | 15 of 15 passed, no data races | |
| New: browser walkthrough (`web/e2e/flow.spec.ts`) | failed, then passed (2 minutes) | **bug:** a new migration's database list was JSON `null`, which crashed the wizard's database step; the API now never returns `null` for a list (D22) |
| Review of screenshots | | fixture migrations showed action buttons the backend refuses (now hidden with a note); a NO-GO listed the failed database below the passing one (failed first now); an engine unit failing before pgcopydb logs anything now reports systemd's reason (D21) |

