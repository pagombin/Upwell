CREATE TABLE watched_tables (
  id TEXT PRIMARY KEY,
  migration_id TEXT NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
  database TEXT NOT NULL,
  schema_name TEXT NOT NULL,
  table_name TEXT NOT NULL,
  added_by TEXT NOT NULL,
  added_at INTEGER NOT NULL,
  source_rows INTEGER,
  target_rows INTEGER,
  counted_at INTEGER,
  count_error TEXT,
  UNIQUE (migration_id, database, schema_name, table_name)
);
