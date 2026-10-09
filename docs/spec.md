# Upwell: Scope and Build Specification

Oct 7, 2026 · @Paul

## How to use this document

This is the build specification for Upwell, a web application installed on a droplet that migrates databases from DigitalOcean Standard to Advanced. A coding agent should build it phase by phase (see Delivery phases), and treat every "must" as a requirement with a test.

The existing `pgmigrate.sh` harness (v1.2, about 2,600 lines of bash) is the reference implementation. It was tested end to end against PostgreSQL 16 with pgcopydb 0.15 and 0.18, including crash and resume. Port its behavior, not its structure. Where this document and the harness disagree, this document wins.

Conventions:

- **Must** = required for the phase to pass. **Should** = expected unless there is a written reason not to. **May** = optional.
- Every behavior about pgcopydb or PostgreSQL that the app relies on is either proven (see Known behaviors, with how it was proven) or marked **verify in Phase 0**. The agent must not build on an unverified behavior without first writing the test that proves it.
- Names in `code` (binary, paths, settings keys, API routes) are proposals the agent may rename consistently, but must keep stable once Phase 1 ships.
- Plain English everywhere the user sees text: UI labels, logs meant for operators, reports, errors. No jargon without a one-line explanation the first time it appears in a screen.

Definition of done for any feature: it works from the UI and the API, it writes structured logs and audit events, it survives an app restart and a droplet reboot in the state the operator expects, it has unit tests and at least one integration test against real PostgreSQL, and its failure modes produce an actionable message (what happened, what it means, what to do).

## Phase 0 amendments (accepted Oct 8, 2026)

The Phase 0 findings ([docs/phase0/findings.md](phase0/findings.md)) proposed fourteen changes. All fourteen are accepted and applied in the sections below; each amended passage cites its change number (PC1 to PC14). In short: engines run from a template unit (PC1); every stop is a SIGKILL of the whole cgroup after 10 s (PC2); the drain nudges immediately (PC3); preflight gains `target_apply_privileges` and per-database freeze availability (PC4); pgcopydb's own positions are never proof (PC5); cleanup drops origins by name and handles runs that never reached CDC (PC6); metrics use a series-plus-samples shape (PC7); prune is never called (PC8); upstream reports (PC9); slot loss is classified by `wal_status` (PC10); preflight compares WAL rate with receive throughput (PC11); the coordinator is never used (PC12); the cutover requires proof that the last writes arrived (PC13); base-copy failure is detected from the log (PC14).

## Product summary

Upwell moves a customer's databases from a Standard cluster to an Advanced cluster with the application online until a short, measured write pause at cutover. It wraps proven tooling (pgcopydb first) in a guided, observable, auditable workflow that a support engineer can run confidently on databases from a few GB to multiple TB.

**Operators, in rollout order:**

1. **DO support engineers**, running migrations for customers who have given written permission. This phase exists to build confidence in the tool. Every migration records who ran it, under which customer permission, and every action taken.
2. **Customers**, running it themselves on their own droplet. The product must already be safe for this audience at the end of the support phase: no step should depend on DO-internal access, knowledge or networks.

**Goals:**

- Migrate every database on a Standard PostgreSQL cluster to Advanced, online, with verification that proves the target matches before anyone switches applications.
- Make every step visible: what is happening, how far along, how long left, what could go wrong, and the raw detail behind it (every log line from every component).
- Survive failure: app restarts, droplet reboots, network drops, crashed copy processes. Recover automatically where it is safe, and stop and explain where it is not.
- Support mixed workloads at scale: many small databases, a few huge tables, high write rates, long-running transactions, large objects, unlogged tables.
- Be engine-agnostic: pgcopydb is the first engine; a MySQL engine must fit later without changing the UI, state machine or storage.

**Non-goals for the first release:**

- Migrating between providers other than DigitalOcean Standard to Advanced (the design must not prevent it).
- Running more than one migration at a time on one install (many may be defined; one runs).
- Schema transformation, filtering rows, or merging databases. The target is a faithful copy.
- A hosted or multi-tenant service. Each install is one droplet, one team.
- Sending any data or telemetry back to DigitalOcean automatically. Support bundles are exported only by an operator, redacted.

## Decisions already made

These are settled; the agent must not revisit them without asking.

| Topic | Decision | What it implies for the build |
| --- | --- | --- |
| Operators | DO support first (with customer written permission), customers later | Role-based access, a mandatory customer-permission record per migration, redacted support bundles, nothing DO-internal in the workflow |
| Cluster discovery | Both: DigitalOcean API token or manual connection details, chosen per migration | Every screen and check works with manual details alone; the API only adds discovery and automation |
| Engine | Pluggable engine interface; pgcopydb is the first implementation | UI, state machine, storage and checks talk to an interface, never to pgcopydb directly |
| Concurrency | Many migrations can be defined; one runs at a time | A run lock per install, a queue the operator controls, and resource budgeting sized for one active migration |
| Stack | Go backend, single binary with the UI embedded | Static assets compiled in; no runtime other than the binary, PostgreSQL client tools and the engine binaries |
| Installer | A bash script that installs everything once, and on later runs updates only what changed | Versioned component manifest, checksums, idempotent steps, no reinstall of unchanged dependencies |
| Write freeze at cutover | Offered as an explicit, confirmed option, off by default | A separate, audited cutover step with its own confirmation, preview of exactly what it will change, and a guaranteed undo |
| TLS | Self-signed certificate generated at install; operator may upload their own | Certificate management screen, expiry warnings |
| Secrets | Encrypted at rest | AES-256-GCM with a local key file; database passwords and API tokens never written in plain text, never logged |
| Metrics export | Prometheus endpoint | `/metrics` behind authentication or a separate token |
| Notifications | Slack, email, generic webhook | Per-event routing rules |
| Audit | Append-only audit log of every operator action, including accepted risks | Audit viewer, export, tamper-evidence (hash chain) |

## Naming conventions

The product is **Upwell**: data rising steadily from Standard to Advanced, in DigitalOcean's ocean naming family, and not tied to one engine or source. Use these names everywhere, from the first commit. A descriptive customer-facing name, if marketing wants one, changes only the UI title and documentation, never the identifiers below.

| Where | Name |
| --- | --- |
| Product name (UI, docs, reports) | Upwell |
| GitHub repository | `upwell` (or `dbaas-upwell` if the org prefixes repositories by team) |
| Go module | `github.com/<org>/upwell` |
| Binary and CLI | `upwell` (`upwell serve`, `upwell selftest`, `upwell migrate ...`) |
| Installer | `install.sh`, published as `install-upwell.sh` |
| App service and health timer | `upwell.service`, `upwell-health.timer` |
| Engine units | `upwell-eng@<migration>-<db>.service`, instances of the installed template `upwell-eng@.service` (PC1) |
| Service user and group | `upwell` |
| Paths | `/etc/upwell`, `/var/lib/upwell`, `/var/log/upwell` |
| Replication slots, origins, publications | `upwell_<migration>_<db>`, lowercase, at most 63 characters |
| Prometheus metrics | `upwell_*` |
| Environment variables | `UPWELL_*` |
| Release artifacts | `upwell-<version>-linux-amd64.tar.gz`, `upwell-<version>-bundle.tar.gz`, `manifest.json` |

The slot and origin prefix matters operationally: anyone looking at `pg_replication_slots` on a customer's cluster can see at once that Upwell owns a slot, which helps support and cleanup. Preflight's leftover checks and cleanup must match on this prefix.

The brand name has had only a web search for collisions, which found no database migration tool called Upwell. A trademark check is required before any customer-facing release.

## Architecture

One Go binary (`upwell`) runs the web server, API, orchestrator and collectors. Engine processes (pgcopydb) run **outside** that process, each in its own systemd transient unit, so restarting or updating the app never kills a running copy. All state lives in one SQLite database on the droplet.

&#91;embedded content: Upwell architecture · app, engine units, clusters\]

The browser, Prometheus and the DO API talk only to the app. The app controls engine units through systemd and tails their log files. The engines read from the source and write to the target.

| Component | Responsibility |
| --- | --- |
| HTTPS server | Serves the embedded UI and the API on port 443 (configurable). TLS 1.2+ only. Redirects port 80 to 443 if enabled. |
| API layer | REST for commands and queries, Server-Sent Events (SSE) for live streams (progress, logs, events). Authentication, RBAC and audit on every mutating call. |
| Orchestrator | Owns the migration state machine. Runs preflight, launches and supervises engine units, drives cutover, applies retry policy, reconciles state after restart or reboot. Single writer for migration state. |
| Engine interface | The contract every engine implements (see Engine interface). The pgcopydb engine translates it into pgcopydb commands, files and signals. |
| Engine units | One systemd service instance per database being migrated (`upwell-eng@<migration>-<db>.service`), an instance of the template `upwell-eng@.service` that the installer ships. The template fixes the user, the engine binary and the kill settings; the app supplies only the engine's arguments through a per-run environment file (PC1). systemd tracks every child process in the unit's cgroup, so stopping the unit stops all of them. |
| Collectors | Goroutines that sample source, target, engine and host metrics on an interval, write them to the metrics store and publish them to the event bus. |
| Check registry | Preflight and verification checks as independent, versioned units with IDs, levels and remediation text. |
| Logger | Structured logging for the app, plus capture and parsing of every engine log line, all addressable by migration, database and component. |
| Event bus | In-process publish/subscribe. Everything the UI shows live flows through it. |
| Store | SQLite in WAL mode via a pure-Go driver (no cgo, so the binary stays static). Holds config, encrypted secrets, migrations, checks, events, audit log and metrics. |
| Notifier | Routes events to Slack, email and webhooks by rule. |
| Watchdog | systemd `WatchdogSec` with `sd_notify` heartbeats from the orchestrator loop, plus an internal health model surfaced at `/healthz` and in the UI. |

Technology choices, with the reason for each:

- **Go 1.23 or later**: single static binary, strong concurrency, mature PostgreSQL drivers (`pgx`).
- **SQLite (pure Go driver such as `modernc.org/sqlite`)**: zero-ops, transactional, survives reboot, enough for one install's state and metrics with rollups.
- **React + TypeScript + Vite**, compiled and embedded with Go's `embed`. Component system: Radix primitives with Tailwind (shadcn/ui style) for an enterprise look with accessible components. Charts: Apache ECharts (handles long time series smoothly). Tables and the log viewer: TanStack Table with row virtualization.
- **systemd** for the app service, the watchdog and every engine unit. Ubuntu 24.04 LTS is the supported OS; 22.04 should work.
- **No external services** required: no Redis, no message broker, no separate database server, no internet access after install (the DO API is optional).

Why engine processes run in systemd units rather than as children of the app: the harness proved that pgcopydb's sub-processes can ignore SIGTERM and outlive their parent, and that a dead main process can leave sub-processes streaming and holding the replication slot. A cgroup-scoped unit with `KillMode=control-group` and a stop timeout followed by SIGKILL stops every process reliably, and the unit outlives app restarts. Phase 0 (S01) proved the cgroup mechanics: stopping every process in the group frees the slot in about 40 ms. It also showed pgcopydb ignores SIGTERM and SIGINT while streaming, so every stop is a SIGKILL of the cgroup (PC2). systemd's own behavior is still to be confirmed on a droplet.

## Engine interface

The orchestrator, UI and store know only this interface. An engine migrates **one database** per run handle; the orchestrator runs one handle per selected database and coordinates them. pgcopydb is the first implementation; a MySQL engine must be addable without touching anything outside its own package.

```go
type Engine interface {
    Name() string
    Version(ctx context.Context) (string, error)
    Capabilities() Capabilities
    Checks() []check.Definition
    Plan(ctx context.Context, m MigrationSpec, d DatabaseSpec) (RunPlan, error)
    Start(ctx context.Context, p RunPlan) (Handle, error)
    Resume(ctx context.Context, p RunPlan, from ResumePoint) (Handle, error)
    Observe(ctx context.Context, h Handle) (Progress, error)
    SetEndPosition(ctx context.Context, h Handle, pos EndPosition) error
    Stop(ctx context.Context, h Handle, mode StopMode) error
    SyncSequences(ctx context.Context, d DatabaseSpec) error
    Verify(ctx context.Context, d DatabaseSpec, opts VerifyOptions) ([]check.Result, error)
    Cleanup(ctx context.Context, d DatabaseSpec, opts CleanupOptions) error
    ParseLog(line string) LogRecord
}

type Capabilities struct {
    Online               bool
    Offline              bool
    ResumeDuringCDC      bool
    ResumeDuringBaseCopy bool
    TableSplitting       bool
    DecodingPlugins      []string
    CopiesRoles          bool
    CopiesExtensions     bool
}
```

| Method | Contract |
| --- | --- |
| `Checks` | Engine-specific preflight checks, registered alongside the generic ones (same levels, IDs, acceptance rules). |
| `Plan` | Pure function: produces the exact command, environment, files and resource needs for one database. Shown to the operator before start (dry run). No side effects. |
| `Start` / `Resume` | Launch the run (as a systemd unit) and return a handle that survives app restarts (unit name, work directory, slot, origin). `Resume` must refuse when `Capabilities` says resume is unsafe at that point. |
| `Observe` | Cheap, safe to call every few seconds. Returns phase, bytes and rows copied, active work, CDC positions, backlog, errors seen. Never blocks longer than its timeout. |
| `SetEndPosition` | Tells a streaming run where to stop. Idempotent. |
| `Stop` | `graceful` (let it finish the current transaction) or `immediate`. Must leave no process and must release the replication slot within a bounded time, or report that it could not. An engine that cannot stop gracefully says so in `Capabilities`; pgcopydb cannot, so its stops are always immediate (PC2). |
| `Verify` | Schema comparison, row counts, sequence checks, data checksums. Results use the same check model as preflight. The engine's own success or positions are never accepted as proof (PC5, PC13). |
| `Cleanup` | Removes everything the engine created on source and target (slots, publications, origins, sentinel objects), with direct SQL fallbacks if the engine's own cleanup fails. |
| `ParseLog` | Turns one raw engine log line into a structured record (time, level, step, table, message), so the UI can filter and highlight. |

**pgcopydb engine mapping** (behavior proven in the harness unless marked):

| Interface | pgcopydb implementation |
| --- | --- |
| `Start` | `pgcopydb clone --follow` (or without `--follow` for offline) with `--dir`, `--slot-name`, `--origin`, `--table-jobs`, `--index-jobs`, `--split-tables-larger-than`, `--skip-extensions`, `--no-owner`, `--skip-ext-comments` when supported, `--plugin` only when set |
| `Resume` | Same command plus `--resume --not-consistent`. Allowed only after the base copy finished (CDC phase). During the base copy, resume takes a new snapshot that pgcopydb itself says cannot guarantee consistency, so the engine refuses and the orchestrator offers a database restart instead. |
| `Observe` | `pgcopydb stream sentinel get --dir` (plain-text output, not `--json`), the pgcopydb log, `pg_stat_progress_copy` and `pg_stat_progress_create_index` on the target, `pg_replication_slots` on the source |
| `SetEndPosition` | `pgcopydb stream sentinel set endpos --current --dir`, immediately followed by `pg_logical_emit_message(true, 'upwell', 'drain nudge')` in the database (PC3) |
| `Stop` | `systemctl stop` on the unit: SIGTERM, then SIGKILL of the whole cgroup after 10 s (PC2), then wait for the slot to be inactive; after 15 s terminate a stale walsender that still holds the slot (same role, no superuser needed) |
| `SyncSequences` | `pgcopydb copy sequences` (clone already syncs them at endpos; this is a safety net) |
| `Verify` | Schema comparison from the catalogs; exact `count(*)` for tables under a size threshold, planner-estimate comparison above it; order-independent row checksums on every table the verification window allows; sequence comparison; final heartbeat present; target origin progress (PC13) |
| `Cleanup` | `pgcopydb stream cleanup` when a work directory exists, then direct drops of any remaining slot and publication on the source and of the origin **by name** on the target (origins live in a shared catalog and survive dropping the target database), also for runs that never reached CDC (PC6) |
| Never used | The follow coordinator (`--host`/`--port`) and `stream prune` (PC8, PC12): coordinator traffic preceded silent loss in Phase 0 (F9), and 0.18 already prunes applied files every 5 minutes |
| Roles | `pgcopydb copy roles`, falling back to `pg_dumpall --roles-only` with `\restrict` lines stripped and pre-existing target roles skipped |

**Future MySQL engine**: the interface deliberately speaks in generic terms (positions, phases, end position) so a MySQL engine can map them to its own concepts. Which MySQL tooling to wrap is an open question for a separate design; nothing in this release may assume PostgreSQL outside the pgcopydb engine package and the PostgreSQL check pack.

## Migration lifecycle and state machine

Two state machines run together: one per migration and one per database inside it. The migration's state is derived from its databases plus operator actions; only the orchestrator changes either, and every change is written to the store before any side effect happens.

&#91;embedded content: Migration lifecycle · main path and side states\]

The main path runs left to right, then right to left after cutover. From Needs attention or Paused, an operator resumes, restarts a database, or aborts.

**Migration states**

| State | Meaning | Operator can |
| --- | --- | --- |
| Draft | Connections and settings being defined | Edit, run preflight, delete |
| Ready | Latest preflight has no unaccepted blockers, and the customer-permission record is filled in | Start (or queue), re-run preflight, edit (sends it back to Draft) |
| Queued | Waiting for the run lock (another migration is running) | Reorder, unqueue |
| Running | At least one database is preparing or in base copy | Monitor, stop one or all databases, abort |
| Streaming | Every database has finished its base copy and is replaying changes | Monitor, start cutover, stop |
| Cutover | Writers stopped, end positions set, databases draining | Watch; abort only before end positions are set |
| Verifying | Sequences synced, verification running | Watch |
| Completed | Verification GO for every database | Download report, clean up, mark applications switched |
| Needs attention | A database failed, degraded, lost its slot, or verification returned NO-GO | Read the diagnosis, resume, restart a database, accept and continue where allowed, abort |
| Paused | Stopped by an operator; slots and state kept | Resume, abort |
| Aborted | Stopped for good by an operator; cleanup pending | Clean up |
| Cleaned up | Slots, publications, origins, harness objects and credentials removed | Archive, export report |

**Database states**: Pending, Preparing (target database, roles, extensions), Base copy, Catch-up, In sync, Draining, Drained, Verified, Failed, Degraded, Stopped, Restart required (base copy crashed, or slot invalidated).

Rules the implementation must follow:

1. **Intent before action.** Before any side effect (launching a unit, setting an end position, dropping a slot), write an intent record with an operation ID. After it, write the outcome. On restart, the orchestrator reconciles every intent without an outcome by checking the real world (unit status, slot status, sentinel) and finishing or rolling back. This is what makes reboot recovery deterministic.
2. **Idempotent commands.** Every API command carries a client-generated operation ID; repeating it returns the original result instead of acting twice. Double-clicking "Start cutover" must not set end positions twice.
3. **Frozen spec.** When a migration leaves Ready, its settings and selected databases are snapshotted. Changes need a stop and a new snapshot, recorded in the audit log.
4. **Attempts.** Each launch of an engine unit for a database is an attempt with its own record (unit name, command, start and end time, exit code, reason it ended). The UI shows every attempt; the report lists them.
5. **Derived, not stored twice.** The migration's state is computed from its databases' states and operator flags whenever either changes, never edited directly, so the two cannot disagree.
6. **One runner.** A run lock (a row in the store plus a lock file) guarantees one active migration per install. Queued migrations start only when an operator confirms, never automatically, unless the operator turned on auto-start for that queue entry.

## Preflight framework

Preflight is a registry of independent checks, each producing one result per scope it applies to (host, source cluster, target cluster, or one database). A migration can start only when no unaccepted blocker remains and the customer-permission record is complete.

**Check result model**: `id`, `version`, `scope` (with database name where relevant), `level`, `title`, `message` (plain English, one sentence), `evidence` (the queries run and raw values seen), `remediation` (what to do), `duration`, and `accepted_by` / `accepted_at` / `acceptance_reason` when accepted.

**Levels**

| Level | Shown as | Effect |
| --- | --- | --- |
| OK | Green | None |
| Info | Blue | None; context such as sizes and plans |
| Warning | Amber | Start requires the operator to tick "I have reviewed the warnings" |
| Blocker (acceptable) | Red, with an Accept button | Start is disabled until accepted with a written reason by an Operator or Admin |
| Blocker (hard) | Red, no Accept button | Start is impossible; the migration would fail with this condition |

Acceptance rules: an acceptance is tied to the check ID, scope and a hash of its evidence. If a re-run produces materially different evidence (for example more tables without a primary key), the acceptance lapses and must be renewed. Every acceptance is an audit event and appears in the report under Accepted risks.

Preflight runs in the background and streams results to the UI as each check finishes, grouped by scope, with total progress. Slow checks (WAL sampling, throughput sampling) show their own countdown. A full run must finish in under 3 minutes for a cluster with 50 databases, excluding the WAL sample window.

**Check catalog** (ported from the harness unless marked new)

| ID | Scope | Fails as | What it checks |
| --- | --- | --- | --- |
| `customer_permission` (new) | Migration | Hard | Customer name, ticket or reference, permission date and scope are recorded |
| `tools` | Host | Hard | Engine and PostgreSQL client binaries present at the required versions |
| `engine_version` | Host | Hard | pgcopydb version supports the server major (0.18 or later for PostgreSQL 18) |
| `client_version` | Host | Hard | pg\_dump major at least the source major; warning if it differs from the target major |
| `host_resources` (new) | Host | Warning | vCPU, RAM and free disk against the sizing table in Scale |
| `clock_sync` (new) | Host | Warning | System clock synchronized (for log correlation across systems) |
| `dns`, `network` | Clusters | Hard / Warning | Hosts resolve; private addresses preferred; pooler port warned |
| `connect` | Clusters | Hard | Both clusters reachable with the given credentials |
| `trusted_sources` (new) | Clusters | Hard | Droplet IP is allowed on both clusters (via DO API when available, else by connecting) |
| `same_cluster` | Clusters | Hard | Source and target are not the same database on the same server |
| `version_order` | Clusters | Hard | Target major at least source major |
| `primary` | Clusters | Hard | Both endpoints are primaries, not standbys |
| `wal_level` | Source | Hard | `wal_level = logical` (online mode) |
| `repl_slots`, `wal_senders` | Source | Hard | One free slot and one free WAL sender per database |
| `repl_privilege`, `repl_protocol` | Source | Hard | REPLICATION or superuser, and a replication-protocol connection works per database |
| `prepared_xacts` | Source | Acceptable | No prepared transactions (slot creation waits behind them) |
| `long_transactions` | Source | Warning | No transactions open longer than 5 minutes |
| `maintenance_window` (new) | Source | Warning | No scheduled maintenance inside the estimated migration window (DO API only) |
| `databases` | Source | Hard if none | Discovery result: databases with user objects, skipped ones with reasons |
| `read_privs` | Database | Hard | The user can read every table and sequence |
| `publication_privs` | Database | Hard, or automatic switch | pgoutput can create its publication (no unlogged tables, ownership, CREATE privilege); otherwise switch to test\_decoding with a warning unless the operator forced pgoutput |
| `replica_identity` | Database | Acceptable | Every table can replay UPDATE and DELETE |
| `large_objects`, `unlogged_tables` | Database | Warning | Copied once, not streamed |
| `materialized_views`, `foreign_servers`, `event_triggers`, `pg_cron`, `pg_partman` | Database | Warning | Need action outside the copy |
| `encoding` | Database | Hard | Same encoding |
| `collation` | Database | Acceptable | Same normalized locale, matching sort probe and collation version (spelling differences pass) |
| `dst_conflicts` | Database | Hard | No object exists on both sides |
| `dst_extra_objects` | Database | Warning | Objects only on the target are left untouched |
| `dst_database` | Database | Hard if cannot create | Target database exists or can be created with matching locale |
| `extensions` | Database | Hard | Every source extension is available on the target |
| `slot_exists`, `origin_exists`, `publication_exists` | Database | Hard | No leftovers from a previous run (unless resuming) |
| `target_apply_privileges` (new, PC4) | Database | Hard | The target user can EXECUTE the `pg_replication_origin_*` functions and SET `session_replication_role` (without it pgcopydb 0.18 silently applies nothing: F2) |
| `write_freeze_available` (new, PC4) | Database | Info | The admin owns the source database (or is a member of its owner) and is a member of `pg_signal_backend`; otherwise the freeze option is hidden for that database |
| `cdc_throughput` (new, PC11) | Clusters | Warning | Sampled source WAL rate against the receive rate measured or calibrated on this droplet |
| `connections` | Clusters | Acceptable | Free connections cover what every engine unit will open |
| `session_timeouts` | Source | Acceptable | Engine sessions run with statement and idle timeouts disabled |
| `slot_wal_cap` | Source | Warning | `max_slot_wal_keep_size` covers the WAL the slots will retain |
| `target_disk` | Target | Acceptable | Target storage covers existing data plus 1.3 x the databases being migrated (size from DO API, or entered manually) |
| `staging_space` | Host | Acceptable | Work volume has room for estimated change staging; warning below 2x |
| `work_dir` | Host | Hard | Work directory exists, is writable, and is not the root filesystem (warning) |
| `throughput_sample` (new) | Clusters | Info | A short read from the source and write to the target to calibrate time estimates |

## Execution and supervision

Each database runs as one systemd transient unit whose main process is pgcopydb itself (no shell wrapper, no `tee`), so systemd's view of the main process is pgcopydb's. All databases of a migration start together, sized by the per-database job settings and the connection budget.

Launch shape (the orchestrator builds this from `Engine.Plan`; values illustrative):

```ini
# /etc/systemd/system/upwell-eng@.service (installed once; PC1)
[Service]
Type=simple
User=upwell
Group=upwell
EnvironmentFile=/var/lib/upwell/runs/%i/engine.env
ExecStart=/usr/local/bin/pgcopydb $UPWELL_ENGINE_ARGS
StandardOutput=append:/var/lib/upwell/runs/%i/engine.log
StandardError=append:/var/lib/upwell/runs/%i/engine.log
KillMode=control-group
KillSignal=SIGTERM
TimeoutStopSec=10
SendSIGKILL=yes
```

A stop sends SIGTERM to every process in the cgroup and SIGKILL to all of them 10 s later (PC2). The SIGTERM lets the apply process exit cleanly; the receive process ignores it (F3), so in practice the SIGKILL ends every streaming run.

The app writes `engine.env` (`UPWELL_ENGINE_ARGS=clone --follow --dir ... --slot-name upwell_m42_orders --origin upwell_m42_orders`) and runs `systemctl start upwell-eng@m42-orders.service`. Values in the arguments never contain spaces.

The environment file (mode 0600, owned by the service user) holds the connection URIs without passwords; passwords reach libpq through a per-run `PGPASSFILE` that is deleted when the run ends. Secrets never appear in the command line, the unit properties or the logs.

**Supervision signals**, sampled every 5 seconds per database:

| Signal | Source | Means |
| --- | --- | --- |
| Unit state | `systemctl show` (ActiveState, SubState, ExecMainStatus, MainPID) | Running, exited cleanly, failed with code |
| Engine main PID | pgcopydb's `pgcopydb.pid` (first line), compared with the unit's MainPID | Mismatch or dead PID with live children = degraded |
| Progress | `Engine.Observe` | Phase, positions, backlog, errors |
| Slot | `pg_replication_slots` on the source | Active, `wal_status`, retained bytes, headroom |
| Log | New lines in `engine.log`, parsed by `Engine.ParseLog` | Errors, step changes, warnings |

With pgcopydb as the unit's main process, when it exits systemd stops every remaining process in the cgroup, so leftover sub-processes cannot keep streaming or hold the slot. Phase 0 (S01, scenario B) confirmed on 0.18 that without this, a dead main process leaves sub-processes streaming and holding the slot.

**Base-copy failure (PC14).** A failed base copy does not end the run: pgcopydb's main and receive processes keep streaming (F10). The supervisor treats `clone process ... has terminated`, `Failed to clone source database` and any `ERROR` line during the base copy as a database failure, stops the unit itself and marks the database Restart required.

**Retry policy**

| Failure | Examples | Action |
| --- | --- | --- |
| Transient, during CDC | Connection reset, network timeout, server restart, too many connections for a moment | Automatic resume with exponential backoff: 10 s, then x2, capped at 10 min, with jitter; at most 5 attempts per hour per database, then Needs attention |
| Transient, during base copy | Same as above | No automatic restart by default (restarting a multi-TB copy is a business decision). Setting `auto_restart_base_copy` (off) allows it for databases under a size limit |
| Slot invalidated | `wal_status = lost`, or the log line `can no longer get changes from replication slot` (PC10). Never classified by exit code: pgcopydb's 12 means several things | Restart required for that database; never automatic |
| Permanent | Authentication failed, permission denied, schema or restore error, disk full, out of memory | Needs attention, with the classified cause and remediation |
| App-side queries | Collector or check query fails | Up to 3 quick retries (1, 2, 4 s), then mark the metric stale and raise an alert after 3 stale samples |
| DO API | HTTP 429 or 5xx | Honor `Retry-After`, otherwise backoff 2 s to 60 s, 6 attempts |

Before any relaunch (automatic or manual), the orchestrator must:

1. Confirm the old unit is stopped and its cgroup is empty.
2. Wait for the replication slot to become inactive. After 15 seconds, terminate the walsender that still holds it. The harness showed a killed client can leave a walsender holding the slot until `wal_sender_timeout`.
3. Check work-volume free space against the staging estimate.
4. Record the attempt and its reason.

**Per-database restart from zero** (a gap in the harness, required here): stop the unit, drop and recreate the target database (or drop its migrated objects when the target database holds other data), drop the slot, publication and origin, wipe the database's work directory, and start a fresh attempt while the other databases keep streaming. Requires an Operator to confirm with the database name typed in.

**Disk protection**: at 85% work-volume use raise a critical alert; at 95% stop the database with the largest staging growth rather than letting the volume fill. Upwell never calls `pgcopydb stream prune`: 0.18 removes applied change files itself every 5 minutes. Staging per database is the received-but-unapplied changes plus up to about 2 GB for the CDC file pair being written (rotation at 1 GiB of `output.db`) (PC8).

## Cutover

Cutover is one guided, coordinated operation across every database of the migration, run from a dedicated Cutover console screen. It ends in a single verdict: GO only if every database verifies. The write pause is measured from the moment writes stop to the verdict, and shown as a live timer.

1. **Readiness gate.** Every database is In sync (as reported by the engine, which is progress, not proof: PC5), with backlog under the threshold (default 16 MiB each) for at least 60 seconds, no red alerts, and verification dry-run checks pass (schema compare is cheap and can run before writes stop). The console shows each condition with a tick.
2. **Stop writes.** Two paths, chosen in the console:
   1. **Manual (default):** the operator confirms the customer has stopped every writer. The app lists active client sessions on every migrated source database, then samples row-change counters twice, 10 seconds apart. If rows are still changing, it refuses to continue unless an Admin overrides with a typed confirmation, recorded in the audit log.
   2. **Write freeze (optional, off by default):** the console first shows a preview of exactly what it will run. For each source database it sets `default_transaction_read_only = on`, then terminates client sessions other than the engine's and the app's own. It requires the operator to type the cluster name. The freeze is a strong guard, not a lock: a session can still turn read-only off for itself, which the preview states. Whether the Standard admin user may run `ALTER DATABASE` and terminate other roles' sessions on Aiven-backed clusters is **verify in Phase 0**; if not, the option is hidden for those clusters.
3. **Final heartbeat (PC13).** In every database, record the source position, then write one heartbeat row with a unique token into the heartbeat table. This is the last write before the end position. Then write a small "push" heartbeat row every 5 s until each final heartbeat is visible on the target (decision D10; pgcopydb applies a transaction only after a later decodable one arrives): pgcopydb 0.18 can drop the last transaction before an idle period when the end position arrives first (F9), and the heartbeat's arrival proves every earlier write arrived. If a heartbeat does not arrive within the drain timeout, the cutover stops here, before any end position is set, and streaming continues.
4. **Set end positions** for every database at the current source position (idempotent per database), and immediately emit a transactional logical decoding message in each database (no data change) so idle streams pass the end position (PC3; without it an idle database never finished in Phase 0).
5. **Drain**: wait until every engine reaches its end position and exits. Per-database progress is shown. After the drain, the target's replication origin must have reached the final heartbeat's position (the last real write before the end position); otherwise the database is NO-GO immediately (PC5, PC13).
6. **Sync sequences** in every database.
7. **Verify** every database (see Engine interface, `Verify`). GO requires, for every database: the final heartbeat row present on the target, the origin check passed, schema match, exact counts equal for tables under the threshold, and checksums equal for every table checksummed in the window. Optional `ANALYZE` on the target afterwards.
8. **Verdict and handover.** GO shows a post-cutover checklist: add application hosts to the target's trusted sources, switch connection strings for every database, refresh materialized views, reset role passwords if roles were copied without them, keep the source untouched until sign-off. NO-GO shows exactly which checks failed and the rollback path.

**Rollback**, available until the operator marks applications as switched:

- Undo the write freeze if it was applied (`RESET default_transaction_read_only` on each database). The undo runs automatically if cutover is aborted, and the console shows it as a separate, always-available button.
- Applications keep using the source, which was never modified apart from the freeze. The target is kept for investigation; restarting the affected databases is offered.

After applications are switched, rolling back means pointing them back at the source and losing writes made on the target since. That is outside the tool; the console says so plainly before the operator marks the switch.

Optional after GO: keep the freeze on the source after the switch, as protection against stray writes going to the old cluster. Release it from the console.

## Monitoring, metrics, alerts and notifications

Collectors sample every 5 seconds by default (configurable 2 to 60), store every sample, compute rates and forecasts, evaluate alert rules, and publish to the UI live. Expensive measurements (database sizes, row counts, directory sizes) run on a slower cadence so monitoring never becomes load on the customer's cluster.

**Metrics catalog**

| Metric | Unit | Scope | Source | Cadence |
| --- | --- | --- | --- | --- |
| Phase and attempt | state | Database | Orchestrator | Every sample |
| Source and target database size | bytes | Database | `pg_database_size` | 30 s |
| Rows inserted on target | rows | Database | `pg_stat_user_tables` | 30 s |
| Active COPY per table, bytes and tuples | bytes, rows | Table | `pg_stat_progress_copy` (PG 14+) | Every sample |
| Index builds, phase and percent | percent | Index | `pg_stat_progress_create_index` | Every sample |
| VACUUM and ANALYZE in progress | count | Database | `pg_stat_progress_vacuum` | Every sample |
| Copy rate (instant, smoothed, average) | bytes/s | Database | Derived | Every sample |
| Source WAL generation rate | bytes/s | Cluster | `pg_current_wal_lsn` deltas | Every sample |
| Received, replayed and end positions | LSN | Database | Engine sentinel | Every sample |
| CDC backlog (WAL not yet applied) | bytes | Database | Current position minus replayed | Every sample |
| Replay rate | bytes/s | Database | Derived | Every sample |
| Heartbeat latency (optional heartbeat) | seconds | Database | Heartbeat table on both sides | Every sample |
| Slot active, `wal_status`, retained WAL, headroom | state, bytes | Database | `pg_replication_slots` | Every sample |
| Oldest snapshot age and xmin age | seconds, transactions | Cluster | `pg_stat_activity` | Every sample |
| Dead tuples on the highest-churn tables | rows | Table | `pg_stat_user_tables` | 60 s |
| Source and target active connections | count | Cluster | `pg_stat_activity` | Every sample |
| Work-volume usage, staging size and growth | percent, bytes, bytes/s | Host | `statfs`, directory walk | 5 s / 60 s |
| Host CPU, load, memory, network | percent, bytes/s | Host | `/proc` | Every sample |
| Engine log errors and warnings | count | Database | Log parser | Every sample |

**Forecasts** shown with their basis ("at the current smoothed rate"): copy ETA per database and overall, catch-up ETA, time until the work volume fills, time until a slot's headroom runs out. Each forecast is hidden when its basis is too noisy (fewer than 6 samples or rate under 1% of its average).

**Retention**: raw 5-second samples kept 72 hours, 1-minute rollups (min, max, average, last) kept 90 days, 15-minute rollups kept with the migration until it is deleted. Proposed budget: 2 GB of metrics for a 7-day, 10-database migration. The agent must measure actual storage with a simulated run of that size and adjust retention to fit.

**Default alert rules** (thresholds editable in Settings)

| Rule | Condition | Severity |
| --- | --- | --- |
| Engine failed | Unit exited non-zero | Critical |
| Engine degraded | Main process dead while the unit still has processes | Critical |
| Slot invalidated | `wal_status = lost` | Critical |
| Slot at risk | `wal_status` unreserved, or headroom gone within 1 hour | Critical |
| Slot headroom falling | Headroom gone within 4 hours | Warning |
| Work volume | 85% used, or full within 30 minutes / 2 hours | Critical / Warning |
| Backlog growing | CDC backlog grew for 5 consecutive samples while applying | Warning |
| Heartbeat latency high | Over 60 seconds while in sync | Warning |
| Watched table diverging | Differs beyond lag for 2 minutes while in sync | Warning |
| Cluster unreachable | Source or target query failed 3 samples in a row | Critical |
| Snapshot held long | Snapshot older than the estimated copy time x 1.5 | Warning |
| Certificate expiring | TLS certificate expires within 14 days | Warning |
| Recorder stale | No metrics written for 3 intervals | Critical |

Alert lifecycle: firing, acknowledged (by whom, with a note), resolved; deduplicated by rule and scope; every transition is an event. A banner on every screen shows unacknowledged critical alerts.

**Prometheus**: `/metrics` exposes current gauges and counters with labels `migration`, `database`, `scope`. Authentication by a separate read-only bearer token, created in Settings.

**Notifications**: channels are Slack (incoming webhook), email (SMTP with TLS), and generic webhook (JSON body, optional HMAC signature header). Routing rules match event type and severity to channels, with quiet hours. Every notification attempt and its result is logged; failures retry 3 times with backoff and raise a warning if they still fail.

## Logging module

Every line any component writes (the app, each engine unit, the installer, the watchdog) becomes a structured record, is stored with its migration, database and component, and can be watched live or searched later in the UI. Nothing an operator might need is only on the droplet's console.

**Record shape** (JSON lines on disk, indexed in the store):

```json
{"ts":"2026-10-07T14:03:22.418Z","level":"info","component":"engine","migration":"m42","database":"orders","attempt":2,"step":"COPY","table":"public.orders","msg":"COPY done","raw":"2026-10-07 14:03:22.418 4704 INFO copydb.c:512 ...","op":"op_7f3a"}
```

Levels: `trace`, `debug`, `info`, `warn`, `error`, `fatal`. Components: `api`, `orchestrator`, `preflight`, `engine`, `collector`, `cutover`, `verify`, `notifier`, `installer`, `watchdog`, `audit`. Every record from an operation carries its operation ID, so one click in the UI shows everything that operation caused.

**Sources and capture**:

- App logs: Go `log/slog` with a JSON handler writing to `/var/log/upwell/app.log` and the in-process event bus.
- Engine output: the unit appends stdout and stderr to the run's `engine.log`; a tailer follows the file (surviving rotation and app restarts by remembering byte offsets in the store), runs `Engine.ParseLog` on each line, and indexes the structured record. Raw lines are always kept.
- Commands the app runs (psql queries for checks, pgcopydb subcommands, systemctl): logged at `debug` with the full command (secrets masked), duration and exit code; their output at `trace`.
- Installer and updater: write to `/var/log/upwell/install.log`; shown in Settings, System.

**Live view requirements**: the UI log viewer streams over SSE with under 1 second of delay, handles at least 2,000 lines per second without freezing (virtualized rendering), filters by level, component, database, attempt and text, pauses and resumes without losing lines, highlights warnings and errors, and jumps from any alert or failed check to the log lines around it.

**Redaction**: a redaction layer runs before any write. It masks passwords in URIs and key-value pairs, `PGPASSWORD`, API tokens, bearer headers, webhook URLs, and SMTP credentials. A unit test feeds known secrets through every log path and fails if any appears in output.

**Retention and rotation**: size-based rotation (100 MB per file, compressed), retained 30 days or 10 GB total by default, never deleting logs of a migration that is not cleaned up. Settings shows log disk use.

**Support bundle**: one button produces a `.tar.gz` containing redacted logs, the migration spec (no secrets), preflight results, events, alert history, metrics rollups, the report, versions of every component and host facts. The operator previews the file list before download. It is never sent anywhere by the app.

## UI specification

The UI is the product: every operation the harness offers, and more, is done from the browser, and every operation shows its steps, live progress and full logs as it runs. The bar is the polish and density of the best operations consoles (Datadog, Grafana, Linear, Vercel): calm, information-rich, fast, and never ambiguous about state.

**Design system**

- **Themes**: dark (default, for long monitoring sessions) and light, switchable per user, both meeting WCAG 2.1 AA contrast.
- **Type**: Inter for interface text, JetBrains Mono for logs, LSNs, commands and identifiers. Sizes 12 / 14 / 16 / 20 / 28.
- **Layout**: 8 px grid, left navigation rail, top bar with migration switcher, global search and alert indicator. Optimized for 1440 px and wider; fully usable at 1280; read-only usable at 1024. A density toggle (comfortable or compact).
- **Colour carries meaning only**: green healthy, amber warning, red critical or blocked, blue informational, violet for cutover. Never decorative.
- **Components**: status pills, phase stepper, progress bars with ETA, sparklines, time-series charts with synchronized crosshairs, data tables with sorting, filtering, column choice and CSV export, a virtualized log viewer, side drawers for detail, toasts, a command palette (Ctrl or Cmd + K) for every action and screen.
- **Numbers**: always with units, human-scaled (`1.4 TiB`, `3h 12m`), exact value on hover; times in UTC or local by user preference, shown with zone.
- **States**: skeleton loaders on first load; empty states that say what to do next; error states that say what happened, what it means and what to do; stale data greyed with "last updated 40 s ago".
- **Safety**: destructive actions need a confirmation dialog stating exactly what will change; irreversible or customer-impacting actions (write freeze, restart database, abort, cleanup) need the resource name typed in. Buttons for actions the user's role cannot perform are hidden, not disabled.

**Operation console pattern**: every long operation (preflight, start, stop, resume, restart database, cutover, verify, cleanup, install update) opens an operation panel showing its numbered steps with live status (waiting, running with elapsed time, done, failed), a progress bar where measurable, and a log pane filtered to that operation's ID at the chosen verbosity. Panels can be minimized to a tray and reopened; finished operations stay in the migration's history with their full logs.

**Screens**

| Screen | Purpose | Contents |
| --- | --- | --- |
| First-run setup | One-time install completion | Create the first Admin, show the TLS fingerprint to verify, time zone, optional DO API token, notification channel test |
| Sign in | Authentication | Username and password, TOTP second factor when enabled, session timeout notice |
| Home | Situation at a glance | Active migration card (phase, overall progress, ETA, readiness for cutover, open alerts), queue with reorder, recent migrations, system health tiles (disk, service, certificate, engine versions) |
| New migration wizard | Define a migration in steps | 1 Customer and permission record. 2 Source: pick from DO API list or enter manually, with a live connection test. 3 Target: same. 4 Databases: discovered list with size, object counts, skip reasons, target name, include toggle. 5 Options: online or offline, plugin, jobs with recommendation, heartbeat, watched tables, notifications. 6 Preflight: live results, accept blockers with a reason. 7 Plan review: exact engine command per database, estimates for duration, disk, WAL retention and connections. Start now or queue |
| Migration overview | One migration's health | Lifecycle stepper, overall progress and ETA, per-database table (phase, size progress, rows, backlog, replay rate, slot, heartbeat, sparklines), forecasts, open alerts, recent events |
| Database detail | Drill into one database | Phase and attempts history, per-table COPY progress bars, index builds, CDC positions and backlog charts, slot health, heartbeat chart, watched tables, filtered logs, actions (stop, resume, restart from zero) |
| Replication | CDC across all databases | Backlog, replay rate and slot retained WAL per database on synchronized charts, source WAL rate, headroom forecasts |
| Source health | Impact on the customer's cluster | Oldest snapshot and xmin age (VACUUM blocked time), dead tuples on top-churn tables, connections, long transactions |
| Host | The droplet | Work volume, staging growth and full-at forecast, CPU, memory, network, engine unit status |
| Logs | Everything, live | The log viewer across all components with filters and saved views; download filtered range |
| Events and audit | What happened, who did it | Unified timeline of phase changes, alerts, operations and operator actions; audit entries show user, IP, before and after |
| Preflight | Checks and history | Latest run grouped by scope, evidence per check, accepted risks, diff between runs |
| Cutover console | Run the cutover | Readiness checklist, stop-writes method (manual or freeze, with preview), large write-pause timer, per-database drain progress, sequence sync, verification results, verdict, post-cutover checklist, rollback controls |
| Verification | Proof the copy matches | Per database: schema comparison, sequences, row counts (exact and estimated), optional checksums, failures with detail |
| Report | Shareable record | Rendered report with download as HTML, PDF and Markdown |
| Alerts | Triage | Firing, acknowledged and resolved alerts across migrations; acknowledge with note |
| Settings | Configure the install | General, migration defaults, alert thresholds, notifications, DO API, users and roles, API tokens, TLS, logging and retention, metrics export, system (versions, updates, services, watchdog) |
| System | Health of the app itself | Service status, uptime, watchdog heartbeats, store size, log disk use, installed component versions, update availability, restart app (safe while a migration runs) |

## Settings catalog

Every default below is editable in Settings by an Admin. Settings marked "per migration" can also be overridden in the wizard; the values in force when a migration starts are snapshotted with it. Each setting shows its default, its allowed range, a one-line explanation, and who changed it last (from the audit log). Defaults come from the harness unless noted.

| Key | Default | Range | Scope | What it controls |
| --- | --- | --- | --- | --- |
| `mode` | online | online, offline | Per migration | CDC with cutover, or one-time copy with writes stopped |
| `table_jobs` | CPU count (4 to 16) divided by number of databases, min 2 | 1 to 64 | Per migration | Parallel COPY workers per database |
| `index_jobs` | Half CPU count (2 to 8) divided by databases, min 1 | 1 to 32 | Per migration | Parallel CREATE INDEX workers per database |
| `split_tables_larger_than` | auto (largest table / table jobs when it is 10 GB or more) | auto, off, size | Per migration | Same-table parallel copy |
| `decoding_plugin` | auto (engine default; switch to test\_decoding when pgoutput cannot work) | auto, pgoutput, test\_decoding, wal2json | Per migration | Logical decoding plugin |
| `target_synchronous_commit_off` | true | true, false | Per migration | Faster loading; engine sessions only |
| `target_maintenance_work_mem` | unset | size | Per migration | Index build memory for engine sessions |
| `create_target_databases` | true | true, false | Per migration | Create missing target databases with source locale |
| `copy_roles` | true | true, false | Per migration | Copy cluster roles once |
| `heartbeat` | true for online migrations (PC13) | true, false | Per migration | Heartbeat table for end-to-end CDC latency and the final cutover heartbeat (writes to source). Turning it off removes a GO requirement and is recorded as an accepted risk |
| `wal_sample_seconds` | 60 | 10 to 600 | Global | Source WAL rate sampling in preflight |
| `assumed_throughput_mibs` | 102 (measured in the team's test migration) | 1 to 2000 | Global | Estimates before a throughput sample exists |
| `long_transaction_minutes` | 5 | 1 to 120 | Global | Warning threshold in preflight |
| `target_disk_factor` | 1.3 | 1.0 to 3.0 | Global | Target capacity check multiplier |
| `staging_factor` | 1.5 | 1.0 to 5.0 | Global | Staging estimate as a multiple of retained WAL |
| `exact_count_max_mb` | 1024 | 0 to 1048576 | Per migration | Tables up to this size get exact counts in verification |
| `compare_data` | false | true, false | Per migration | Full checksum comparison (slow on large data) |
| `cutover_lag_threshold_mb` | 16 | 1 to 1024 | Per migration | Backlog per database before writes may stop |
| `cutover_stable_seconds` | 60 (new) | 0 to 600 | Global | How long the backlog must stay under threshold |
| `cutover_write_sample_seconds` | 10 | 5 to 120 | Global | Row-change sampling window after writers stop |
| `cutover_drain_timeout_seconds` | 1800 | 60 to 86400 | Global | Maximum wait for drain |
| `cutover_nudge_seconds` | 0 (PC3) | 0 to 600 | Global | Delay before the drain nudge; 0 nudges immediately after setting the end position |
| `verify_checksum_budget_seconds` | 600 (new) | 0 to 86400 | Per migration | Time the verification may spend on row checksums per database; tables beyond the budget are reported as not checksummed |
| `analyze_after_go` | false | true, false | Per migration | ANALYZE target databases after GO |
| `write_freeze_offered` | false | true, false | Global | Whether the freeze option appears in the console at all |
| `retry_base_seconds` / `retry_cap_seconds` | 10 / 600 | 1 to 3600 | Global | Backoff for automatic resume |
| `retry_max_per_hour` | 5 | 0 to 60 | Global | Automatic resumes per database per hour |
| `auto_restart_base_copy` | false | true, false | Global | Restart a database whose base copy failed |
| `auto_restart_base_copy_max_gb` | 50 | 1 to 100000 | Global | Size limit for the above |
| `auto_resume_after_reboot` | true (CDC phases only) | true, false | Global | Resume streaming databases on boot |
| `sample_interval_seconds` | 5 | 2 to 60 | Global | Collector cadence |
| `metrics_raw_retention_hours` | 72 | 6 to 720 | Global | Raw samples kept |
| `alert_disk_warn_pct` / `alert_disk_crit_pct` | 70 / 85 | 50 to 99 | Global | Work-volume alerts |
| `disk_guard_stop_pct` | 95 | 80 to 99 | Global | Stop the fastest-growing database to protect the volume |
| `alert_slot_headroom_hours_warn` / `_crit` | 4 / 1 | 0.1 to 48 | Global | Slot invalidation forecasts |
| `alert_heartbeat_seconds` | 300 (D19) | 5 to 3600 | Global | Heartbeat latency warning while in sync |
| `log_level` | info | trace to error | Global | App log verbosity (engine logs always kept in full) |
| `log_retention_days` / `log_max_gb` | 30 / 10 | 1 to 365 / 1 to 500 | Global | Log retention |
| `session_idle_minutes` / `session_max_hours` | 30 / 12 | 5 to 240 / 1 to 72 | Global | Sign-in sessions |
| `mfa_required_for_admins` | true | true, false | Global | TOTP for the Admin role |
| `listen_address` / `https_port` / `http_redirect` | 0.0.0.0 / 443 / true | any | Global | Network exposure |
| `max_concurrent_base_copies` | 4 (new) | 1 to 32 | Per migration | Databases copying their base data at once; the rest wait and start as earlier ones reach catch-up (see Scale) |

## DigitalOcean API integration and the manual path

The operator chooses per migration: connect with a DO API token, or enter connection details by hand. Both paths must produce the same migration spec, and every check, screen and operation must work on the manual path alone. The API only adds discovery and automation.

**What the API path adds**

| Capability | Use | Without the API |
| --- | --- | --- |
| List clusters | Pick source and target from the account's PostgreSQL clusters, with region, version, size and VPC | Type host, port, user, password |
| Connection details | Prefer private hostnames automatically; fill users and ports | Typed; private hostname recommended in the form |
| Trusted sources | Show whether this droplet is allowed on each cluster; offer to add it, and to remove it at cleanup | Operator adds it in the control panel; preflight confirms by connecting |
| Storage size | Read target storage for the `target_disk` check | Operator enters the size in GB |
| Maintenance window | Warn if scheduled maintenance falls inside the estimated window | Not checked |
| VPC match | Confirm the droplet and both clusters share a VPC | Inferred from private addresses |

API rules the implementation must follow:

- **Token handling**: validated on entry with a read call, stored encrypted, never logged, never shown again (only its last 4 characters and scopes). Revocable from Settings. Recommend a custom-scoped token with the least scopes needed: read-only for discovery; update scope only if the operator wants trusted-source changes.
- **Trusted sources are customer configuration.** Any change shows a before and after preview, needs confirmation, is audited, and must preserve every existing rule. If the API replaces the whole rule set on update, the implementation must read, merge and write, never send a partial list. **Verify in Phase 0** against the current API reference, including the exact endpoints and field names for clusters, firewall rules, storage size and maintenance windows.
- **Droplet identity** comes from the droplet metadata service, so the rule added is for this droplet specifically, not a broad IP range.
- **Rate limits and errors**: honor `Retry-After`; show API errors verbatim in the operation log alongside a plain-English explanation.
- **Credentials from the API** are used only for the migration they were fetched for, stored encrypted like typed ones, and deleted at cleanup.

**Manual path fields**: host, port, user, password, database for cluster-level queries (default `defaultdb`), SSL mode (`require` default, `verify-full` recommended), CA certificate upload, target storage size in GB, and an optional note for the maintenance window. A "Test connection" button runs connectivity, version and privilege probes and reports each result.

## Security and compliance

The app holds credentials to a customer's production databases and reads their catalog and row counts, so it is built as security-sensitive software: least privilege, encrypted secrets, strong authentication, and a tamper-evident record of every action.

**Customer permission record (hard requirement).** In support-operated mode, no connection to a customer cluster is attempted until the migration has a permission record: customer name, DO team or account ID, support ticket reference, who granted permission and when, the scope (clusters and databases), and an optional attachment of the written permission. The record is immutable once a connection has been made, appears in the report, and is checked by the `customer_permission` preflight check. In customer self-serve mode (a later install-wide setting), it becomes the customer's own acknowledgement that they operate the tool on their data.

**What the app reads and keeps**: catalog metadata (names, sizes, settings), statistics, row counts, and the engine's own state. It never stores row data. Reports and support bundles contain table names and sizes, so they are labeled customer-confidential. Deleting a migration purges its secrets, logs, metrics and reports, and is audited.

**Roles**

| Role | Can |
| --- | --- |
| Viewer | See everything except secrets; read logs, metrics, reports |
| Operator | Viewer, plus create and run migrations, accept acceptable blockers, stop, resume, restart databases, run a manual cutover, verify, clean up |
| Admin | Operator, plus settings, users, API tokens, TLS, DO API token, write freeze, override detected writes at cutover, updates |

**Authentication**: local users with Argon2id password hashing, a minimum of 14 characters, lockout for 15 minutes after 10 failures, TOTP second factor (required for Admins by default), recovery codes. Sessions use Secure, HttpOnly, SameSite=Strict cookies with idle and absolute timeouts; CSRF tokens on every mutating request. API tokens for automation are scoped, expiring, and shown once. SSO (OIDC) should be supported in a later phase; the user model must not prevent it.

**Secrets at rest**: AES-256-GCM, with a 256-bit key generated at install in `/etc/upwell/master.key` (mode 0600, owned by the service user). Database passwords, API tokens, SMTP and webhook credentials are encrypted in the store. A key-rotation command re-encrypts everything. The UI warns that losing the key means re-entering secrets, and the installer prints where it is.

**TLS**: at install, generate an ECDSA P-256 self-signed certificate valid 397 days, with the droplet's hostname and public and private IPs as subject alternative names. Print its SHA-256 fingerprint at the end of install and on the first-run screen so the operator can verify it. Admins can upload their own certificate and key (validated, with chain), and renew the self-signed one. TLS 1.2 minimum; HSTS enabled only once a trusted certificate is installed.

**Process and host hardening**:

- The app runs as an unprivileged `upwell` user.
- Managing engine units needs systemd rights. Grant them through a polkit rule that allows only start, stop, restart and reset-failed of instances of `upwell-eng@.service` (PC1). A rule over transient units would let the service user run any command as root, because polkit cannot inspect a transient unit's properties. **Verify in Phase 0** on Ubuntu 24.04.
- Commands are executed with argument arrays, never through a shell with interpolated input.
- SQL identifiers are quoted by the driver.
- Database names are validated against an allowed pattern before use.
- The installer configures `ufw` to allow SSH and the HTTPS port only.

**Audit log**: every authentication event, settings change, migration action, acceptance, cutover step, and secret access (by name, never value) is written append-only. Each entry stores the hash of the previous entry, so tampering is detectable; the Audit screen verifies the chain and exports it as JSON lines.

## Installer and updates

One bash script, `install.sh`, installs everything on a fresh Ubuntu droplet and, on every later run, changes only the components whose version or checksum differs from the release manifest. Running it twice in a row must change nothing and finish in under 30 seconds.

```bash
curl -fsSLO https://<release-host>/upwell/install.sh
sudo bash install.sh
sudo bash install.sh --check
sudo bash install.sh --version 1.3.0
sudo bash install.sh --bundle /root/upwell-1.3.0-bundle.tar.gz
sudo bash install.sh --rollback
sudo bash install.sh --uninstall --purge-data
```

**Release manifest**: each release ships `manifest.json` listing every component with its version, source and SHA-256. The installer records what it installed in `/var/lib/upwell/installed.json` and compares the two on every run.

| Component | Installed when | Notes |
| --- | --- | --- |
| OS packages (`curl`, `ca-certificates`, `gnupg`, `ufw`, `chrony`, `jq`) | Missing | Checked with `dpkg-query`, never reinstalled |
| PGDG apt repository | A required client version has no candidate | Added once with the `postgresql-common` helper |
| PostgreSQL clients | A required major is missing | Majors chosen from the manifest (default 14 to 18), so any supported source and target pair works |
| pgcopydb | Missing or older than the manifest pin | PGDG package first; if that is older than the pin, build the pinned tag from source into `/usr/local/bin` (proven in the harness) |
| App binary | Checksum differs | Previous binary kept as `upwell.prev` for rollback |
| Service user, directories, permissions | Missing or wrong | `/var/lib/upwell`, `/var/log/upwell`, `/etc/upwell` |
| systemd units and polkit rule | Content differs | `daemon-reload` only when changed |
| TLS certificate | Missing | Never replaced on update |
| Master key | Missing | Never replaced or printed; the installer refuses to continue if the store has secrets but the key is missing |
| Firewall rules | Missing | SSH and the HTTPS port |
| Log rotation config | Content differs |  |

Update rules:

1. **Safe while a migration runs.** Updating the app restarts only `upwell.service`; engine units keep running and the orchestrator re-attaches to them on start (see Watchdog).
2. **Never swap the engine under a running copy.** If any `upwell-eng@*` unit is active, the installer refuses to change pgcopydb or PostgreSQL client packages and says which migration to finish or pause first. App-only updates still proceed.
3. **Store schema changes** are forward-only, versioned, and preceded by a copy of the store file (`store.db.bak-<version>`). A failed migration of the schema restores the copy and the previous binary automatically.
4. **Verify before switching**: the new binary must pass `upwell selftest` (config readable, store opens, key decrypts a canary, port bindable) before the service is restarted on it.
5. **Offline installs** use a bundle tarball containing the binary, the manifest and `.deb` files for the pinned dependencies.
6. `--check` prints exactly what would change and changes nothing.

Output: each step prints one line (checked, unchanged, installed or updated, with versions), everything is also written to `/var/log/upwell/install.log`, and the script exits non-zero with a plain-English reason on the first failure. The final lines print the URL, the certificate fingerprint, and how to create the first Admin.

## Watchdog, reboot survival and self-healing

The app is a systemd service with a watchdog, so it restarts on crash or hang. Engine units run separately, so an app restart never interrupts a copy. After a droplet reboot, the orchestrator reconciles every migration against reality and resumes what is safe to resume.

```ini
[Unit]
Description=Upwell
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=notify
User=upwell
Group=upwell
ExecStart=/usr/local/bin/upwell serve --config /etc/upwell/config.yaml
Restart=always
RestartSec=5
WatchdogSec=30
KillMode=process
LimitNOFILE=65536
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/upwell /var/log/upwell

[Install]
WantedBy=multi-user.target
```

**Watchdog**: the orchestrator loop, collectors and notifier each report a heartbeat to an internal health model. The app sends `WATCHDOG=1` to systemd only while every critical loop has reported within its deadline, so a hung orchestrator makes systemd restart the app. `/healthz` (liveness) and `/readyz` (store open, orchestrator running, last reconciliation finished) expose the same model. A `upwell-health.timer` calls `/healthz` every minute and records failures in the journal, as a second, independent signal.

**After an app restart** (crash, update or watchdog): the orchestrator finds running engine units by name, re-attaches to their logs at the stored byte offsets, resumes collection, and finishes any intent left without an outcome. The UI shows a banner: "Console restarted at 14:03; migrations continued without interruption."

**After a droplet reboot**: transient engine units do not survive a reboot, so every running copy has stopped. On start, the orchestrator:

1. Waits until both clusters are reachable (up to 10 minutes, with status shown in the UI).
2. For each database that was streaming (CDC), resumes it if `auto_resume_after_reboot` is on, after the slot-release wait. Otherwise it marks the database Paused.
3. For each database that was in base copy, marks it Restart required. It restarts it only if `auto_restart_base_copy` allows that size.
4. Records a "droplet rebooted" event with what was resumed and what needs attention, and sends a notification.

**Self-healing actions** the app takes on its own, each logged and notified:

- Restart the metrics recorder if it stalls.
- Re-attach log tailers after rotation.
- Release a replication slot held by a stale walsender before a relaunch.
- Stop the fastest-growing database at the disk guard threshold.
- Retry transient failures per the retry policy.

Everything else goes to Needs attention with a diagnosis, never a silent retry loop.

## Scale: from a few GB to multiple TB

The same workflow must handle a 2 GB single database and an 8 TB cluster with dozens of databases, with honest estimates and without harming the source. The one measured data point today is the team's test rate of about 102 MiB/s, which puts 8 TB at roughly 23 hours of copying. Everything else here is a starting point to calibrate in the Phase 5 scale tests.

**What changes with size, and what the app does about it**

| Pressure | Why it matters | App behavior |
| --- | --- | --- |
| Snapshot held for the whole base copy | VACUUM cannot remove newer dead rows on the source; busy tables bloat | Show snapshot age and dead tuples on top-churn tables live; warn when the snapshot outlives 1.5 x the estimate |
| WAL retained by slots | Fills the source disk, or hits `max_slot_wal_keep_size` and invalidates the slot | Preflight estimate from sampled WAL rate; live headroom forecast; critical alert an hour before invalidation |
| Change staging on the droplet | Grows with the write rate for the whole copy | Staging estimate in preflight, growth rate and full-at forecast live, disk guard at 95% |
| A few huge tables | One table on one worker sets the wall-clock time | Automatic same-table splitting; per-table progress so the long pole is visible |
| Many databases | Each needs a slot, a WAL sender and its own connections | Scheduler (below); hard block when slots or WAL senders are short |
| Connections | Each engine opens many; small plans have low limits | Connection budget check sized across all concurrent databases, with a suggested job count that fits |
| Estimates | A single assumed rate is wrong for many workloads | Optional throughput sample in preflight; live smoothed rate replaces the estimate as soon as copying starts |

**Scheduler for many databases** (new; the harness started every database at once): setting `max_concurrent_base_copies` (default 4) limits how many databases run their base copy at the same time. Further databases wait in Pending and start as earlier ones reach Catch-up, so every database is streaming before cutover. Each database's slot is created when its copy starts, so slot and WAL-sender limits apply to the total, not the concurrent count. When the cluster has more databases than free slots, preflight blocks and suggests splitting the work into several migrations.

**Droplet sizing starting points** (to calibrate):

| Total data | vCPU | RAM | Work volume |
| --- | --- | --- | --- |
| Up to 100 GB | 4 | 8 GB | 100 GB |
| 100 GB to 1 TB | 8 | 16 GB | Staging estimate x 2, at least 250 GB |
| 1 TB to 10 TB | 16 or more | 32 GB or more | Staging estimate x 2, at least 1 TB |

The `host_resources` check compares the droplet with this table, warns when it is undersized, and recommends a dedicated block-storage volume for the work directory.

**Keeping monitoring cheap on the customer's cluster**:

- Collectors share a pool of at most 2 connections per cluster, with a 5-second statement timeout.
- Database sizes and row statistics refresh every 30 seconds, not every sample.
- Exact `count(*)` runs only during verification and only for tables under `exact_count_max_mb`; larger tables are compared by planner estimates.
- Watched tables use the counts the operator asked for.
- The heartbeat is opt-in.

## Data model

One SQLite file (`/var/lib/upwell/store.db`, WAL mode, foreign keys on) holds all durable state. Tables below are the minimum; the agent may add columns and indexes. Every table has `id` (ULID), `created_at`, and `updated_at` where rows change, except metric samples and rollups (PC7).

| Table | Key fields | Notes |
| --- | --- | --- |
| `users` | username, password\_hash, role, totp\_secret (encrypted), disabled, last\_login\_at |  |
| `sessions` | user\_id, token\_hash, ip, user\_agent, expires\_at |  |
| `api_tokens` | name, token\_hash, scopes, expires\_at, last\_used\_at | Shown once at creation |
| `settings` | key, value (JSON), updated\_by | Global defaults |
| `secrets` | name, ciphertext, nonce, key\_version | Every credential; referenced by ID elsewhere |
| `connections` | kind (source or target), discovery (api or manual), host, port, user, db, sslmode, ca\_cert, password\_secret\_id, do\_cluster\_id |  |
| `migrations` | name, state, mode, engine, source\_conn\_id, target\_conn\_id, settings\_snapshot (JSON), queued\_position, created\_by | State derived per lifecycle rules |
| `permission_records` | migration\_id, customer, account\_id, ticket, granted\_by, granted\_at, scope, attachment\_path, locked\_at | Immutable once locked |
| `migration_databases` | migration\_id, source\_name, target\_name, state, slot\_name, origin\_name, plugin, split\_size, work\_dir, size\_bytes, rows\_estimate |  |
| `attempts` | database\_id, number, unit\_name, command (redacted), started\_at, ended\_at, exit\_code, end\_reason, resume\_point | One per engine launch |
| `operations` | migration\_id, kind, operation\_key (client ID), state, steps (JSON), started\_by, started\_at, ended\_at, result | Idempotency and the operation console |
| `intents` | operation\_id, action, target, recorded\_at, outcome, outcome\_at | Reconciliation after restart |
| `preflight_runs` | migration\_id, started\_at, ended\_at, summary |  |
| `check_results` | run\_id, check\_id, check\_version, scope, database, level, hard, message, evidence (JSON), evidence\_hash, remediation |  |
| `acceptances` | migration\_id, check\_id, scope, evidence\_hash, reason, accepted\_by, accepted\_at, lapsed\_at |  |
| `metric_series` | migration\_id, database (nullable), name | One row per series (PC7) |
| `metric_samples`, `metric_rollups_1m`, `metric_rollups_15m` | series\_id, ts, value (rollups add min, max, last) | Primary key (series\_id, ts), WITHOUT ROWID, no surrogate ID: the exception to the ULID rule (PC7; 0.50 GB instead of 2.45 GB in Phase 0) |
| `alerts` | migration\_id, rule, scope, severity, state, first\_at, last\_at, acked\_by, ack\_note, resolved\_at |  |
| `events` | migration\_id, database, type, severity, message, data (JSON), ts | The timeline |
| `log_index` | migration\_id, database, attempt, component, level, ts, file, offset, length, step, table\_name | Points into log files; full text stays on disk |
| `log_offsets` | file, inode, offset | Tailer positions across restarts |
| `verifications` | migration\_id, database, check, result, detail, ts |  |
| `notifications` | channel, rule, event\_id, attempt, status, error, ts |  |
| `audit_log` | ts, user\_id, ip, action, target, before (JSON), after (JSON), prev\_hash, hash | Append-only, hash-chained |
| `schema_version` | version, applied\_at | Forward-only migrations of the store |

## API

The UI uses only the public API, so everything in the UI is scriptable. Base path `/api/v1`; JSON in and out; errors as `{"code","message","remediation","details"}`. Every mutating call takes an `Idempotency-Key` header, is authorized by role, and writes an audit entry. Long operations return `202` with an operation ID to follow on the stream. An OpenAPI 3.1 document is generated from the code and served at `/api/v1/openapi.json`.

| Method and path | Purpose |
| --- | --- |
| `POST /auth/login`, `POST /auth/logout`, `POST /auth/totp` | Sessions |
| `GET/POST/PATCH/DELETE /users`, `/api-tokens` | Access management (Admin) |
| `GET/PATCH /settings` | Global defaults (Admin) |
| `POST /do/token`, `GET /do/clusters`, `GET /do/clusters/{id}`, `POST /do/clusters/{id}/trusted-sources` | DigitalOcean API path |
| `POST /connections`, `POST /connections/{id}/test` | Manual or API-sourced connections |
| `GET/POST /migrations`, `GET/PATCH/DELETE /migrations/{id}` | Define and manage migrations |
| `PUT /migrations/{id}/permission` | Customer permission record |
| `POST /migrations/{id}/discover` | Database discovery |
| `POST /migrations/{id}/preflight`, `GET /migrations/{id}/preflight/latest` | Run and read preflight |
| `POST /migrations/{id}/acceptances` | Accept a blocker with a reason |
| `GET /migrations/{id}/plan` | Dry run: commands and estimates per database |
| `POST /migrations/{id}/start`, `/queue`, `/pause`, `/resume`, `/abort` | Lifecycle |
| `POST /migrations/{id}/databases/{db}/stop`, `/resume`, `/restart` | Per-database control |
| `POST /migrations/{id}/cutover`, `POST /migrations/{id}/cutover/freeze`, `POST /migrations/{id}/cutover/unfreeze`, `POST /migrations/{id}/cutover/confirm-writers-stopped` | Cutover steps |
| `POST /migrations/{id}/verify`, `GET /migrations/{id}/verification` | Verification |
| `POST /migrations/{id}/cleanup` | Remove slots, publications, origins, harness objects, credentials |
| `GET /migrations/{id}/metrics?names=&from=&to=&step=` | Time series |
| `GET /migrations/{id}/logs?level=&component=&database=&q=&from=&to=` | Log search and download |
| `GET /migrations/{id}/events`, `GET /alerts`, `POST /alerts/{id}/ack` | Timeline and alerts |
| `GET /migrations/{id}/report?format=html` (or `pdf`, `md`) | Report |
| `POST /migrations/{id}/support-bundle` | Redacted bundle |
| `GET /operations/{id}` | Operation status and steps |
| `GET /audit`, `GET /audit/verify` | Audit log and chain verification |
| `GET /system`, `GET /healthz`, `GET /readyz`, `GET /metrics` | Health and Prometheus |

**Live streams** (Server-Sent Events, resumable with `Last-Event-ID`):

- `GET /api/v1/stream/migrations/{id}`: state changes, progress snapshots each sample, alerts, events, operation step updates.
- `GET /api/v1/stream/logs?migration=&database=&level=&component=`: log records as they are written.
- `GET /api/v1/stream/system`: service health, disk, watchdog heartbeats.

SSE is chosen over WebSockets because every stream is server-to-client, it works through proxies, and it resumes natively.

## Known pgcopydb and PostgreSQL behaviors

Each behavior below was found while building and testing the harness (PostgreSQL 16.15, pgcopydb 0.15 and 0.18 on Ubuntu 24.04) or read from pgcopydb's source. The app must handle every one, and the test suite must reproduce each "reproduced" item.

| Behavior | Evidence | What the app must do |
| --- | --- | --- |
| `clone --all-databases` in 0.18 does not support `--follow` | Source: `cli_clone_follow.c` hands off to `multi_db.c`, which has no follow path | One engine run per database, coordinated by the orchestrator |
| 0.18 defaults to the `pgoutput` plugin and creates a publication `FOR TABLE` every table | Source: `defaults.h`, `pgsql.c`, `snapshot.c` | Check publication feasibility per database |
| That publication fails when the database has an unlogged table | Reproduced: "cannot add relation ... not supported for unlogged tables" | Switch that database to `test_decoding` automatically, with a warning |
| The publication also needs table ownership and CREATE on the database | PostgreSQL rule; reproduced with a non-owner role | Same automatic switch, or a hard block when pgoutput is forced |
| On resume, pgcopydb reads the plugin from its own catalog | Source: `cli_common.c` | Never pass `--plugin` on resume |
| `stream sentinel get --json` prints the same LSN in every field in 0.15 | Reproduced | Parse the plain-text output |
| `stream sentinel get` needs `--dir` even when its help does not list it (0.15) | Reproduced | Always pass `--dir` |
| `pg_dumpall` from current minor releases emits `\restrict`, which breaks `copy roles` in 0.15; 0.18 copies roles fine | Reproduced | Prefer the engine; fall back to `pg_dumpall` with those lines stripped, skipping roles that already exist on the target |
| 0.15 has a CDC apply bug (`computeTxnMetadataFilename ... xid: 0`, exit 12) | Reproduced twice; not seen on 0.18 | Pin 0.18 or later; block older versions for online mode |
| Killing pgcopydb's main process leaves sub-processes streaming and holding the log pipe; the failure surfaces only at drain (exit 137) | Reproduced | Run pgcopydb as the unit's main process so systemd stops the whole cgroup; detect a dead main PID |
| pgcopydb sub-processes can ignore SIGTERM and outlive their parent | Reproduced | Stop by cgroup with SIGKILL after the stop timeout |
| A killed client can leave a walsender holding the slot ("slot is active for PID") | Reproduced on resume | Wait for the slot to go inactive; terminate the stale walsender after 15 s |
| `pgcopydb.pid` holds the main PID on its first line and is left behind after exit | Reproduced | Trust it only when newer than the current attempt's start |
| Resume after a crash during CDC reuses the slot and origin and skips the base copy | Reproduced on 0.18, including after SIGKILL | Automatic resume is allowed in CDC phases |
| Resume during the base copy takes a new snapshot that pgcopydb says cannot guarantee consistency | pgcopydb manual (`--not-consistent`) | Never resume a base copy; restart the database from zero |
| `compare schema` reports no differences when the target has extra objects (Advanced's `doadmin.aries_*` tables) | Reproduced | Treat target-only objects as a warning, not a blocker |
| `en_US.UTF-8`, `en_US.utf-8` and `en_US.utf8` sort identically, with the same collation version | Reproduced on glibc 2.39 | Normalize locale names before comparing, plus a sort-order probe |
| Logical decoding does not carry schema changes, sequence values, large objects or unlogged tables | PostgreSQL documentation | Schema freeze guidance, sequence sync at cutover, warnings in preflight |
| A Standard cluster offered 20 replication slots and WAL senders, and `doadmin` had REPLICATION | Observed in a real preflight run | Do not assume these limits; check them per cluster |
| An idle database never reaches its end position; a logical decoding message gets it there in about 0.4 s | Reproduced in Phase 0 (S05) | Nudge immediately after setting each end position (PC3) |
| 0.18 prunes applied CDC files itself every 5 minutes; `stream prune` works but is not needed | Reproduced in Phase 0 (S08) | Never call prune (PC8) |
| Without SET on `session_replication_role` on the target, 0.18 applies nothing and reports success | Reproduced in Phase 0 (S02, F2) | Hard preflight check; independent verification (PC4, PC13) |
| 0.18 intermittently never applies the last large transactions before the end position and reports success | Reproduced in Phase 0 (S09, F9: 5 of 7 runs) | Final heartbeat, origin check and data verification gate GO (PC13); report upstream (PC9) |
| The receive process ignores SIGTERM and SIGINT while streaming | Reproduced in Phase 0 (S01, F3) | Stop by SIGKILL of the cgroup (PC2) |
| A failed base copy leaves the main and receive processes streaming | Reproduced in Phase 0 (F10) | Detect from the log and stop the unit (PC14) |
| Replication origins survive dropping the target database | Reproduced in Phase 0 (F4) | Cleanup drops origins by name (PC6) |

## Testing strategy

Every phase ships with tests that run in CI, against real PostgreSQL rather than mocks wherever the database is involved. The fault-injection suite is the core: it reproduces every failure the harness met, and the app must recover exactly as specified.

**Layers**

| Layer | Scope | Tooling |
| --- | --- | --- |
| Unit | State machine transitions, retry classification, check logic, redaction, LSN math, forecasts, settings validation | Go `testing`, table-driven |
| Integration | Engine against real clusters; each check against crafted schemas | Docker Compose pairs for PostgreSQL 14 to 18; a Standard-like source (admin user with REPLICATION, not superuser) and an Advanced-like target (pre-existing `doadmin.aries_*` tables) |
| Fault injection | The scenarios below, automated | Scripts that kill processes, block network, fill disks, reboot VMs |
| Scale | 100 GB and 1 TB synthetic datasets; 50 small databases; a write load during CDC | Data generator plus pgbench-style writers on DO droplets |
| Soak | 72 hours of CDC under steady writes, then cutover | Scheduled run |
| UI end-to-end | Wizard, preflight acceptance, monitoring, cutover, rollback, settings, RBAC visibility | Playwright, against a real backend |
| Security | Redaction of known secrets on every log path, authentication and lockout, CSRF, role enforcement on every endpoint, audit chain verification | Go tests plus an automated endpoint-by-role matrix |
| Installer | Fresh install, second run changes nothing, upgrade, rollback, offline bundle, refusal to swap pgcopydb during a run | Ubuntu 22.04 and 24.04 VMs |

**Fault-injection scenarios** (each must end in the stated outcome, with an event, a log trail and, where noted, a notification):

| Scenario | Expected outcome |
| --- | --- |
| SIGKILL pgcopydb's main process during CDC | Unit stops all sub-processes; slot freed; automatic resume; database back in sync |
| SIGKILL during base copy | Restart required; no automatic resume; other databases keep streaming |
| Kill the app (SIGKILL) during base copy and during CDC | Engines keep running; app restarts via systemd; re-attaches without losing log lines |
| App hang (block the orchestrator loop) | Watchdog restarts the app within about 30 seconds; engines unaffected |
| Droplet reboot during CDC | Databases resume after clusters are reachable; event and notification sent |
| Droplet reboot during base copy | Restart required, shown with a clear next step |
| Network partition to the source for 2 minutes | Transient retries; resume once reachable; no duplicate attempts |
| Stale walsender holding the slot | Released within 15 to 20 seconds; relaunch succeeds |
| Work volume filling | Warning, critical, then the guard stops the fastest-growing database at 95%; nothing corrupts |
| Slot invalidated (small `max_slot_wal_keep_size`, heavy writes) | Critical alert; Restart required; no automatic action |
| Writes still happening at cutover | Refused without an Admin override |
| Write freeze applied, then cutover aborted | Freeze undone automatically; source writable again |
| Verification mismatch (rows deleted on target) | NO-GO naming the table; rollback offered |
| Double-submitted commands (same idempotency key) | Acted on once |
| Unlogged table with default plugin | Automatic switch to `test_decoding` for that database only |
| Name conflict on the target | Hard blocker that cannot be accepted |
| Database added to the source mid-migration | Not migrated silently; shown as new in a re-run of discovery |
| App update during CDC | Engines keep running; update completes; re-attach works |

## Delivery phases

Build in seven phases, each ending at a gate the team signs off before the next begins. Phase 0 proves the assumptions this design rests on; nothing else starts until its findings are written up. Durations are for the team to estimate after Phase 0.

&#91;embedded content: Delivery roadmap · seven phases, each closed by a gate\]

Each diamond is a gate; the label under it is what must be true to pass it. Details per phase follow.

1. **Phase 0, validation spikes.** Prove or disprove every item marked "verify in Phase 0":
   - pgcopydb under `systemd-run`, cgroup stop, and slot release timing.
   - The polkit rule for the service user.
   - DO API endpoints, fields and firewall merge behavior.
   - Write-freeze privileges on a Standard (Aiven-backed) cluster.
   - The logical-message drain nudge and `pgcopydb stream prune`.
   - Measured metrics storage for the proposed budget.

   *Gate*: a findings document with evidence for each item, and the design updated where an item failed.
2. **Phase 1, core backend.** Store and schema migrations, config, secrets, authentication, RBAC, audit log, logger, engine interface, pgcopydb engine, orchestrator with the state machine, intents and attempts, and per-database start, stop, resume and restart. A `upwell` CLI exposes the same operations for debugging. *Gate*: the harness's tested scenarios (single and multi-database, crash and resume, cleanup with nothing left behind) pass through the API.
3. **Phase 2, preflight, cutover and verification.** The full check catalog with acceptance rules, the cutover flow including the optional write freeze and rollback, verification, and reports in HTML, PDF and Markdown. *Gate*: an end-to-end migration through the API reaches GO; the cutover-related fault scenarios pass.
4. **Phase 3, UI.** The design system, every screen in the UI specification, the operation console, the live log viewer, and the cutover console. *Gate*: a complete migration done from the browser only, with no terminal; the Playwright suite passes; an accessibility check passes at WCAG 2.1 AA.
5. **Phase 4, operations hardening.** Installer and updates, watchdog and reboot reconciliation, the scheduler for many databases, notifications, Prometheus, support bundle, and DO API integration. *Gate*: the full fault-injection suite and the installer suite pass on Ubuntu 22.04 and 24.04.
6. **Phase 5, scale and pilot.** 100 GB and 1 TB tests, 50-database test, 72-hour soak, sizing table calibrated from results, and support-operated pilot migrations with customer permission. *Gate*: the 1 TB run finishes within the app's own estimate band; the soak shows no leaks or drift; every pilot ends GO or in a clearly diagnosed, documented failure.
7. **Phase 6, customer readiness.** Self-serve mode, SSO, customer-facing documentation, and a security review sign-off. *Gate*: the review's findings are closed and a customer dry run succeeds without DO help.

## Open questions and risks

The biggest risk is relying on engine and platform behavior that has not been proven on real Standard and Advanced clusters; Phase 0 and the pilots exist to retire it. The open questions below need an owner before Phase 1 ends.

**Open questions**

- Do databases created on Advanced through SQL appear and behave normally in the DigitalOcean control panel, backups and failover? If not, the app must create them through the DO API instead.
- Where are releases hosted and signed (binary, manifest, bundle), and who can publish one?
- Which MySQL tooling should the future MySQL engine wrap, and does Advanced MySQL impose constraints the interface must anticipate?
- What written-permission format does support use today, so the permission record can mirror it?
- Should finished migrations' data (reports, logs) be exportable to a DO-internal system during the support phase, and under what retention?

**Risks**

| Risk | Effect | Mitigation |
| --- | --- | --- |
| pgcopydb defects in new versions (the harness hit a CDC apply bug in 0.15) | Failed or stuck migrations | Pin tested versions in the manifest; run the integration and fault suites before raising a pin |
| pgcopydb 0.18 silent-loss and stop defects found in Phase 0 (F2, F3, F9, F10) | Data loss reported as success | Data-safety gate before GO (PC13); report upstream with the S02 and S09 reproductions (PC9); rerun S09 before raising the pin |
| Standard (Aiven-backed) restrictions differ from test clusters | Checks pass in CI but fail in production | Phase 0 against a real Standard cluster; pilot migrations before customer rollout |
| Long snapshots bloat busy source tables | Customer-visible slowdown during multi-day copies | Live bloat visibility, warnings, and table splitting to shorten the copy |
| Slot invalidation or source disk pressure from retained WAL | Restart from zero, or source outage | Preflight estimate, headroom forecast, critical alerts before the limit |
| Operator error at cutover | Lost writes or premature switch | Readiness gate, write detection, typed confirmations, rollback until the switch is marked |
| Secrets exposure through logs or bundles | Credential leak | Redaction layer with tests on every log path; encrypted store; audit of secret access |
| SQLite growth on very long runs | Slow UI, disk use | Rollups and retention; storage measured in Phase 0 and Phase 5 |
