-- Repurpose owner_hash to hold the per-paste *identity* (used for
-- both quota accounting and capability gating). New values are:
--   "key:<sha256-fingerprint>"  — keyed upload; identity can manage
--   "ip:<subnet>"               — anonymous; identity is quota-tracked
-- The column rename clarifies intent.

ALTER TABLE pastes RENAME COLUMN owner_hash TO identity;

-- Indexes were named after the old column; recreate.
DROP INDEX IF EXISTS idx_pastes_owner;
CREATE INDEX idx_pastes_identity ON pastes (identity);

-- api_tokens still scopes by the owner's key fingerprint — tokens
-- are issued only to keyed users — so the column there keeps its
-- old name (it never held an "ip:" value).
