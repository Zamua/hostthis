-- pinned_version=0 now means "unpinned, serve latest"; N>0 means
-- "pinned to a specific version that survives updates."
--
-- Pre-migration the column was always set to "the most recent
-- version at the time of last write" and update bumped it on every
-- new version, so every existing paste was effectively-but-implicitly
-- unpinned. Migrate them to the new explicit 0 to match.

UPDATE pastes SET pinned_version = 0;
