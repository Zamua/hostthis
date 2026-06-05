-- Pastes table — one row per slug. Versions table (added when we
-- spec versioning beyond a single update) lives separately and
-- references this row.

CREATE TABLE pastes (
    slug          TEXT PRIMARY KEY,
    owner_hash    TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL CHECK (kind IN ('html', 'markdown')),
    content_sha   TEXT NOT NULL,
    size          INTEGER NOT NULL CHECK (size >= 0),
    name          TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL
);

-- Owner lookups (list, whoami quotas) hit owner_hash, and the expiry
-- sweep hits expires_at. Both want indexes.
CREATE INDEX idx_pastes_owner ON pastes (owner_hash);
CREATE INDEX idx_pastes_expires_at ON pastes (expires_at);
