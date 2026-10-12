CREATE TABLE apply_measurements (
  id TEXT PRIMARY KEY,
  migration_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  tps REAL NOT NULL,
  txns INTEGER NOT NULL,
  seconds REAL NOT NULL,
  lower_bound INTEGER NOT NULL,
  suggested REAL NOT NULL,
  assumed_then REAL NOT NULL,
  by_user TEXT NOT NULL
);
