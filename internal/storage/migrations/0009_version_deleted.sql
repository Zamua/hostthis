-- Per-version delete tombstone column.
--
-- When `delete <slug> <ver>` runs, the version row stays put so the
-- audit trail + version numbering survives, but `deleted = 1` flags
-- that the blob bytes were freed. Quota SUMs, served-version pickers,
-- and the `versions` verb render all filter on this column.
--
-- See docs/SPEC.md "Delete (permanent) → Per-version delete" for the
-- product behavior.

ALTER TABLE versions ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_versions_active ON versions(slug, ver_num) WHERE deleted = 0;
