-- Paste lifecycle status for the async blob write (docs/SPEC.md "Paste
-- lifecycle status (async blob write)"). A paste is pending while its
-- content blob is still landing in the object store, ready once the
-- write succeeds, failed if it does not.
--
-- Every paste that exists before this migration was written by the old
-- synchronous path, so its blob is already durable: default ready.
ALTER TABLE pastes ADD COLUMN status TEXT NOT NULL DEFAULT 'ready';
