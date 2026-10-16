# pgcopydb patches

`deploy/install.sh` builds pgcopydb from the `PGCOPYDB_TAG` tag with every
`*.patch` here applied, as version `PGCOPYDB_BUILD` (for example
`0.18+upwell.4`). Upwell refuses online migrations with any other build
(preflight check `engine_commit_order`, and again at each launch).

| Patch | Why |
| --- | --- |
| `0001-take-transactions-in-commit-order.patch` | pgcopydb 0.18 (and its main branch as of 2026-10-14) took the next transaction by BEGIN LSN, so a transaction that committed while another was open was never applied (D84). |
| `0002-wait-longer-for-the-catalog-lock.patch` | Workers gave up after 5 s waiting for pgcopydb's SQLite catalog and failed the whole base copy ("database is locked", D87); they now wait up to 2 minutes. |
| `0003-create-constraints-after-the-index-iteration.patch` | A table's constraints were created inside the catalog iteration over its indexes, holding the catalog for as long as each ALTER TABLE ran on the target; a constraint that took longer than the 2-minute wait failed the base copy with "database is locked" (D99). The index OIDs are collected first and the constraints created after the iteration. |
| `0004-apply-without-waiting-and-cdc-files-without-fsync.patch` | The change apply was capped far below a busy source (D103). It waited up to 100 ms before every transaction, even with work waiting. Each lookup of the next transaction sorted every transaction left in the CDC file, and in `replay.db` scanned it without an index, so draining a backlog cost N squared. Every decoded message was also written with its own fsync, twice (output, then replay). The apply now waits only when idle. Each lookup takes the first row of each branch through an index (`r_a_endlsn` is created on open, so existing files get it). The CDC files use `synchronous = NORMAL`, which is safe because the replication slot, not these files, is the durable record. |

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
