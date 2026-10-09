# Upwell Phase 0 findings

Oct 7, 2026 · status: **partial, gate not yet passable**

This document records the Phase 0 validation spikes required by the [build specification](../spec.md) (Delivery phases, Phase 0). Each item marked "verify in Phase 0" in the spec appears below with what was tested, the evidence, the verdict, and the design change it implies.

The gate for Phase 0 is "a findings document with evidence for each item, and the design updated where an item failed". This document has evidence for the items that can be proven on local PostgreSQL. The items that need a real DigitalOcean account, real Standard and Advanced clusters, or a droplet with systemd as PID 1 have runnable scripts but no results yet (see [What is still open](#what-is-still-open)). The spec has **not** been edited; proposed changes are listed in [Proposed spec changes](#proposed-spec-changes) for the team to accept or reject.

## Bottom line

- **The biggest result is not on the spec's list.** pgcopydb 0.18 lost committed changes silently, while reporting success with no error in its log, in two different ways: when the target lacked one privilege (F2), and intermittently for the last large transactions before the end position (F9, 5 of 7 runs). Until F9 is understood, an online migration's GO verdict cannot rest on anything pgcopydb reports, and the cutover needs a proof that the last writes arrived (proposed spec change 13).
- **Two design changes are needed regardless of the remaining tests:** run engines from a template unit instead of `systemd-run` (the proposed polkit rule would otherwise give the service user root), and treat every engine stop as a SIGKILL of the whole unit (pgcopydb ignores graceful stop signals while streaming).
- **The remaining items need a droplet, real Standard and Advanced clusters and a DO account.** The scripts are ready. The single most important one is the S02 probe on Advanced: if Advanced's admin lacks the two apply privileges, pgcopydb cannot run there as that user.

## Summary

| # | Item from the spec | Verdict | Design impact |
| --- | --- | --- | --- |
| 1 | pgcopydb under `systemd-run`, cgroup stop, slot release timing | **Partly proven** (cgroup mechanics proven; systemd itself not run) | Stop needs SIGKILL; graceful signals do not stop a streaming run |
| 2 | polkit rule for the service user | **Not run** (needs a droplet). Design flaw found by analysis | Transient units under a polkit name rule are a root escalation; use a template unit |
| 3 | DO API endpoints, fields, firewall merge behavior | **Proven from the API's OpenAPI source**; not exercised against a live account | Firewall PUT replaces all rules; read, merge, write. Advanced is `engine: advanced_pg` |
| 4 | Write-freeze privileges on Standard (Aiven-backed) clusters | **Mechanics proven locally**; real Standard cluster not run | Freeze fails on databases the admin does not own |
| 5 | Logical-message drain nudge | **Proven** | Needed: an idle database never reaches its end position without it. Nudge immediately, not after 60 s |
| 6 | `pgcopydb stream prune` | **Proven.** Prune works on a live run, and 0.18 already prunes on its own every 5 minutes. The run that tested it lost data silently, which led to F9 | Do not call prune; do not use the coordinator |
| 7 | Measured metrics storage for the proposed budget | **Proven, budget exceeded by the spec's table shape** | Spec's metrics table: 2.45 GB for the 7-day, 10-database case (budget 2 GB). Compact shape: 0.50 GB |

**New findings not anticipated by the spec.** F2, F3, F9 and F10 were reproduced in repeated runs; F7 was seen once; F8 is a measurement of this environment:

| ID | Finding | Severity |
| --- | --- | --- |
| F1 | pgcopydb's change apply needs two target privileges a non-superuser admin lacks by default: EXECUTE on the `pg_replication_origin_*` functions, and SET on `session_replication_role` | Blocking if Advanced's admin lacks them |
| F2 | Without SET on `session_replication_role`, pgcopydb 0.18 **applies nothing, logs no error, and reports the end position as reached**. The target silently misses every change made during streaming | Critical: silent data loss without independent verification |
| F3 | pgcopydb's receive process does not stop on SIGTERM or SIGINT while streaming (tested 30 s); only SIGKILL stops it. Its sentinel and log say it stopped cleanly and then it reconnects | High: affects Stop, cutover abort and every relaunch |
| F4 | Replication origins live in a cluster-wide catalog on the target. Dropping the target database does not remove them, and a leftover origin makes the next run fail | Medium: cleanup and restart-from-zero must drop origins explicitly |
| F5 | A run that fails early (for example on F1) leaves its slot and publication behind on the source | Medium: cleanup must handle runs that never reached CDC |
| F6 | pgcopydb's sentinel `replay_lsn` can move past changes that were never applied (F2, F9). The replication origin on the target is better but not sufficient: it exposed F2 and the S08 loss, yet in S09 it moved past the skipped transaction | High: neither position proves the data arrived; only a data comparison does |
| F7 | A burst of about 2.8 GB of WAL outran pgcopydb's receive and invalidated the slot at `max_slot_wal_keep_size=1GB`. pgcopydb exited with code **12**, the same code as the 0.15 apply bug, so the exit code alone cannot classify the failure | High: classify by `wal_status = lost` and the log line, not the exit code |
| F8 | Receive throughput here was about **2 MB/s of WAL**, with the receive process in IO wait. That bound comes from this container's disk, not a measured pgcopydb limit, but any source writing WAL faster than receive can never catch up | High until measured on a droplet: it decides whether busy sources can be migrated online at all |
| F9 | pgcopydb 0.18 **intermittently never applies the last large transaction(s) written before an idle period and the end position**, logs no error, and reports success. S09: lost in **5 of 7 runs**: 4 of 4 while the run's coordinator was being polled, 1 of 3 without. S08 lost 3 of 30 batches the same way | Critical: data verification is the only reliable guard |
| F10 | When the base copy failed (one index worker hit `SQLite: database is locked` in pgcopydb's own catalog, with the default 4 jobs), the clone step exited, but pgcopydb's main process and its receive process **kept running and streaming**, holding the slot for 5 minutes until the spike killed them | High: a failed base copy does not end the unit, so "unit exited non-zero" never fires |

## Test environment

- Container: Ubuntu 24.04.5, kernel 6.18, 4 vCPU, 15 GB RAM. **No systemd as PID 1**, so `systemd-run` cannot run here. Engine units were emulated with a cgroup v2 group per unit; the stop sequence mirrors `KillMode=control-group`: SIGTERM to every process, wait `TimeoutStopSec` (30 s), then `cgroup.kill`. Results from this mode are labelled `cgroup-emulated`.
- PostgreSQL 16.15 (Ubuntu package), two local clusters from [`spikes/env/local-clusters.sh`](../../spikes/env/local-clusters.sh):
  - source: `wal_level=logical`, 20 slots and WAL senders, `max_slot_wal_keep_size=1GB` (raised to 12 GB from S08's second attempt on, see F7), `wal_sender_timeout=60s`; admin `doadmin` with LOGIN, REPLICATION, CREATEDB and CREATEROLE, member of `pg_signal_backend`, `pg_read_all_stats` and `pg_monitor`, **not superuser** (the spec says the real Standard `doadmin` had REPLICATION; the rest is an assumption to check on a real cluster).
  - target: same non-superuser `doadmin`, plus a pre-existing `doadmin.aries_health` table, as Advanced has.
- pgcopydb **0.18**, built from the `v0.18` tag (commit `95ebd55`): the PGDG apt repository is blocked by this environment's network policy, and Ubuntu's own package is 0.15.
- Workloads: S01, S02 and S05 ran a writer that committed one INSERT and one UPDATE about every 50 ms. S08 and S09 wrote large single-transaction batches (60 to 100 MB of row data each).

Every spike is a script under [`spikes/`](../../spikes/README.md) and writes JSON lines under [`evidence/`](evidence/). Numbers below are from the last complete run of each spike on quiet clusters.

## 1. Engine units: stop, crash, resume and slot release

Spike: [`s01-unit-stop.sh`](../../spikes/s01-unit-stop.sh). Evidence: [`evidence/s01/results.jsonl`](evidence/s01/results.jsonl), engine logs alongside. All scenarios run during CDC (base copy finished, changes streaming).

| Scenario | Result |
| --- | --- |
| A. Stop the unit (SIGTERM to every process, SIGKILL after 30 s) | SIGTERM did **not** stop the run. The unit emptied only at the SIGKILL: 30.16 s. Slot inactive **43 ms** after the group was empty |
| B. SIGKILL pgcopydb's main process | **Reproduces the harness finding**: 10 s later the receive and apply processes were still alive (3 of 4 processes) and the slot was still active. Stopping the group cleared it; slot inactive 44 ms after. `pgcopydb.pid` held the main PID (matched the unit's main PID) |
| C. Resume after B (`--resume --not-consistent`) | Reused the slot and origin (start position unchanged), **no base copy** started, drained to the end position in 15.3 s, target matched the source (row count and sum) |
| D. Stale walsender (every engine process frozen, TCP left open) | The slot stayed active past 20 s, as expected with `wal_sender_timeout=60s`. `pg_terminate_backend` as the same non-superuser role succeeded; slot inactive **41 ms** later |
| E. SIGTERM to the main process only | Main did not exit within 30 s; all 4 processes still alive; slot still active |
| F. SIGINT (pgcopydb's "fast stop") to every process | Did **not** empty within 30 s; slot still active after 60 s |
| H. SIGTERM to every process plus terminate the walsender | Did **not** empty within 30 s. The apply process exited at once; the receive process logged a clean stream end, then **reconnected** and kept streaming (finding F3) |
| G. Resume after D, E, F and H | Drained in 16.7 s; target matched the source |

What this proves:

- **cgroup-wide stop works and is required.** Stopping every process in the group frees the slot within about 50 ms. The harness finding that a dead main process leaves sub-processes streaming is confirmed on 0.18, so running pgcopydb as the unit's main process with `KillMode=control-group` is necessary.
- **Graceful signals do not stop a streaming run** (F3). In every attempt the apply process exited on the signal and the receive process did not. pgcopydb catches the signals (`/proc/<pid>/status` shows SIGINT and SIGTERM caught, none blocked or ignored). In `ld_stream.c` the receive loop is written to stop on them, but in practice it logged "reconnecting in 1s" and kept streaming. The root cause is not established; it should be reported upstream with these logs.
- **Resume after SIGKILL is safe in CDC** on 0.18: C and G both converged with the source.
- **Stale walsender release** works as specified: the same role can terminate it without superuser, and the slot frees in well under a second.

Not proven here, needs a droplet (`RUNNER=systemd bash spikes/s01-unit-stop.sh`):

- That systemd itself stops the remaining processes when pgcopydb's main process dies (B's `systemd_auto_cleanup_ms`). The spec relies on this. With `Type=simple`, systemd stops the unit when the main process exits, and `KillMode=control-group` then kills the rest of the cgroup; that is documented systemd behavior, but it has not been observed with pgcopydb.
- `systemd-run --uid=upwell` launching pgcopydb with the property set in the spec. Section 2 explains why the template unit should replace it.

Design impact:

1. `Engine.Stop` cannot offer a meaningful `graceful` mode with pgcopydb 0.18. Use `immediate`: SIGKILL the whole cgroup after a short grace period. The grace period buys nothing today, so `TimeoutStopSec` can be short (for example 10 s). Correctness comes from resume, proven by C and G.
2. Every stop is followed by the slot-release wait from the spec (terminate the walsender after 15 s if needed). Measured release is about 40 ms once the processes are gone.
3. Cutover abort and the restart-from-zero flow assume a stop takes up to `TimeoutStopSec` plus the slot wait, not seconds.

## 2. polkit rule for the service user

Spike: [`s07-polkit/`](../../spikes/s07-polkit/) (rule, template unit, and `test.sh` for a droplet). **Not run**: this environment has no systemd as PID 1.

Analysis that changes the design before any test:

- The spec grants `org.freedesktop.systemd1.manage-units` for units named `upwell-eng-*` and launches them with `systemd-run`. polkit cannot inspect the properties of a transient unit: it sees the action, the unit name and the verb. A rule that allows "start `upwell-eng-*`" therefore lets the `upwell` user start **any command as any user, including root**, simply by naming the unit `upwell-eng-x` (for example `systemd-run --unit=upwell-eng-x --uid=root /bin/sh -c ...`). That defeats running the app unprivileged.
- Proposed instead: a pre-installed template unit `upwell-eng@.service` that fixes `User=upwell`, the engine binary and the kill settings, and reads only the engine's arguments from a per-run environment file. polkit then allows start, stop, restart and reset-failed of `upwell-eng@*.service` only. The service user can choose engine arguments, never the command or the user.
- pgcopydb is still the unit's main process: `ExecStart=/usr/local/bin/pgcopydb $UPWELL_ENGINE_ARGS` splits the unquoted variable into separate arguments. Values must not contain spaces; paths under `/var/lib/upwell/runs/<id>` and slot names do not.
- Ubuntu 22.04 ships polkit 0.105, which does not read JavaScript `.rules` files (only `.pkla`, which cannot match unit names). The spec says 22.04 "should work"; the droplet test records `js_rules_supported`. If it is false, 22.04 needs another mechanism (for example a sudoers entry limited to `systemctl start|stop upwell-eng@*`), or 22.04 support is dropped.

`test.sh` checks, as the `upwell` user: T1 the template starts without a password, **T2 `systemd-run --uid=root` is refused**, T3 unrelated units are refused, T4 the instance runs as `upwell` with pgcopydb as main process.

## 3. DigitalOcean API

Evidence: [`evidence/s04-do-api.md`](evidence/s04-do-api.md), every fact cited to the `digitalocean/openapi` repository (commit `97dca99`, Oct 7 2026) or DigitalOcean's own `doctl`, `godo` and `go-metadata` code. `docs.digitalocean.com` was not reachable from this environment, so facts from it are marked as search summaries. **No call was made against a live account.**

| Question | Answer |
| --- | --- |
| Standard vs Advanced | Only `engine` distinguishes them: `pg` (Standard) and `advanced_pg` (Advanced). No edition or tier field; size slugs are not reliable |
| Firewall (trusted sources) | `GET` / `PUT /v2/databases/{uuid}/firewall`, body `{"rules":[{type, value, description?, uuid?}]}`, PUT returns 204. The reference does not say "replace", but `doctl databases firewalls replace` documents replacing all rules, and `doctl ... append` reads, merges and writes "so that we don't destroy existing rules". **Treat PUT as replace-all.** No ETag, so concurrent edits can overwrite each other; preserve each rule's `description`. Limit 100 rules, no IPv6 |
| Storage size | `storage_size_mib` (MiB). Whether a GET returns the total or only the additional storage is **unconfirmed** |
| Maintenance window | No GET endpoint; read `maintenance_window` (`day`, `hour` in UTC, `pending`, `description`) from the cluster object |
| VPC | `private_network_uuid` (needs `vpc:read` scope) |
| Credentials | `connection` and `private_connection` give `host`, `port`, `user`, `password`, `database`, `ssl`, `uri`; user and password need the `database:view_credentials` scope |
| Databases | `GET` / `POST /v2/databases/{uuid}/dbs` exist; relevant to the open question on creating Advanced databases by API rather than SQL |
| Droplet identity | Metadata service `http://169.254.169.254/metadata/v1/id`, `/region`, `/interfaces/private/0/ipv4/address`. The VPC UUID is not in the metadata service; use `GET /v2/droplets/{id}` |
| Rate limits | 5,000 requests per hour and 250 per minute per token; `ratelimit-*` headers; 429 when exceeded. `Retry-After` appears only on 429s from the per-minute limit |
| Least-privilege scopes | Discovery: `database:read` (+ `vpc:read`). Credentials: `database:view_credentials`. Trusted-source changes: `database:update` (not `firewall:update`) |

Still to confirm with a live token (script to be written when an account is available): the replace semantics by observation, whether `storage_size_mib` is the total, and the exact error bodies.

## 4. Write freeze privileges

Spike: [`s03-write-freeze.sh`](../../spikes/s03-write-freeze.sh), safe to run against a real Standard cluster (it creates and drops its own database and role). Evidence: [`evidence/s03/results.jsonl`](evidence/s03/results.jsonl). Run here against the local non-superuser `doadmin` only.

| Check | Result |
| --- | --- |
| `ALTER DATABASE ... SET default_transaction_read_only = on` on a database the admin owns | Works |
| Same on a database owned by another role | **Fails: "must be owner of database"** |
| New application session after the freeze | Writes refused ("cannot execute INSERT in a read-only transaction") |
| Session opened before the freeze | **Keeps writing** (3 more rows in 3 s), so terminating sessions is mandatory, as the spec says |
| Terminate the application's session | Works, but only because `doadmin` is a member of `pg_signal_backend` locally (an assumption) |
| A frozen session turning read-only off for itself | Works (`SET default_transaction_read_only = off`), so the freeze is a guard, not a lock, as the spec says |
| `ALTER DATABASE ... RESET` | Undoes it for new sessions |
| `pg_logical_emit_message` as the admin | Allowed (needed for item 5) |

Design impact: the freeze can only be offered per database when the admin owns that database (or is a member of its owner). Preflight must check ownership and `pg_signal_backend` membership per database and hide or block the freeze option per database, not only per cluster. On a real Standard cluster, the run must also confirm that `doadmin` is a member of `pg_signal_backend`.

## 5. Drain nudge with a logical decoding message

Spike: [`s05-drain-nudge.sh`](../../spikes/s05-drain-nudge.sh). Evidence: [`evidence/s05/results.jsonl`](evidence/s05/results.jsonl). The end position was set with `stream sentinel set endpos --current` right after writes stopped.

| Plugin | Database idle, cluster idle | Database idle, another database busy |
| --- | --- | --- |
| pgoutput | **Never reached the end position** (still running after 30 s). After `pg_logical_emit_message(true, 'upwell', 'drain nudge')`: exited in **431 ms**, target matched | Exited by itself in 6.5 s, target matched |
| test_decoding | Same: still running after 30 s; exited **432 ms** after the nudge, target matched | Exited by itself in 6.6 s, target matched |

Design impact: at cutover writes have stopped, so an idle cluster is the normal case, and without a nudge the drain would never finish. The spec's "emit a message if replay does not move for 60 seconds" would add 60 s to every write pause for no benefit. **Emit the nudge in every database immediately after setting its end position.** It is a transactional message with no data change, and the non-superuser admin may emit it.

## 6. `pgcopydb stream prune`

Spike: [`s08-stream-prune.sh`](../../spikes/s08-stream-prune.sh). Evidence: [`evidence/s08/results.jsonl`](evidence/s08/results.jsonl).

From pgcopydb 0.18 source, before running:

- `stream prune` exists in 0.18. It deletes only closed CDC file pairs (`*-output.db`, `*-replay.db`) whose end is below the sentinel `replay_lsn`.
- CDC files rotate when the current one reaches `maxReplayDBSize`. `clone` exposes **no option** to change it, so `clone --follow` always rotates at **1 GiB**. A running clone therefore frees space only in steps of about 1 GiB, and never frees the file it is writing.
- Two modes: direct (`--dir`, opens the run's catalog file) and coordinator (`--host` / `--port`, asks the running follow process to prune, which `clone` accepts as hidden `--host` / `--port` options).

Results (second attempt; the first invalidated its slot, see F7). For this run the local source had `max_slot_wal_keep_size=12GB` and writes were paced so receive stayed within about 200 MB:

| Step | Result |
| --- | --- |
| Rotation | Happened at 1.16 GB of `output.db` ("reached 1161347072 bytes (threshold 1073741824 bytes), rotating") |
| Coordinator prune, dry run, then real | Removed the closed pair while the run kept streaming: **2,035 MB freed** (output plus replay file) |
| **Automatic cleanup** | Not in the spec's assumptions: the 0.18 follow process **prunes applied file pairs on its own every 5 minutes**, with or without the coordinator (`follow_coordinator_maybe_cleanup`, called from the follow loop in `follow.c`). It freed the second rotated pair (2,037 MB) with no command from us |
| Direct-mode prune while running | Ran and removed files from a live run |
| Drain and compare | **Target missing 300,000 rows (3 of 30 batches), pgcopydb reported success, no error logged.** The loss happened *before* the direct prune; it became finding F9 |

Leftover detail: after a pair is removed, its SQLite `-wal` sidecar file (about 4 MB) is left behind.

Design impact:

1. **Upwell does not need to call prune.** pgcopydb 0.18 already removes applied CDC files every 5 minutes. Staging on the droplet is bounded by the changes received but not yet applied, plus the file pair being written (up to about 2 GB per database, since rotation happens at 1 GiB of `output.db` and the replay file is of similar size). The disk guard and the `staging_space` estimate should use that model.
2. **Upwell should not run prune or any other coordinator command against a live run** (F9). The disk guard's action at 95% stays as specified: stop the fastest-growing database.
3. The 1 GiB rotation cannot be changed through `clone`, so the staging floor per database is about 2 GB. With many databases on one droplet, that floor alone sets a minimum work-volume size: preflight should include it.

## 7. Metrics storage

Spike: [`s06-metrics-storage/`](../../spikes/s06-metrics-storage/). Evidence: [`evidence/s06/results.json`](evidence/s06/results.json).

The simulation writes the store as it would be at the end of day 7 of a 10-database migration: 423 series (18 per database every 5 s, 3 per database every 30 s, 10 dead-tuple series per database every 60 s, per-job COPY and index series during the base copy, cluster and host series), raw 5 s samples for the last 72 h, 1-minute rollups for 7 days, 15-minute rollups for 7 days.

| Table shape | Raw rows | Rollup rows | File size | Bytes per row | 6 h chart query | 24 h overview query |
| --- | --- | --- | --- | --- | --- | --- |
| Spec's `metrics` table: ULID id, migration, database, name, ts, value, created_at, index on (migration, name, database, ts) | 10.6 M | 3.8 M | **2.45 GB** | 169 | 0.7 ms | 12 ms |
| Compact: `series` table plus `samples(series_id, ts, value)` WITHOUT ROWID | 10.6 M | 3.8 M | **0.50 GB** | 34 | 0.5 ms | 1.4 ms |

Design impact: the spec's table shape exceeds the 2 GB budget. A per-row ULID and repeated text names cost about five times the space of the data. Use the compact shape: a `series` table, and samples keyed by `(series_id, ts)` with no surrogate ID. Projected size for a 30-day run is about 0.9 GB with the same retention. This deviates from "Every table has `id` (ULID)" in the Data model; samples are the exception.

## Additional findings

### F1 and F2: target privileges, and silent data loss without them

Spike: [`s02-target-privileges.sh`](../../spikes/s02-target-privileges.sh). Evidence: [`evidence/s02/results.jsonl`](evidence/s02/results.jsonl).

| Target privileges for the non-superuser admin | Engine log errors | pgcopydb says end position reached | Target matches source |
| --- | --- | --- | --- |
| None beyond defaults | 5 (`permission denied for function pg_replication_origin_oid`); run exits | No | No (nothing copied) |
| EXECUTE on `pg_replication_origin_*` | **0** | **Yes** | **No**: 205 rows written during streaming are missing (180 in an earlier run) |
| Origin functions plus `GRANT SET ON PARAMETER session_replication_role` | 0 | Yes | Yes |

In the middle case the target server logs `permission denied to set parameter "session_replication_role"` once, for the first transaction pgcopydb tries to apply; nothing is applied after it. pgcopydb's apply code (`ld_apply.c`) sets `session_replication_role = 'replica'` at the start of every applied transaction, and the failure never appears in its own log. In the first run (its evidence is in commit `2a9e737`; the rerun replaced `results.jsonl`) its sentinel still advanced `replay_lsn` to the end position (`0/32F76A80`) and it ended the run as successful, while the replication origin on the target stayed at its creation position (`0/32F64F88`): the origin told the truth, the sentinel did not (F6). The rerun matched (sentinel `2/74ED1300`, origin `2/74EBDC38`, the slot's start).

Design impact:

1. New hard preflight check `target_apply_privileges` (target, per database): EXECUTE on the origin functions and SET on `session_replication_role` (PostgreSQL 15+: `has_parameter_privilege`). The probe in S02 part 1 is that check, and it is read-only.
2. **Phase 0 must run the S02 probe against a real Advanced cluster.** If Advanced's `doadmin` lacks either privilege, pgcopydb cannot run CDC there as `doadmin`, and the migration approach needs a decision (a DO-provided role, a pgcopydb change, or an engine option).
3. The app must never treat pgcopydb's own success as proof. A cheap check that would have caught this case: after the drain, the target's replication origin (`pg_replication_origin_progress`) must have reached the end position; here it stayed at its creation position. That check is necessary but not sufficient: in F9 the origin did move past a transaction that was never applied, so verification must still compare data (F6).
4. Engine `Observe` should also watch the target server for apply errors where it can (`pg_stat_database.xact_rollback` growth on the target database is one cheap signal).

### F7: slot invalidated under a write burst

Evidence: [`evidence/s08-invalidated/`](evidence/s08-invalidated/) (engine log and CDC-rate samples from the first S08 attempt). Fourteen 100 MB transactions written back to back on the source while receive ran at about 2 MB/s. Within about three minutes the slot passed `max_slot_wal_keep_size` (1 GB on the test source) and PostgreSQL invalidated it. pgcopydb logged `can no longer get changes from replication slot ... This slot has been invalidated because it exceeded the maximum reserved size` and the run exited with code 12.

Design impact: this is the spec's "Slot invalidated" path, now reproduced with 0.18. The orchestrator must classify it from `pg_replication_slots.wal_status = 'lost'` (or the log line) and go to Restart required. Exit code 12 also meant "CDC apply bug" on 0.15, so it is not a classification. The headroom forecast in the spec is the right protection; the spike shows how fast it can be consumed.

### F8: receive throughput

Evidence: [`evidence/s08/cdc-rate.csv`](evidence/s08/cdc-rate.csv) and [`evidence/s08-invalidated/cdc-rate.csv`](evidence/s08-invalidated/cdc-rate.csv), sampled every 30 s. pgcopydb's receive process wrote the changes to its SQLite store at about 1.8 to 2.1 MB of WAL per second, with its process in uninterruptible IO wait and the container under sustained IO pressure (`/proc/pressure/io` "full" around 14 to 19%). The source generated WAL far faster during the bursts.

This number describes this container, not a droplet with block storage, so it must not be used as an estimate. It does show the shape of the risk: CDC on a busy source is bounded by receive speed on the droplet's disk, and if the source's sustained WAL rate exceeds it, the backlog only grows until the slot is invalidated (F7). Phase 0 should measure receive and apply throughput on a droplet sized per the Scale section, and preflight should compare the sampled source WAL rate (`wal_sample_seconds`) with the measured or calibrated receive rate.

### F9: silent loss of the last large transaction before the end position

Evidence: [`evidence/s08/`](evidence/s08/) (`engine.log`, `cdc-rate.csv`) and [`evidence/s09/`](evidence/s09/). Spike: [`s09-large-txn-skip.sh`](../../spikes/s09-large-txn-skip.sh).

Found in S08. That script waited for something to become prunable by calling `stream prune --dry-run` through the run's coordinator every 0.2 s (about 2,800 requests over 10 minutes). Batch 28 (committed 36 s before the polling started, and not yet applied when it started) and batches 29 and 30 never reached the target. At 20:39:53 the sentinel's `replay_lsn` jumped to the source position, and nothing was applied after that. The target's replication origin stopped at `1/5F33A3B8` and was still there after the drain, well short of the end position `1/72E5D9D8`. The direct prune at 20:47 then deleted the CDC files that still held those transactions, which made the loss unrecoverable, but it did not cause it: the dry run 10 seconds earlier had already reported that file pair as fully applied.

S09 tries to isolate it: eight transactions of 60,000 rows (about 60 MB each), 45 s apart, then 60 s idle, end position, nudge, drain and compare. `poll` sends coordinator dry-run requests every 0.2 s for the whole run; `quiet` sends none. Both start the run with `--host`/`--port` and sample the sentinel directly (`--dir`) every 5 s.

| Run | Mode | Jobs | Target rows (of 480,000) | Missing | pgcopydb reported success | Final origin past batch 8's commit |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | quiet | default | 480,000 | none | yes | yes |
| 1 | poll | default | 420,000 | batch 8 | yes | **yes** (would not have flagged it) |
| 2 | quiet | default | 480,000 | none | yes | yes |
| 2 | poll | default | 420,000 | batch 8 | yes | no |
| 3 | poll | default | 420,000 | batch 8 | yes | no |
| 4 | poll | 2 | 420,000 | batch 8 | yes | no |
| 3 | quiet | 2 | 420,000 | batch 8 | yes | no |

No run logged an error. Two further quiet attempts with default jobs failed during the base copy (F10) and are not in the table; one attempt was voided by an editing mistake on my side, and one by starting before the previous run's cleanup had finished.

What this shows:

- The loss is real, repeatable and silent, and it always hit the **last** transaction(s) before an idle period and the end position.
- Coordinator polling makes it more likely here (4 of 4 against 1 of 3), but **it also happens without it**. The trigger is not established. Run 3 quiet also differed in using 2 jobs, but jobs only affect the base copy, and run 4 poll lost the same batch with 2 jobs.
- Checking after the drain that the target's replication origin has passed the last commit before the end position would have flagged 4 of these 5 losses and S08, with no false alarm on the 2 good runs. It missed poll run 1. It is a useful cheap alarm, not proof.

Design impact:

1. **Verification is the only reliable guard, and it must look at data.** The spec's row counts would catch whole missing transactions only if they insert or delete rows in tables that are counted exactly; a lost UPDATE batch, or rows in a table above `exact_count_max_mb` (compared by planner estimates), would pass. Proposed: make the heartbeat table on by default for online migrations, write one final heartbeat row **after** writes stop and before the end position is set, and require it on the target before GO. A heartbeat written after the application's last transaction proves everything before it was applied, if pgcopydb applies transactions in commit order, which is its design. Keep `compare_data` as the recommended setting where the window allows.
2. Add the origin check above to the drain step as an alarm: origin not past the last commit means NO-GO immediately.
3. Do not start runs with `--host`/`--port` and never send coordinator commands. That avoids the condition that made the loss more frequent here, though it does not prevent it.
4. **This finding blocks pgcopydb 0.18 for online migrations until it is understood.** Report upstream with S09 (it reproduces on a laptop in about 10 minutes per run), and run the same script against any newer pgcopydb release or fix before pinning a version.

### F10: a failed base copy does not end the run

Evidence: [`evidence/s09/engine-quiet-3-basecopy-failed.log`](evidence/s09/engine-quiet-3-basecopy-failed.log). In one S09 attempt (default 4 table and index jobs), an index worker failed to update pgcopydb's SQLite catalog (`database is locked`), so creating the constraints for `public.orders` failed. The workers exited with code 12, the clone step with 6 ("clone process ... has terminated [6]"), and the apply process quit. The main process and the receive process did not: they kept streaming changes into the CDC store and holding the slot until the spike's cleanup killed the group five minutes later. The same failure happened in the next attempt too ([`engine-quiet-3b-basecopy-failed.log`](evidence/s09/engine-quiet-3b-basecopy-failed.log)), after four S09 runs with identical settings had passed, so it is intermittent but not rare here.

Design impact: supervision cannot wait for the unit to exit. The pgcopydb engine's `ParseLog` must treat `clone process ... has terminated`, `Failed to clone source database` and any `ERROR` during the base copy as a database failure, and the orchestrator must then stop the unit itself (F3: by SIGKILL) and mark the database Restart required. The SQLite lock contention inside pgcopydb at default concurrency also belongs in the upstream report.

### F4 and F5: leftovers

- Replication origins are stored in a shared catalog on the target. Dropping the target database leaves the origin, and the next `clone` fails with "Replication origin ... already exists". Cleanup and restart-from-zero must call `pg_replication_origin_drop` (needs the F1 privilege) by name, `upwell_<migration>_<db>`.
- A run that failed before CDC (F1 case) left its slot **and** its publication on the source. The `slot_exists` and `publication_exists` checks catch this; cleanup must also handle runs that never got past setup.

## Proposed spec changes

For the team to accept or reject at the gate. None of these has been applied to [`docs/spec.md`](../spec.md).

1. **Engine units:** replace `systemd-run` transient units with the template `upwell-eng@.service` (section 2); polkit allows only that template. Rename "Engine units" to `upwell-eng@<migration>-<db>.service` in Naming conventions.
2. **Stop:** `Engine.Stop` is effectively immediate for pgcopydb: SIGKILL of the cgroup after a short timeout (proposed 10 s), then the slot-release wait. Keep the `graceful` mode in the interface for future engines; the pgcopydb engine reports it as unsupported in `Capabilities`.
3. **Cutover drain:** emit the logical-message nudge right after setting each end position; drop `cutover_nudge_seconds` or set its default to 0.
4. **Preflight:** add `target_apply_privileges` (hard), and make `write_freeze` availability per database (owner or member of owner, plus `pg_signal_backend`).
5. **Observe and verification:** report CDC progress from the sentinel, but never treat it as proof. After the drain, require the target's replication origin to have reached the end position (catches F2 and the S08 loss), and still compare data, since the origin can move past a skipped transaction (F9). The origin lags the sentinel by one or two transactions in normal operation (S09 samples), so a live gap between them is not an alert on its own.
6. **Cleanup:** drop origins by name on the target, and clean up runs that never reached CDC.
7. **Metrics store:** the compact series-plus-samples shape; exempt samples from the ULID rule.
8. **Disk guard and staging:** do not call `stream prune`; rely on pgcopydb's 5-minute automatic cleanup; size staging as unapplied changes plus about 2 GB per database (section 6).
9. **pgcopydb upstream:** report F2 (silent apply failure), F3 (receive ignores stop signals), F9 (silent loss of the last transaction before the end position) and F10 (run keeps streaming after a failed base copy; catalog lock contention) with the scripts and logs in `evidence/`.
10. **Failure classification:** classify slot invalidation by `wal_status = 'lost'` and the log message, never by exit code (F7).
11. **Preflight `cdc_throughput`** (new, warning or acceptable blocker): compare the source's sampled WAL rate with the receive rate measured on this droplet, once Phase 0 has measured it (F8).
12. **No coordinator:** never start runs with `--host` / `--port` and never send coordinator commands to a live run (F9).
13. **Cutover proof of apply:** heartbeat on by default for online migrations; write a final heartbeat after writes stop and before setting the end position, and require it on the target before GO. Add the origin-past-last-commit alarm to the drain (F2, F9).
14. **Base-copy failure detection:** detect it from the engine log, then stop the unit; do not wait for the unit to exit (F10).

## What is still open

These need resources this environment does not have. Each has a ready script.

| Item | Needs | Command |
| --- | --- | --- |
| systemd behavior for item 1 | Ubuntu 24.04 droplet, local or real clusters | `RUNNER=template bash spikes/s01-unit-stop.sh` (the proposed `upwell-eng@` template, after `s07-polkit/test.sh` installed it) and `RUNNER=systemd bash spikes/s01-unit-stop.sh` (the spec's `systemd-run` shape, for comparison) |
| polkit (item 2) | Disposable Ubuntu 24.04 and 22.04 droplets | `sudo bash spikes/s07-polkit/test.sh` |
| Target privileges on Advanced (F1) | A test Advanced cluster | `UPWELL_DST_URI=... PROBE_ONLY=1 bash spikes/s02-target-privileges.sh` |
| Write freeze on Standard (item 4) | A test Standard cluster | `UPWELL_SRC_URI=... bash spikes/s03-write-freeze.sh` |
| End-to-end CDC on real clusters | A test Standard and Advanced pair | `s01`, `s05` and `s08` with `UPWELL_SRC_URI` / `UPWELL_DST_URI` |
| DO API live behavior (item 3) | A DO account and a scoped token | Script to be written |
| Receive and apply throughput (F8) | Droplet with block storage, real or local clusters | `s08-stream-prune.sh` records `cdc-rate.csv`; a dedicated sustained-write benchmark should follow |
| Throughput and timings at scale | Droplet and clusters sized per the Scale section | Phase 5, but a first number would calibrate the estimates |
