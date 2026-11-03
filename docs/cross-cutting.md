# Cross-cutting change checklists

These checklists summarize the shared implementation boundaries; they do not replace service-specific authorization and tests.

## When you add an HTTP route

- Register it with the appropriate authentication, scope or session/capability wrapper. Cookie-authenticated mutations also need CSRF protection: the session's token in the `X-CSRF-Token` header (or a `csrf_token` form field for HTML form posts) plus a same-origin request (`Sec-Fetch-Site` same-origin/none, or a matching `Origin` when it is absent).
- For dashboard capabilities, update the mapping in `internal/httpx/dashboard_caps.go` and the corresponding matrix row in `internal/httpx/dashboard_matrix_test.go`.
- Update `internal/httpx/route_auth_test.go` only when the route is genuinely public; give its allow-list entry a reason. A new private route must have a recognized wrapper at registration.

## When you add a model call

- Use the shared executor in `internal/inference`; pass the validated principal, operation, input references and bounded request. Do not call a provider directly from a handler, worker or helper.
- Preserve endpoint and input policy, admission, reservation and settlement, including unknown outcomes.

## When you add a table or migration

- Scope durable resources and keys by tenant, including foreign keys and uniqueness where applicable.
- Add a new numbered SQL file in `internal/store/postgres/migrations/`; never edit an applied migration.

## When you add a durable job

- Persist immutable inputs, policy and an idempotency identity bound to tenant, operation and submitter/delegation/billing context; conflicting replays must fail.
- Use the lease and fencing mechanisms in `internal/jobs` for progress and publication. An expired worker must not publish.
- Represent unknown upstream outcomes honestly; cancellation is not proof that a dispatched side effect did not occur.

## When you change connector content or permissions

- In `internal/connectors`, finalize durable bytes and enqueue ingestion before advancing a content checkpoint.
- Preserve content, grant and membership completeness and snapshot semantics; incomplete snapshots must not remove unseen grants or become authoritative.
- Skip and count unmapped principals rather than granting them access; do not infer identity from email alone. Recheck source authorization at serving and publication boundaries.

## When you add an outbound HTTP client

- Bind credentials and redirects to the approved tenant, purpose and origin. Resolve, validate and dial the approved address rather than allowing DNS rebinding; bound time and response sizes. See `internal/llm/provider.go` and `internal/mcp/streamable.go`.

## When you add a configuration knob

- Define and validate it in `internal/config/config.go`, document it in `.env.example`, and wire it in `cmd/enclava/main.go`.

## When you add a search filter

- Apply tenant, generation and access candidate filters to every relevant index branch, then recheck authoritative PostgreSQL/source authorization before returning results or expanding context. See `internal/rag` and `internal/source/authorize.go`.

## Acting user and accounting

A principal carries the verified tenant, actor, credential and billing subject; source ACLs are not conferred by a connector credential. The precise inputs still determine which source checks apply. See `internal/auth/service.go`, `internal/jobs/service.go`, `internal/chatbot/runs.go`, `internal/connectors/service.go`, and `internal/rag/ingest.go`.

| Surface | Acting principal | Source ACLs | Billing subject |
|---|---|---|---|
| Dashboard session | Verified session user | Session user's applicable source grants | Session user |
| API key | Verified key owner, user or service account | Key owner's applicable source grants | User or service-account owner; credential fallback for a nonconforming ownerless record |
| Service-account key | Verified service-account actor; delegation can further constrain authority | Service account's applicable grants, subject to delegation | Service account, or delegating user for delegated authority |
| Assistant run worker | Frozen submitter refreshed against current authority | Caller source ACLs; caller and assistant owner must both access selected collections | Submitter's billing subject |
| Extraction job worker | Frozen submitting principal | Owned uploaded input; source restrictions apply to referenced inputs | Submitter's billing subject |
| Connector sync worker | Frozen submitter; scheduled sync uses source creator tenant and actor | Provider observations maintain source grants; sync credentials do not grant readers access | Frozen subject if supplied; scheduled creator principal has no explicit billing subject |
| Ingest/embed worker | Frozen initiating principal, including connector sync submitter | Object revision and publication checks apply; readers are separately source-authorized | Frozen billing subject, if supplied |
| MCP tool call | Run caller, with assistant-owner scope checks | Input references are authorized for the caller; assistant owner must also satisfy tool scopes | Run caller's billing subject for model calls; MCP tool calls do not themselves create model usage |
