CREATE TABLE users (
  id TEXT PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('viewer','operator','admin')),
  disabled INTEGER NOT NULL DEFAULT 0,
  failed_logins INTEGER NOT NULL DEFAULT 0,
  locked_until INTEGER,
  last_login_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL UNIQUE,
  csrf_token TEXT NOT NULL,
  ip TEXT, user_agent TEXT,
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE TABLE settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_by TEXT,
  updated_at INTEGER NOT NULL
);
CREATE TABLE secrets (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  ciphertext BLOB NOT NULL,
  nonce BLOB NOT NULL,
  key_version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL
);
CREATE TABLE connections (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('source','target')),
  discovery TEXT NOT NULL DEFAULT 'manual',
  host TEXT NOT NULL, port INTEGER NOT NULL,
  username TEXT NOT NULL, dbname TEXT NOT NULL,
  sslmode TEXT NOT NULL,
  ca_cert TEXT,
  password_secret_id TEXT REFERENCES secrets(id),
  storage_gb REAL,
  created_at INTEGER NOT NULL
);
CREATE TABLE migrations (
  id TEXT PRIMARY KEY,
  short_id TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  state TEXT NOT NULL,
  mode TEXT NOT NULL DEFAULT 'online',
  engine TEXT NOT NULL DEFAULT 'pgcopydb',
  source_conn_id TEXT REFERENCES connections(id),
  target_conn_id TEXT REFERENCES connections(id),
  settings TEXT NOT NULL DEFAULT '{}',
  settings_snapshot TEXT,
  flags TEXT NOT NULL DEFAULT '{}',
  fixture INTEGER NOT NULL DEFAULT 0,
  created_by TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  started_at INTEGER,
  frozen_at INTEGER
);
CREATE TABLE permission_records (
  id TEXT PRIMARY KEY,
  migration_id TEXT NOT NULL UNIQUE REFERENCES migrations(id) ON DELETE CASCADE,
  customer TEXT NOT NULL, account_id TEXT NOT NULL, ticket TEXT NOT NULL,
  granted_by TEXT NOT NULL, granted_at TEXT NOT NULL, scope TEXT NOT NULL,
  notes TEXT,
  locked_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE migration_databases (
  id TEXT PRIMARY KEY,
  migration_id TEXT NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
  source_name TEXT NOT NULL,
  target_name TEXT NOT NULL,
  include INTEGER NOT NULL DEFAULT 1,
  skip_reason TEXT,
  state TEXT NOT NULL DEFAULT 'pending',
  slot_name TEXT NOT NULL,
  origin_name TEXT NOT NULL,
  plugin TEXT,
  instance TEXT NOT NULL,
  size_bytes INTEGER, rows_estimate INTEGER, table_count INTEGER,
  last_error TEXT, error_class TEXT,
  in_sync_since INTEGER,
  backlog_bytes INTEGER,
  replay_lsn TEXT, write_lsn TEXT,
  hb_before_lsn TEXT, hb_token TEXT, endpos TEXT, origin_lsn TEXT,
  verdict TEXT,
  retry_count INTEGER NOT NULL DEFAULT 0,
  retry_window_start INTEGER,
  next_retry_at INTEGER,
  base_copy_done INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE (migration_id, source_name)
);
CREATE TABLE attempts (
  id TEXT PRIMARY KEY,
  database_id TEXT NOT NULL REFERENCES migration_databases(id) ON DELETE CASCADE,
  number INTEGER NOT NULL,
  kind TEXT NOT NULL,
  unit_name TEXT NOT NULL,
  command TEXT NOT NULL,
  started_at INTEGER NOT NULL,
  ended_at INTEGER,
  exit_code INTEGER,
  end_reason TEXT,
  log_offset INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE operations (
  id TEXT PRIMARY KEY,
  migration_id TEXT,
  kind TEXT NOT NULL,
  operation_key TEXT UNIQUE,
  state TEXT NOT NULL,
  steps TEXT NOT NULL DEFAULT '[]',
  params TEXT NOT NULL DEFAULT '{}',
  started_by TEXT,
  started_at INTEGER NOT NULL,
  ended_at INTEGER,
  result TEXT,
  error TEXT
);
CREATE INDEX operations_migration ON operations(migration_id, started_at);
CREATE TABLE intents (
  id TEXT PRIMARY KEY,
  operation_id TEXT,
  action TEXT NOT NULL,
  target TEXT NOT NULL,
  payload TEXT NOT NULL DEFAULT '{}',
  recorded_at INTEGER NOT NULL,
  outcome TEXT,
  outcome_at INTEGER
);
CREATE INDEX intents_open ON intents(outcome) WHERE outcome IS NULL;
CREATE TABLE idempotency (
  key TEXT PRIMARY KEY,
  user_id TEXT,
  method TEXT NOT NULL,
  path TEXT NOT NULL,
  status INTEGER NOT NULL,
  response TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE preflight_runs (
  id TEXT PRIMARY KEY,
  migration_id TEXT NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
  operation_id TEXT,
  state TEXT NOT NULL,
  started_at INTEGER NOT NULL,
  ended_at INTEGER,
  summary TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE check_results (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES preflight_runs(id) ON DELETE CASCADE,
  check_id TEXT NOT NULL, check_version INTEGER NOT NULL,
  scope TEXT NOT NULL, database TEXT NOT NULL DEFAULT '',
  level TEXT NOT NULL, hard INTEGER NOT NULL DEFAULT 0,
  title TEXT NOT NULL, message TEXT NOT NULL,
  evidence TEXT NOT NULL DEFAULT '{}', evidence_hash TEXT NOT NULL,
  remediation TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX check_results_run ON check_results(run_id);
CREATE TABLE acceptances (
  id TEXT PRIMARY KEY,
  migration_id TEXT NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
  check_id TEXT NOT NULL, scope TEXT NOT NULL, database TEXT NOT NULL DEFAULT '',
  evidence_hash TEXT NOT NULL,
  reason TEXT NOT NULL,
  accepted_by TEXT NOT NULL,
  accepted_at INTEGER NOT NULL,
  lapsed_at INTEGER
);
CREATE TABLE metric_series (
  id INTEGER PRIMARY KEY,
  migration_id TEXT NOT NULL,
  database TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL,
  UNIQUE (migration_id, database, name)
);
CREATE TABLE metric_samples (
  series_id INTEGER NOT NULL, ts INTEGER NOT NULL, value REAL,
  PRIMARY KEY (series_id, ts)
) WITHOUT ROWID;
CREATE TABLE metric_rollups_1m (
  series_id INTEGER NOT NULL, ts INTEGER NOT NULL,
  vmin REAL, vmax REAL, vavg REAL, vlast REAL,
  PRIMARY KEY (series_id, ts)
) WITHOUT ROWID;
CREATE TABLE metric_rollups_15m (
  series_id INTEGER NOT NULL, ts INTEGER NOT NULL,
  vmin REAL, vmax REAL, vavg REAL, vlast REAL,
  PRIMARY KEY (series_id, ts)
) WITHOUT ROWID;
CREATE TABLE alerts (
  id TEXT PRIMARY KEY,
  migration_id TEXT,
  rule TEXT NOT NULL, scope TEXT NOT NULL,
  severity TEXT NOT NULL, state TEXT NOT NULL,
  message TEXT NOT NULL,
  first_at INTEGER NOT NULL, last_at INTEGER NOT NULL,
  acked_by TEXT, ack_note TEXT, acked_at INTEGER,
  resolved_at INTEGER
);
CREATE INDEX alerts_open ON alerts(state);
CREATE TABLE events (
  id TEXT PRIMARY KEY,
  seq INTEGER NOT NULL,
  migration_id TEXT,
  database TEXT,
  type TEXT NOT NULL,
  severity TEXT NOT NULL,
  message TEXT NOT NULL,
  data TEXT NOT NULL DEFAULT '{}',
  ts INTEGER NOT NULL
);
CREATE INDEX events_migration ON events(migration_id, ts);
CREATE TABLE logs (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL,
  level TEXT NOT NULL,
  component TEXT NOT NULL,
  migration_id TEXT,
  database TEXT,
  attempt INTEGER,
  step TEXT,
  table_name TEXT,
  op TEXT,
  msg TEXT NOT NULL,
  raw TEXT
);
CREATE INDEX logs_migration ON logs(migration_id, seq);
CREATE INDEX logs_op ON logs(op) WHERE op IS NOT NULL;
CREATE TABLE log_offsets (
  file TEXT PRIMARY KEY,
  inode INTEGER NOT NULL,
  offset INTEGER NOT NULL
);
CREATE TABLE verifications (
  id TEXT PRIMARY KEY,
  migration_id TEXT NOT NULL,
  database TEXT NOT NULL,
  run_id TEXT NOT NULL,
  check_id TEXT NOT NULL,
  result TEXT NOT NULL,
  title TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '{}',
  ts INTEGER NOT NULL
);
CREATE INDEX verifications_migration ON verifications(migration_id, run_id);
CREATE TABLE audit_log (
  seq INTEGER PRIMARY KEY,
  id TEXT NOT NULL UNIQUE,
  ts INTEGER NOT NULL,
  user_id TEXT, username TEXT, ip TEXT,
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  before TEXT, after TEXT,
  prev_hash TEXT NOT NULL,
  hash TEXT NOT NULL
);
CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit_log BEGIN SELECT RAISE(ABORT, 'audit log is append-only'); END;
CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit_log BEGIN SELECT RAISE(ABORT, 'audit log is append-only'); END;
