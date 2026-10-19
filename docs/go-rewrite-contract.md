# Enclava Go Application Contract

This documents the selected Go application as composed today. It is an implementation reference, not a deployment, security-certification, or compliance claim. The Python plugin subsystem and legacy SQLite/import runtime are not part of this application.

## Runtime and data ownership

`cmd/enclava` composes one application: PostgreSQL stores tenant-scoped identity, source metadata, jobs, reservations and usage; the configured durable filesystem stores artifacts; configured inference endpoints are reached through the shared `internal/inference` executor. SQLite metadata storage and SQLite migrations have been removed. A working deployment needs an operator-provided PostgreSQL DSN and durable data directory; RAG/search additionally needs Qdrant. External OIDC and upstream inference require configured, reachable services and valid credentials.

Local users, roles, credentials, API keys and sessions are tenant-owned PostgreSQL records. Local account creation is administrator-controlled through the bootstrap/identity flows; there is no public signup. OIDC identities are bound by issuer and subject, and account linking requires an authenticated explicit flow. Session revocation is checked server-side. Public API credentials are verified against stored identity rows; request-supplied tenant/actor fields do not establish authority. Cookie-authenticated mutations use CSRF protection.

## Selected public inference API

Only these public inference paths are mounted; there is no generic upstream catch-all or `/api/v1` inference alias:

- `GET /v1/models` and `GET /v1/models/{model_id}`
- `POST /v1/chat/completions` (unary and streaming where supported by the endpoint)
- `POST /v1/embeddings`
- Responses lifecycle routes under `/v1/responses`: create/stream, retrieve, delete, cancel, input items, input-token calculation and compact

All model calls use the shared executor and its authenticated principal, endpoint/model authorization, admission and accounting. Responses references are bound to their local tenant/owner/endpoint context. Unsupported operations, model capabilities and unsafe resource shapes are rejected; the service does not fabricate provider responses. Provider-specific behavior still requires validation against the configured upstream. `openai.proxy` is not a general-purpose passthrough permission and does not mount additional products.

The app also mounts tenant-scoped provider/endpoint metadata and processing-restriction controls, RAG, prompt-template, chatbot, connector, and extraction routes. These are described in the route matrix; the presence of a route or a deterministic test does not demonstrate real connector permissions, retrieval quality, external identity, or upstream interoperability.

## Identity, ownership and authorization

Every operation uses the verified tenant and actor from the authenticated principal. Durable resources are owner- or audience-scoped, and authorization is enforced by the owning service/repository path. Unknown and forbidden resource identifiers are intended to have the same public outcome. Provider credentials are resolved from operator-configured secret bindings for an approved tenant, purpose and origin; public configuration does not accept secret values or turn a user-supplied environment-variable name into a secret lookup.

Local login, OIDC provider setup, and upstream endpoint credentials are external operator prerequisites. OIDC login/linking against a real identity provider was not exercised as part of this source-only cutover. The Go tests cover deterministic local flows and protocol cases, not a claim of live IdP availability.

## Pricing, reservations and capacity

Money is stored as signed integer currency-micros, with an explicit currency. A configured rate is `rate_micros` per `unit_quantity` for a named billable unit. The selected accounting units are input, output and cached input; cached input is a subset settled separately from uncached input. Price snapshots preserve every selected rational rate, currency and rounding rule (ceiling); no implicit free unit or foreign-exchange conversion is assumed. A priced attempt reserves checked per-unit quantities and a monetary ceiling before dispatch. Unknown upstream outcomes retain their hold; settlement uses the immutable reservation snapshot and raw reported usage.

PostgreSQL reservations enforce applicable tenant and billing-subject budget limits across processes. HTTP/upstream admission limits (active work, queue depth/bytes and stream bounds) are process-local configuration and are not a distributed concurrency guarantee. Cancellation/disconnect releases local work resources and propagates cancellation; it does not prove that an already-dispatched upstream operation had no side effect. Do not infer multi-process capacity bounds from the database accounting limit.

## Capability and assurance boundaries

An endpoint is usable only for its configured protocol, capabilities, origin, credentials and authorization. A configured private endpoint is not automatically confidential; ordinary TLS is not attestation. Attested/confidential assurance is reported only when the selected provider supports and verifies it. Failed required verification denies dispatch. The application makes no guarantee about endpoint availability, provider data handling, deployment hardening, or regulatory compliance.

## Current verification evidence and prerequisites

`go build ./...` succeeds. With the supplied PostgreSQL and Qdrant test services, `go test -count=1 ./...` passes all packages except the external OpenAI SDK contract test, which requires `OPENAI_CONTRACT_BASE_URL`, `OPENAI_CONTRACT_API_KEY`, and chat, embedding, and Responses model names. `ENCLAVA_PROXY_INTEGRATION=1 go test -count=1 ./tests -run TestProxyTwoProcess` passes against its controlled upstream and shared PostgreSQL fixture; this proves the in-repository two-process admission/accounting scenario, not compatibility with an external inference service. The selected app/identity/provider/usage browser scenarios, real OIDC linking, and a real external inference contract need configured live services and must be run in the post-merge application harness. Missing those prerequisites is not a pass. Keep phase-2 document-processing, phase-3 knowledge-quality, and later connector/agent capability claims separate from these core tests.
