CREATE TABLE run_lock (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  migration_id TEXT,
  acquired_at INTEGER
);
INSERT INTO run_lock(id) VALUES (1);
CREATE TABLE queue_entries (
  migration_id TEXT PRIMARY KEY REFERENCES migrations(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  auto_start INTEGER NOT NULL DEFAULT 0,
  warnings_reviewed INTEGER NOT NULL DEFAULT 0,
  queued_by TEXT NOT NULL,
  queued_at INTEGER NOT NULL
);
