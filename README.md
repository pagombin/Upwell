# Upwell

Upwell is a migration console for moving PostgreSQL databases between DigitalOcean managed clusters, for example from a Standard cluster to an Advanced one, with the applications online until a short cutover. It runs on one droplet, drives [pgcopydb](https://github.com/dimitri/pgcopydb) 0.18 as the copy and change-streaming engine, and does everything from the browser: define the migration, check it, start it, watch it, stop and resume it, cut over with a data-safety verdict, read the report and clean up.

**Status: demo build, not GA.** It covers Phases 1 to 3 of [the spec](docs/spec.md) as a vertical slice, plus the Phase 4 pieces needed to install it and survive restarts. [`docs/PROGRESS.md`](docs/PROGRESS.md) has the item-by-item status; [`docs/DECISIONS.md`](docs/DECISIONS.md) records every decision made while building it.

## What works and what is stubbed

**Works**

- Install on a fresh Ubuntu 24.04 droplet with `deploy/install.sh` (idempotent: a second run changes nothing). A systemd service with a watchdog, one systemd unit per database being copied, and a polkit rule limited to those units.
- First-run setup in the browser (a setup token plus a TLS fingerprint check), local users with three roles (Viewer, Operator, Admin), a hash-chained audit log, encrypted stored passwords.
- A seven-step wizard: customer permission record, source and target with a live connection test, database selection, options, preflight (more than 50 checks; acceptable blockers can be accepted with a reason), and a plan review that shows the exact engine command for each database.
- Online migrations of one or many databases, with a scheduler for concurrent base copies, live progress, charts and logs, per-database stop, resume and restart from zero, and pause, resume and abort for the whole migration.
- Automatic recovery: transient failures resume with backoff; the engine being killed, the app restarting and the droplet rebooting are reconciled; a failed base copy or a lost slot goes to Restart required with a diagnosis.
- A cutover console with manual stop-writes (Upwell samples the source to confirm writes really stopped), a write-pause timer, drain, sequence sync and **verification that compares data**, ending in GO or NO-GO.
- An HTML and Markdown report, cleanup of slots, publications, origins, the heartbeat schema, work files and stored credentials, and alerts, events, logs, host metrics and system health screens.

**Stubbed** (labelled "Coming soon" in the UI; nothing does nothing silently): DigitalOcean API discovery (enter connection details by hand), notifications (Slack, email, PagerDuty), the Prometheus endpoint, PDF reports, the support bundle, SSO and TOTP, write freeze, a queue of several migrations, offline migrations and bundles, and self-serve mode.

## Install

On a fresh Ubuntu 24.04 droplet (2 vCPU and 4 GB is enough for a test; give the work volume room for the copy's staging files):

```bash
sudo apt-get update && sudo apt-get install -y git
git clone https://github.com/pagombin/Upwell.git && cd Upwell
git checkout overnight-build
sudo bash deploy/install.sh
```

The first run takes 5 to 15 minutes: it adds the PostgreSQL apt repository, installs the newest PostgreSQL client, builds pgcopydb 0.18 from source when the packaged version is older, downloads Go to build Upwell (the UI is committed prebuilt, so Node is not needed), creates the `upwell` user, writes `/etc/upwell/config.yaml`, creates the master key and a self-signed certificate, opens SSH and HTTPS in `ufw`, and starts `upwell.service`. It ends by printing:

```
Upwell:       https://203.0.113.10/
Certificate:  SHA-256 8B:D3:85:...
First sign-in: open the URL, check the fingerprint above, and use this setup token:
               3f9c0d...
```

Useful variants: `--check` prints what would change and changes nothing; `--binary ./upwell` installs a binary you built elsewhere; `--rollback` returns to the previous binary; `--uninstall [--purge-data]` removes it. Running the installer again is how you update: it changes only what differs, restarts only `upwell.service`, and refuses to touch pgcopydb while an engine unit is running.

## First sign-in

1. Open the printed URL. The browser warns about the self-signed certificate; compare its SHA-256 fingerprint with the one install.sh printed before you accept it.
2. On **Finish setting up Upwell**, tick that the fingerprint matches, paste the setup token, and create the first Admin (password of at least 14 characters).
3. Add other users under **Settings → Users and roles**. Viewers see everything; Operators also run migrations; Admins also manage users and settings.

## Test migration, step by step

You need a source and a target PostgreSQL cluster that this droplet can reach.

**Option A: two DigitalOcean managed clusters.** Add the droplet to both clusters' trusted sources. The target's `doadmin` must be able to apply replicated changes. Phase 0 found that pgcopydb needs EXECUTE on the `pg_replication_origin_*` functions and SET on `session_replication_role` (finding F1), and whether an Advanced cluster's `doadmin` has them is not yet confirmed. Preflight checks this (`target_apply_privileges`, a hard blocker) and the connection test shows it, so you will know before anything is copied.

**Option B: two local clusters on the droplet** that mimic the pair (a non-superuser `doadmin` with REPLICATION on the source; a non-superuser `doadmin` with the apply privileges on the target):

```bash
sudo apt-get install -y postgresql-16
sudo bash spikes/env/local-clusters.sh up
# source: 127.0.0.1:55432, target: 127.0.0.1:55433, user doadmin, password spike-password-not-secret, TLS mode "disable"
```

Then:

1. **Create test data** on the source: `dev/demo-data.sh create 'postgresql://doadmin:PASSWORD@HOST:PORT/defaultdb?sslmode=require' 200000` creates a `shop` database with about 600,000 rows.
2. **New migration** (left rail). Step 1: fill in the permission record (any test values). Steps 2 and 3: enter the source and target, press **Test connection** on each; every line should be green. Step 4: include `shop` only. Step 5: keep the defaults (heartbeat on). Step 6: **Run preflight**; it should end with no blockers. Step 7: read the engine command, tick that you reviewed the warnings, and press **Start migration**.
3. **Watch it.** The overview shows the base copy, then Catch-up, then In sync. Open the database for its charts, attempts and logs; **Logs** shows everything live.
4. **Keep writing** while it streams: `dev/demo-data.sh write 'postgresql://...source.../defaultdb?sslmode=require'` inserts 200 orders a second.
5. **Stop and resume** one database: open it and press **Stop** (it stops within about 10 seconds and keeps its slot), then **Resume**. It returns to In sync with nothing lost.
6. **Cut over.** Stop the writer (Ctrl+C), open **Cutover**, wait until every readiness condition is green, press **Start the cutover** and confirm that writers are stopped. Upwell checks that writes really stopped, writes a final heartbeat, waits for it on the target, sets each end position, drains, checks the target's replication origin, syncs sequences and compares the data. Watch the steps and the write-pause timer.
7. **Read the verdict and the report.** **Verification** lists every check per database; **Report** shows the record and downloads it as HTML or Markdown. `dev/demo-data.sh count` on both sides should show identical counts.
8. **Clean up.** Press **Clean up** and type the migration's name. Upwell removes the slot and publication from the source, the replication origin from the target, the heartbeat schema, the work files and the stored passwords. Migrated data stays on the target.

To try a failure, revoke a privilege on the target mid-stream (Option B: `REVOKE SET ON PARAMETER session_replication_role FROM doadmin` as `postgres` on port 55433), write some rows, and cut over: the verdict must be NO-GO.

## Reading the data-safety verdict

The verdict is decided per database and the migration is GO only if every database is GO. Upwell never accepts pgcopydb's own success (Phase 0 showed it can report success after losing changes). Each database must pass all of these:

| Check | What it proves |
| --- | --- |
| Final heartbeat on the target | A row written to the source after writers stopped reached the target, so everything committed before it did too |
| Target origin reached the heartbeat position | The target's replication origin advanced at least to the WAL position just before that heartbeat |
| Schema | The same tables, columns and indexes exist on both sides |
| Row counts | Exact counts match for every table up to `exact_count_max_mb` (1 GB by default); larger tables are compared by estimate and shown as such |
| Checksums | Row checksums match for every table that fits in the time budget (`verify_checksum_budget_seconds`, smallest first); tables beyond the budget are listed as not checksummed |
| Sequences | Target sequences are at or past the source's values |

**GO** (green) means applications may switch to the target. **NO-GO** (red) means at least one check failed: do not switch; the source is unchanged, so restart writers there and read the failed checks. If the final heartbeat never arrives, the cutover stops with NO-GO before any end position is set, and the cutover can be retried after you fix the cause. Turning the heartbeat off is possible but is recorded as an accepted risk in the report.

## Known limitations

- **F2, silent loss without target privileges.** Without SET on `session_replication_role` on the target, pgcopydb 0.18 applies nothing, logs no error and reports success. Upwell blocks this in preflight, re-checks it inside each target database before launching, and the verdict would catch it anyway; but if Advanced's `doadmin` lacks the privilege, online migrations to Advanced need a decision on access (see the findings).
- **F8, receive throughput.** In the Phase 0 container the engine received about 2 MB/s of WAL; a source writing faster could never catch up. This has not been measured on a droplet. Watch the backlog trend; preflight's `cdc_throughput` check compares the sampled source WAL rate with what this droplet can receive.
- **F9, the last transactions before the end position.** pgcopydb 0.18 sometimes never applies the last large transactions before an idle period and reports success. Upwell's final heartbeat (pushed until it arrives) and the data comparison make this a NO-GO instead of a silent loss; it is not fixed in the engine.
- **F11, apply rate.** In the build container pgcopydb 0.18 applied about 190 rows a second for a large transaction and about 5 transactions a second for small ones (the target itself accepted 17,000 inserts a second). If that holds on a droplet, an online migration only keeps up with databases that commit a few transactions a second. Preflight's `apply_rate` check compares each database's commit rate with `assumed_apply_tps`, and the cutover gate requires the target to be less than `cutover_max_lag_seconds` behind. **Measure this first on the droplet** with `dev/demo-data.sh write` against a test migration.
- **F12, pgcopydb's internal catalog lock.** pgcopydb's processes share a SQLite file and sometimes fail with "database is locked". During the base copy Upwell restarts the database from zero once automatically; during streaming it stops the stalled engine and resumes it (F13).
- **Roles are copied without passwords.** A non-superuser cannot read role passwords, so login roles are created on the target without one; the migration's events list them. Set their passwords on the target before switching applications.
- **DDL is not streamed.** Schema changes made on the source during a migration do not reach the target; verification then ends NO-GO (tested). Freeze schema changes until the cutover.
- Write freeze is not available, so stopping writers is manual. Only online mode is available. One migration runs at a time per operator action (no queue).
- systemd as PID 1, the polkit rule, the watchdog and reboot recovery could not be exercised in the build container; they are implemented to spec and marked "needs droplet" in `docs/PROGRESS.md`.

## Where things are

- **Logs:** in the UI under **Logs** (live, searchable, downloadable). On the droplet: `journalctl -u upwell -f`, `/var/log/upwell/app.log`, engine logs in `/var/lib/upwell/runs/<instance>/engine.log`, installer log in `/var/log/upwell/install.log`.
- **Config and keys:** `/etc/upwell/config.yaml`, `/etc/upwell/master.key` (back it up; stored passwords cannot be decrypted without it), `/etc/upwell/tls/`.
- **Data:** `/var/lib/upwell/store.db` (SQLite).
- **Screenshots:** [`docs/screenshots/`](docs/screenshots/), every screen at 1440 and 1280 pixels wide in dark and light.
- **Status and decisions:** [`docs/PROGRESS.md`](docs/PROGRESS.md), [`docs/DECISIONS.md`](docs/DECISIONS.md), [`docs/phase0/findings.md`](docs/phase0/findings.md), [`docs/spec.md`](docs/spec.md).

## Development

```bash
go build ./cmd/upwell                      # the UI in internal/webui/dist is embedded
(cd web && npm ci && npm run build)        # rebuild the UI after changing web/src
go test ./...                              # unit tests
sudo bash spikes/env/local-clusters.sh up  # local source and target for the suites below
go test -tags integration -timeout 60m ./test/integration/   # API, data-safety and fault tests
dev/e2e.sh                                 # Playwright: screenshots, layout, axe, states, keyboard, CLS
dev/run.sh start                           # dev server on https://127.0.0.1:8443 (dev/config.dev.yaml)
```
