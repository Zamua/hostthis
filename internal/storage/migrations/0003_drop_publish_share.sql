-- The publish/unpublish/link/unshare verbs were removed: the slug is
-- already an unguessable secret (32^8 ≈ 10^12 possibilities), so
-- "share the URL with whoever you want" is the access model. No need
-- for a separate published flag or per-paste HMAC secret.

ALTER TABLE pastes DROP COLUMN published;
ALTER TABLE pastes DROP COLUMN share_secret;
