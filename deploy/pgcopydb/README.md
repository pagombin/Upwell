# pgcopydb patches

`deploy/install.sh` builds pgcopydb from the `PGCOPYDB_TAG` tag with every
`*.patch` here applied, as version `PGCOPYDB_BUILD` (for example
`0.18+upwell.2`). Upwell refuses online migrations with any other build
(preflight check `engine_commit_order`, and again at each launch).

| Patch | Why |
| --- | --- |
| `0001-take-transactions-in-commit-order.patch` | pgcopydb 0.18 (and its main branch as of 2026-10-14) took the next transaction by BEGIN LSN, so a transaction that committed while another was open was never applied (D84). |
| `0002-wait-longer-for-the-catalog-lock.patch` | Workers gave up after 5 s waiting for pgcopydb's SQLite catalog and failed the whole base copy ("database is locked", D87); they now wait up to 2 minutes. |

## Reproducing the loss

Two sessions on the source while `pgcopydb clone --follow` streams:

```sql
-- session 1                     -- session 2
BEGIN;
INSERT INTO t VALUES (1);
                                 BEGIN;
                                 INSERT INTO t VALUES (2);
                                 COMMIT;
COMMIT;
```

Without the patch the target never gets row 2. `TestOverlappingTransactionsReachTarget`
runs this ten times through Upwell.

## Changing the engine

Bump `PGCOPYDB_BUILD` (the `+upwell.N` suffix) in `deploy/install.sh` whenever
a patch is added or changed, and `pgpack.EngineBuildRequired` with it, so that
installed engines are rebuilt. Before moving to a newer upstream tag, check
whether it fixes the ordering (then the patch may be dropped) and run the
integration suite against the new build (`spikes/env/build-pgcopydb.sh`).
