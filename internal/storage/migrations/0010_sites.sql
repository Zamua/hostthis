-- Static-site uploads. A site is a directory served off one slug,
-- stored as an owner + a JSON manifest (path -> blob sha + size +
-- content-type) + the same 7-day retention clock a paste carries.
--
-- The slug shape is identical to a paste's, drawn from the same
-- alphabet, so a slug is unique ACROSS pastes and sites: a read path
-- looks a slug up in one table, then the other. To keep slug
-- generation collision-checking simple, the upload path retries on the
-- pastes UNIQUE(slug) and on this table's PRIMARY KEY independently;
-- the chance a fresh random slug collides with EITHER table is
-- negligible at 32^8.
--
-- The deduped blob bytes a site references count toward the owner's
-- per-identity quota exactly like a paste's versions do; the manifest
-- JSON is small metadata, not user content, so it is not charged.

CREATE TABLE sites (
    slug          TEXT PRIMARY KEY,
    identity      TEXT NOT NULL DEFAULT '',
    manifest      TEXT NOT NULL,           -- JSON: {"files": {"<path>": {"sha","size","ct"}}}
    deduped_size  INTEGER NOT NULL CHECK (deduped_size >= 0),
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL
);

-- Owner lookups (quota sums) hit identity; the expiry sweep hits
-- expires_at. Both want indexes, mirroring the pastes table.
CREATE INDEX idx_sites_identity ON sites (identity);
CREATE INDEX idx_sites_expires_at ON sites (expires_at);
