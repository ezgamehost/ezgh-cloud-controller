# Go kit

`github.com/ezgamehost/service-template/kit/go`: the [platform contracts](../../contracts/) in Go,
for product services. Go 1.27, the standard library, pgx (Postgres driver) and ent (typed queries;
the SQL migrations remain the schema).

The [Go template](../../templates/go/) is a complete product built on it. Services outside this
repository vendor the kit (private cross-repo modules aren't set up in CI): the template's
`scaffold.sh` copies it to `internal/kit` and records the commit, and `sync-kit` updates it.

## Baselines

Consumers sync the kit with their `sync-kit` scripts (the template's `scripts/sync-kit.sh --from
<service-template checkout>`), which record the commit they synced. This repository has no tags;
a baseline is a commit on main whose CI passed, named here.

**A kit migration that adds a table needs a grant migration in each consumer that grants its
runtime roles per table, in the same change as the kit sync.** Before syncing, list the kit
migrations the sync brings in (`git diff --stat` under `internal/kit/db/migrations`) and, for every
new table, grant the roles that use it (the notes below name them per migration). The known
per-table granters are obs-stack (`internal/store/migrations/0003_roles.sql` and later),
mcp-stack and certificates-stack (`internal/dbroles/dbroles.go`). A database-backed test, such as
obs-stack's `TestWorkerKitStatements`, catches a missing grant only where its database is
available, so `go test -short` alone doesn't.

- **MCP Central baseline: `f6a6edd`** (2026-10-05; f6a6edd21570237f5c9b9348c1735b1e913f03dc), superseding `c7a6461`.
  The kit and contracts for MCP Central's first deploy (mcp-stack implementation plan M0.1–M0.8b):
  edge hosts and components (`manifest.EdgeHost`, `Endpoint.Host` `edge`/`Component`,
  `service.Options.OrgResolver`/`EdgeDomain`, `Service.Handler(component)`, `Call.Alias`,
  `Call.PublicBase`); public metadata and redirect endpoints (`Endpoint.Public`,
  `Call.Authenticate`, `Call.TrailEvent`); Trails per JSON-RPC method and Data events
  (`Trail.ByRPCMethod`/`Otherwise`/`EventCategory`, `Call.SetRPCMethod`, `Call.SetTrailError`);
  `Call.CheckBatch` and `Call.Credential`; bearer-only routes and the product's edge cookie
  (`Endpoint.Credentials`, `EdgeCookie`, `authz.BearerOnly`, `authz.ProductCookie`,
  `Credential.WithRequestURL`); manifest `scopes` (`manifest.Scope`, sent by `registration`);
  monthly counters and renewable slots in `ratelimit` (`Bucket.Monthly`, `Decision.Denied`,
  `Limiter.Renew`); long endpoints and the shutdown drain (`Endpoint.TimeoutSeconds`,
  `service.ServeWith`/`ServeOptions`, config's `SHUTDOWN_DRAIN_SECONDS`). Every addition is backward compatible: existing manifests and callers build
  and behave as before (`Handler()` is `Handler("api")`, `Serve` is `ServeWith` with the old defaults). Sync to it, or any later commit.
- **Registry baseline: `c6a9002`** (2026-10-09; c6a90024f237d235370a4385c21c2366ead62972, K5).
  registry-stack syncs at `c6a9002`; later K5 commits change contract text only. It carries registry-stack API plan tasks K1–K5: the
  registry host kind (`manifest.RegistryHost`, `HostRegistry`, `AuthRegistry`, `RegistryPaths`,
  `RegistryEndpoints`, `CheckRegistryRoutes`), registry credentials (`authz.RegistryCredentialFrom`,
  `RegistryToken`), repository names as resource-name paths (`resource.InProjectPath`) and the
  lifecycle-feed client (`orgfeed`). Backward compatible: manifests without `registry` validate
  and serve as before.
- **Workflows baseline: `97768cf`** (2026-10-09; 97768cf4a98b285eff4001d958f9af30735c358d, the
  Workflows phase 0–1 plan's K5). workflows-stack syncs at `97768cf`; later commits that only name
  it change no code. It carries
  K1–K7: `manifest.AuthInvocationToken`, `InvocationPathPrefix`, `Endpoint.InvocationToken`,
  `PublicCallback`, `CallbackPathPrefix`, `Trail.Record`, `RecordRefusals`, and any product's
  `<namespace>-api.ezghcloud.com` API host; `service.Call.InvocationToken`, `HandOffEvent`,
  `Recorded`; `service.Response.ReplayBody`; `httpapi.CORSExcept`. Backward compatible: existing
  manifests and callers build and behave as before.
- **Receipt-handle endpoints** (2026-10-12, the Queues phase 1 plan's K1), beside invocation tokens:
  `manifest.AuthReceiptHandle`, `ReceiptPathPrefix`, `Endpoint.ReceiptHandle`; the service reads
  only the body (no credential, org-api call or Origin check) and answers `httpapi.CORSAnyOrigin`
  on `/v1/receipts/`. queues-stack syncs to it or later. Backward compatible. **K1b** adds a
  guard: on receipt-handle and invocation-token endpoints, `Call.Authorize`, `Check`,
  `CheckBatch`, `AuthorizeProject[Only]`, `AuthorizeMissing`, `CheckQuotas` and `CheckQuotaItems`
  answer `500 internal_error` without asking anyone (no kit or workflows-stack invocation handler
  calls them).
- **Inference baseline: `ee94721`** (2026-10-10; ee94721a47d24d8b3f267cb99d5899e003e4c949, the
  inference launch plan's K1, K3, K5 and K2 with their review fixes, the fail-open slot release,
  and refusals-only endpoints' refusals recorded `readOnly: false`), superseding `c329118`.
  inference-stack syncs at `ee94721`. It carries `errorModel: "openai"` (`manifest.ErrorModelOpenAI`;
  `httpapi.WriteOpenAIError`, `OpenAIErrorType`, `OpenAIBody`, `x-should-retry: false` on
  `insufficient_quota`; the service's and router's failures in OpenAI's body; `OpenAIError` in
  OpenAPI, bearer-only `security`), `record: "refusals"` on computation `POST`s (with
  `insufficient_quota` recorded as `quota_exceeded`, and every refusal `readOnly: false`, so
  audit-api always keeps it), and `ratelimit`'s `Limiter.Debit`,
  `Limiter.Held`, `Decision.Remaining` and `ReasonDebit`; a fail-open `Allow` keeps the slots its
  script may have taken, so `Release` gives them back. Backward compatible: existing manifests
  and callers build and behave as before; `ratelimit_fail_open_total` gains the `debit` reason.
- **Workflows phase 2 baseline: `ff3e22f`** (2026-10-10; ff3e22fcdb5f3385f919afaabf1f9d164801359b,
  the Workflows phase 2 plan's K8, on top of the Inference baseline). workflows-stack syncs at
  `ff3e22f` for phase 2 (A8); later commits that only name it change no code: a live-host `auth: caller`
  endpoint may declare `trail.record: "refusals"` (Workflows' `PollTasks`), and the live component
  records only its refusals (`TestLivePollRecordsOnlyRefusals`). No kit API changes. Backward
  compatible: existing manifests and callers build and behave as before.
- **Watermark re-sends and `metering.Withdraw`** (2026-10-12, the Workflows phase 3 final review).
  `Watermarks.Once` also re-sends an already-watermarked hour whose control totals changed and now
  differ (`Watermarks.Resend`), once its usage has left the outbox; `metering.Withdraw` drops an
  organization's unsendable usage (unsent, or refused with the given codes) from the outbox and
  the control totals, so a deleted organization's hours are re-sent with what ingest has. Kit
  migration `0005_usage_watermark_resend.sql` adds `usage_control_totals.changed_at` (old rows
  `-infinity`: an upgrade re-sends nothing) and `usage_watermark_hours`; products that grant per
  table add `SELECT, INSERT, UPDATE ON usage_watermark_hours` for the watermark sender. Backward
  compatible: existing callers build and behave as before, plus the re-sends. Consumers pick it up
  at their next sync (workflows-stack first).
- **Delegated calls** (2026-10-20, **[being built]**, the delegated-calls plan's G1–G4; service-template
  `46385ee` and later). **Sync note: kit migration `0006_service_credential_namespace.sql` adds the
  nullable column `service_credentials.namespace`** (`^[a-z]{2,32}$`): no new table, so no new
  table grant, but `credential.Store.Verify` reads the column, so a product that grants
  `service_credentials` by column (certificates-stack's `dbroles`) adds `namespace` to its
  `SELECT (...)` list, or every `/internal` call is `503`; existing rows stay `NULL`, bound to no namespace, and keep working on `auth: "service"`
  routes. A receiver issues an edge's credential with `--namespace <edge's namespace>`
  (`credential.Store.IssueFor` with `IssueRequest.Namespace`; `Issue` and `Verify` are unchanged,
  `VerifyCaller` adds the namespace); only such a credential may call `auth: "delegated"` routes.
  The rest is additive: `authz.BatchAnswer.Credential`, `authz.Client.CheckDelegated`, the org-api
  calls' `X-Request-Id` (`httpapi.RequestIDFrom`), `httpapi.CallerUnauthenticated`,
  `DelegationForbidden` and `InvalidCallerReference`; `manifest.Delegation`, `AuthDelegated`,
  `Endpoint.Delegated`; package `delegation` (`Reference`, `Parse`, `FromAnswer`, `Client`,
  `Outbound`, `CustomerError`); `service.Call.Delegated`, `Delegation` and `Delegate`, and the
  `auth: "delegated"` dispatch; `trails.DelegatedUserAgent`. compute-stack and images-stack sync
  now; other consumers pick it up at their next sync.
- **Replays before quotas, a replay's resources, units** (2026-10-22, the Kit review of the bug-bash
  fixes). **Sync note: kit migration `0007_idempotency_resources.sql` adds the nullable column
  `idempotency_keys.resources`**: no new table, so no new grant (every known granter grants
  `idempotency_keys` by table; one that granted by column would add `resources`); rows stored
  before it replay as they did. Run the migration before the new code serves (the kit's `db.Migrate` at start does);
  as with every kit migration, an older build then refuses to start on that database, so a rollback
  needs the image with this migration or the migration's row removed by hand.
  A keyed write stores the resources its Trails event named, and its replay names them too, so a
  replayed `DELETE` names what it deleted. `Authorize`, `AuthorizeProject`, `CheckQuotas` and
  `CheckQuotaItems` look for a stored response before asking Quotas and, finding one, answer it
  and return `ErrReplayed` (handlers return it as is), so a retry at the limit is its stored
  answer, not `429`. A handler that answers through its own response writer (sso-stack's
  unsealing writer) installs it in `c.W` before `Authorize`, or a replay bypasses it. Quota messages leave units that aren't words alone (`iops`, `reqps`) and
  pluralize a `-per-` unit's first word (`megabytes-per-second`). Backward compatible otherwise.
- **`qcow2`** (2026-10-17, the images plan's K2): the shared qcow2 check, `qcow2.Check(r, fileSize, maxVirtual)`. A new package with no dependencies on the rest of the kit; images-stack vendors it now and blockstore-stack later. `QCOW2_DIFF=1` (set in CI, with qemu-utils) also checks the accepted fixtures with `qemu-img check` and `qemu-img info`. Backward compatible.
- **`PurgeIdempotencyKeys` deletes in batches** (2026-10-13, from the Queues hardening review):
  `service.PurgeBatch` (1,000) keys per statement, oldest first, until a batch is short, so a busy
  product's day of keys isn't one long delete. Same signature and result. Products pick it up at
  their next sync.
- **The manifest check enforces every quota-definition rule quotas-api does** (2026-10-13):
  `manifest.QuotaIssues` (so `Check`, `Parse` and a service's manifest test) refuses a unit that
  isn't lowercase letters and dashes, a displayName outside 1-100 characters or a description
  outside 1-500, a resourceType that isn't camelCase, a default, max, burst or tier default outside
  0..10^12 (`manifest.MaxQuotaValue`), a tier name that isn't `^[a-z][a-z0-9-]{1,31}$`, and a quota
  ID over 96 characters, naming the quota and field. Before, these failed only at registration
  (obs-stack's register hook, on units like "alert rule"). quotas-api runs the same function.
  Products pick it up at their next sync; a manifest that now fails would have failed to register.
- **Migrations take table locks with a 5 s `lock_timeout`** (2026-10-11, from the Workflows
  integration review). `db.Migrate` runs each migration with `SET LOCAL lock_timeout` set to
  `db.MigrationLockTimeout` (5 s) after its advisory lock, so a migration queued behind a long
  transaction fails with `db.ErrMigrationLockTimeout` (retry it) instead of blocking every later
  query on that table. Waiting for another migrate run isn't bounded. Products pick it up at their
  next sync; a migration that needs longer locks belongs in online DDL (`CREATE INDEX
  CONCURRENTLY` per partition outside a migration, `ADD CONSTRAINT … NOT VALID` then `VALIDATE`).
  Backward compatible for migrations that get their locks.
- **Sync note for `afde1fa` and later: kit migration `0004_idempotency_claims.sql` needs
  grants in products that grant their runtime roles per table.** It creates the raw table
  `idempotency_claims`, which `service.Call.TransactPrepared` uses when a request has an
  Idempotency-Key and a prepare step. Its purges use it too.
  - **Roles that serve keyed `TransactPrepared`** (any endpoint with `idempotencyKey: true` whose
    handler passes a prepare step): `SELECT, INSERT, UPDATE, DELETE ON idempotency_claims`. Without
    them every such request fails with `permission denied` (`500`). Plain `Transact`, and
    `TransactPrepared` without a key, never touch the table.
  - **Roles that run `service.PurgeIdempotencyKeys` or `service.DeleteIdempotencyKeys`** (the
    worker's housekeeping, organization deletion): `DELETE ON idempotency_claims` (and the
    `SELECT` a `WHERE` needs). Without it they still purge `idempotency_keys` but **silently skip
    claims**: org deletion then leaves that organization's claim rows (key strings only, each
    lapsing within minutes) until a role that can delete them purges.
  - **Known per-table granters, which must add the grants in the same change as their kit sync:**
    obs-stack (`internal/store/migrations/0003_roles.sql`), mcp-stack (`internal/dbroles/dbroles.go`,
    which uses keyed `TransactPrepared`) and certificates-stack (`internal/dbroles/dbroles.go`, the
    same). secrets-stack re-grants `ALL TABLES` after migrating, and the Go template, registry-stack,
    ocr-stack, sso-stack and quotas-stack grant no kit table per table (checked 2026-10-10), so
    they need nothing. A product that adds per-table grants later includes this table.

## Packages

| Package | What | Contract |
| --- | --- | --- |
| `config` | Env settings: `APP_ENV=local` defaults, production refusing them, every problem at once; **[being built]** the optional `ORG_API_SERVICE_CREDENTIAL` (`ok_`, `Platform.OrgAPIServiceCredential`) | [deployment](../../contracts/deployment.md#configuration) |
| `service` | Routing from the manifest (the Trails catalog check), credentials, the Origin check, malformed IDs (**[being built]** prefixed IDs in `…Id` path parameters become UUIDs before the handler: `orgId`/`projectId` through `authz.Client.LookupSlugs`, a product's own through `Service.PathIDs(map[string]service.PathID{"widgetId": {Prefix: "wgt", Table: "widgets"}})`; undeclared parameters stay UUID-only), `Call.Authorize` through org-api (then the endpoint's quota check), `Call.QuotaLimit`, `Service.QuotaUsage` (`GET /internal/v1/quotas/usage`), `Call.Transact` (change, Trails event and idempotency record in one transaction), `/health`, `/ready`, `/openapi.json`, access logs, graceful shutdown; **[being built]** ingest-host endpoints are the ingest server's, never served here; **[being built]** edge-host endpoints (`https://<alias>.<edge domain>`): `Options.OrgResolver` names the organization from the alias before any credential is read (`404` unknown, `308` retired, `503` failed), the credential gets the tenant's public base, `Call.Alias`/`Call.PublicBase`; `Service.Handler(component)` serves and drift-checks one component's endpoints (`api` by default); **[being built]** the live component (`Handler(manifest.ComponentLive)`): live-host streams with the platform's credentials, CORS, the Origin check and `timeoutSeconds`, other components' routes `404`; **[being built]** public endpoints (`public: metadata|redirect`, auth none, trail null), where a redirect handler may `Call.Authenticate`, check, and record the operation it completes with `Call.TrailEvent`; **[being built]** an endpoint's `timeoutSeconds` (its own read and write deadlines and context deadline) and `ServeWith`'s `ShutdownDrain` (config's `SHUTDOWN_DRAIN_SECONDS`); a trail per JSON-RPC method (the body's `method` or `Call.SetRPCMethod`), `Data` events, and `Call.SetTrailError` for a JSON-RPC error answered with HTTP 200; **[being built]** registry-host endpoints are the registry server's, never served here (the handler builds without one and refuses one that's registered), and so are **[being built]** data-host endpoints, the data server's | [identity](../../contracts/identity-and-authz.md), [API conventions](../../contracts/api-conventions.md); **[being built]** `Call.Replay` and `ErrReplayed` (a retry's stored response before the handler's state checks; `AuthorizeMissing` replays a retried `DELETE` whose target is gone, except on a stream route before `SetIdempotencyInput`, which answers `404`), `Call.CheckCreate` (a create checked on its planned name names the project, not the planned resource), and other products' resources typed by `resource.TypeOf` |
| `authz` | The caller's credential (the session cookie, an API key `Bearer ezgh_…` an OAuth access token `Bearer ezgh_at_…` or an EZGH SSO resource token `Bearer ezgh_sat_…`, told apart by exact match, `CustomerCredentialKind`; anything else is 401 without a call), org-api's permissions endpoint (forwarding only that credential and their `X-Forwarded-For`, no cache, 401/404/403/429/503) and the directory lookup. **[being built]** A project ingest key `Bearer ezgh_ik_…` is `Classify`'s `KindIngestKey`: `CredentialFrom` refuses it (401) on every product route, and `IngestCredentialFrom` is an ingest host's check, accepting exactly one ingest key or API key (`IngestKeyFrom` for a host that takes only ingest keys). **[being built]** Product data-plane keys (`Bearer ezgh_fs_…`, `Bearer ezgh_fc_…`, Feature Flags' SDK keys) are refused (401) on every product route too; only the product's data server resolves them. `CredentialFrom(r, BearerOnly())` never reads cookies (edge-host endpoints, `credentials: bearer`); **[being built]** `ProductCookie(name)` takes only a product's own edge cookie (`credentials: cookie`), forwarded as a bearer token with the audience label as `requestUrl` (`Credential.WithRequestURL`). `CheckBatch`: org-api's `POST /internal/v1/permissions/batch` with `Client.ServiceCredential` (the product's `ok_`, its namespace declaring `permissions.batch`) and the customer's credential in the body (never both kinds, never in `Authorization`), their `X-Forwarded-For` and, for `ezgh_sat_` only, `requestUrl`; more than 50 checks split into concurrent calls merged in order; 401, then 404, then 400 (500) are the whole answer, no call succeeding is 429 or 503, otherwise failed calls' checks come back nil with `rate_limited`/`unavailable` (`BatchResult.Allowed()` is true only for `true`); answers validated one for one, nothing cached. **[being built]** In `service`, `Call.CheckBatch` makes the call attributable from the answer (principal, `userIdentity`), with the credential's public base, and `Call.Credential` is the credential the kit read. **[being built]** `RegistryCredentialFrom` is a registry host's check: exactly one `Authorization` holding an API key, as HTTP Basic's password or a bearer, or no credential (no header, or `Bearer anonymous`, `RegistryAnonymousToken`); everything else is 401 without a call, and cookies are never read. `RegistryToken` is the key a token endpoint hands back. **[being built]** `Client.CheckPrincipalBatch` asks org-api's `POST /internal/v1/permissions/principal-batch` about a stored principal (`Principal`, a user or a bot) with the product's `ok_` alone, where no customer credential exists on the request (Private CA's ACME server, for a binding's owner): 1–50 checks a call, `404 principal_not_found` as every check denied with no identity, `429` kept, a `200` for another organization or principal and everything else `503` (`500` for a malformed body); never cached | [identity](../../contracts/identity-and-authz.md#checking-a-request), [batch checks](../../contracts/identity-and-authz.md#batch-checks), [principal checks](../../contracts/identity-and-authz.md#principal-checks-being-built), [registry credentials](../../contracts/identity-and-authz.md#registry-credentials-being-built) |
| `resourceurl` | **[being built]** The canonical resource URL (`Canonical`), the configured public base URL's origin (`Origin`), and the `requestUrl` built from it and the raw request-target (`RequestURL`), never from `Host` or `X-Forwarded-*`. `authz.Client.PublicBaseURL` (config's `PUBLIC_BASE_URL`) makes `Check` send it for `ezgh_sat_` tokens only; a path with no canonical form is 401 without a call | [API conventions](../../contracts/api-conventions.md#canonical-resource-urls-being-built), [identity](../../contracts/identity-and-authz.md#resource-binding) |
| `httpapi` | The error body, JSON, request IDs, the client IP (the rightmost `X-Forwarded-For` entry), CORS, page limits, strict body validation; **[being built]** the Prometheus HTTP API's error body (`WritePrometheusError`, `PrometheusErrorType`); **[being built]** the OpenAI API's error body (`WriteOpenAIError`, `OpenAIErrorType`, `OpenAIBody`) | [API conventions](../../contracts/api-conventions.md); **[being built]** `WithSecurityHeaders` (nosniff and HSTS on every response, applied by `Service.Handler`) |
| `cursor` | Signed cursors, audit-api's format | [pagination](../../contracts/api-conventions.md#pagination) |
| `trails` | The Trails event, what a call records (secrets hidden; a body's values only through `Call.SetRequestParameters`, otherwise its field names, each hidden, and nothing from a refused call's body), the outbox insert, the relay to audit-api; **[being built]** `IngestKeyIdentity`; **[being built]** `AcmeBindingIdentity` (an ACME client acting through a Private CA binding: the owner from the principal-batch answer, the binding's `eab_` slug as `accessKeyId`, credential `acmeBinding`) | [Trails](../../contracts/trails.md) |
| `outbox` | The relay both outboxes share: the claim (SKIP LOCKED, lease), backoff, rejections, 413 splitting | [Trails relay](../../contracts/trails.md#the-relay) |
| `usage` | Usage events: deterministic IDs, hour splitting, the meter registry, validation identical to service-kit's | [metering](../../contracts/metering.md#the-usage-event) |
| `metering` | The usage outbox with control totals, the relay to metering-ingest (`pk_`), watermarks, organization closures | [metering](../../contracts/metering.md) |
| `operations` | The operation resource (202, poll, cancel, list), one active operation per resource, the durable worker; `Waiting{After}`, Run's answer when the work goes on in a product's own queue: back to the queue, no attempt counted | [operations](../../contracts/operations.md) |
| `quota` | The product side of Quotas: `Enforcer` (each endpoint's quotas from the manifest, one `check` per request after authorization, `429 quota_exceeded` without `Retry-After`, allocation values, leases released after the response, `KeepAlive` for long work, failing open in 50 ms with `quotas_fail_open_total` and paced logs, a `401` to the service's own credential also counted in `quotas_credential_rejected_total` and logged every time, the local fallback); `Client` (check, resolve, release, renew); `Reserve`/`Release` of allocation counters in `quota_usage` against the resolved value; `UsageOf`; `BillingPeriod`, `ResolveValue`, `BucketCapacity` (the vectors). `quotatest` is a fake quotas-api | [quotas](../../contracts/quotas.md) |
| `ratelimit` | **[being built]** A product's own rate limits: token buckets (exact `Retry-After`) and concurrency slots in Redis, checked and taken all-or-nothing by one Lua script (`Limiter.Allow`, `Release`, `RetryAfterSeconds`); **[being built]** monthly fixed-window counters per UTC month (`Bucket.Monthly`, `Limit`, `MonthKey`), which buckets refused (`Decision.Denied`), and `Limiter.Renew` for long requests' slots; **[being built]** `Limiter.Debit` (a cost taken after the response: a token bucket down to minus its burst, a monthly counter past its limit), `Limiter.Held` (live slot counts, a load signal) and `Decision.Remaining` (each token bucket's tokens left); failing open in 50 ms, counted in `ratelimit_fail_open_total{reason}` with paced logs | [API conventions](../../contracts/api-conventions.md#rate-limits) |
| `rabbitmq` | **[being built]** The platform RabbitMQ broker. `Dial(url, connectionName)` gives a confirming `Publisher` (`Publish(ctx, []Message)` sends each `Message{Exchange, RoutingKey, Type, MessageID, Headers, Body}` persistent as `application/json`, with AMQP `type` and `message_id`, waits for every confirm and answers how many, from the first, were confirmed). `Relay{DB, Table, Channel, Lock, Dial, Logger, Batch}.Run` publishes a product's outbox table (`seq, exchange, routing_key, type, message_id, subject_id, version, body`; `subject_id` and `version` become the `subjectId` and `version` headers) in `seq` order while it holds the advisory lock `Lock`, woken only by `NOTIFY Channel` from the table's insert trigger, deleting confirmed rows; a broker outage keeps rows and retries with backoff, publishing unconfirmed ones again (at least once). A row the broker refuses for good (`RefusedError`, `IsRefused`: the channel closed with ACCESS_REFUSED, NOT_FOUND or PRECONDITION_FAILED, or no routing key) is retried like an outage unless `Parked` names a table with the outbox's columns plus `reason` and `parked_at`; then the relay sends the rest of that batch one row at a time (on a new channel of the same connection) to find it and, once its exchange has refused continuously for `ParkAfter` (default 5 minutes; no confirm to it and no quiet gap between refusals; in memory, reset each time the relay takes its lock), moves it there, logs an error and carries on (replay: insert it into the outbox again); until then it waits in order. Once an exchange had a row parked, its next row refused with the same code (nothing confirmed to it since) parks at once, however long after. An ACCESS_REFUSED from a second exchange right after a row parked for ACCESS_REFUSED, with nothing confirmed between (the relay's user losing its permissions), waits until its own exchange has refused for twice `ParkAfter`. `Consumer{URL, Queues, Name, Prefetch, OnConnect, Handle, Logger}.Run` reads a product's shard queues (quorum, `x-single-active-consumer` where one reader at a time is wanted; declared by the chart, never here) on one connection: it starts consuming, runs `OnConnect` (the catch-up) before handling the first delivery, then handles each queue in order (queues in parallel); `Handle` nil acknowledges, an error wrapping `ErrPoison` and `ErrHeld` rejects without requeueing (to the dead-letter exchange), any other error (or a failed `OnConnect`, or shutdown) ends the session, returning every delivery it holds with a nack (requeue) after cancelling its consumers, so that message and those after it return in order without counting toward a quorum queue's delivery limit (RabbitMQ 4.3 counts a closing connection's returns, not nacks; before 4.3 nacks counted too), then reconnects with backoff and catches up again. `Dispatch(map[type]Handler)` routes by AMQP `type`, acknowledging types it doesn't name. `URL(base, user, password, vhost)` | [network design](https://github.com/ezgamehost/network-stack/blob/main/docs/design.md#events-published), [firewall design](https://github.com/ezgamehost/firewall-stack/blob/main/docs/design.md) |
| `orgfeed` | **[being built]** A product worker's client of org-api's lifecycle feed and existence check (`New`, `Client.Events`, `Client.Exists`) with the product's `ok_`: kinds (and `Client.WithKinds`, a client whose `Events` asks for `kinds=`, e.g. the opt-in `ProjectCreated`; an event of a kind not asked for is `ErrUnavailable`), page and existence limits (`MaxPage`, `MaxExists`), 410/429/401/403/400 as `ErrFeedExpired`/`ErrRateLimited` (`Error.RetryAfter`)/`ErrDenied`/`ErrInvalid`, everything else `ErrUnavailable`; nothing cached | [organization lifecycle](../../contracts/org-lifecycle.md#the-lifecycle-feed-being-built) |
| `registration` | `PUT` the manifest's IAM part to org-api with the namespace's `rk_` credential (**[being built]** with `orgApi` and `scopes` when the manifest declares them), then its quotas to quotas-api with its `qk_` credential and `INTERNAL_URL` | [registration](../../contracts/registration.md#the-registration-api), [quotas](../../contracts/quotas.md#registering-quotas) |
| `openapi` | The customer API's OpenAPI 3.1 document, served at `GET /openapi.json`: built from the manifest (paths, methods, path parameters, credentials, operation IDs, `Idempotency-Key`, the platform's errors) and the product's `openapi.overlay.json` (`info`, query parameters, bodies, responses, schemas); an overlay operation that isn't a customer endpoint, or a customer endpoint without one, refuses to start; edge-host and **[being built]** registry-host and data-host endpoints are never in it; **[being built]** live-host endpoints are, with a `servers` entry naming the live host; **[being built]** an `errorModel: "prometheus"` operation documents the `PrometheusError` responses instead of the platform's, and a computation POST sharing its GET's event is `<eventName>Post` unless the overlay names it | [API conventions](../../contracts/api-conventions.md#openapi) |
| `manifest` | `service-manifest.json` and the rules the schema can't express. **[being built]** The PKI host (`PKIHost`, `Manifest.PKI`, `HostPKI`; exactly `PKIHostname`, reserved to `PKINamespace`), PKI-host endpoints (`auth: none`, under the host's parts, `PKIPart`: `PKIPartACME` and `PKIPartRevocation`; `Endpoint.OnPKIHost`), `PKIEndpoints` and `CheckPKIRoutes` (a PKI server's route-against-manifest check, per part it serves, with `PKIRoute`); `service.Handler` never serves them and `openapi` leaves them out. The namespace `pki` is reserved, and `ReservedOperations` (`permissions.principalBatch`: `certificates` and `objects`; `instanceBots.lookup`: `compute` and `objects`; `instanceBots.mint`: `compute` only; `organizations.rootContact`: `network` only) are refused in any other namespace's registration. **[being built]** The data host (`DataHost`, `Manifest.Data`, `HostData`), data-host endpoints (`auth: productKey`, `AuthProductKey`, `Endpoint.OnDataHost`), `DataEndpoints` and `CheckDataRoutes` (a data server's route-against-manifest check). **[being built]** Ingest actions (`ingest: true`), ingest-host endpoints (`host`, `protocol`, `auth: ingestKey`, or **[being built]** an internal producer route, `auth: service` under `/internal/`), `Action`, `IngestEndpoints`, `IngestEndpoint` and `CheckIngestRoutes` (an ingest server's route-against-manifest check, both kinds). **[being built]** `orgApi.operations` (`OrgAPI`), carried into the registration: 1–20 unique `<group>.<operation>` names, shape only (org-api refuses names it doesn't have). **[being built]** The edge host (`Edge`), edge-host endpoints (`host: edge`), `component` (`ComponentName`, `Builtin`, `EdgeEndpoints`), public endpoints (`Public`), `credentials` and the edge cookie (`EdgeCookie`); scopes (`Scope`, org-api's rules, `MatchesActionPattern`); trails per JSON-RPC method (`Trail.ByRPCMethod`, `Otherwise`, `ForMethod`) and `eventCategory`. **[being built]** The live host (`Live`, `LiveHost`), live-host endpoints (`host: live`, `component: live`; `OnLiveHost`, `LiveEndpoints`). **[being built]** `errorModel: "prometheus"` (`ErrorModel`, its rules), a GET and a computation POST of one path sharing an event (`ReadPair`), `Endpoint.ReadOnly` (computation POSTs are recorded `readOnly`, except per JSON-RPC method), and remote write's `/api/v1/write` ingest path (`RemoteWritePath`). **[being built]** `errorModel: "openai"` (`ErrorModelOpenAI`: caller `GET`s and `POST`s under `/v1/organizations/:orgId/projects/:projectId/openai/v1/` with `credentials: "bearer"`; the service answers their failures, and the router's 404s under that base, in OpenAI's body, recording `insufficient_quota` as `quota_exceeded`), and `record: "refusals"` on computation `POST`s as well as writes (EZGH Inference's `CreateEmbeddings`). **[being built]** The registry host (`Registry`, `RegistryHost`, its domain exactly `<namespace>.ezghcloud.com`, the API then on `<namespace>-api.ezghcloud.com`), registry-host endpoints (`HostRegistry`, `AuthRegistry`, the OCI paths `RegistryPaths`; `OnRegistryHost`, `RegistryEndpoints`) and `CheckRegistryRoutes` (a registry server's route-against-manifest check, with `RegistryRoute`). **[being built]** The Git host (`Git`, `GitHost`, the same domain rule), Git-host endpoints (`HostGit`, `AuthGit`, Git's and Git LFS's paths `GitPaths`, the refusals-only reads `GitReads`; `OnGitHost`, `GitEndpoints`) and `CheckGitRoutes` (with `GitRoute`) | [registration](../../contracts/registration.md#the-service-manifest) |
| `credential` | Per-service bearer credentials: parsing, hashing, and the service's own `service_credentials` | [service auth](../../contracts/service-auth.md) |
| `canonical`, `decimal`, `instant`, `ids`, `resource` | Canonical JSON and `hash16`, exact decimals, timestamps, UUIDv7 and slugs, resource names (**[being built]** `resource.InProjectPath`: a name carrying its path, a registry repository's `rep_…/<scope>/<image>`) | [metering](../../contracts/metering.md#deterministic-ids-and-hashing), [resources](../../contracts/resources.md) |
| `telemetry/push` | **[being built]** `push.Metrics(ctx, push.Options{…})`: a process nothing scrapes pushes its Prometheus registry to obs-ingest's internal producer route, every 30 s from a random offset, with a heartbeat gauge and a `pod` label. A failed push is dropped, and the next carries the totals. `push.Encode(families, service, start)` is the same OTLP request as bytes, with no heartbeat and no `pod`, for a process that sends its own way (dbagent, over its stream). Obs translates names at read, so a counter should end `_total` and have no other `total` word. Metrics only: traces stay unexported. A package apart from `telemetry`, so services that don't push link none of it. It uses `go.opentelemetry.io/proto/slim/otlp`, so a binary that also links `go.opentelemetry.io/proto/otlp` panics at start: obs-stack can't import it | [service auth](../../contracts/service-auth.md), [internal producers](../../docs/observability/otlp-ingest.md#internal-producers-being-built) |
| `nodeplane/wire`, `nodeplane/seal` | **[being built]** A node plane's envelope and sealing, lifted from databases-stack's `nodeproto` (databases plan Task 22's design; kubernetes-stack is the first consumer, lb-stack and databases-stack switch later): `wire.NewCodec(frames...)`, `Codec.Decode` (unknown frame types as `wire.Unknown`, unknown fields ignored, a missing body an error), `wire.Encode(seq, m)`; `seal.Seal(info, recipient, scope, name, version, plaintext)` and `seal.Open`, HPKE X25519/HKDF-SHA256/AES-256-GCM with `scope\nname\nversion` as the additional data. The frames stay each product's | — |
| `nodeplane/delivery`, `signals`, `commands`, `enroll`, `ca` | **[being built]** A node plane's control half, lifted from databases-stack (9ff61c1; plan Task 9 of kubernetes-stack's control-plane plan), each keyed by the product's `Prefix` (`<prefix>:presence:<scope>`, `:pod:`, `:acks`, `:alive:`, `:result:`, `:issued:`) and naming a *scope* (a database, a cluster) and a *node* (its `ins_`): `delivery.Dispatcher` (a `Pointer{scope, generation}` held until every connected node acks, retried, then parked), `Presence`, `Bus`, `Sender`, `Shard`/`RoutingKey`/`ShardQueues(prefix, n)`, `BrokerURL(env, vhost)`; `signals.Publisher`/`Consumer` (`Signal{kind, scope, node, body}`, a second issue stream for `enroll`, `csr`, `sealKey`), `ReadStatus[T]`; `commands.Sender.Send(ctx, Request{Scope, Instance, Generation, Verb, Args, Key})` with the product's command frame; `enroll.Handler` (`InstanceOf` admits only a `ComputeInstance` source holding the product's action; rows behind the product's `Store`, the answer its `Respond`) and `ReadCSR`; `ca.CA.Issue`. Rows, issuance and desired state stay each product's. kubernetes-stack is the first consumer; lb-stack and databases-stack switch later | — |
| `qcow2` | **[being built]** `Check`: whether a qcow2 disk image is self-contained and sound before anything reads it as a disk. Version 2 or 3; no backing file or format, external data file or encryption; no unknown incompatible features or extended L2; clusters 512 B to 2 MiB; zlib or zstd; the virtual size under the caller's cap; then a walk of the refcount table and the whole active L1/L2 tree, refusing any table, cluster or compressed extent outside the file or sharing a cluster (compressed extents may share with each other), and what qemu calls corruption. One `Err…` per refusal. Bounded memory (one cluster of buffer, two bits per file cluster). EZGH Images runs it on every upload and blockstore's converter on every image, so both admit exactly the same files | [images design](https://github.com/ezgamehost/images-stack/blob/main/docs/design.md#upload) |
| `db`, `testdb`, `logging`, `redis` | Migrations (the kit's tables, then the product's), the connection, transactions and ent driver (see [Data layer](#data-layer)), a database per test and the drift test, JSON logs, a minimal Redis client | |

**[being built] Pub/sub in `redis`.** `Client.Subscribe(ctx, channels...)` dials a connection of
its own (never the pool's), sends `SUBSCRIBE` and returns once the server has confirmed every
channel, so a message published after it returns is heard. `Subscription.Receive(ctx)` blocks for
the next `Message{Channel, Payload}`; a cancelled context returns its error and leaves the
subscription working, and any other error means the subscription is dead (the connection failed,
the server killed it, or it went silent), for good: the caller subscribes again and catches up on
what it may have missed. While idle for `PingEvery` (default 15 s, set before the first
`Receive`) it writes `PING` and swallows the `pong`; a connection silent for `PingEvery` plus 5 s
is dead. `Client.Publish(ctx, channel, payload)` answers how many subscribers received it.
Delivery is at most once, so a product announces from a transactional outbox with sequence
numbers (Feature Flags' `flags:changes`) and never treats a message as its only copy of a change.

## Data layer

Postgres is reached through `database/sql` over a pgx pool (pgx's `stdlib` driver), with
[ent](https://entgo.io) (v0.14.6) as a typed query layer on the same connections. The SQL
migrations in [`db/migrations`](db/migrations/) (and each product's own) remain the only
schema; ent never creates or changes a table.

### Connections and transactions (`db`)

- `db.Open(ctx, url, db.PoolOptions{MinConns, MaxConns})` builds the pool and returns a
  `*db.DB`; zero options keep pgxpool's defaults. `db.FromPool` wraps a pool built elsewhere
  (testdb does), and `db.ConfigurePool` makes every `timestamptz` scan in UTC, for pgx and
  `database/sql` alike (`Open` calls it). It also answers a context ending mid-statement with a
  server-side cancel (`db.CancelHandler`) and keeps the connection; pgx's default closed it, and
  over TLS the backend then stayed idle in its transaction, holding its locks, for up to 15 s.
- `*db.DB` has `SQL *sql.DB`, which everything uses, and `Pool *pgxpool.Pool`, which is only
  for the pgx-native exceptions: `db.Migrate` (still on a `*pgx.Conn`), testdb's admin DDL and
  raw test fixtures. It has `Ping` (readiness), `Close`, `ExecContext`/`QueryContext`/
  `QueryRowContext` and `Driver()` (ent's driver; closing a client never closes the pool).
- `d.RunTx(ctx, opts, func(tx *db.Tx) error)` is the only way to open a transaction. An
  error from fn rolls back and is returned as is. A panic rolls back and re-panics. Otherwise
  it commits and returns the commit error unwrapped, so a deferred constraint's name still
  matches. One exception: when ctx ends after fn's last statement, database/sql rolls back
  and `Commit` says only `sql.ErrTxDone`; `RunTx` then returns `ctx.Err()` joined with it, so
  the commit matches `context.Canceled` or `context.DeadlineExceeded` (503 in `service`) as a
  statement would. `*db.Tx` has no `Commit` or `Rollback`. Use `nil` options (READ COMMITTED), or
  `&sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}` for a snapshot.
- One `*db.Tx` carries raw SQL, the kit's ent client and the product's ent client together:
  they all run on the same `*sql.Tx`. ent opens a transaction of its own for some operations
  (`UpdateOne`, edge writes). Inside a `db.Tx` that joins the outer transaction. Its commit
  does nothing, and its rollback poisons the transaction, so `RunTx` rolls back and returns
  `db.ErrJoinedRollback` even if fn returned nil. `BeginTx` on a client built from a
  `db.Tx` is refused.
- `tx.Savepoint(ctx, "name", fn)` (the name matches `^[a-z_]+$`) undoes only fn's work when fn
  fails, and the transaction carries on. That includes a poisoned ent rollback inside fn.
  `operations.Create` uses it to turn a 23505 into `409 operation_in_progress`.
- `d.OpenListener(ctx, applicationName)` opens a `*db.Listener`, a connection of its own outside
  the pool with TCP keepalives, for what database/sql can't carry: `Lock` (a blocking session
  advisory lock, held until `Close`), `Listen` and `Wait` (LISTEN/NOTIFY), `Ping`. A context ending
  expires its socket's deadline at once (not the pool's server-side cancel), so `Wait` ends at its
  deadline; close it after a `Lock` or `Ping` whose context ended. The rabbitmq relay holds its lock
  and waits for its outbox's notifications on one.
- `d.Pin(ctx)` holds one connection (`*db.Conn`) for session state. `service.Transact` keeps
  its idempotency advisory lock on one and runs its transactions with `conn.RunTx`. `Close`
  returns the connection to the pool. `Discard` destroys it, along with any session lock it
  still holds.
- `db.Querier` is the raw-SQL methods plus `Driver()`; `db.Driverer` is `Driver()` alone.
  `*db.DB`, `*db.Tx` and `*db.Conn` implement both. Read functions take a `db.Querier`, so
  they work on the pool or inside a transaction. Transactional writes take `*db.Tx`, so a
  caller can't run them on the pool and lose atomicity.

### Building an ent client

A client is a struct allocation with no I/O. Build one per call from the handle at hand, and
never store, cache or `Close` it:

```go
// in the service's internal/entdb, over its own generated package
func Client(q db.Driverer) *ent.Client { return ent.NewClient(ent.Driver(q.Driver())) }

// inside the request's transaction, next to the kit's helpers
err := c.Transact(func(tx *db.Tx) (*service.Response, error) {
	id := ids.NewV7()
	if err := entdb.Client(tx).Widget.Create().SetID(id) /* ... */ .Exec(c.Ctx); err != nil {
		return nil, err
	}
	if err := quota.Reserve(c.Ctx, tx, counter, 1, limit); err != nil {
		return nil, err
	}
	// ...
})

// a read on the pool
w, err := entdb.Client(s.DB).Widget.Query().Where(widget.ID(id)).Only(ctx)

// a transaction the service opens itself
err := s.DB.RunTx(ctx, nil, func(tx *db.Tx) error { /* entdb.Client(tx)..., tx.ExecContext(...) */ })
```

The kit's own client is `internal/entdb.Client` over the generated `internal/ent`. It is
internal to the kit (vendored as `internal/kit/internal/...`), so a service can't import it.
The service writes kit tables only through kit functions, and may still read them with raw
SQL (reconciliation, reports).

A service that models its own tables has one generation root:

- `internal/ent/schema/*.go`: one schema per table, mirroring its DDL;
- `internal/ent/generate.go`:
  `//go:generate go tool ent generate --feature sql/upsert,sql/lock,sql/modifier,sql/execquery ./schema`;
- the generated `internal/ent`, which is committed;
- `internal/entdb`: `Client`, `Tables` (the modelled table names, from the generated
  `migrate.Tables`), `RawTables`, `AllowedDrift` and the drift test.

A service schema never declares a kit table.

### Kit API v2

Every kit function that touches the database takes a `db` handle:

| Package | API |
| --- | --- |
| `service` | `Options.DB *db.DB`; `(*Call).Transact(func(tx *db.Tx) (*Response, error))`; `TransactPrepared(prepare, func(tx *db.Tx) (*Response, error))`; `Options.ReplayOperation func(ctx, db.Querier, organizationID, operationID)`; `PurgeIdempotencyKeys(ctx, db.Querier, age)`; `DeleteIdempotencyKeys(ctx, *db.Tx, organizationID)`; `IdempotencyKeyExists(ctx, db.Querier, organizationID, key)`. A database that can't be reached (`db.Unavailable`) is 503 |
| `trails` | `Insert(ctx, *db.Tx, e)`, `InsertAlone(ctx, *db.DB, e)`, `NewRelay(*db.DB, auditAPIURL, internalToken, logger)` |
| `outbox` | `Relay{DB *db.DB, ...}`, `CountsOf(ctx, *db.DB, table)`, `Requeue(ctx, *db.DB, table, rowIDs)` |
| `metering` | `(*Producer).Insert(ctx, *db.Tx, e)`, `MarkClosurePending(ctx, *db.Tx, org, completeThrough)`, `NewRelay(*db.DB, ...)`, `Watermarks{DB}`, `Closures{DB}`, `ControlTotals(ctx, db.Querier, hour, meters)`, `ClosureOf(ctx, db.Querier, organizationID)`, and the predicate `OrganizationNotClosed(col)` (as `predicate.X(metering.OrganizationNotClosed(x.FieldOrganizationID))`) |
| `operations` | `Create(ctx, *db.Tx, n)`, `Abandon(ctx, *db.Tx, targetID, cause)`, `Get`/`Active`/`InProgress`/`Purge(ctx, db.Querier, ...)`, `Handler.Succeed(ctx, *db.Tx, op)` and `Undo(ctx, *db.Tx, op, cause)`, `Worker{DB *db.DB}` |
| `quota` | `Reserve(ctx, *db.Tx, c, amount, limit)`, `Release(ctx, *db.Tx, ...)`, `Used`/`UsageOf(ctx, db.Querier, ...)`, `DeleteOrganization(ctx, *db.Tx, organizationID)` |
| `credential` | `Store{DB *db.DB}`, with `Verify`, `Issue`, `Revoke` and `List` |
| `testdb` | `DB{Name, URL, Pool, DB}`: `Pool` for raw fixtures and assertions, and `DB` (the same pool, 16 connections) for kit and service code |

`db` also has the helpers that replace pgx idioms:

- `IsNoRows(err)`, which matches `sql.ErrNoRows` and `pgx.ErrNoRows`;
- `PgError(err)`, which works through ent's `ConstraintError` and `database/sql`;
- `IsUnique(err, constraint)`, a 23505, optionally on that constraint;
- `Unavailable(err)`;
- `Inserted(err)`, described under Writing queries;
- `Collect[T](rows, err, scan)` and `Strings(rows, err)`, which never return a nil slice;
- `RowsAffected(res, err)`;
- `QuoteIdent(name)`, for constant names only;
- `TextArray`, for `text[]`;
- the `PGText`/`PGUUID`/`PGJSONB`/`PGTextArray`/`PGInteger`/`PGTimestamptz` schema types.

### Writing queries

A statement moves from SQL to an ent builder only when nothing observable changes. That
covers the rows, the affected-count or no-row signal, the SQLSTATE and constraint name, the
lock clause, which clock supplies the time (`now()` stays in the database), the response and
ETag bytes, and the number of round trips on hot paths. Otherwise the SQL text stays exactly
as it was. With ent:

- **Placeholders.** `sql.Expr`/`sql.ExprP` don't number `?` for Postgres. Pass them a bare
  expression (`sql.ExprP("expires_at > now()")`). With arguments, use
  `sql.P(func(b *sql.Builder){ ...; b.Arg(v) })` or the `db` predicates. Never hard-code `$1`.
- **Predicates.** `predicate.X(db.LowerEQ(x.FieldName, v))` is `lower(col) = lower($n)`, the
  form the case-insensitive unique indexes use. ent's `EqualFold`/`ContainsFold` render
  `ILIKE` and are refused. `db.DistinctFrom`/`db.NotDistinctFrom` are the NULL-safe
  comparisons. A hand-written predicate qualifies its column with `s.C(col)`.
- **Updates.** Prefer `Update().Where(x.ID(id)).Save(ctx)`. It is one statement with no
  internal transaction, and it returns the affected count, which serves as the fence.
  `UpdateOne` re-reads the row, and its NotFound inside a `db.Tx` poisons the transaction.
  Use it only when the entity is needed and a missing row is fatal anyway.
- **Upserts.** Read the result of a single-row `OnConflict(...).DoNothing()`, or of a
  conditional `DO UPDATE ... WHERE`, through `.ID(ctx)` and `db.Inserted(err)`. A skipped row
  gives `false` (ent's `sql.ErrNoRows`). What `true` means depends on the clause:
  - for `DoNothing`, `true` means the row was inserted;
  - for a conditional `DO UPDATE ... WHERE`, `true` means the row was written, either inserted
    or updated (the old affected count of 1), and `false` means the `WHERE` skipped it. Postgres
    returns the id for an update too, so this can't tell an insert from an update. A caller
    that needs that distinction stays raw (for example `RETURNING (xmax = 0)`).

  Never call `.Exec(ctx)` on them: it reports the skipped row as an `sql.ErrNoRows` error.
- **Empty lists.** An empty list encodes as `[]`, never `null`. `db.Collect`/`db.Strings` never
  return nil, and every ent-to-view mapper starts from `make([]T, 0, len(rows))`.
- **One row.** Use `First()` where the old query wasn't guaranteed a single row. Use `Only()`
  only when a primary key or unique index guarantees one.
- **Eager loads** carry an explicit order matching the old SQL.
- **Creates** return only the id. When the created entity feeds a response or an ETag, set
  database-defaulted fields in Go only where the old SQL did, or re-read the row. Don't change
  which clock produced the value.
- **NULLs.** `SetNillableX(nil)` leaves the column out of the statement. Where the old SQL wrote
  NULL, call `ClearX()`. A nil `db.TextArray` is SQL NULL, so set `db.TextArray{}` where `{}`
  is meant.
- **Id lists.** An unbounded id list stays `= ANY($n)` with one array argument (through
  `sql.P`). `IDIn`/`In` are for small, bounded lists.

### What stays raw SQL, and why

ent can't express these with the same observable behaviour, so they stay raw, in a named
function in the owning package. They run on the same handle (`tx.ExecContext`/`QueryContext`/
`QueryRowContext`, the `*db.DB` methods, or `client.ExecContext` from `sql/execquery`):

- **Tables without a single-column key**, listed in `entdb.RawTables`:
  - `idempotency_keys` (`service`);
  - `quota_usage` (`quota`);
  - `usage_control_totals` (`metering`; also `numeric(38,12)`, which is never modelled or read
    as a float).

  `schema_migrations` belongs to the migration runner. `testdb` always allows it, so neither
  the kit nor a service lists it.
- **Session and transaction state:** advisory locks, `SAVEPOINT` (through `Tx.Savepoint`) and
  `SET LOCAL`.
- **Statements whose shape is the point:**
  - single-statement lease claims (`UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED
    LIMIT n) RETURNING`) and the generic outbox relay. Its table is a constant set by
    `trails` or `metering`, never input, and is quoted with `db.QuoteIdent`;
  - `INSERT ... ON CONFLICT` that is multi-row or target-less `DO NOTHING`, whose caller needs
    more than one row or a count above 1, whose `DO UPDATE` returns columns other than the id,
    or whose caller must tell an insert from an update. A single-row `DoNothing`, or a
    conditional `DO UPDATE ... WHERE` whose caller only needs written or skipped, goes through
    `.ID(ctx)` and `db.Inserted` (see Upserts above);
  - `RETURNING` of database-computed or multi-row values;
  - writable CTEs, `UNION`, `INSERT ... SELECT`/`unnest`, `UPDATE ... FROM`, `FULL JOIN`,
    `FILTER` aggregates and `sum()::text`.
- **Hot single-round-trip reads**, and projections that feed ETags or formatted timestamps.
- **Migrations, grants and catalog probes, test fixtures and fault injection.**

### Zero DDL

The SQL migrations are the schema and `db.Migrate` applies them, as before. ent adds no
migration, column or surrogate id. Its schemas mirror the DDL:

- `field.String` with `SchemaType(db.PGText)` or `db.PGUUID`;
- ids stay Go strings, set by the caller (`ids.NewV7()`), with no ent default;
- jsonb is `field.Bytes(...).GoType(json.RawMessage{})`, never `field.JSON`;
- `text[]` is `field.Other(..., db.TextArray{})`;
- a database default is `entsql.DefaultExpr("<the DDL's expression>")`, never
  `Default(time.Now)`;
- every index has its real name;
- CHECKs and foreign keys are not modelled, and the database enforces them.

ent is never a migration engine. `Schema.Create`, `Schema.Diff`, the generated `migrate`
package's `Create` and `NewSchema`, ent's `schema.NewMigrate`/`NewAtlas`, the generated `enttest`
package (its `Open` runs `Schema.Create`), Atlas `migrate apply`, versioned migrations and
entcache are refused everywhere, tests included, and `Schema.WriteTo` only runs in the drift
test. Reading `migrate.Tables` is fine.

### The drift test

Each generation root has one drift test, `internal/entdb/drift_test.go`. The kit's is
[`internal/entdb/drift_test.go`](internal/entdb/drift_test.go):

```go
func TestEntSchemaMatchesMigrations(t *testing.T) {
	d := testdb.New(t /*, the service's migration sets */)
	testdb.AssertEntSchema(t, entdb.Client(d.DB).Schema.WriteTo, entdb.AllowedDrift...)
	testdb.AssertTablesCovered(t, d, entdb.Tables(), entdb.RawTables)
	testdb.AssertAllowlistLive(t, d, entdb.AllowedDrift...)
}
```

- `AssertEntSchema` has ent render what it would change to make the migrated database match
  the ent schema, and fails unless that is nothing. Dropped columns and indexes count.
  Foreign keys and CHECKs are ignored, and so are the names in `AllowedDrift` (index or
  constraint names, or `table.column`, each with a comment saying why). The kit allows one
  name: `usage_outbox_pending_org_idx`, an expression index.
- `AssertTablesCovered` fails on a migrated table that is neither modelled nor raw.
  `testdb.KitTables()` is always allowed, so a service lists only its own tables.
- `AssertAllowlistLive` fails on an allowance that no longer names anything.

When the drift test fails, fix the ent schema, never the database.

### Regenerating

The kit's ent code is regenerated only by [`scripts/generate.sh`](scripts/generate.sh). It
generates `internal/ent` from `internal/ent/schema`, and the contract tests' second client
`internal/testent`, both with exactly `--feature sql/upsert,sql/lock,sql/modifier,sql/execquery`.
The generator is pinned as a Go tool in `go.mod` (`go tool ent`). There is no `go:generate`
under `internal/ent`. `sync-kit` vendors the generated code without `scripts/`, so services
never regenerate the kit. It pins ent, Atlas and pgx to the kit's versions, which are recorded
as `ent:` and `pgx:` in `KIT_VERSION`. A service regenerates its own root with
`go generate ./...`.

CI checks the data layer (`.github/workflows/go.yml`):

- the committed code is what `scripts/generate.sh` (in a service, `go generate ./...`) produces,
  untracked files included;
- [`scripts/check-data-layer.sh`](scripts/check-data-layer.sh) passes. It refuses a
  transaction opened by hand outside `db/`, and pgx used directly outside `db/`, `testdb/`,
  `_test.go` files and the paths in `scripts/data-layer.allow`. It also refuses an ent
  generate with any other feature set, `EqualFold`/`ContainsFold`, `sql.Expr`/`ExprP` with
  arguments, ent as a migration engine, and a new or changed migration that drops or retypes
  without a `-- reviewed: destructive` line.

## Tests

Every file in [`contracts/vectors`](../../contracts/vectors/) is a Go test here. The relay,
operation worker, quota and service tests use a real Postgres:

```sh
scripts/test.sh                 # a throwaway Postgres on apple/container, then go test -race ./...
DATABASE_TEST_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres go test -race ./...
```

Without `DATABASE_TEST_URL` those tests skip (and fail in CI, where it's always set). The same
goes for `REDIS_TEST_URL` (`redis`, `ratelimit`) and `RABBITMQ_TEST_URL` (`rabbitmq`:
`amqp://guest:guest@127.0.0.1:5672/%2f`, which `scripts/test.sh` starts on apple/container and
CI runs as a service container); `rabbitmq`'s tests cut the broker's connections with a TCP proxy
of their own. A product vendoring the kit runs them only when its CI sets `RABBITMQ_TEST_URL`;
without it they skip even in CI, so products that don't use RabbitMQ needn't run a broker.

## Private streaming endpoints

Use `Service.HandlePrivate(method, path, handler)` for sensitive uploads. It keeps the normal
credential, Origin and path-ID checks, but does not read or buffer `Call.R.Body`; `Call.Body()`
is empty. The handler must authorize first, then enforce its own body size, parsing and time
limits. Declare the endpoint and its Trails event in the usual manifest and OpenAPI overlay.

Private routes record an empty `requestParameters` object unless the handler calls
`Call.SetRequestParameters(map[string]any{...})` with safe operational metadata. This replaces
all automatic path, query and body capture; catalog and secret-key redaction still apply.
Never put document contents, filenames or extracted text in this metadata, `Additional`,
public errors, resource names or response identifier fields. Private routes suppress panic
values and internal error causes in service logs; public error codes/messages remain visible.

Use `Call.Transact` for successful writes so audit persistence precedes the response. Private
failures also persist their attributable Trails event before responding, with a detached
five-second timeout; a failed audit insert returns `503 unavailable`. Failed commits attempt a
separate failure event. Declare `idempotencyException: "computation"` on synchronous private caller POSTs; retries
recompute and may be metered again. Routes declaring persisted idempotency are rejected: the normal JSON
fingerprint and stored response mechanism is unsuitable for private streaming data. Existing
`Handle` routes retain their normal JSON buffering and recording behavior.

`http.NewResponseController(call.W)` can set transport read deadlines through the service's
access-log wrapper. An exact configured `X-Internal-Token` suppresses duplicate Trails events
on public endpoints, as the contract requires; it never bypasses customer authentication,
Origin validation or IAM checks. Missing and invalid markers do not suppress events.

## Persisted streaming resources

`Service.HandleStream(method, path, handler)` retains all `HandlePrivate` privacy and
credential checks while allowing a manifest endpoint with `idempotencyKey: true`.
Authorize first, stream to temporary object storage while hashing, then call
`Call.SetIdempotencyInput(value)` with the content SHA-256 and every relevant metadata
field (size, MIME type and display name). Only the canonical fingerprint is retained;
this input is never automatically copied into Trails. Never pass document bytes or
extracted text. Set the input before `Transact`/`TransactPrepared`, even when the caller
omits the key. Transactions refuse an unset fingerprint on idempotent stream routes.

Use `Transact` to create the resource with a safe metadata response. Existing key locking,
replay and conflicts apply after the upload: repeated uploads may create redundant temporary
objects, which the product must delete on replay, conflict or failure. Perform upload and
cleanup outside database transactions. Stored replay responses must not contain documents,
extracted text or secrets. Keep filenames out of Trails request metadata and response
identifier fields (`name` in particular); use a distinct display-name field in API metadata.

For private downloads, register `HandlePrivate` and call
`Call.Stream(status, headers, func(io.Writer) error)`. It persists attributable success audit
before headers or bytes, fails closed with 503 if audit persistence fails, and aborts the
transport on a writer error or panic without logging the cause. A success audit denotes
permission to start the download, not proof that the client received every byte. Stream
is not a transaction mechanism; database mutations still use `Transact`.

Private route audit responses capture only string `id`, `slug` and `resourceName` fields
(at the top level or one wrapper deep). Display names, filenames and catalog response
extras are omitted even when the customer response includes them. Normal routes retain
their established response-identifier behavior.
