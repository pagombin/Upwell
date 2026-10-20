# pgcopydb patches

`deploy/install.sh` builds pgcopydb from the `PGCOPYDB_TAG` tag with every
`*.patch` here applied, as version `PGCOPYDB_BUILD` (for example
`0.18+upwell.11`). Upwell refuses online migrations with any other build
(preflight check `engine_commit_order`, and again at each launch).

| Patch | Why |
| --- | --- |
| `0001-take-transactions-in-commit-order.patch` | pgcopydb 0.18 (and its main branch as of 2026-10-14) took the next transaction by BEGIN LSN, so a transaction that committed while another was open was never applied (D84). |
| `0002-wait-longer-for-the-catalog-lock.patch` | Workers gave up after 5 s waiting for pgcopydb's SQLite catalog and failed the whole base copy ("database is locked", D87); they now wait up to 2 minutes. |
| `0003-create-constraints-after-the-index-iteration.patch` | A table's constraints were created inside the catalog iteration over its indexes, holding the catalog for as long as each ALTER TABLE ran on the target; a constraint that took longer than the 2-minute wait failed the base copy with "database is locked" (D99). The index OIDs are collected first and the constraints created after the iteration. |
| `0004-apply-without-waiting-and-cdc-files-without-fsync.patch` | The apply waited up to 100 ms before every transaction, even with work waiting; it now waits only when idle. The CDC files (output, replay) use `synchronous = NORMAL`: the replication slot, not these files, is the durable record (D103). |
| `0005-indexed-next-transaction-lookups.patch` | Each transaction cost three lookups that scanned the rest of its change file: the next COMMIT in `output.db` (sorting every one left), the next BEGIN in `replay.db` (no index on `endlsn`), and the transaction's own messages (no usable index on `xid`). Draining a backlog cost N squared. Each lookup now takes the first row through an index; `replay(action, endlsn)` and `output(xid)` are created on open, so existing files get them (D106). |
| `0006-confirm-the-apply-pipeline-and-publish-only-durable-progress.patch` | The apply pipelined statements to the target and never read the answers: a refused change went unnoticed, the send buffer grew until libpq failed ("cannot allocate memory for output buffer"), and the sentinel's replay_lsn, which the source slot is confirmed to, advanced with what was only queued. It now syncs every 1,000 statements, 64 MB or 500 ms, stops on an error, and publishes only positions the target's replication origin has durably reached (D108). |
| `0007-wait-for-busy-sqlite-files.patch` | The CDC files (shared by receive and apply) had no busy timeout, so a statement meeting the other process's write failed the engine at once ("database is locked"); they now wait up to two minutes. The source catalog keeps its semaphore and retries (D108). |
| `0008-write-the-change-files-in-batches.patch` | The receive process committed every row of `output.db` on its own (about a dozen SQLite commits per changed row, under the shared semaphore), and the transform did the same in `replay.db`. Receive now commits at each transaction boundary or every 1,000 messages, the transform once per pass, and both files checkpoint every 64 MB. Nothing durable depends on these files: the slot is confirmed only to what the target committed durably (0006). It also reads `PGCOPYDB_MAX_REPLAYDB_SIZE`, used by the tests to rotate the files early (D111). |
| `0009-leave-the-engine-publication-out-of-the-restore.patch` | pgcopydb creates its publication on the source before the schema dump, so the restore created it on the target, where it outlived the migration and made the target refuse UPDATE and DELETE on listed tables without a replica identity. The publication named in `PGCOPYDB_SKIP_PUBLICATION` (Upwell sets the slot name) is left out of the restore (D112). |
| `0010-finish-a-closed-change-file-before-the-next.patch` | When the receive rotated the change file, the apply could see the file closed right after a pass that had not yet read the file's last messages, and move to the next file: the transaction whose COMMIT ended the file was skipped for good. The apply now reads a closed file once more before it moves on, and in catchup mode stops only after a pass that began once the receive was done. `PGCOPYDB_TEST_ADVANCE_DELAY_MS` widens the window for tests (D114). |
| `0011-keep-every-row-that-shares-an-lsn.patch` | A COPY writes a page of rows in one WAL record, so its rows share one LSN, and the change file, keyed on (action, LSN) with INSERT OR REPLACE, kept only the last of them: 26,100 copied rows reached the target as 172 (upstream v0.18 bug). Messages are now keyed on (action, LSN, ordinal); a transaction sent again after a restart gets the same ordinals and still replaces its first copy. Files from older builds are upgraded when opened (D114). |

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
