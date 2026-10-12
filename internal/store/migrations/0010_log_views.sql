CREATE TABLE log_views (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  filters TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE (user_id, name)
);
