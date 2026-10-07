# Phase 0 spikes

Scripts that prove or disprove the "verify in Phase 0" items of the [spec](../docs/spec.md).
Findings and evidence: [docs/phase0/findings.md](../docs/phase0/findings.md).

Each spike writes JSON lines to `docs/phase0/evidence/<spike>/results.jsonl`, plus the raw
engine logs it produced. Spikes are throwaway code: they are not part of the product and
are not built by the product build.

| Spike | Proves | Runs where |
| --- | --- | --- |
| `env/local-clusters.sh` | Creates a local Standard-like source and Advanced-like target (PostgreSQL 16, admin `doadmin` without superuser) | Any Ubuntu with PostgreSQL server packages |
| `s01-unit-stop.sh` | Engine unit stop, SIGKILL of pgcopydb's main process, resume, stale walsender, signals | Local pair. `RUNNER=systemd` on a droplet, `RUNNER=cgroup` in a container (emulated) |
| `s02-target-privileges.sh` | What pgcopydb's CDC apply needs on the target, and what happens without it | Probe: any target, read-only. Matrix: local pair only |
| `s03-write-freeze.sh` | Whether the admin can apply and undo the cutover write freeze | Local pair or a real Standard cluster (uses a throwaway database and role) |
| `s04` (document) | DigitalOcean API endpoints, fields, firewall replace semantics, scopes | [evidence/s04-do-api.md](../docs/phase0/evidence/s04-do-api.md) |
| `s05-drain-nudge.sh` | Whether a stream reaches its end position with no new writes, and whether a logical message nudges it | Local pair or real clusters |
| `s06-metrics-storage/` | Measured SQLite size of the proposed metrics retention | Anywhere with Go 1.23+ |
| `s07-polkit/` | polkit rule and engine template unit for the unprivileged service user | Disposable Ubuntu 24.04 and 22.04 droplets (needs systemd as PID 1) |
| `s08-stream-prune.sh` | Whether `pgcopydb stream prune` frees applied CDC files during a live run | Local pair or real clusters |

## Running locally

```bash
sudo bash spikes/env/local-clusters.sh up    # once
sudo bash spikes/s01-unit-stop.sh             # each spike is independent
```

pgcopydb 0.18 must be on `PATH` (or set `PGCOPYDB=`). The PGDG apt repository has it;
where that is unreachable, build the `v0.18` tag from https://github.com/dimitri/pgcopydb.

## Running against real clusters

Set `UPWELL_SRC_URI` and `UPWELL_DST_URI` to admin URIs **without passwords**
(for example `postgres://doadmin@host:25060`), `UPWELL_SSL_QS=sslmode=require`, and
`PGPASSFILE` to a 0600 pgpass file. Run only the spikes marked safe for real clusters
above, against clusters created for the test, never a customer's.
