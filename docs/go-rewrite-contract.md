# Enclava Go Rewrite Contract

This document freezes the product/API contracts that the Go rewrite must preserve
while intentionally excluding the Python plugin subsystem.

## Scope Decisions

The Go application is the primary implementation target. It preserves the
operator and public API workflows that matter for Enclava as a confidential LLM
appliance:

- OpenAI-compatible model, chat completion, streaming chat completion, and
  embedding endpoints.
- Confidential provider routing through OpenAI-compatible upstreams, Tinfoil,
  and later RedPill-style/private confidential providers.
- API key authentication and authorization with scopes, endpoint restrictions,
  model restrictions, chatbot restrictions, agent restrictions, source IP
  restrictions, expiration, activation, regeneration, rate limits, soft delete,
  and bootstrap provisioning.
- Usage recording, budget checks, pricing snapshots, and billing/pricing audit
  data.
- Extract document processing, templates, settings, job history, and usage
  attribution.
- RAG collections/documents/chunks backed by Qdrant.
- Chatbot and prompt-template configuration with conversation persistence.
- First-party connectors for GitHub, Slack, Notion, and Linear.
- A Go-rendered operational dashboard using server-rendered HTML and HTMX.

The rewrite drops plugin marketplace, upload, install, sandbox, registry,
dynamic plugin execution, plugin gateway, plugin cron, and Zammad plugin/module
compatibility.

## Authentication

Public API routes use API key auth. Clients may pass the key in either:

- `Authorization: Bearer <key>`
- `X-API-Key: <key>`

Auth failures return an OpenAI-compatible JSON envelope:

```json
{
  "error": {
    "message": "missing API key",
    "type": "authentication_error",
    "code": "invalid_api_key"
  }
}
```

Admin dashboard routes use password-backed dashboard sessions when
`ADMIN_EMAIL` and `ADMIN_PASSWORD` are configured. If dashboard password auth is
not configured, dashboard partials fall back to `platform.admin` API key auth.
API key source-IP allowlists accept exact IP addresses, CIDR ranges, and `*`.
When `TRUST_PROXY_HEADERS=true`, the leftmost valid `X-Forwarded-For` address is
used before `X-Real-IP` and `RemoteAddr`.

## Scopes

The kept scope set is:

- `models.list`
- `chat.completions`
- `embeddings.create`
- `responses`
- `openai.proxy`
- `extract`
- `extract.manage`
- `rag`
- `rag.manage`
- `chatbot`
- `chatbot.manage`
- `connectors.manage`
- `pricing.manage`
- `platform.admin`

Existing dashboard-created operator keys may also include legacy mini scopes
such as `api_keys.manage`, `analytics.read`, and `budgets.manage`; these are
compatibility aliases for the current dashboard and should be consolidated as
the rewrite matures.

Current Go implementation includes provider registry CRUD for non-secret
provider metadata, provider/model pricing list/create APIs with pricing audit
snapshots, billing-audit list APIs with API-key and subject filters, and usage
cost calculation from effective pricing snapshots.

## OpenAI-Compatible API

The public OpenAI-compatible surface is mounted only under `/v1` and covers
the selected core: `GET /v1/models`, `GET /v1/models/{model_id}`,
`POST /v1/chat/completions` (unary + SSE stream) and `POST /v1/embeddings`.
Every dispatch runs through the shared inference executor — policy, approved
model-alias resolution, reservation and usage settlement — and model listings
expose only tenant-approved aliases, never upstream ids or other tenants'
endpoints. Unsupported products, unselected `/v1` paths and the legacy
`/api/v1` inference aliases return explicit errors; there is no catch-all
upstream passthrough.

### `GET /api/v1/models`

Requires `models.list`.

Returns an OpenAI-compatible models list:

```json
{
  "object": "list",
  "data": [
    {
      "id": "model-id",
      "object": "model",
      "owned_by": "provider"
    }
  ]
}
```

If an API key has allowed models, the list is filtered to those model IDs.

### `GET /api/v1/models/{model_id}`

Requires `models.list`.

Returns the upstream model object. The request is rejected with `403` if the
API key is restricted and the requested model is not allowed.

### `POST /api/v1/chat/completions`

Requires `chat.completions`.

Accepts OpenAI-compatible chat completion JSON. The Go proxy must preserve
common fields including:

- `model`
- `messages`
- `temperature`
- `max_tokens`
- `top_p`
- `frequency_penalty`
- `presence_penalty`
- `stop`
- `stream`
- `response_format`
- `tools`
- `tool_choice`

Non-streaming calls return the upstream JSON response. Streaming calls return
SSE with `Content-Type: text/event-stream`, no normal write timeout, and
best-effort usage parsing from final usage chunks when the provider emits them.

Budget checks run before the upstream provider call. Usage is recorded after
successful calls and after streams complete when usage/status evidence exists.

### `POST /api/v1/embeddings`

Requires `embeddings.create`.

Accepts OpenAI-compatible embeddings JSON and returns the upstream JSON
response. Budget checks run before the upstream provider call. Usage is recorded
after successful calls.

### Responses API

Requires `responses`.

The Go app keeps the documented OpenAI Responses API family as a passthrough to
OpenAI-compatible providers that support it:

- `POST /api/v1/responses`
- `GET /api/v1/responses/{response_id}`
- `DELETE /api/v1/responses/{response_id}`
- `GET /api/v1/responses/{response_id}/input_items`
- `POST /api/v1/responses/{response_id}/cancel`
- `POST /api/v1/responses/input_tokens`
- `POST /api/v1/responses/compact`

Create calls preserve request bodies, support SSE streaming when `stream:true`
or `?stream=true` is used, apply endpoint/scope/model/IP/budget restrictions,
and record usage from `input_tokens`, `output_tokens`, and `total_tokens` when
providers emit usage data.

Documented Responses routes use the typed local handler above. Newer or
provider-specific `/responses/...` subroutes fall back to generic upstream
passthrough when the API key also has `openai.proxy`, so the upstream provider
remains authoritative for endpoint-specific validation and support.

## Provider And Attestation API

### `GET /api/v1/providers`

Requires `pricing.manage`.

Returns managed provider definitions plus current runtime provider readiness
metadata. Provider records store non-secret config and credential references;
the active runtime provider remains controlled by environment/config.

### `POST /api/v1/providers`

Requires `pricing.manage`.

Creates a provider registry record for `tinfoil`, `privatemode`,
`openai_compatible`, or `redpill`. Secret-looking config keys are rejected; use
`credential_ref` for external secret references.

### `GET|PUT|DELETE /api/v1/providers/{provider_id}`

Requires `pricing.manage`.

Reads, updates, or deletes a provider registry record. Updating this registry
does not hot-swap the process-wide inference client.

### `GET /api/v1/attestation/status`

Requires `models.list`.

Returns provider attestation status. Non-attested providers return
`{"enabled":false,"provider":"non-attested"}`.

### `POST /api/v1/attestation/verify`

Requires `models.list`.

Triggers provider attestation verification when supported. Verification
failures return a safe OpenAI-compatible server error without leaking provider
credentials.

## Extract API

### `POST /api/v1/extract/process`

Requires `extract`.

Accepts multipart document uploads. Kept Python-compatible fields:

- `file`
- `template_id`
- `template`
- `context`
- `buyer_context`

Current Go implementation accepts `template_id`, `template`, or
`extract_template_id` as compatibility aliases.

The response contains job/result metadata, extracted JSON data, token usage, and
validation diagnostics. Usage is attributed to the API key.

### `GET /api/v1/extract/jobs`

Requires `extract`.

Returns paginated jobs for the authenticated API key.

### `GET /api/v1/extract/jobs/{job_id}`

Requires `extract`.

Returns job detail and result data for a job owned by the authenticated API key.

### Template, Settings, Models, Health

The Extract API keeps:

- `GET /api/v1/extract/templates`
- `POST /api/v1/extract/templates` with `extract.manage`
- `GET /api/v1/extract/templates/{template_id}`
- `PUT /api/v1/extract/templates/{template_id}` with `extract.manage`
- `DELETE /api/v1/extract/templates/{template_id}` with `extract.manage`
- `POST /api/v1/extract/templates/reset-defaults` with `extract.manage`
- `GET /api/v1/extract/models`
- `GET /api/v1/extract/settings`
- `PUT /api/v1/extract/settings` with `extract.manage`
- `GET /api/v1/extract/health`

## Admin API

The platform admin API keeps:

- `GET /api/v1/admin/api-keys`
- `POST /api/v1/admin/api-keys`
- `GET /api/v1/admin/api-keys/{id}`
- `PUT /api/v1/admin/api-keys/{id}`
- `DELETE /api/v1/admin/api-keys/{id}`
- `POST /api/v1/admin/api-keys/{id}/regenerate`
- `POST /api/v1/admin/api-keys/{id}/restore`
- `GET /api/v1/admin/usage/summary`

These require `platform.admin`.

## Admin Dashboard Contract

The Go dashboard is the long-term replacement for the coupled Next.js frontend.
It must remain operational and dense rather than marketing-oriented. Current
kept pages/partials:

- `/dashboard`
- `/dashboard/login`
- `/dashboard/logout`
- `/dashboard/keys`
- `/dashboard/budgets`
- `/dashboard/analytics`
- `/dashboard/extract`
- `/dashboard/providers`
- `/dashboard/pricing`
- `/dashboard/billing-audit`
- `/dashboard/rag`
- `/dashboard/chatbots`
- `/dashboard/prompt-templates`
- `/dashboard/connectors`
- `/dashboard/partials/stats`
- `/dashboard/partials/keys`
- `/dashboard/partials/usage`
- `/dashboard/partials/budgets`
- `/dashboard/partials/extract`
- `/dashboard/partials/extract/*`
- `/dashboard/partials/providers`
- `/dashboard/partials/pricing`
- `/dashboard/partials/billing-audit`
- `/dashboard/partials/rag`
- `/dashboard/partials/chatbots`
- `/dashboard/partials/prompt-templates`
- `/dashboard/partials/connectors`
- `/dashboard/static/*`

These resource pages include operational create/update workflows for API keys,
budgets, Extract, RAG collection/document upload, chatbots, prompt templates,
connectors, pricing, and provider metadata where the Go API supports mutation.

## RAG Contract

RAG is a first-party Go service backed by Qdrant. It must support collection
CRUD, ACLs, document upload/download/delete/reprocess, extraction for common
document types, chunking with overlap, provider-generated embeddings, Qdrant
vector indexing/search, and connector-ingested documents.

Current Go implementation includes collection CRUD, owner/public/API-key ACL
checks, multipart document upload, original document download, document delete,
document reprocessing, text extraction for text/Markdown/CSV/JSON/JSONL/HTML,
DOCX, and best-effort PDF, chunking with overlap, embeddings through
`/embeddings`, Qdrant indexing, Qdrant cleanup, and vector search.

Connector fixture/static-document sync and API-backed GitHub, Slack, Notion,
and Linear fetchers can ingest into RAG collections.

## Chatbot Contract

Chatbots are first-party Go resources backed by prompt templates, provider/model
preferences, optional RAG collections, external OpenAI-compatible endpoints, and
conversation/message persistence.

Current Go implementation includes prompt template CRUD, default prompt template
seeding, chatbot config CRUD, enabled/disabled enforcement, model restrictions,
external model listing, non-streaming and SSE streaming external
chat-completions, optional bound RAG retrieval injected into rendered prompts,
conversation and message persistence, and usage records attributed by chatbot
ID.

Per-chatbot API-key allowlists are enforced for external chatbot routes. JSONL
migration imports for prompt templates, chatbots, conversations, RAG metadata,
connectors, usage, and API-key hashes are available through
`enclava migrate-import`.

## Connector Contract

Connectors are first-party sync sources for GitHub, Slack, Notion, and Linear.
They must support source CRUD, local secret strategy or encrypted credentials,
manual/scheduled sync, checkpoints, fixture tests, mocked API tests, and RAG
document updates.

Current Go implementation includes source CRUD, source type validation for
GitHub/Slack/Notion/Linear, manual sync job creation, sync job history/detail,
fixture/static-document sync into target RAG collections, API-backed fetchers
with mocked tests for GitHub file contents, Slack channel history, Notion page
blocks, and Linear issues, and incremental checkpoint behavior. Sync stores
per-document content hashes to skip unchanged documents on later runs; Slack API
sync also stores the latest message timestamp and sends it as the next
`conversations.history` `oldest` cursor. Scheduled sync is opt-in with
process-level `CONNECTOR_SYNC_INTERVAL` and per-source
`config.sync_interval_seconds`. Current real API tokens are read from the
environment variable named by `credential_ref`; encrypted credential storage
remains replaceable by this documented local secret strategy.

## Compatibility Test Contract

The launch contract suite includes:

- `go test ./...`
- `QDRANT_INTEGRATION_URL=http://127.0.0.1:6333 go test ./...`
- `npx playwright test`
- `OPENAI_CONTRACT_BASE_URL=<server>/v1 OPENAI_CONTRACT_API_KEY=<key> OPENAI_CONTRACT_CHAT_MODEL=<alias> OPENAI_CONTRACT_EMBEDDING_MODEL=<alias> go test ./tests/contract`

The SDK contract uses the official OpenAI Go client with a custom base URL and
exercises model listing, unary and streaming chat completions, embeddings and
denial of unselected products.

## Health And Operations

The app keeps:

- `GET /health`
- `GET /livez`
- `GET /readyz`

`/readyz` reflects provider readiness. `/health` is unauthenticated and must
redact sensitive attestation details.

The HTTP stack must set request IDs, structured access logs without sensitive
headers or query strings, panic recovery, CORS when configured, secure response
headers, and body/upload limits.
