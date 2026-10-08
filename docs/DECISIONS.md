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
Found in the first end-to-end run tonight: in one of two databases the final heartbeat, the last transaction before the end position, was never applied, while pgcopydb reported the end position reached. That is Phase 0's F9 (pgcopydb 0.18 drops the last transaction before an idle period when the end position arrives first). The gate caught it (NO-GO), but a GO path that depends on luck is not acceptable. The cutover now writes the final heartbeat, then emits a drain nudge every 5 s until the heartbeat is visible on the target, and only then sets end positions. The heartbeat's arrival proves every earlier write arrived; whatever pgcopydb drops after that is a nudge with no data. If the heartbeat does not arrive within the drain timeout (capped at 15 minutes), the cutover stops before setting end positions, streaming continues, and the operator sees why. Spec cutover step 3 amended accordingly. The origin check and data verification still run after the drain.

## D11. Target-database creation prefers template1; apply privileges are re-checked in the created database
Function grants are per database. Creating target databases from template0 dropped the replication-origin grants in the first test run, so the engine failed. Upwell now creates from template1 when its encoding and locale match the source (falling back to template0), and before launching the engine it re-runs the `target_apply_privileges` check inside the target database and refuses to start without the privileges (F2 would otherwise lose every change silently).

## D12. Verification ignores the `upwell` schema in table comparisons
The heartbeat table has its own checks (final heartbeat present, origin progress). Counting it as user data produced a confusing duplicate failure.
