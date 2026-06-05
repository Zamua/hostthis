-- Extend pastes for publish/unpublish, version pinning, and signed
-- share URLs. Add a versions table so update keeps history within
-- the 24h retention window.

ALTER TABLE pastes ADD COLUMN published      INTEGER NOT NULL DEFAULT 1;
ALTER TABLE pastes ADD COLUMN pinned_version INTEGER NOT NULL DEFAULT 1;
-- HMAC secret used to sign share URLs. Bumped on `unshare` to revoke
-- all outstanding tokens for the slug in one move. ALTER TABLE
-- can't take a function default in SQLite, so application code fills
-- it on insert and on existing rows the next time they're touched.
ALTER TABLE pastes ADD COLUMN share_secret   BLOB    NOT NULL DEFAULT x'';

CREATE TABLE versions (
    slug         TEXT NOT NULL,
    ver_num      INTEGER NOT NULL CHECK (ver_num > 0),
    kind         TEXT NOT NULL CHECK (kind IN ('html', 'markdown')),
    content_sha  TEXT NOT NULL,
    size         INTEGER NOT NULL CHECK (size >= 0),
    created_at   TEXT NOT NULL,
    PRIMARY KEY (slug, ver_num),
    FOREIGN KEY (slug) REFERENCES pastes(slug) ON DELETE CASCADE
);

CREATE INDEX idx_versions_slug ON versions (slug);

-- Backfill: every existing paste gets a v1 row matching its current
-- content_sha + size + kind + created_at.
INSERT INTO versions (slug, ver_num, kind, content_sha, size, created_at)
SELECT slug, 1, kind, content_sha, size, created_at FROM pastes;

-- HTTP API tokens — issued via `ssh hostthis.dev token create`.
-- We store the sha256 of the token (NOT the token itself); the raw
-- bytes are emitted once at creation and never persisted.
CREATE TABLE api_tokens (
    token_sha256  TEXT PRIMARY KEY,
    owner_hash    TEXT NOT NULL,
    prefix        TEXT NOT NULL,     -- first 8 chars of raw token, for `token list` UX
    created_at    TEXT NOT NULL,
    last_used_at  TEXT             -- NULL until first use
);
CREATE INDEX idx_api_tokens_owner ON api_tokens (owner_hash);
