-- 0019: a version is a whole-MANIFEST snapshot.
--
-- One artifact type replaces the paste/site split (docs/SPEC.md "One artifact,
-- not two aggregates"). A single document is a one-entry manifest at "/"; a
-- directory is an N-entry one. Nothing downstream distinguishes them.
--
-- SAFETY: this migration is ADDITIVE and does not drop `sites`. Every site row
-- is COPIED into pastes+versions, and the original stays readable so a rollback
-- to the previous binary keeps serving. Dropping `sites` is a separate
-- migration, taken only once the collapsed read path has been verified against
-- real data.

-- 1. Versions carry a manifest. Rebuilt rather than ALTERed because the kind
--    CHECK must also accept 'site', which sqlite cannot widen in place.
CREATE TABLE versions_stash AS SELECT * FROM versions;
DROP TABLE versions;

CREATE TABLE versions (
    slug         TEXT NOT NULL,
    ver_num      INTEGER NOT NULL CHECK (ver_num > 0),
    kind         TEXT NOT NULL CHECK (kind IN ('html', 'markdown', 'diff', 'mermaid', 'pdf', 'csv', 'json', 'flamegraph', 'log', 'text', 'site')),
    content_sha  TEXT NOT NULL,
    size         INTEGER NOT NULL CHECK (size >= 0),
    -- manifest is the version's whole content: {"files":{"<path>":{"sha","size","ct","kind"}}}.
    -- Empty string means "not yet projected" and the reader falls back to the
    -- flat columns, so a row written by an older binary still resolves.
    manifest     TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    deleted      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (slug, ver_num),
    FOREIGN KEY (slug) REFERENCES pastes(slug) ON DELETE CASCADE
);

INSERT INTO versions (slug, ver_num, kind, content_sha, size, manifest, created_at, deleted)
SELECT slug, ver_num, kind, content_sha, size,
       -- Project each existing version as a one-entry manifest at "/". Built
       -- with json_object so a slug or sha containing a quote cannot break the
       -- document.
       json_object('files', json_object('/', json_object(
           'sha',  content_sha,
           'size', size,
           'kind', kind
       ))),
       created_at, deleted
FROM versions_stash;

DROP TABLE versions_stash;
CREATE INDEX idx_versions_slug ON versions (slug);
CREATE INDEX idx_versions_active ON versions(slug, ver_num) WHERE deleted = 0;

-- 2. The pastes kind CHECK must accept 'site' too, since a directory is now a
--    pastes row. sqlite cannot widen a CHECK in place, so the table is rebuilt.
--    Every column is listed EXPLICITLY on both sides: `SELECT *` here would
--    depend on the column ORDER left by 0017's rebuild and 0018's drops, which
--    is exactly the kind of positional coupling that breaks silently.
CREATE TABLE pastes_stash AS SELECT * FROM pastes;
DROP TABLE pastes;

CREATE TABLE pastes (
    slug           TEXT PRIMARY KEY,
    identity       TEXT NOT NULL DEFAULT '',
    kind           TEXT NOT NULL CHECK (kind IN ('html', 'markdown', 'diff', 'mermaid', 'pdf', 'csv', 'json', 'flamegraph', 'log', 'text', 'site')),
    content_sha    TEXT NOT NULL,
    size           INTEGER NOT NULL CHECK (size >= 0),
    name           TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    pinned_version INTEGER NOT NULL DEFAULT 1,
    status         TEXT NOT NULL DEFAULT 'ready'
);
INSERT INTO pastes (slug, identity, kind, content_sha, size, name, created_at, updated_at, pinned_version, status)
SELECT slug, identity, kind, content_sha, size, name, created_at, updated_at, pinned_version, status
FROM pastes_stash;
DROP TABLE pastes_stash;
CREATE INDEX idx_pastes_identity ON pastes (identity);

-- 3. Every site becomes an artifact: one pastes row plus one v1 version whose
--    manifest is the site's. content_sha/size describe the ROOT entry, which is
--    what a listing shows; the CHARGED size stays the manifest's deduped total,
--    so a directory is never under-charged to the size of its index page.
INSERT INTO pastes (slug, identity, kind, content_sha, size, name, created_at, updated_at, pinned_version, status)
SELECT s.slug, s.identity, 'site',
       COALESCE(json_extract(s.manifest, '$.files."/".sha'), ''),
       COALESCE(json_extract(s.manifest, '$.files."/".size'), 0),
       '', s.created_at, s.updated_at, 0, 'ready'
FROM sites s
WHERE NOT EXISTS (SELECT 1 FROM pastes p WHERE p.slug = s.slug);

INSERT INTO versions (slug, ver_num, kind, content_sha, size, manifest, created_at, deleted)
SELECT s.slug, 1, 'site',
       COALESCE(json_extract(s.manifest, '$.files."/".sha'), ''),
       COALESCE(json_extract(s.manifest, '$.files."/".size'), 0),
       s.manifest, s.created_at, 0
FROM sites s
WHERE NOT EXISTS (SELECT 1 FROM versions v WHERE v.slug = s.slug AND v.ver_num = 1);
