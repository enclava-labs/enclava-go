# Enclava Go Application Route Matrix

This matrix describes paths registered by the current Go application, not Python rewrite targets or routes promised for later phases. Methods are checked by handlers; a path registration alone does not imply every method is supported. API permissions below name the middleware scope; resource ownership and tenant authorization are also enforced by the handlers/services. There is no generic OpenAI upstream catch-all.

## Health and identity

| Path | Methods | Access | Current behavior |
| --- | --- | --- | --- |
| `/` | GET | public | Redirects to `/dashboard`. |
| `/health`, `/livez`, `/readyz` | GET | public | Health, process liveness and configured provider readiness. Health output redacts sensitive attestation data. |
| `/dashboard/login`, `/dashboard/logout` | GET/POST | session flow | Local password-backed login and server-side logout. |
| `/auth/oidc/{provider}/start`, `/callback` | GET | configured OIDC flow | OIDC authorization/callback for configured provider; requires real operator IdP configuration. |
| `/auth/oidc/link` | POST | authenticated session | Explicit account-linking flow; requires configured OIDC. |
| `/api/v1/identities/users`, `/api/v1/identities/users/{id}` | handler-checked | `platform.admin` | Tenant-scoped user administration. |
| `/api/v1/identities/roles`, `/api/v1/identities/roles/{id}` | handler-checked | `platform.admin` | Tenant-scoped roles and grants. |
| `/api/v1/identities/service-accounts`, `/api/v1/identities/service-accounts/{id}` | handler-checked | `platform.admin` | Tenant-scoped service accounts and keys. |
| `/api/v1/identities/me`, `/api/v1/identities/me/{action}`, `/api/v1/identities/link` | handler-checked | authenticated identity | Current-principal and explicit identity-link operations; cookie mutations require CSRF. |

## Public inference

Only `/v1` selected operations are mounted. `/api/v1` inference aliases and arbitrary additional OpenAI products are not mounted.

| Path | Methods | Scope | Notes |
| --- | --- | --- | --- |
| `/v1/models`, `/v1/models/{model_id}` | GET | `models.list` | Tenant-authorized model aliases. |
| `/v1/chat/completions` | POST | `chat.completions` | Shared-executor chat; streaming depends on the selected endpoint. |
| `/v1/embeddings` | POST | `embeddings.create` | Shared-executor embedding operation. |
| `/v1/responses` | POST | `responses` | Create, including streaming when supported. |
| `/v1/responses/{response_id}` | GET, DELETE | `responses` | Owner/tenant-bound retrieval and deletion. |
| `/v1/responses/{response_id}/cancel` | POST | `responses` | Cancellation remains available independently of ordinary inference budget. |
| `/v1/responses/{response_id}/input_items` | GET | `responses` | Bound to the locally owned response reference. |
| `/v1/responses/input_tokens`, `/v1/responses/compact` | POST | `responses` | Require the selected endpoint to support the operation. |

## Platform and feature APIs

| Path | Methods | Scope | Notes |
| --- | --- | --- | --- |
| `/api/v1/admin/api-keys`, `/api/v1/admin/api-keys/{id}` | handler-checked | `platform.admin` | API-key list/create, detail/update, revoke/regenerate and related actions. |
| `/api/v1/admin/usage/summary` | GET | `platform.admin` | Usage summary from the billing boundary. |
| `/api/v1/admin/pricing` | handler-checked | `pricing.manage` | Pricing configuration. |
| `/api/v1/admin/billing-audit` | handler-checked | `platform.admin` | Billing audit. |
| `/api/v1/providers`, `/api/v1/providers/{id}` | handler-checked | `pricing.manage` | Tenant-scoped endpoint/provider metadata; never a secret-value API. |
| `/api/v1/processing-restrictions` | handler-checked | `pricing.manage` | Tenant processing policy controls. |
| `/api/v1/rag/collections`, `/api/v1/rag/collections/{id}` | handler-checked | `rag` | Tenant/resource checks apply in addition to the route scope. |
| `/api/v1/rag/documents/{id}` | handler-checked | `rag` | Owned document operations only. |
| `/api/v1/prompt-templates`, `/api/v1/prompt-templates/{id}` | handler-checked | `chatbot` | Template permissions and ownership are checked by handlers. |
| `/api/v1/chatbots`, `/api/v1/chatbots/{id}` | handler-checked | `chatbot` | Chatbot resource management. |
| `/api/v1/connectors/sources`, `/api/v1/connectors/sources/{id}` | handler-checked | `connectors.manage` | Connector configuration; live upstream permission parity requires provider-specific validation. |
| `/api/v1/connectors/sync-jobs`, `/api/v1/connectors/sync-jobs/{id}` | handler-checked | `connectors.manage` | Connector sync-job status. |
| `/api/v1/extract/*` | handler-checked | `extract` / `extract.manage` | Process, jobs, templates, models, settings and health, as registered by the extraction module. |

## Dashboard

| Path family | Access | Notes |
| --- | --- | --- |
| `/dashboard`, `/dashboard/`, `/dashboard/{page}` | dashboard session | Embedded server-rendered pages including identity, keys, budgets, providers, pricing, billing audit, extraction, RAG, chatbots, prompt templates and connectors. |
| `/dashboard/partials/*` | dashboard session | Stats, keys, usage, budgets and feature handlers; cookie-authenticated mutations use CSRF checks. |
| `/dashboard/rag/documents/{id}/download` | dashboard session | Owned document download. |
| `/dashboard/static/*` | public static | Embedded static assets. |

## Not mounted / not claimed

- `/api/v1/models`, `/api/v1/chat/completions`, `/api/v1/embeddings`, and `/api/v1/responses` legacy inference aliases.
- Generic `/api/v1/{openai_route}` passthrough (including files, audio, images, assistants, batches, realtime and other unselected products).
- SQLite-backed API, import command, or migration route.
- A route for a future connector, processing, knowledge, or agent capability solely because its package exists.

A passing package test or route registration does not establish live OIDC, upstream inference compatibility, source permission parity, browser accessibility, or deployment/compliance properties. Those require the configured real service and browser scenarios; see `go-rewrite-contract.md`.
