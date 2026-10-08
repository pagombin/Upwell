# Decisions

Decisions made while building, with the reason for each. Newest last. Where a decision departs from the spec, it says so.

## D1. Branch
Work happens on `overnight-build`, branched from the Phase 0 branch `claude/new-session-agkd2d`. `main` is untouched.

## D2. Phase 0 amendments applied to the spec
All fourteen proposed changes from `docs/phase0/findings.md` are applied to `docs/spec.md` and cited inline as PC1 to PC14.

## D3. The origin gate compares against the final heartbeat, not the literal end position
The spec (PC13, cutover step 5) says the target's replication origin must reach the end position. Taken literally that produces false NO-GO verdicts: in Phase 0's passing runs (S09 quiet) the origin finished 56 bytes *below* the end position, because pgcopydb records the commit position of the last applied transaction while `--current` end positions include WAL written after it (the nudge, running-transaction records). Upwell therefore records the source WAL position immediately before writing the final heartbeat (`hb_before_lsn`) and requires `origin >= hb_before_lsn` after the drain. With writers stopped, the only transaction in that database after `hb_before_lsn` is the heartbeat itself, so passing this check means the heartbeat transaction (the last real write before the end position) was applied. The heartbeat row on the target is checked separately.

## D4. Data checksums are order-independent hash sums
Verification computes, per table and on both sides, `count(*)` and `sum` of the first 60 bits of `md5(row::text)` (as bigint, summed into numeric). It streams, needs no sort and no large memory, and detects missing, extra or changed rows. Tables are checksummed smallest first until `verify_checksum_budget_seconds` (default 600) is spent; the rest are reported as "not checksummed" (warning) and still get counts. A mismatch on any checksummed table is NO-GO.

## D5. Heartbeat table
`upwell.heartbeat(id bigserial primary key, kind text, token text, written_at timestamptz)` in a dedicated `upwell` schema, created on the source before the engine starts (so the clone copies it and the publication includes it). Cleanup drops the schema on both sides. Preflight checks the user can create it.

## D6. UI stack
React + TypeScript + Vite, embedded with `embed`, as the spec says. Departures, to keep the bundle small and offline-friendly and to fit tonight: plain CSS with design tokens instead of Tailwind/Radix, hand-written accessible components, and hand-written SVG charts instead of ECharts. The log viewer virtualizes rows itself.

## D7. Engine runner
Production uses the systemd template (PC1): `systemctl start upwell-eng@<instance>`. For development and the test suite in containers without systemd as PID 1, a `local` runner starts the engine detached (`setsid`), in its own cgroup v2 group when available (else its own process group), and stops it with SIGKILL to the whole group after 10 s, which mirrors the template's behavior. The runner is chosen by `engine.runner` in the config (`auto` picks systemd when PID 1 is systemd).

## D8. Store
SQLite in WAL mode through `modernc.org/sqlite` (pure Go). Metrics use the compact series-plus-samples shape (PC7).

## D9. The orchestrator calls the PostgreSQL check pack directly
Heartbeat, verification, discovery and target-database creation live in `internal/pgpack` and the orchestrator calls them through narrow functions. Engine-specific behavior (commands, sentinel, log parsing, slot release) stays behind the engine interface. A MySQL engine would come with its own pack; the orchestrator's calls are the seam.

## D10. The cutover waits for the final heartbeat to reach the target before setting end positions
Found in the first end-to-end run tonight: in one of two databases the final heartbeat, the last transaction before the end position, was never applied, while pgcopydb reported the end position reached. That is Phase 0's F9 (pgcopydb 0.18 drops the last transaction before an idle period when the end position arrives first). The gate caught it (NO-GO), but a GO path that depends on luck is not acceptable. The cutover now writes the final heartbeat, then writes a small "push" heartbeat row every 5 s until the final heartbeat is visible on the target, and only then sets end positions. (A logical-message nudge is not enough here: tonight's runs showed pgcopydb applies a transaction only after a later decodable transaction arrives, and pgoutput skips transactions without table changes. The logical message still serves its Phase 0 purpose of getting an idle stream past its end position.) The heartbeat's arrival proves every earlier write arrived; whatever pgcopydb drops after that is a nudge with no data. If the heartbeat does not arrive within the drain timeout (capped at 15 minutes), the cutover stops before setting end positions, streaming continues, and the operator sees why. Spec cutover step 3 amended accordingly. The origin check and data verification still run after the drain.

## D11. Target-database creation prefers template1; apply privileges are re-checked in the created database
Function grants are per database. Creating target databases from template0 dropped the replication-origin grants in the first test run, so the engine failed. Upwell now creates from template1 when its encoding and locale match the source (falling back to template0), and before launching the engine it re-runs the `target_apply_privileges` check inside the target database and refuses to start without the privileges (F2 would otherwise lose every change silently).

## D12. Verification ignores the `upwell` schema in table comparisons
The heartbeat table has its own checks (final heartbeat present, origin progress). Counting it as user data produced a confusing duplicate failure.

## D13. The built UI is committed under `internal/webui/dist`
`go build` embeds the UI. Committing the Vite output means the binary builds on a droplet (or in CI) without Node, and install.sh only needs Go or a prebuilt binary. `npm run build` regenerates it; the e2e suite always rebuilds before running.

## D14. Signed-out detection without a 401
The UI asks `GET /api/v1/auth/session` (public, 200 either way) on load instead of `GET /auth/me`, so the sign-in page loads with no failed network request or console error. `/auth/me` still returns 401 for API clients.

## D15. Same-origin framing is allowed
The Report screen shows the HTML report in an iframe from the same origin, so `frame-ancestors` is `'self'` and `X-Frame-Options` is `SAMEORIGIN` (was `'none'`/`DENY`). Other origins still cannot frame Upwell.

## D16. "Restart Upwell" works only under systemd
`POST /api/v1/system/restart` exits the process so systemd (`Restart=always`) brings it back; engines run in their own units and keep going. Without `NOTIFY_SOCKET` it refuses with a clear message instead of killing a server nobody will restart.

## D17. Plan estimates for connections and WAL
The plan review shows peak connections per side (concurrent databases × (table jobs + index jobs + engine sessions)) and WAL retained during the copy (current source WAL rate × copy time ÷ concurrency) when a rate has been measured; otherwise it says the rate is not known yet.

## D18. Lint policy
`golangci-lint` (v2, `standard` linters) runs clean with `.golangci.yml`. errcheck ignores best-effort calls with no useful handling (closing, removing temp files, writing to a client that left, single-row reads of optional facts, bookkeeping writes). State writes that decisions depend on (`setDB`, `saveFlags`, `refreshState`) log their own failures at error level, and the calls whose failure changes what happens next (stopping an engine the supervisor decided to stop, the disk guard, the drain nudge, the cleanup password file, the automatic restart's target reset) are now handled explicitly. staticcheck's quick-fix style suggestions (QF*) are off. Running staticcheck also showed that the catalog's `client_version` check was never emitted; it now runs (hard blocker when pg_dump or pg_restore is older than the source's major version).

## D19. Heartbeat alert threshold is 300 s, not 60 s
On an idle database pgcopydb 0.18 applies streamed changes in batches: in the e2e environment the newest heartbeat on the target trailed the source by 30 to 90 s with no backlog, which made the 60 s alert fire on every quiet database. The default is now 300 s. Cutover readiness does not depend on it (it uses the backlog), and the cutover pushes heartbeat rows until the final one arrives (D10).

## D20. UI verification is part of the build
`dev/e2e.sh` starts a fresh server, runs a real migration against the local clusters with a writer, seeds the demonstration fixtures (50 databases, 63-character names, more than 10 billion rows, more than 10 TiB, more than 7 days, long errors) and runs Playwright: every screen at 1440x900 and 1280x800 in both themes is screenshotted into `docs/screenshots` and must pass the overlap, clipping, horizontal-scroll and axe checks; every screen renders its loading, empty, error and stale states; cumulative layout shift on the live overview stays under 0.1; no console errors or failed requests; every control is reachable by Tab with a visible focus ring; viewers see no action buttons; and a small visual regression baseline (`web/e2e/__visual__`) catches unintended changes. Tables hide optional columns, last first, when they would otherwise scroll horizontally; the column chooser brings them back.

## D21. Engine unit failures carry systemd's reason
If an engine unit ends without success before pgcopydb logs an error (for example systemd cannot apply the unit's sandboxing or read its environment file), the database's error now includes systemd's `Result` and the unit's last three journal lines, so the operator sees the real cause instead of "the engine exited unexpectedly". install.sh adds the `upwell` user to `systemd-journal` for this. Needs a droplet to see in action.

## D22. The tester's walkthrough is automated
`web/e2e/flow.spec.ts` drives the README's test migration through the browser against the local clusters: the wizard from an empty form, a live connection test, preflight, start, In sync, stop and resume of the database, cutover to GO, verification, the report and cleanup. `dev/e2e.sh` runs it after the screen suite with the writer stopped. It found one real bug: a new migration's database list came back as JSON `null` and crashed the wizard; the API now never returns `null` for a list.

## D23. Roles are copied without passwords, as a non-superuser can
`pgcopydb copy roles` runs `pg_dumpall --roles-only`, which reads `pg_authid`, and only a superuser may read it: on DigitalOcean it always failed, so application roles were never created on the target and any `GRANT` to them in the schema could fail the restore. Upwell now dumps roles with `--no-role-passwords` (from `pg_roles`), skips platform and existing roles, turns off attributes only a superuser may grant (SUPERUSER, REPLICATION, BYPASSRLS), drops `GRANTED BY`, and applies the rest statement by statement. The event lists login roles created without a password; their passwords must be set on the target (for example in the DigitalOcean control panel) before applications switch.

## D24. Extensions are created on the target before the engine starts
The engine runs with `--skip-extensions` (a non-superuser cannot restore extension objects), and nothing created them, so a table using an extension type, function or operator (citext, hstore, pgcrypto, PostGIS) would fail its base copy. Upwell now creates every extension of the source database in the target database, in the same schema when it exists there, before launching the engine. Extension versions are the target's defaults.

## D25. A reused Idempotency-Key with a different request is refused
Keys now remember a hash of the method, path and body; the same key with a different request returns 422 `idempotency_key_reused` instead of replaying another request's answer (store migration 0002).

## D26. Engine failures during streaming are detected from the log (F13)
In the fidelity test a resumed engine hit pgcopydb's internal SQLite lock, logged a fatal error, and its apply process exited; pgcopydb's receive process ignores SIGTERM (F3), so the unit stayed active and the database showed In sync while nothing was applied for 15 minutes. The heartbeat gate turned that into NO-GO, so nothing was lost, but nothing recovered either. During streaming, a fatal engine line or an apply process that exits outside a cutover's end position now stops the unit and goes through the retry policy.

## D27. One automatic restart from zero for pgcopydb's internal lock during the base copy (F12)
pgcopydb's processes share a SQLite catalog; under load one of them intermittently fails with "database is locked" and the base copy fails. Because the copy had not finished, restarting it from zero is safe, so a database whose base copy failed this way restarts once automatically (under `auto_restart_base_copy_max_gb`); a second failure goes to Restart required.

## D28. Readiness also checks lag in time, and preflight checks the commit rate (F11)
pgcopydb 0.18 applied about 190 rows a second for a large transaction and about 5 transactions a second for small ones in the build container (TestApplyThroughput), while the target accepted 17,000 single-row inserts a second directly. A backlog under the byte threshold could therefore hide many minutes of apply time, and the database showed In sync while it was. The readiness gate now also requires each database's heartbeat lag to be under `cutover_max_lag_seconds` (180 s), so the write pause is bounded by how far behind the target really is, and preflight's new `apply_rate` check warns when a database commits more transactions a second than `assumed_apply_tps` (5, to be calibrated on a droplet).

## D29. The migration lock is keyed on the full ID
The API accepts a migration's full or short ID; the per-migration lock was keyed on whichever string the caller used, so an API client using the short ID was not mutually excluded with the supervisor. The lock now resolves the full ID first.

## D30. The drain nudge is a heartbeat row, not a logical message (F14)
With test_decoding, pgcopydb 0.18 cannot parse a logical decoding message and its receive process fails on it ("Failed to parse test_decoding message ... drain nudge"); Phase 0 had only proved the nudge with pgoutput. The nudge after setting end positions is now a one-row transaction in Upwell's own heartbeat table, which every plugin decodes; it falls after the end position, so it is never applied, and verification ignores the `upwell` schema (D12). The logical message remains only as the fallback for pgoutput when the heartbeat is off; with test_decoding and no heartbeat the nudge is refused with a clear message.

## D31. Log lines from before an attempt are never classified against it
The tailer resumes at its stored offset, so the last lines of a stopped run (for example "Apply process has terminated") could be read after the next attempt started and counted against it. Lines timestamped before the attempt began (by less than 30 minutes, so a clock or time-zone difference can never hide real errors) are now indexed but not classified. Stall detection (D26) also stays out of drains, where apply exiting at the end position is expected.
