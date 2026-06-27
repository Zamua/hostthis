-- Add 'diff' to the allowed content kinds. SQLite can't ALTER a CHECK
-- constraint in place, so both the pastes table and the versions table
-- (each carry `kind TEXT CHECK (kind IN ('html','markdown'))`) are rebuilt
-- with the relaxed constraint. Column order is preserved so the
-- `INSERT ... SELECT *` copies line up positionally.
--
-- DATA-LOSS HAZARD this guards against: the migrator opens the db with
-- foreign_keys ON and runs each migration in a transaction. Inside a
-- transaction `PRAGMA foreign_keys` is a no-op, and defer_foreign_keys only
-- defers the FK *check* - NOT the `ON DELETE CASCADE` *action*. So a naive
-- `DROP TABLE pastes` would fire the cascade and empty `versions` BEFORE we
-- could copy its rows. To avoid that, the versions rows are stashed in an
-- FK-free table (CREATE TABLE ... AS SELECT copies rows only, no
-- constraints, so the cascade can't reach it) before the parent is dropped,
-- then refilled after both tables are rebuilt. defer_foreign_keys=ON still
-- holds the FK *check* to COMMIT so the drop/refill ordering doesn't trip it;
-- at commit the data is intact and the constraint holds.
PRAGMA defer_foreign_keys = ON;

-- Stash the FK child rows out of cascade range before touching the parent.
CREATE TABLE versions_stash AS SELECT * FROM versions;

-- Rebuild pastes (the FK parent). Its DROP fires ON DELETE CASCADE on the
-- (original) versions table, but those rows are safe in versions_stash.
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

-- Rebuild versions (the FK child) with the relaxed CHECK, then refill from
-- the stash. The stash is the source of truth - the original table was
-- emptied by the cascade above.
DROP TABLE versions;
CREATE TABLE versions (
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
INSERT INTO versions SELECT * FROM versions_stash;
CREATE INDEX idx_versions_slug ON versions (slug);
CREATE INDEX idx_versions_active ON versions(slug, ver_num) WHERE deleted = 0;
DROP TABLE versions_stash;
