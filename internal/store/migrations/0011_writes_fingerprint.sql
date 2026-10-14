-- writes_fingerprint: what the row counter cannot see (tables' storage, which
-- TRUNCATE and rewrites change; sequence values; large objects), sampled at the
-- writers step and compared at every resample (D83).
ALTER TABLE migration_databases ADD COLUMN writes_fingerprint TEXT;
