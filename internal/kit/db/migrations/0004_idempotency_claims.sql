-- Idempotency-Key claims (contracts/api-conventions.md, "Idempotency keys"): a request with a key
-- whose handler runs a slow step before its transaction (service.Call.TransactPrepared's prepare)
-- claims the key here, in a short transaction, instead of holding a pooled connection and a
-- session lock across that step. A second request with the key meanwhile answers 409
-- idempotency_in_progress. The claim is deleted when the request ends; one left by a crash lapses
-- at expires_at.
CREATE TABLE idempotency_claims (
  organization_id uuid NOT NULL,
  key             text NOT NULL,
  token           uuid NOT NULL,           -- the claiming request's own
  expires_at      timestamptz NOT NULL,
  PRIMARY KEY (organization_id, key)
);
CREATE INDEX idempotency_claims_expires_at_idx ON idempotency_claims (expires_at);
