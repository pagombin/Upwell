# Upwell overnight build: progress

## Base copy lost its target connection again (2026-10-12), branch `claude/new-session-agkd2d`

| Item | Status | Evidence |
| --- | --- | --- |
| The failure showed pgcopydb's last error ("Failed to close snapshot") instead of the lost target connection | done (D78) | `TestBaseCopyTargetConnectionClosed` failed before ("Sub-process ... exited with code 12") and passes |
| Whether the target restarted was unknown | done (D78) | `TestBaseCopyTargetRestarted`, `TestBaseCopyTargetConnectionClosed` |
| `split_tables_larger_than = auto` documented but never applied | done (D78) | `TestAutoSplit`, `TestPlanSplitResume`, `TestSplitTablesGO`, `TestCopyProgressSplitTable` |
| A concurrent freeze undo (abort and the cancelled cutover) left the freeze recorded as applied | done (D79) | `TestWriteFreezeAbortAndRestart`: 2 of 4 runs failed before ("tuple concurrently deleted"), 6 of 6 pass after |
| Why the Advanced target closed the connection on the droplet | needs the target's logs | the cluster's metrics and logs around 2026-10-12 18:09:17 UTC and 2026-10-11 00:51:16 UTC |

## Base copy display and architecture (2026-10-12), branch `claude/new-session-agkd2d`

| Item | Status | Evidence |
| --- | --- | --- |
| Per-table base copy progress bars were empty | done (D77) | `TestCopyProgressHasAFraction` (bytes_total 0 for COPY FROM STDIN; fraction from the source's reltuples); browser "database page during a base copy" |
| Charts had no visible titles; empty charts did not say what or when | done (D77) | every `TimeChart` shows its title; database and Replication charts explain when they start; new "Copied to the target" chart |
| Architecture diagram in the README | done | `docs/architecture.png` from `docs/architecture/architecture.html` (`dev/render-architecture.sh`) |
| Where data is staged on the droplet | answered in the README | measured locally with pgcopydb 0.18: the base copy writes only the schema dump; changes are staged in `clone/cdc/*-output.db` and `*-replay.db` (274 MB for 120 MB of new rows) and kept until Clean up |
| README review | done | removed the `overnight-build` checkout, F1 wording updated with the droplet result, disk guard numbers, integration timeout, version matrix clusters, architecture section |

## Droplet restart failure (2026-10-12), branch `claude/new-session-agkd2d`

Restart from zero of `migration_db` failed in pg_restore: the source database gained DigitalOcean Standard's `aiven_extras` extension after the first launch, and the grants on its functions could not be restored on the target. Fixed by D76: a provider's management extension is left out of the copy (pgcopydb filters, kept for resume), an extension the target cannot provide stops the launch with its name, and one it can provide is created (schema first). **Status: done.**

| Evidence | Result |
| --- | --- |
| Local reproduction (stand-in extension shaped like aiven_extras) | the droplet's exact pg_restore errors before; clone succeeds with the schema left out |
| `TestProviderExtensionLeftOut` | failed before ("The base copy failed: Terminating all processes in our process group"); passes: In sync, no schema on the target, event, stop and resume, cutover GO |
| `TestUnavailableExtensionRefusedAtLaunch` | failed before (reached In sync with the extension silently missing); passes with a reason naming it |
| `TestExtensionInOwnSchemaCreated`, `TestPlanExcludeSchemas` | pass |
| Full integration suite (PG14, 15, 16, 18) | pass: 61 tests, 0 failures, 1 skipped (`TestApplyThroughput`, opt-in measurement), 79 min |
| go vet, staticcheck, golangci-lint, `go test -race ./...` | clean |

## Final regression (bug bash 12), 2026-10-11

Run from a fresh clone of `origin/overnight-build-2` at `ed3470b` (the only later commit changes one README line), by `regress.sh`: every step must pass, and the clusters are checked for leftovers afterwards. **Overall: pass (exit 0).**

| Step | Result | Time |
| --- | --- | --- |
| `go build ./...` | pass | 4 s |
| `go vet` (plus the integration tag) | pass | 1 s |
| `staticcheck` (plus the integration tag) | pass, 0 findings | 4 s |
| `golangci-lint` (plus the integration tag) | pass, 0 findings | 21 s |
| `go test -race -count=1 ./...` | pass | 15 s |
| `shellcheck -S warning` on deploy, dev and spikes scripts | pass | 1 s |
| `npm ci`, `npm audit`, `tsc --noEmit`, `eslint src e2e` | pass, 0 vulnerabilities | 11 s |
| Integration suite (`-tags integration`, local clusters PG14, 15, 16 and 18) | pass: 58 tests, 0 failures, 1 skipped (`TestApplyThroughput`, a 15-minute measurement run only with `UPWELL_MEASURE=1`); includes `TestVersionMatrix` (PG14, 15, 16, 18), `TestOfflineWithFreeze`, `TestReadinessFailsClosed`, `TestRedactionCanaries` | 75 min |
| `dev/e2e.sh` (Playwright 1.56.1, Chromium) | pass: UI 229/229, walkthrough 4/4, features 12/12; visual baseline unchanged from `ed3470b` | 12 min |
| Leftovers | 0 replication slots on either source, 0 replication origins on any target, 0 `it_` or `upwell_rate_` databases; the four `e2e_*` databases are the browser-test fixtures, dropped and re-created at the start of every `dev/e2e.sh` run | |


Branch `overnight-build` (the first build; the second is above, on `overnight-build-2`). Status legend: **done**; **partial** (what is missing is stated); **needs droplet** (implemented to spec and tested as far as a container allows, but it needs systemd as PID 1, a real DigitalOcean cluster or a DO account to prove); **stub** (labelled "Coming soon" in the UI, never a dead button).

## Overnight build 2 (2026-10-11): the audit of 643e4c1

Branch `overnight-build-2`. Every item below was first given a test that failed because of the problem, then fixed, then the test was run again; the decision entry has the before and after evidence. Tests run against the local clusters only (PostgreSQL 16 source and target, PostgreSQL 18 target without ICU).

### Codebase findings

| # | Finding | Status | Tests | Decision |
| --- | --- | --- | --- | --- |
| 1 | Preflight fails open | done: all 73 injected query failures end as a blocker (13 before) | `TestPreflightFailsClosed` | D47 |
| 2 | Writers check fails open, misses open transactions | done | `TestWritersOpenTransactionRefused`, `TestWritesAfterVerificationNoGo`, `TestWritersCheckFailsClosed` | D48 |
| 3 | Stale readiness | done (with E6) | `TestStaleReadinessTurnsRed` | D50 |
| 4 | Admin override makes verification meaningless | done | `TestOverrideWithWritesRunningNoGo` | D49 |
| 5 | mode=offline ignored | done: refused until offline mode exists | `TestModeOfflineNeverIgnored` | D51 |
| 6 | Silent collation fallback | done (confirmed, and wider: the provider was ignored too) | `TestCollationFallbackWarned` | D53 |
| 7 | Counts and quoting | done | `TestPreflightCountsAndQuoting` | D52 |
| 8 | Plan errors ignored | done | `TestPlanErrorStopsEndPositions` (hung 15 min before) | D55 |
| 9 | Sequence sync failures shown as done | done | `TestSequenceSyncFailureShown` | D56 |
| 10 | Verification rows can be lost | done: NO-GO | `TestVerificationStoreFailureNoGo` | D57 |
| 11 | Overrides proceed without their audit entry | done: refused | `TestAuditFailureRefusesOverrides` | D58 |

### Preflight findings

| # | Finding | Status | Tests | Decision |
| --- | --- | --- | --- | --- |
| 1 | Replica identity message wrong under test_decoding | done: verified on PG16 (the statement succeeds and is decoded without the old key) | `TestReplicaIdentityMatrix` | D59 |
| 2 | FULL cost understated | done: `n_tup_upd`, `n_tup_del`, size and a cost verdict per table | `TestReplicaIdentityMatrix` | D59 |
| 3 | lock_timeout | done | `TestReplicaIdentityMatrix` | D59 |
| 4 | PK rewrite warning | done | `TestReplicaIdentityMatrix` | D59 |
| 5 | Who can run it | done: ownership per table, named in the message | `TestReplicaIdentityMatrix` | D59 |
| 6 | Privilege trade-off and revoke | done: stated, revoke script shipped and proven | `TestApplyPrivilegeGuidance` | D60 |
| 7 | "before PostgreSQL 15" | done | `TestApplyPrivilegeGuidance` | D60 |
| 8 | template1 gap | confirmed; guidance in the remediation and the grant script, launch message (E7) | `TestApplyPrivilegeGuidance`, `TestTemplate0LaunchExplains` | D54, D60 |
| 9 | `'i'` with a dropped index; partitioned parents | verified on PG16 and pgcopydb's source: `'i'` behaves like NOTHING (now covered); parents were a false blocker (now leaf tables only) | `TestReplicaIdentityMatrix` | D59 |
| 10 | Verify apply progress | done (with E2) | `TestApplyWatchStall`, `TestApplyStallDetected` | D61 |

### Enhancements

| # | Enhancement | Status | Tests | Decision |
| --- | --- | --- | --- | --- |
| E1 | Apply-rate measurement and calibration | done: 4.8 tps measured here | `TestApplyRateMeasured` | D62 |
| E2 | Apply-progress watchdog | done | `TestApplyWatchStall`, `TestApplyStallDetected` | D61 |
| E3 | Copy-ready fix SQL | done: `fix_sql`, `grant_script`, `revoke_script` with a Copy button | `TestReplicaIdentityMatrix`, `TestApplyPrivilegeGuidance` | D59, D60 |
| E4 | FULL evidence | done | `TestReplicaIdentityMatrix` | D59 |
| E5 | Post-cutover checklist | done | `TestPostCutoverChecklist` | D63 |
| E6 | Sample age | done | `TestStaleReadinessTurnsRed` | D50 |
| E7 | template0 explanation at launch | done | `TestTemplate0LaunchExplains` | D54 |

### Overnight items

Each works end to end from the browser (`web/e2e/features.spec.ts`, run by `dev/e2e.sh` after the walkthrough), keeps the data-safety gate (verification still decides GO), and its "Coming soon" label is gone. Nothing needs an external service.

| # | Item | Status | Tests | Decision |
| --- | --- | --- | --- | --- |
| 1 | Write freeze | done: preview, typed confirmation, audited, automatic undo (stopped, NO-GO, aborted, cleanup, restart), undo button; the target never inherits the freeze | `TestWriteFreezeGO`, `TestWriteFreezeOverrideDetected`, `TestWriteFreezeAbortAndRestart`; browser "write freeze" | D64 |
| 2 | Offline mode | done: writers stopped or frozen first, one copy, no slot, origin or heartbeat, verification with `writes_stopped`; resumes after a restart | `TestOfflineMigrationGO`, `TestOfflineWritesDuringCopyNoGo`, `TestOfflineWithFreeze`, `TestModeOfflineNeverIgnored`; browser "offline migration" | D65 |
| 3 | TOTP and recovery codes | done (no QR code: secret and URI to copy) | `TestTOTP`, `TestTOTPLockout`, `TestTOTPVectors`; browser "second factor" | D66 |
| 4 | API tokens | done | `TestAPITokens`, `TestAPITokenExpiry` | D67 |
| 5 | Prometheus `/metrics` | done | `TestPrometheusMetrics` | D68 |
| 6 | Support bundle | done | `TestSupportBundle`; browser "support bundle" | D69 |
| 7 | Migration queue | done: run lock (store row and lock file), queue, reorder, unqueue, auto-start (not for offline) | `TestQueueRunLock`; browser "queue" | D70 |
| 8 | PDF report | done: go-pdf/fpdf v0.9.0 vendored in `third_party/fpdf` | `TestPDFReport`; browser "PDF report" | D71 |
| 9 | Watched tables | done | `TestWatchedTables`; browser (inside "write freeze") | D72 |
| 10 | Saved log views | done | `TestSavedLogViews`; browser "saved log views" | D73 |
| 11 | Network throughput | done | `TestParseNetDev`; browser "host" | D74 |
| 12 | TLS certificate upload | done: validated before swap, served without a restart, previous pair kept for rollback | `TestCertificateUpload`; browser "TLS" | D75 |

Still labelled "Coming soon", by instruction (they need an external service): DigitalOcean API discovery and token, notifications (Slack, email, PagerDuty) and the channel test. SSO, automatic update checks and self-serve mode stay out of this build.

### Bug bashes

Each bash ran its tests until they passed; the final regression (bash 12, at the top of this file) ran everything again from a clean checkout.

**1. Fail closed.** `TestPreflightFailsClosed` fails each preflight query in turn (every query now ends as a blocker naming the failure); new `TestReadinessFailsClosed` fails the readiness collectors' own queries (heartbeat latency, the apply watchdog's origin read, the schema comparison) with every cluster reachable: lag, apply and schema turn red with the cause, the gate refuses a cutover, and they recover. `TestStaleReadinessTurnsRed` and `TestWritersCheckFailsClosed` cover lost access and the writers sample. Found: nothing new (the gaps were fixed under finding 1 and finding 3).

**2. Replica identity matrix.** `TestReplicaIdentityMatrix` (`'d'` and `'n'`, `'i'` with its index dropped, keyed and unkeyed partitions, mixed case, a table owned by another role, both plugins), `TestPreflightCountsAndQuoting` (150 tables), new `TestPartitionedKeyedLeavesGO` (no blocker for a keyed-partition parent; UPDATE and DELETE through the parent while streaming; GO with equal data), `TestEdgeNoReplicaIdentity` (the application's UPDATE on the source during the migration, then GO after the fix). Found: `TestEdgeNoReplicaIdentity` only logged the verdict; it now requires GO. The partitioned-parent false blocker was fixed under preflight finding 9.

**3. Writers at cutover.** `TestWritersOpenTransactionRefused` (a transaction open through the writers step, committed after), `TestWritesAfterVerificationNoGo` (a writer resuming after verification), `TestOverrideWithWritesRunningNoGo` (Admin override with writes running): NO-GO in every case with the reason named. Found: nothing new.

**4. Write freeze.** `TestWriteFreezeGO`, `TestWriteFreezeOverrideDetected` (a session turns read-only off and writes: detected, cutover stopped, freeze undone), `TestWriteFreezeAbortAndRestart` (undone on abort, and by the restart after the process went away with the freeze applied), every step audited; browser "write freeze". Found while building: the undo ran in a read-only session and failed (fixed); with the offline mode, pgcopydb copied the freeze's `default_transaction_read_only` to the target (fixed, D65).

**5. Stale readiness.** `TestStaleReadinessTurnsRed`: access to the target revoked mid-stream for longer than three collection intervals; lag and schema turn red within about a minute and show their age; they recover. Found: nothing new.

**6. Template and collation.** `TestTemplate0LaunchExplains` (the launch re-check names template0 as the cause), `TestCollationFallbackWarned` (an ICU collation missing on a PostgreSQL 18 target without ICU: warning event, in the report). Found: nothing new.

**7. Offline mode.** Browser "offline migration from the wizard to GO" (identical data), `TestOfflineMigrationGO`, `TestOfflineWritesDuringCopyNoGo`, `TestOfflineWithFreeze`, `TestModeOfflineNeverIgnored` (the setting through the API changes the engine command). Found while building: the scheduler refused launches during the offline run's copy step (the first test hung; fixed).

**8. Auth.** `TestTOTP` (one step of clock skew each way, replay, recovery codes once, Admin enforcement, Admin reset), `TestTOTPLockout` (wrong codes lock the account), `TestAPITokens` and `TestAPITokenExpiry` (revoked and expired tokens fail on the next request); browser "second factor". Found: the browser test needed its console guard to accept the intended 401 `mfa_required` answer (test fix, not a product bug).

**9. Redaction.** New `TestRedactionCanaries` plants the clusters' real password, an API token and a bearer header in log lines (through the logger) and searches the support bundle, `/metrics`, the HTML, Markdown and PDF reports (PDF streams inflated) and the logs, events and audit APIs: no canary anywhere, and the redacted forms are present. `TestSupportBundle` and `TestPrometheusMetrics` plant canaries in stored connections. Found: an Upwell API token written bare (without `token=`) was not redacted by the logger; a pattern for `upw_` tokens was added (`TestRedactUpwellToken`).

**10. Queue.** `TestQueueRunLock`: one runner at a time, the queue survives a restart with one running and two queued, reorder while running, only the auto-start entry starts by itself; browser "queue". Found: nothing new beyond the run-lock ripple in `dev/e2e.sh` (the screen-test migration now ends before the walkthrough).

**11. Version matrix.** New `TestVersionMatrix` with targets on PostgreSQL 14.22, 15.18, 16 and 18.6: 14 gets the "before PostgreSQL 15" hard blocker and the start is refused; 15, 16 and 18 reach GO with equal data (14 and 15 fed from a PostgreSQL 14 source, since a source may not be newer than its target). PostgreSQL 14 and 15 came from the `@embedded-postgres` npm packages (the PostgreSQL download site and GitHub are blocked by the session's network policy). Found: `spikes/env/local-clusters.sh` failed on a PostgreSQL 14 target (`GRANT SET ON PARAMETER` exists from 15); it now skips that grant before 15.

**12. Full regression.** vet, staticcheck, golangci-lint (also over the integration tests), the race detector, shellcheck, npm audit, tsc, ESLint, the integration suite and the browser suite with the walkthrough and the feature tests, from a clean checkout; results at the top of this file. Found during the sweep and fixed before it: an ineffectual assignment and an unused handler, two unchecked errors, `t.Context()` (needs Go 1.24, the module says 1.23), and 17 npm advisories (dev tools and React Router) cleared by raising Playwright to 1.56.1 (it matches the installed Chromium 141), ESLint 9.39, typescript-eslint 8.71, Vite 6 and React Router 7.18; the login redirect also refuses backslashes. `TestComplexWorkloadGO` once timed out waiting for In sync while three test runs shared the container; it passed alone in 80 s, and its wait is now 5 minutes.

The droplet fixes made while this branch started (D43 to D46: target connection lost mid-COPY reported as such, `pg_dumpall --database`, supervision deadlines, restart from zero with `--drop-if-exists`) are on `overnight-build` and merged here.

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
- Known engine limitations F2, F8, F9, F11, F12 and F13 are guarded against, not fixed (README, "Known limitations"). Roles are copied without passwords; DDL is not streamed.

**Needs a droplet** (implemented to spec; the container has no systemd as PID 1 and no DigitalOcean access)

- `deploy/install.sh` for real, and its idempotency (`--check` was verified here; the Go build it uses is reproducible, so a second run sees the binary unchanged).
- `upwell.service` with the watchdog, `upwell-eng@.service`, the polkit rule, `upwell-health.timer`, Restart Upwell from the System screen, and systemd's reason in engine errors (D21).
- A real reboot (simulated in-process here).
- A real Advanced target: whether its `doadmin` has the F1 privileges is unverified; preflight and the connection test say so before anything is copied.
- Receive throughput (F8) and apply rate (F11) on a droplet; calibrate `assumed_apply_tps` from it.

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

### Sweep 3, final (about 05:00 to 06:00 UTC)

| Tool | Result |
| --- | --- |
| `go vet ./...` and `-tags integration` | clean |
| `staticcheck ./...` | clean |
| `golangci-lint run ./...` | 0 issues |
| `go test -race ./internal/...` | all 9 packages pass |
| `tsc --noEmit`, ESLint `--max-warnings 0` | clean |
| `shellcheck -S warning` on install.sh, dev/*.sh, spikes/env/local-clusters.sh | clean (two unused loop variables and a missing shell directive fixed) |
| Integration suite, 15 tests (data-safety gate, happy path, fault scenarios, RBAC, simulated reboot) | 15 of 15 passed in 22 minutes; no slot or origin left on either cluster |
| Playwright e2e from a fresh server (`dev/e2e.sh --update`) | 229 of 229 passed; browser walkthrough passed (GO); screenshots and visual baseline regenerated after the intentional UI changes of this sweep |


## Data fidelity pass (2026-10-08, evening)

Goal: run real transactions through real migrations and prove that nothing is lost between source and target, checked by a comparison independent of Upwell's own verification (`count(*)` plus an md5 over every row in key order, per table and per partition, on both clusters).

**What the tests do**

| Test | Scenario | Result |
| --- | --- | --- |
| `TestFidelityPgoutput`, `TestFidelityTestDecoding` | Two databases, 13 tables covering identity columns, an enum, jsonb, arrays, bytea, inet, uuid, NaN and infinity, a pgcrypto default, foreign keys with ON DELETE CASCADE, a table without a primary key (REPLICA IDENTITY FULL), TOAST values, a partitioned table, generated columns, Unicode text and a sequence. A live workload (multi-statement transactions, updates, TOAST updates, deletes, primary key updates, hot rows, savepoints, rollbacks) runs through the base copy and streaming; one database is stopped and resumed, the other's engine is killed with SIGKILL; a final 10,000-row transaction just before the cutover | GO for both plugins; the independent comparison finds 24 tables in 2 databases (about 330,000 rows each) identical row for row, sequences at or past the source, no rolled-back row on the target |
| `TestEdgeTruncateDuringCDC` | TRUNCATE and reinsert while streaming | GO, identical |
| `TestEdgeDDLDuringCDC` | A column added on the source while streaming | NO-GO (DDL is not streamed) |
| `TestEdgeNoReplicaIdentity` | A table with no key and no replica identity | Hard blocker; start refused; after REPLICA IDENTITY FULL the application's UPDATE works during the migration and the result is GO, identical |
| `TestApplyThroughput` (`UPWELL_MEASURE=1`) | Engine apply rate | about 190 rows a second for one large transaction, about 5 transactions a second for small ones (F11) |

**Found and fixed during this pass** (decisions D23 to D32)

- pgcopydb's role copy needs superuser; Upwell copies roles itself, without passwords (D23). Extensions are created on the target before the copy (D24).
- F11, the apply rate: readiness now also requires the heartbeat lag under `cutover_max_lag_seconds`, and preflight's `apply_rate` check warns about busy databases (D28).
- F12, pgcopydb's internal SQLite lock: one automatic restart from zero during the base copy (D27).
- F13, an apply process that dies while receive keeps the unit alive: detected from the log and resumed (D26, D31).
- F14, the drain nudge broke test_decoding: the nudge is now a heartbeat row (D30).
- A table without a replica identity is now a hard blocker: with a FOR ALL TABLES publication the application's UPDATE and DELETE on it would fail on the source.
- A reused Idempotency-Key with a different body is refused (D25); the migration lock is keyed on the full ID (D29).
- Heartbeat lag is measured even when the target has no heartbeat at all (D32), found by the regression run.

**Regression run after the fixes**: all 19 integration tests pass (TestS02PrivilegeLostMidStream after D32, re-run with the happy path, the no-identity edge and the pgoutput fidelity test), no slot, publication or origin left on either cluster.

**Browser e2e after the fixes** (`dev/e2e.sh --update`): 229 of 229 passed, and the browser walkthrough (define, preflight, start, stop and resume, cut over, report, clean up) passed with GO; screenshots regenerated (the cutover screen now shows the lag condition).

## First droplet test and five bug bashes (2026-10-09, night)

**Droplet report** (fixed and pushed first, D33 to D36): the target (an Advanced cluster, PostgreSQL 18.3 from Percona) sits behind a pooler that refused Upwell's `statement_timeout` startup parameter, which Upwell then misreported as a timeout; pgcopydb would have been refused too (PGOPTIONS); the "Session timeouts" blocker was a false alarm (pgcopydb clears those timeouts itself); pgcopydb from the PGDG package was not found at /usr/local/bin; child processes wrote to systemd's notify socket. A full migration through PgBouncer in session mode now ends GO with identical data; transaction pooling is a hard blocker.

| Bash | What | Found and fixed |
| --- | --- | --- |
| 1. PostgreSQL 18 | Built PostgreSQL 18.6 from source (postgresql.org is blocked here) as a second target; ran the happy path, pgoutput fidelity, the verification tests, S02, and, with PostgreSQL 18's own client tools (pg_dump 18, as on the droplet), test_decoding fidelity, the complex workload and TRUNCATE | No incompatibility; the schema comparison treats PostgreSQL 18's NOT NULL constraints like 16's |
| 2. Complex workloads | Partitions with rows moving between them, a transaction with 200 savepoints, full replica identity with float8, jsonb and arrays, large objects, materialized views refreshed while streaming, a partition and an enum value added while streaming | Large objects changed while streaming were lost silently (now compared: NO-GO); materialized views were not compared (now compared, refreshed on the target when needed); schema changes were found only at the cutover (now an alert and a readiness condition within a minute); json or geometric columns under full identity would fail the engine's apply (now a hard blocker); identical rows under full identity (warning) |
| 3. Static analysis | vet, staticcheck, golangci-lint, the race detector, shellcheck, npm audit | Clean; react-router-dom raised to 6.30.6 for two high advisories (two moderate ones need React Router 7 and concern server rendering and untrusted links, neither used) |
| 4. Adversarial review | Three independent reviews of the orchestrator, verification and preflight, and the API, auth and installer; every finding traced before fixing | Verification: a column named `t` shadowed the checksum's row alias, descending sequences could never fail, indexes and constraints were not compared, large tables were "compared by estimates" that were never compared, the role's timeouts and row-level security applied to verification (D37). Cutover: a database that was not running could be marked drained, a failed drain could still verify, abort raced the end positions (D38). Supervisor blocked behind long operations (watchdog), cleanup raced automatic resumes and deleted other migrations' CA files, endless automatic restarts (D39). Auth: no limit on Argon2 checks (memory), a timing oracle for usernames, a non-atomic lockout counter, sessions kept after a password change; installer: old Go or PostgreSQL client accepted, unsafe store backup, ufw could lock out a custom SSH port (D40) |
| 5. Final regression and droplet paths | The installer's `--check` mode, every integration test, a subset under the race detector, the browser suite, a re-read of the night's diff | Row-level security that applies to the admin user would make the copy silently incomplete (now a hard blocker); otherwise clean |

**Results after the five bug bashes:** all 28 integration tests pass (`TestApplyThroughput` is a measurement, skipped by default) in 44 minutes, with no slot, origin or test database left on any cluster; the race-detector subset (happy path, double submit, reboot, pooler, schema drift) finds no data race; the browser suite passes 229 of 229 and its walkthrough ends GO; vet, staticcheck, golangci-lint and shellcheck are clean.

**Still needs a droplet:** the installer for real (including tonight's changes: the PostgreSQL client 17+ rule, sqlite3 backups, SSH port detection for ufw), systemd, polkit, the watchdog and a reboot; whether the Advanced target's pooler runs in session mode and whether its `doadmin` has the apply privileges (the connection test now reports both); the engine's apply rate (F11).
