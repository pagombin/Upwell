# Upwell

Upwell is a migration console for moving PostgreSQL databases between DigitalOcean managed clusters, for example from a Standard cluster to an Advanced one, with the applications online until a short cutover. It runs on one droplet, drives [pgcopydb](https://github.com/dimitri/pgcopydb) 0.18 as the copy and change-streaming engine, and does everything from the browser: define the migration, check it, start it, watch it, stop and resume it, cut over with a data-safety verdict, read the report and clean up.

**Status: demo build, not GA.** It covers Phases 1 to 3 of [the spec](docs/spec.md) as a vertical slice, plus the Phase 4 pieces needed to install it and survive restarts. [`docs/PROGRESS.md`](docs/PROGRESS.md) has the item-by-item status; [`docs/DECISIONS.md`](docs/DECISIONS.md) records every decision made while building it.

## Architecture

![Upwell architecture: the operator's browser and Prometheus talk to upwell.service on the droplet; upwell.service checks both clusters with SQL and starts one pgcopydb unit per database; the base copy streams rows from the source to the target through memory, while changes are decoded from the source's slot, staged in the work directory on the droplet's disk, and applied to the target after the base copy](docs/architecture.png)

Upwell is one Go binary (`upwell.service`, with the UI embedded) plus one systemd unit per database being copied (`upwell-eng@<migration>-<database>.service`, running pgcopydb). Upwell never moves data itself: it checks both clusters, starts and supervises the engine, and at cutover compares the data with its own queries to decide GO or NO-GO. The diagram's source is [`docs/architecture/architecture.html`](docs/architecture/architecture.html); `dev/render-architecture.sh` renders it.

**What is stored on the droplet.**

- **Base copy: nothing.** Each table is read with `COPY ... TO STDOUT` on the source and written with `COPY ... FROM STDIN` on the target by the same pgcopydb process, so rows pass through memory only. Only the schema dump (no data) is written to disk.
- **Changes (CDC): yes, staged on disk.** From the moment the base copy starts, pgcopydb decodes the source's changes from its replication slot into `/var/lib/upwell/runs/<instance>/clone/cdc/` (`*-output.db`), converts them to SQL (`*-replay.db`), and applies them only after the base copy has finished. Both files together take about twice the volume of the changes (measured locally: 274 MB for 120 MB of new rows), they grow for the whole migration, and they are removed by Clean up, not after they are applied. Preflight's `staging_space` check estimates the room needed, the Host screen shows the staging size, a critical alert fires when the work volume reaches 85% (`alert_disk_crit_pct`), and the disk guard stops the database with the largest staging at 95% (`disk_guard_stop_pct`). A source that writes a lot during a long base copy therefore needs a large work volume, and its backlog takes a while to drain afterwards (see F11 below).

## What works and what is stubbed

**Works**

- Install on a fresh Ubuntu 24.04 droplet with `deploy/install.sh` (idempotent: a second run changes nothing). A systemd service with a watchdog, one systemd unit per database being copied, and a polkit rule limited to those units.
- First-run setup in the browser (a setup token plus a TLS fingerprint check), local users with three roles (Viewer, Operator, Admin), a hash-chained audit log, encrypted stored passwords.
- A seven-step wizard: customer permission record, source and target with a live connection test, database selection, options, preflight (more than 50 checks; acceptable blockers can be accepted with a reason), and a plan review that shows the exact engine command for each database.
- Online migrations of one or many databases, with a scheduler for concurrent base copies, live progress, charts and logs, per-database stop, resume and restart from zero, and pause, resume and abort for the whole migration.
- Automatic recovery: transient failures resume with backoff; the engine being killed, the app restarting and the droplet rebooting are reconciled; a failed base copy or a lost slot goes to Restart required with a diagnosis.
- A cutover console with manual stop-writes or an optional **write freeze** (with a preview of exactly what it runs and ends, an audit entry, automatic undo when the cutover does not end GO, and an always-available undo button), in both cases Upwell sampling the source to confirm writes really stopped, a write-pause timer, drain, sequence sync and **verification that compares data**, ending in GO or NO-GO.
- An HTML and Markdown report, cleanup of slots, publications, origins, the heartbeat schema, work files and stored credentials, and alerts, events, logs, host metrics and system health screens.
- Offline migrations (writes stopped first, confirmed or frozen; one copy; verification), a TOTP second factor with recovery codes (required for Admins by default), read-only API tokens, a Prometheus `/metrics` endpoint, a redacted support bundle with a preview, a run lock with a queue (reorder, unqueue, optional auto-start), PDF reports rendered on the droplet, watched tables with exact counts on both sides, saved log views, network throughput on the Host screen, and TLS certificate upload with rollback.

**Stubbed** (labelled "Coming soon" in the UI; nothing does nothing silently): DigitalOcean API discovery (enter connection details by hand) and notifications (Slack, email, PagerDuty). **Not in this build** (no UI at all, since each needs an external service or was out of scope): SSO, update checks, offline install bundles, and self-serve mode.

## Install

On a fresh Ubuntu 24.04 droplet (2 vCPU and 4 GB is enough for a test; give the work volume room for the copy's staging files):

```bash
sudo apt-get update && sudo apt-get install -y git
git clone https://github.com/pagombin/Upwell.git && cd Upwell
sudo bash deploy/install.sh
```

The first run takes 5 to 15 minutes: it adds the PostgreSQL apt repository, installs the newest PostgreSQL client, builds pgcopydb from the v0.18 tag with Upwell's fixes in `deploy/pgcopydb` (version `0.18+upwell.2`; the packages lose transactions that overlap another one, D84, and fail a base copy when their own catalog stays locked for 5 seconds, D87), downloads Go to build Upwell (the UI is committed prebuilt, so Node is not needed), creates the `upwell` user, writes `/etc/upwell/config.yaml`, creates the master key and a self-signed certificate, opens SSH and HTTPS in `ufw`, and starts `upwell.service`. It ends by printing:

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

**Option A: two DigitalOcean managed clusters.** Add the droplet to both clusters' trusted sources. The target's `doadmin` must be able to apply replicated changes. Phase 0 found that pgcopydb needs EXECUTE on the `pg_replication_origin_*` functions and SET on `session_replication_role` (finding F1). On the first droplet test (October 2026) the Advanced cluster's `doadmin` did not have them until the grant script was run on the target; preflight reported it as a hard blocker until then, and as passing afterwards. Preflight checks this (`target_apply_privileges`) and the connection test shows it, so you will know before anything is copied; the check's evidence has the grant script.

**Option B: two local clusters on the droplet** that mimic the pair (a non-superuser `doadmin` with REPLICATION on the source; a non-superuser `doadmin` with the apply privileges on the target):

```bash
sudo apt-get install -y postgresql-16
sudo bash spikes/env/local-clusters.sh up
# source: 127.0.0.1:55432, target: 127.0.0.1:55433, user doadmin, password spike-password-not-secret, TLS mode "disable"
```

Then:

1. **Create test data** on the source: `dev/demo-data.sh create 'postgresql://doadmin:PASSWORD@HOST:PORT/defaultdb?sslmode=require' 200000` creates a `shop` database with about 600,000 rows.
2. **New migration** (left rail). Step 1: fill in the permission record (any test values). Steps 2 and 3: enter the source and target, press **Test connection** on each; every line should be green. Step 4: include `shop` only. Step 5: keep the defaults (heartbeat on). Step 6: **Run preflight**; it should end with no blockers. Step 7: read the engine command, tick that you reviewed the warnings, and press **Start migration**.
3. **Watch it.** The overview shows the base copy, then Catch-up, then In sync. Open the database for its charts, attempts and logs; **Logs** shows everything live. During the base copy the database page lists each table being copied with a bar (rows copied against the source's estimated row count) and the indexes being built; its **Copied to the target** and **Slot retained WAL** charts fill from the start, while **Backlog**, **Replay rate** and **Heartbeat latency** start once the base copy has finished and changes are being applied. Slot retained WAL rises and falls while the engine works: the source keeps WAL for the slot until the engine confirms it has received it, and WAL is cluster-wide, so writes in other databases on the source count too.
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
| Schema | The same tables and columns (with NOT NULL, identity and generated expressions) exist on both sides |
| Schema objects | The same indexes, constraints (including NOT VALID ones), triggers, views, functions, partitions and enum values |
| Row counts | Exact counts match for every table up to `exact_count_max_mb` (1 GB by default); a larger table outside the checksum window is only checked for being empty or truncated on the target, and is listed as not counted |
| Checksums | Row checksums match for every table that fits in the time budget (`verify_checksum_budget_seconds`, smallest first) and has the same row count on both sides; tables beyond the budget are listed as not checksummed |
| Large tables, row by row | With `verify_large_tables = full` (or "Verify every table fully" after a verdict), every row of the tables beyond the budget is compared by primary key range, in parallel and resumable; a differing range is named |
| Coverage | Lists every table whose rows were not all compared; a GO with any such table is shown as "GO, not fully verified" with the tables, never as a plain GO |
| Sequences | Target sequences are at or past the source's values (in their direction, for descending ones); a sequence the user may not read is a failure, not "unused" |
| Large objects and materialized views | Large objects match exactly; materialized views match (refreshed on the target when the source refreshed them while streaming); a view that cannot be read on either side is a failure, never "differs" |
| Engine reached the end position | The engine applied everything up to the end position; a database that did not drain is NO-GO |
| Writes stopped | The source's row-change counter has not moved since the writers step, no client holds an open write transaction and no prepared transaction waits, and no table was truncated or rewritten and no sequence or large object changed (a fingerprint); resampled after each database's comparison and once more for every database before the verdict, so a writer that resumed (or an Admin's override of detected writes) ends NO-GO |
| Results recorded | Every result above was stored for the report; a database whose results could not be recorded is NO-GO |

Every check fails closed: a query that could not run is a failure, never a pass, and a verdict over databases that could not be read, or over none, is never GO. The test suite proves this by failing each query verification makes in turn (`TestP1EveryVerificationQueryFails`), and every Part 1 scenario compares source and target independently of Upwell's own checks (see `docs/PROGRESS.md`).

**GO** (green) means applications may switch to the target. **NO-GO** (red) means at least one check failed: do not switch; the source is unchanged, so restart writers there and read the failed checks. If the final heartbeat never arrives, the cutover stops with NO-GO before any end position is set, and the cutover can be retried after you fix the cause. Turning the heartbeat off is possible but is recorded as an accepted risk in the report.

## Known limitations

- **F2, silent loss without target privileges.** Without SET on `session_replication_role` on the target, pgcopydb 0.18 applies nothing, logs no error and reports success. Upwell blocks this in preflight, re-checks it inside each target database before launching, and the verdict would catch it anyway; but if Advanced's `doadmin` lacks the privilege, online migrations to Advanced need a decision on access (see the findings). The check's evidence has the grant script the platform runs (per database and in template1; databases Upwell creates from template0 need it too) and the revoke script for after cutover, which the report's post-cutover checklist repeats. While streaming, an apply watchdog independent of pgcopydb compares the target's replication origin with the source: if the source's tables change and the origin does not move for `apply_stall_seconds` (5 minutes), a critical alert fires and the cutover is held.
- **F8, receive throughput.** In the Phase 0 container the engine received about 2 MB/s of WAL; a source writing faster could never catch up. This has not been measured on a droplet. Watch the backlog trend; preflight's `cdc_throughput` check compares the sampled source WAL rate with what this droplet can receive.
- **Overlapping transactions (fixed in Upwell's engine build, D84).** pgcopydb 0.18 and its main branch skip a transaction that began before the previously applied one committed, so with concurrent writers committed changes never reach the target. Upwell builds pgcopydb with a fix and refuses online migrations with any other build (preflight `engine_commit_order`). A migration made with an unfixed engine under concurrent writes may have missed rows in tables verification only estimated (larger than `exact_count_max_mb` and outside the checksum window): compare those tables before relying on it.
- **F9, the last transactions before the end position.** pgcopydb 0.18 sometimes never applies the last large transactions before an idle period and reports success. Upwell's final heartbeat (pushed until it arrives) and the data comparison make this a NO-GO instead of a silent loss; it is not fixed in the engine.
- **F11, apply rate.** In the build container pgcopydb 0.18 applied about 190 rows a second for a large transaction and about 5 transactions a second for small ones (the target itself accepted 17,000 inserts a second). If that holds on a droplet, an online migration only keeps up with databases that commit a few transactions a second. Preflight's `apply_rate` check compares each database's commit rate with `assumed_apply_tps`, and the cutover gate requires the target to be less than `cutover_max_lag_seconds` behind. **Measure this first on the droplet**: the preflight screen's "Measure apply rate" runs the engine on scratch databases on both clusters, reports how many single-row transactions a second it applies (4.8 in the build container), offers that value less 20% as `assumed_apply_tps`, and removes everything it created.
- **F12, pgcopydb's internal catalog lock.** pgcopydb's processes share a SQLite file and sometimes failed with "database is locked"; Upwell's engine build waits up to 2 minutes for it instead of 5 seconds (D87). During the base copy Upwell restarts the database from zero once automatically; during streaming it stops the stalled engine and resumes it (F13).
- **Offline mode stops writes for the whole copy.** Writers stop (or the write freeze is applied) before the copy and must stay stopped until the verdict; any write in between ends NO-GO. It needs no replication privileges on either side.
- **Watched tables cost a scan each.** Their exact counts read the whole table on both sides once a minute; watch only the tables you need.
- **An uploaded TLS certificate lives in the data directory.** It takes precedence over `tls_cert`/`tls_key` in the configuration file; roll back from Settings, TLS.
- **The second factor has no QR code.** Enter the secret (or paste the `otpauth://` URI) into the authenticator app. Admins must set it up at their first sign-in unless `mfa_required_for_admins` is turned off; `upwell user reset-totp` turns it off from the droplet's shell for a lost device.
- **Roles are copied without passwords.** A non-superuser cannot read role passwords, so login roles are created on the target without one; the migration's events list them. Set their passwords on the target before switching applications; the report's post-cutover checklist lists them with the other follow-ups (revoking the migration's privileges, refreshing materialized views, re-creating event triggers and foreign-server user mappings).
- **Connection poolers.** Use the cluster's direct port when it has one. Behind a pooler, Upwell and pgcopydb work in session mode (tested with PgBouncer); transaction mode is refused by the connection test and preflight because the engine's session settings would be lost.
- **DDL is not streamed.** Schema changes made on the source during a migration (a new table or partition, a changed column, a new enum value) do not reach the target. Upwell notices within a minute (a critical alert and a readiness condition that blocks the cutover), and verification would end NO-GO anyway (tested). Freeze schema changes until the cutover, or apply the same change on the target.
- **The droplet's firewall and long connections (D88).** With ufw enabled, conntrack could mark packets of a long, fast stream INVALID and ufw dropped them, cutting copies hours in. install.sh keeps `net.netfilter.nf_conntrack_tcp_be_liberal = 1`, and the root unit `upwell-firewall.path` allows each cluster address Upwell lists (`/var/lib/upwell/firewall/clusters`, rules tagged `upwell cluster`, removed when no migration uses them). Preflight's `network_path` check blocks until both are in place.
- **A base copy cannot resume.** If a connection to either cluster closes in the middle of the base copy, the database goes to Restart required and the copy starts again from zero (pgcopydb does not retry a table, and resuming would not be consistent). The reason says which connection closed, during which table, and whether that cluster's PostgreSQL server restarted, or reset all its connections after one of its processes crashed or was killed (which points at memory, disk or a failover), or kept running (which points at a proxy, load balancer, connection limit or something that ended that one session). The failed copy's replication slot is dropped right away, so the source does not keep WAL for it while it waits for the restart. Large tables are copied in parts by default (`split_tables_larger_than = auto`), which keeps each COPY short; lowering `table_jobs` and `index_jobs` reduces the load on the target.
- **Provider extensions are left out.** `aiven_extras`, the management extension of DigitalOcean Standard (Aiven) clusters, is never copied: its schema and the grants on it are left out, and an event says so. Any other extension the target cannot provide stops the launch with its name, including on a restart from zero after the source gained it.
- **A lost slot means a restart from zero (D95).** After a source failover the new primary has no logical slot (before PostgreSQL 17), and a slot past `max_slot_wal_keep_size` loses its WAL; either way the changes since the last apply cannot be streamed again. A resume checks the slot first and puts the database in Restart required with the reason; Upwell never resumes past such a gap and never restarts from zero by itself. A target failover is different: the replication origin's progress is replayed on the standby with the data, so streaming resumes (tested with an asynchronous standby promoted behind PgBouncer).
- **An open transaction can hold the base copy back (D94).** Creating the slot waits for every transaction running on the source cluster at that moment, in any database. Upwell names each session it waits for (event, overview, database page, `upwell_snapshot_wait_seconds`); ending them is the customer's call, Upwell never does.
- **REPLICA IDENTITY FULL without a unique key.** When two rows are exactly identical, the engine applies an update or delete of one of them to both. Preflight warns about such tables and verification ends NO-GO naming the cause; give those tables a key before migrating.
- **xml needs XML support on the target.** Preflight's `xml_support` blocks xml columns when the target was built without it.
- **Large objects are not streamed.** They are copied during the base copy and compared at verification; a change made while streaming ends NO-GO.
- **The write freeze is a strong guard, not a lock.** It makes new sessions read-only by default (for the database, and for each login role whose own setting would override it) and ends client sessions; a session can still run `SET default_transaction_read_only = off` and write, which the writers check detects. Sessions it could not end are named in an event. After a GO the freeze is kept, so the old source stays read-only: undo it from that migration's cutover console before migrating the same database again (preflight's `source_writable` check blocks until then). It needs the admin user to own each source database and be a member of `pg_signal_backend`; otherwise preflight says so and writers are stopped by hand. One migration runs at a time: others wait in the queue and start when an operator starts them (or by themselves with auto-start, never for offline migrations).
- systemd as PID 1, the polkit rule, the watchdog and reboot recovery could not be exercised in the build container; they are implemented to spec and marked "needs droplet" in `docs/PROGRESS.md`.

## Where things are

- **Logs:** in the UI under **Logs** (live, searchable, downloadable). On the droplet: `journalctl -u upwell -f`, `/var/log/upwell/app.log`, engine logs in `/var/lib/upwell/runs/<instance>/engine.log`, installer log in `/var/log/upwell/install.log`.
- **Config and keys:** `/etc/upwell/config.yaml`, `/etc/upwell/master.key` (back it up; stored passwords cannot be decrypted without it), `/etc/upwell/tls/`.
- **Data:** `/var/lib/upwell/store.db` (SQLite).
- **Architecture:** [`docs/architecture.png`](docs/architecture.png), drawn from [`docs/architecture/architecture.html`](docs/architecture/architecture.html).
- **Screenshots:** [`docs/screenshots/`](docs/screenshots/), every screen at 1440 and 1280 pixels wide in dark and light.
- **Status and decisions:** [`docs/PROGRESS.md`](docs/PROGRESS.md), [`docs/DECISIONS.md`](docs/DECISIONS.md), [`docs/phase0/findings.md`](docs/phase0/findings.md), [`docs/spec.md`](docs/spec.md).

## Development

```bash
go build ./cmd/upwell                      # the UI in internal/webui/dist is embedded
(cd web && npm ci && npm run build)        # rebuild the UI after changing web/src
go test ./...                              # unit tests
sudo bash spikes/env/local-clusters.sh up  # local source and target for the suites below
go test -tags integration -timeout 180m ./test/integration/  # API, data-safety and fault tests (about 80 minutes)
# TestVersionMatrix also uses PostgreSQL 14, 15 and 18 targets when they run (see spikes/env/local-clusters.sh)
# Online tests need Upwell's pgcopydb build: install it with deploy/install.sh, or build it with
#   spikes/env/build-pgcopydb.sh DIR and set UPWELL_IT_PGCOPYDB (and UPWELL_PGCOPYDB for dev/e2e.sh) to the printed path
# UPWELL_IT_KEEP=DIR keeps each test's store and engine logs
dev/e2e.sh                                 # Playwright: screenshots, layout, axe, states, keyboard, CLS
dev/run.sh start                           # dev server on https://127.0.0.1:8443 (dev/config.dev.yaml)
```
