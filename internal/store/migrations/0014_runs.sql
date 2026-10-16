-- D105: each finished run of a migration (a verdict, an abort, or the run a
-- re-run replaced), with its headline numbers and its report as it was.
-- Kept when the migration is deleted (no foreign key), until deleted itself.
CREATE TABLE runs (
  id TEXT PRIMARY KEY,
  migration_id TEXT NOT NULL,
  migration_name TEXT NOT NULL,
  short_id TEXT NOT NULL,
  number INTEGER NOT NULL,
  started_at INTEGER NOT NULL,
  ended_at INTEGER NOT NULL,
  outcome TEXT NOT NULL,
  summary TEXT NOT NULL,
  report_html TEXT,
  report_md TEXT,
  created_at INTEGER NOT NULL,
  UNIQUE (migration_id, started_at)
);
CREATE INDEX runs_ended ON runs(ended_at);
