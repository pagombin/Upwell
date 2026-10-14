# The migration overview: design note

## Who looks at it

- **The migration engineer** (Operator or Admin) who started the migration and keeps the page open for hours, often on a second screen, glancing at it between other work. They act on it: resume, restart, start the cutover.
- **A colleague or manager** (Viewer) checking on progress, or taking over a shift.
- **The customer's contact**, over a screen share, at the start and at the cutover.

All of them read the page before they read any one number. Most visits last seconds.

## The first five seconds

The top of the page must answer three questions without scrolling, in this order:

1. **Is my data safe?** No data-safety signal may be hidden or softened. The page states, in words, whether every database is copying, catching up or in sync, how far behind the target is *in time* (the heartbeat's age on the target, which is what a cutover waits for), and whether the numbers are fresh: every live value comes from a sample, and a sample older than a minute is shown as stale, in amber, with its age. After a verdict: GO, NO-GO, or GO not fully verified, never a plain GO that rests on uncompared tables.
2. **Is it on track?** Progress and ETA per database (not only overall: one 300 GB database and one 30 GB database finish hours apart), and whether the target keeps up: apply rate against the source's write rate, and the backlog's trend over the run, not just its current value.
3. **Does anything need me?** One line, at the top, naming the database and the action (Resume, Restart from zero, look at an alert), or saying plainly that nothing does.

Then: **what happens next**, one sentence ("migration_db finishes its base copy in about 2 h 10 min; then both databases stream and the cutover can start").

## When something goes wrong

- The cause comes first, in the words Upwell already uses in its errors (the failed connection, the stalled apply, the lost slot), with the database named, the time it started, and the one action that fixes it.
- Problems are never collapsed into a count: each database with a problem gets its own line.
- The page keeps showing the last good values with their age rather than blanking them, and says that it could not refresh.
- The evidence is one click away: the database page (its attempts, its logs filtered to the failing attempt), the operation log, the alert.

## What it shows, top to bottom

1. **Status summary** (new): a sentence for overall state, the action line, and the "what happens next" line.
2. **Data safety strip** (new): worst lag in time, heartbeat age, sample freshness, apply against source writes, and verification coverage once there is a verdict. Each tile turns amber or red by threshold and says why.
3. **Lifecycle stepper**.
4. **Databases table**: phase, progress with ETA, lag in time, backlog with its trend, apply rate, sample age; problems shown on the row.
5. **Trends** (new): backlog per database over the run and heartbeat lag per database over the run (charts), and apply rate against the source's WAL rate.
6. Forecasts, open alerts, recent events (kept).

## Rules it keeps

Dark and light themes, density setting, keyboard access, axe-clean, no layout shift while values stream (fixed-width numbers, reserved space for charts), 1440 and 1280 widths, and loading, empty, error and stale states for every section.
