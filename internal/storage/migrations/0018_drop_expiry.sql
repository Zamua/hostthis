-- Drop the expiry columns and their indexes.
--
-- Pastes, sites and rooms persist indefinitely (docs/SPEC.md
-- "Persistence"), so nothing reads or writes expires_at and no sweep
-- scans it. The columns were NOT NULL with no default, so they have to
-- go rather than simply be ignored: an INSERT that omits one fails.
--
-- The indexes are dropped first because sqlite refuses to drop an
-- indexed column.

DROP INDEX IF EXISTS idx_pastes_expires_at;
ALTER TABLE pastes DROP COLUMN expires_at;

DROP INDEX IF EXISTS idx_sites_expires_at;
ALTER TABLE sites DROP COLUMN expires_at;

DROP INDEX IF EXISTS idx_rooms_expires_at;
ALTER TABLE rooms DROP COLUMN expires_at;
