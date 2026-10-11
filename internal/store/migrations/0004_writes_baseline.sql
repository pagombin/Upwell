-- writes_baseline: the source's row-change counter when the cutover's writers
-- step confirmed writes stopped; verification resamples it (D44).
ALTER TABLE migration_databases ADD COLUMN writes_baseline INTEGER;
