-- The resources a keyed request's Trails event named (trails.Resource objects), so its replay
-- names them too: a retried DELETE whose target is gone still names what the first one deleted
-- (contracts/trails.md, "The event"). NULL on rows stored before this column.
ALTER TABLE idempotency_keys ADD COLUMN resources jsonb;
