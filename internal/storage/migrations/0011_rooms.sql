-- Rooms: the no-auth, capability-based app-persistence tier (see
-- SPEC.md "Rooms (app persistence)"). A room is a key-value namespace
-- created under a deployed static-site app and addressed by an
-- unguessable UUIDv4. Possession of the UUID is the entire access
-- model; the storage layer namespaces every value by the triple
-- (app_slug, room_id, key) so a room can never cross-read another
-- room's or another app's data.
--
-- Two tables, mirroring the shale shard-family shape documented in the
-- spec (rooms/<app-slug>/<room-uuid> + room_kv/<app-slug>/<room-uuid>/
-- <key>). The single-host sqlite backend stores the same logical rows
-- in its own tables; the observable contract is identical across
-- backends.

-- The room record: identity is the OWNING APP's slug, the id is the
-- UUIDv4 capability. The retention clock (updated_at + a 30-day window)
-- is the same shape pastes/sites carry, just a longer window. A write
-- (PUT or DELETE) resets updated_at + expires_at; a read does not.
CREATE TABLE rooms (
    app_slug    TEXT NOT NULL,
    room_id     TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    PRIMARY KEY (app_slug, room_id)
);

-- Per-app aggregate + creation-rate accounting hit app_slug; the expiry
-- sweep hits expires_at. Both want indexes, mirroring the pastes/sites
-- tables.
CREATE INDEX idx_rooms_app_slug ON rooms (app_slug);
CREATE INDEX idx_rooms_expires_at ON rooms (expires_at);

-- One stored value per (app_slug, room_id, key). The value is opaque
-- app STATE - hostthis never parses it; val_size is the value's byte
-- length, summed for the per-room and per-app byte caps (keys are
-- small metadata, not charged). The FK cascade drops a room's values
-- when the room is deleted (by the sweep on expiry, or directly).
CREATE TABLE room_kv (
    app_slug    TEXT NOT NULL,
    room_id     TEXT NOT NULL,
    key         TEXT NOT NULL,
    val         BLOB NOT NULL,
    val_size    INTEGER NOT NULL CHECK (val_size >= 0),
    PRIMARY KEY (app_slug, room_id, key),
    FOREIGN KEY (app_slug, room_id) REFERENCES rooms (app_slug, room_id) ON DELETE CASCADE
);

-- Per-app byte aggregation sums val_size grouped by app_slug.
CREATE INDEX idx_room_kv_app_slug ON room_kv (app_slug);

-- Room-creation rate limiting: one row per created room with the source
-- IP subnet and creation time. The creation gate counts in-window rows
-- per subnet AND per app; the sweep prunes rows past the window so the
-- table stays bounded. Separate from the rooms table so a room's
-- lifetime (30 days) does not pin its creation-accounting row (1 hour).
CREATE TABLE room_creates (
    app_slug    TEXT NOT NULL,
    ip_subnet   TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE INDEX idx_room_creates_subnet ON room_creates (ip_subnet, created_at);
CREATE INDEX idx_room_creates_app ON room_creates (app_slug, created_at);
