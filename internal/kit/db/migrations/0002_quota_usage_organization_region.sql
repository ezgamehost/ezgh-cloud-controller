-- Quotas 1.0 (contracts/quotas.md, "Allocation in the product"): each counter carries the
-- organization it belongs to (GET /internal/v1/quotas/usage lists by it) and, for a perRegion
-- quota, its region ('' otherwise), which is part of its key.
ALTER TABLE quota_usage ADD COLUMN organization_id uuid;
ALTER TABLE quota_usage ADD COLUMN region text NOT NULL DEFAULT '';
ALTER TABLE quota_usage DROP CONSTRAINT quota_usage_pkey;
ALTER TABLE quota_usage ADD PRIMARY KEY (quota_id, scope_key, region);
CREATE INDEX quota_usage_organization_idx ON quota_usage (organization_id);

-- Only the product knows which organization an older counter's scope key belongs to: a product
-- with counters from before this fills organization_id in a migration of its own and then sets it
-- NOT NULL (the widgets template's 0002 does). With none, it's NOT NULL now.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM quota_usage) THEN
    ALTER TABLE quota_usage ALTER COLUMN organization_id SET NOT NULL;
  END IF;
END $$;
