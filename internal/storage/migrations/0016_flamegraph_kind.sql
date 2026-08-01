-- Add 'flamegraph' to the allowed content kinds.
--
-- SQLite cannot ALTER a CHECK constraint in place, so both tables are rebuilt
-- with the relaxed constraint, exactly as 0013 and 0015 did. Column order is
-- preserved so `INSERT ... SELECT *` copies line up positionally.
--
-- DATA-LOSS HAZARD this guards against (same as 0015): the migrator opens the
-- db with foreign_keys ON and runs each migration in a transaction. Inside a
-- transaction `PRAGMA foreign_keys` is a no-op, and defer_foreign_keys only
-- defers the FK *check* - NOT the `ON DELETE CASCADE` *action*. So a naive
-- `DROP TABLE pastes` fires the cascade and empties `versions` BEFORE its rows
-- can be copied. The versions rows are therefore stashed in an FK-free table
-- (CREATE TABLE ... AS SELECT copies rows only, no constraints, so the cascade
-- cannot reach it) before the parent is dropped, then refilled after both
-- tables are rebuilt.
PRAGMA defer_foreign_keys = ON;

CREATE TABLE versions_stash AS SELECT * FROM versions;

CREATE TABLE pastes_new (
    slug          TEXT PRIMARY KEY,
    identity      TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL CHECK (kind IN ('html', 'markdown', 'diff', 'mermaid', 'pdf', 'csv', 'json', 'flamegraph')),
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

DROP TABLE versions;
CREATE TABLE versions (
    slug         TEXT NOT NULL,
    ver_num      INTEGER NOT NULL CHECK (ver_num > 0),
    kind         TEXT NOT NULL CHECK (kind IN ('html', 'markdown', 'diff', 'mermaid', 'pdf', 'csv', 'json', 'flamegraph')),
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
