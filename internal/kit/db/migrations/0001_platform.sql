-- The tables every product needs to plug into the platform. A product's own migrations run after
-- these (db.Migrate applies the kit's set first).

-- Trails: the transactional outbox (contracts/trails.md, "The transactional outbox"). A call's
-- event is written in the transaction of its change; the relay ships it to audit-api.
CREATE TABLE audit_outbox (
  id           uuid PRIMARY KEY,                    -- the event's eventId
  event        jsonb NOT NULL,                      -- the event, stored as a JSON object
  created_at   timestamptz NOT NULL DEFAULT now(),
  available_at timestamptz NOT NULL DEFAULT now(),  -- when the relay may (re)try it
  attempts     integer NOT NULL DEFAULT 0,
  last_error   text,
  rejected_at  timestamptz                          -- audit-api refused it: kept, never retried
);
CREATE INDEX audit_outbox_available_at_idx ON audit_outbox (available_at);

-- Metering: the usage outbox (contracts/metering.md, "The usage outbox"). The id is the event's
-- deterministic ID, so a checkpoint that runs twice writes nothing the second time.
CREATE TABLE usage_outbox (
  id           text PRIMARY KEY,
  event        jsonb NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  available_at timestamptz NOT NULL DEFAULT now(),
  attempts     integer NOT NULL DEFAULT 0,
  last_error   text,
  rejected_at  timestamptz
);
CREATE INDEX usage_outbox_available_at_idx ON usage_outbox (available_at);
-- Watermarks and closures ask whether anything is still pending for an hour or an organization.
CREATE INDEX usage_outbox_pending_org_idx ON usage_outbox ((event->>'organizationId')) WHERE rejected_at IS NULL;

-- Control totals for watermarks: what the producer wrote per usage hour and meter, counted only
-- for rows actually inserted, in the same transaction.
CREATE TABLE usage_control_totals (
  usage_hour timestamptz NOT NULL,
  meter      text NOT NULL,
  events     bigint NOT NULL DEFAULT 0,
  quantity   numeric(38, 12) NOT NULL DEFAULT 0,
  PRIMARY KEY (usage_hour, meter)
);

-- The last watermark sent per region: the watermark never moves back.
CREATE TABLE usage_watermarks (
  region           text PRIMARY KEY,
  complete_through timestamptz NOT NULL,
  sent_at          timestamptz NOT NULL DEFAULT now()
);

-- Organization closures: pending once the producer has nothing left in a deleted organization,
-- sent once the outbox has drained past complete_through.
CREATE TABLE organization_closures (
  organization_id  uuid PRIMARY KEY,
  complete_through timestamptz NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  sent_at          timestamptz
);

-- Idempotency keys (contracts/api-conventions.md, "Idempotency keys"): only successes are stored,
-- in the transaction of the change, and kept at least 24 hours.
CREATE TABLE idempotency_keys (
  organization_id uuid NOT NULL,
  key             text NOT NULL,
  fingerprint     text NOT NULL,                    -- SHA-256 of canonical {method, path, body}
  status          integer NOT NULL,
  body            jsonb,
  location        text,
  operation_id    uuid,                             -- replays answer the operation as it is now
  created_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (organization_id, key)
);
CREATE INDEX idempotency_keys_created_at_idx ON idempotency_keys (created_at);

-- Operations (contracts/operations.md): long-running changes, run by a worker from this table.
CREATE TABLE operations (
  id                   uuid PRIMARY KEY,
  organization_id      uuid NOT NULL,
  project_id           uuid,
  type                 text NOT NULL,               -- the eventName of the call that started it
  action               text NOT NULL,               -- the IAM action it was authorized with
  status               text NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
  target_id            uuid NOT NULL,
  target_resource_name text NOT NULL,
  target_type          text NOT NULL,
  created_by           jsonb NOT NULL,              -- org-api's principal: { type, id }
  input                jsonb NOT NULL DEFAULT '{}',
  error                jsonb,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  started_at           timestamptz,
  finished_at          timestamptz,
  -- The worker's queue: when a queued operation may be claimed, and how long a running one's
  -- worker holds it (heartbeats push it on; a lapsed lease means the worker vanished).
  available_at         timestamptz NOT NULL DEFAULT now(),
  lease_until          timestamptz,
  attempts             integer NOT NULL DEFAULT 0,
  CHECK ((status IN ('succeeded', 'failed', 'cancelled')) = (finished_at IS NOT NULL)),
  CHECK ((status = 'failed') = (error IS NOT NULL))
);
-- One active operation per resource.
CREATE UNIQUE INDEX operations_one_active_per_target_idx ON operations (target_id) WHERE status IN ('queued', 'running');
CREATE INDEX operations_list_idx ON operations (organization_id, created_at DESC, id DESC);
CREATE INDEX operations_queued_idx ON operations (available_at) WHERE status = 'queued';
CREATE INDEX operations_running_idx ON operations (lease_until) WHERE status = 'running';
CREATE INDEX operations_finished_idx ON operations (finished_at) WHERE finished_at IS NOT NULL;

-- Allocation quotas (contracts/quotas.md): exact counters next to the rows they count, changed in
-- the same transaction, so concurrent creates serialize on the counter row.
CREATE TABLE quota_usage (
  quota_id  text   NOT NULL,
  scope_key text   NOT NULL,
  used      bigint NOT NULL DEFAULT 0 CHECK (used >= 0),
  PRIMARY KEY (quota_id, scope_key)
);

-- Credentials this service issues for its /internal endpoints (contracts/service-auth.md). Only
-- the secret's hash is stored.
CREATE TABLE service_credentials (
  key_id       text PRIMARY KEY,
  caller       text NOT NULL,
  secret_hash  text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  revoked_at   timestamptz,
  last_used_at timestamptz,
  created_by   text NOT NULL
);
