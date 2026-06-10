-- Track when each (key fingerprint, IP subnet) pair was first seen by
-- the server. The Sybil-rate-limit gate caps how many distinct fresh
-- fingerprints can come from one /24 (v4) or /48 (v6) within 24h.
--
-- Why include ip_subnet in the primary key (not just identity): a
-- legitimate user might use the same key from home and from a
-- coffee shop. The rate limit fires per (key, ip), not per key
-- alone - so the same key from a new network is a new "row" but
-- it's identifiable as a known key.

CREATE TABLE key_first_seen (
    identity      TEXT NOT NULL,
    ip_subnet     TEXT NOT NULL,
    first_seen_at TEXT NOT NULL,
    PRIMARY KEY (identity, ip_subnet)
);

CREATE INDEX idx_key_first_seen_subnet ON key_first_seen (ip_subnet, first_seen_at);
