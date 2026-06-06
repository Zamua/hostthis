-- Bump retention from 24h to 7 days. Existing pastes were inserted
-- under the old regime with expires_at = updated_at + 24h. Raise them
-- to updated_at + 7 days so the live URLs keep working through the
-- transition.
--
-- Only touches pastes whose expires_at is still in the FUTURE —
-- past-expired rows stay 404'd; the sweep harvests them on the
-- next tick.
--
-- Timestamps are stored as RFC3339Nano strings (e.g.
-- "2026-06-06T00:30:00.123456789Z"). SQLite's datetime function
-- parses ISO-8601-ish strings; we emit back in compatible form using
-- strftime('%Y-%m-%dT%H:%M:%fZ', ...). Go's time.Parse(time.RFC3339Nano)
-- handles the three-digit fractional seconds that produces.

UPDATE pastes
SET expires_at = strftime('%Y-%m-%dT%H:%M:%fZ', updated_at, '+7 days')
WHERE expires_at > strftime('%Y-%m-%dT%H:%M:%fZ', 'now');
