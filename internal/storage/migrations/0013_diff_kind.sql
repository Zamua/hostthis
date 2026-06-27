-- Add 'diff' to the allowed content kinds. SQLite can't ALTER a CHECK
-- constraint in place, so both the pastes table and the versions table
-- (each carry `kind TEXT CHECK (kind IN ('html','markdown'))`) are rebuilt
-- with the relaxed constraint. Column order is preserved so the
-- `INSERT ... SELECT *` copies line up positionally.
--
-- defer_foreign_keys=ON holds the versions->pastes foreign key check until
-- COMMIT, so the drop/rename dance below doesn't trip the FK mid-migration;
-- at commit the data is intact and the constraint holds. This pragma (unlike
-- foreign_keys) is settable inside the transaction the migrator runs us in.
PRAGMA defer_foreign_keys = ON;

-- Rebuild pastes (the FK parent).
CREATE TABLE pastes_new (
    slug          TEXT PRIMARY KEY,
    identity      TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL CHECK (kind IN ('html', 'markdown', 'diff')),
    content_sha   TEXT NOT NULL,
    size          INTEGER NOT NULL CHECK (size >= 0),
    name          TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL,
    pinned_version INTEGER NOT NULL DEFAULT 1,
    status        TEXT NOT NULL DEFAULT 'ready'
);
INSERT INTO pastes_new SELECT * FROM pastes;
DROP TABLE pastes;
ALTER TABLE pastes_new RENAME TO pastes;
CREATE INDEX idx_pastes_expires_at ON pastes (expires_at);
CREATE INDEX idx_pastes_identity ON pastes (identity);

-- Rebuild versions (the FK child).
CREATE TABLE versions_new (
    slug         TEXT NOT NULL,
    ver_num      INTEGER NOT NULL CHECK (ver_num > 0),
    kind         TEXT NOT NULL CHECK (kind IN ('html', 'markdown', 'diff')),
    content_sha  TEXT NOT NULL,
    size         INTEGER NOT NULL CHECK (size >= 0),
    created_at   TEXT NOT NULL,
    deleted      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (slug, ver_num),
    FOREIGN KEY (slug) REFERENCES pastes(slug) ON DELETE CASCADE
);
INSERT INTO versions_new SELECT * FROM versions;
DROP TABLE versions;
ALTER TABLE versions_new RENAME TO versions;
CREATE INDEX idx_versions_slug ON versions (slug);
CREATE INDEX idx_versions_active ON versions(slug, ver_num) WHERE deleted = 0;
