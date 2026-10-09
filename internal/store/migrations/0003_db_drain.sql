-- drained: the engine reached this database's end position in the current
-- cutover (set with state drained; cleared when the end position is cleared).
-- auto_restarts: automatic restarts from zero of the base copy, capped.
ALTER TABLE migration_databases ADD COLUMN drained INTEGER NOT NULL DEFAULT 0;
ALTER TABLE migration_databases ADD COLUMN auto_restarts INTEGER NOT NULL DEFAULT 0;
UPDATE migration_databases SET drained = 1 WHERE state IN ('drained', 'verified') AND endpos IS NOT NULL AND endpos <> '';
