-- D91: key ranges a full verification already found equal, so an
-- interrupted verification resumes instead of starting over.
CREATE TABLE verify_chunks (
  run_key TEXT NOT NULL,
  table_name TEXT NOT NULL,
  lo TEXT NOT NULL,
  hi TEXT NOT NULL,
  rows INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (run_key, table_name, lo, hi)
);
