# Enclava platform API

Everything the dashboard does is available as JSON over `/api/v1` — the
dashboard itself is a client of this API. Authenticate every call with a
personal access token (PAT):

```
Authorization: Bearer enc_…
```

Base URL is your instance origin (for the dev walkthrough:
`https://enclava-ai-dev.tail4d379.ts.net`).

## 1. Mint a PAT

PATs are issued by an administrator session (sign in to the dashboard
first — the cookie authorizes the mint):

```bash
# 1. sign in (session cookie)
curl -c jar.txt https://HOST/dashboard/login \
  -d 'email=admin@example.com&password=…'

# 2. mint a scoped key; secret_key is shown ONCE
curl -b jar.txt https://HOST/api/v1/admin/api-keys \
  -H 'content-type: application/json' \
  -d '{"name":"ci-imports","scopes":["chat.completions","embeddings.create","extract","rag","rag.manage"]}' | jq .secret_key
```

Scope names (mint fails if you ask for one the caller doesn't hold):
`platform.admin models.list chat.completions embeddings.create responses
openai.proxy extract extract.manage rag rag.manage chatbot chatbot.manage
connectors.manage pricing.manage`. Omitting `scopes` mints a key with NO
scopes — pass the list explicitly. Keys can also be rate-limited
(`rate_limit_per_minute`), IP-bound (`allowed_ips`), and expired
(`expires_at`, RFC3339); manage them from Access → API keys or
`GET/DELETE /api/v1/admin/api-keys/{id}`.

## 2. Knowledge collections

```bash
KEY=enc_…
BASE=https://HOST

# create a collection (admin or manager on it)
CID=$(curl -s -H "authorization: Bearer $KEY" -H 'content-type: application/json' \
  $BASE/api/v1/rag/collections -d '{"name":"Support docs"}' | jq -r .id)

# upload a document (txt md html csv xlsx json jsonl pdf docx images epub)
curl -s -H "authorization: Bearer $KEY" \
  -F "file=@invoice.epub;type=application/epub+zip" \
  $BASE/api/v1/rag/collections/$CID/documents | jq .document.id

# poll until status is ready (processing → ready | failed)
curl -s -H "authorization: Bearer $KEY" \
  $BASE/api/v1/rag/collections/$CID/documents | jq '.documents[].status'

# search
curl -s -H "authorization: Bearer $KEY" -H 'content-type: application/json' \
  $BASE/api/v1/rag/search \
  -d "{\"collection_ids\":[\"$CID\"],\"query\":\"invoice total\",\"limit\":5}" | jq '.results[].content'

# optional retrieval assists: query rewrite and section selection use the
# utility model (UTILITY_MODEL); unconfigured → 400 utility_model_not_configured
curl -s -H "authorization: Bearer $KEY" -H 'content-type: application/json' \
  $BASE/api/v1/rag/search \
  -d "{\"collection_ids\":[\"$CID\"],\"query\":\"invoice total\",\"limit\":5,\"rewrite\":true,\"select_sections\":true}" | jq '.results[].content'

# the same flags apply per collection on POST /api/v1/rag/collections/$CID/search
```

Uploads dedupe by content hash against documents the uploader can read within
the collection: a re-upload of the same bytes returns 200 with
`deduplicated: true` and the existing document — no new version, ingest job
or billing. An unreadable match is not disclosed and does not prevent an
upload; the same bytes in another collection also create their own document.
On failure,
`documents[].error_message` carries the
parser's reason (e.g. malformed document, encrypted PDF). Non-fatal parse
and chunk problems are persisted per generation and returned on
`documents[].warnings`: non-UTF-8 input decoded as windows-1252 or UTF-16,
XLSX sheets over the cell cap, CSV/XLSX header rows leading each table
chunk, and `truncated:` markers when a document exceeds the 4,096-chunk cap
(only the first 4,096 chunks are indexed).

Search prefilters indexed points by their `acl` access tokens and `published`
flag, rather than enumerating every published document. Caller tokens cover
the actor, delegated actor, tenant-wide access, mapped source identities and
source groups from a complete membership snapshot. Every hit is re-authorized
against PostgreSQL for the object, version, document status and current
published generation before results, evidence, locations or facets are
produced, so revocations and temporary connection disablement take effect
immediately. Each index fetches up to four times the per-index result limit,
capped at 200 points per round, for at most three rounds (the initial round
and two extra rounds); fewer results can be returned when post-checks discard
hits.

`POST /api/v1/rag/search` has no per-collection or aggregate 2,000-document
bound. It returns `search_scope_too_large` when more than 20 distinct
collections are requested, or when no selection is supplied and more than
20 collections are readable.

`POST /api/v1/rag/search` pages through an opaque `cursor`: the response's
`next_cursor` feeds the next request's `cursor` (same `query`, `mode`,
`collection_ids` and `limit`) and pages are stable and disjoint — the fused
order breaks score ties on the result's stable id. Results are paged over a
pool bounded at twice the maximum `limit`; when the pool is exhausted the
response has no `next_cursor`. A malformed cursor is a 400.

Access tokens are written at embedding and refreshed before publication.
Complete grants snapshots re-project connector objects with revision fencing.
The durable `rag.access_backfill` job backfills older points, triggered at
startup and periodically; points without the projection remain searchable
through the authoritative post-check. Temporary connection disablement is
enforced by that post-check, not by the tokens.

Query embedding and rewrite send only the query to their models. Section
selection sends the source references of its candidate results with the model
call, so input restrictions apply.

A collection embeds with the deployment default (`DEFAULT_EMBEDDING_MODEL`,
`DEFAULT_EMBEDDING_DIMENSIONS`) unless the create body sets
`embedding_model` and/or `embedding_dimensions` (0 = the model's full output;
otherwise 32–8192, vectors truncated and re-normalized — only for
Matryoshka-trained models such as Qwen3-Embedding). A shortened size is
checked with one test embedding at create time (400 if the model can't
produce it). Changing either with `PUT` is checked the same way, then every
document re-embeds into a new index in the background; search keeps reading
each document from its previous index until it is done. Per-index rankings
are fused using plain (unweighted) reciprocal rank fusion; a binding whose
model the caller cannot use is skipped (if no binding can be searched, the
search fails). The old index is dropped once no document uses it.

`query_prefix` and `passage_prefix` (create and `PUT`, returned in
collection responses) control the strings prepended to queries and chunks
for the embedding model. Defaults come from a per-model registry:
qwen3-embedding and multilingual-e5-large-instruct get the Qwen3 instruction
(`Instruct: Given a web search query…`) on queries only, nomic-embed-text
gets `search_query: ` / `search_document: `, e5 models get `query: ` /
`passage: `, bge-en/arctic/mxbai get `Represent this sentence…` on queries
only; anything else defaults to no prefix. `passage_prefix` is stored per
index generation, so changing it (like model/dimensions) moves embedding to
a new index and re-embeds; `query_prefix` applies to new queries only and
changes without re-embedding.

Re-embedding runs as a durable `rag.reembed_scan` job whose progress
("N of M documents left") is visible to collection managers. A retired index
is dropped only after a soak period (`INDEX_RETIREMENT_SOAK`, default `24h`)
has passed since it left use — and only once its replacement index passed a
point-count canary before being adopted.

`embed_titles` (default `true` for new collections) embeds each chunk behind
its document's title — connector-provided titles are used, chat-message
objects get no title, and titles are whitespace-normalized and capped at
120 runes. Scalar document metadata is appended to the embedded text only
while it stays under ~25% of the chunk size. BM25 indexes a `search_text`
payload of title + chunk + keyword metadata regardless of `embed_titles`.
Changing `embed_titles` with `PUT` re-embeds in the background.

Every `PUT` also queues any document still embedded under older settings, so
repeating a `PUT` that failed midway finishes the re-embedding it started.

## 3. Connector sources and access

`POST /api/v1/connectors/sources` creates a connector source and
`GET /api/v1/connectors/sources`, `GET/PUT/DELETE
/api/v1/connectors/sources/{id}` manage it (scope `connectors.manage`). The
source's `target_collection_id` routes synced documents into a knowledge
collection; reads then pass through the source's ACL snapshot in addition
to collection membership. Source views report `unmapped_principals` — provider
accounts without an Enclava identity binding get no access until an
administrator binds the observed identity. Its observed grants and memberships
then apply without another sync.

GitHub sources require `config.owner` and `config.repo`. A nonempty GitHub
`credential_ref` must be a `CONNECTOR_GITHUB_*` reference declared in
`SECRET_BINDINGS` for the tenant, connector purpose and GitHub API origin.
Public repository content can sync without a credential, but collaborator and
membership lookup requires a credential with push access even for public
repositories. `POST /api/v1/connectors/sources/{id}/sync` starts a fresh manual
sync; send the same `Idempotency-Key` header to retry the same submission.
Syncing a disabled source returns 409.

Slack sources require `config.channel_id` (one channel per source; invite the
app's bot user to it) and a `CONNECTOR_SLACK_*` `credential_ref` bound in
`SECRET_BINDINGS` to `https://slack.com` with a bot token. Required scopes:
`channels:history`/`groups:history` and `channels:read`/`groups:read` for
public/private channels, plus `users:read` for author names and `files:read`
for attachments. Every message and thread reply becomes one document headed
with channel, author and UTC time, with mentions resolved and a permalink back
to Slack; join/leave notices are skipped. Slack rate limits are waited out
using `Retry-After` (up to five times per call).

Connector sync workers resolve the source creator's current user grants and
billing identity for ingest and embedding. A missing or disabled creator fails
the job; model and endpoint access must be granted to that creator. Indexing
also needs the creator's own source access: on a new source the first sync's
items fail with "the indexing user has no access to this item in the source"
until the creator's observed identity is bound. Bind it, then sync again —
every sync retries items whose indexing failed.

Grants inherit down container hierarchies (Slack channels, GitHub
repositories, Teams channels and chats); group grants additionally require
the reader's group membership from a fresh membership snapshot. Drive files
flagged copy/download-restricted sync a deny-everyone grant and are denied
for every reader. A complete access snapshot expires after
`SOURCE_ACCESS_MAX_STALE` (default `168h`) for grants and memberships alike.

Sync dedupes on content hash: unchanged bytes reuse the artifact and version
unless the ingest handoff is missing or title/URI/name/mime metadata changed
(which creates a new version). Editing a source's config resets its stored
listing checkpoint so the next sync re-lists from the start.

## 4. Structured extraction

```bash
# list templates (walkthrough ships Invoice and Contract defaults)
curl -s -H "authorization: Bearer $KEY" $BASE/api/v1/extract/templates | jq '.templates[].id'

# run a job: multipart, same formats as knowledge upload
curl -s -H "authorization: Bearer $KEY" \
  -F "template_id=detailed_invoice" -F "file=@invoice.epub" \
  $BASE/api/v1/extract/jobs | jq .job_id

# poll, then read the typed result
curl -s -H "authorization: Bearer $KEY" $BASE/api/v1/extract/jobs | jq '.jobs[0]'
curl -s -H "authorization: Bearer $KEY" \
  $BASE/api/v1/extract/jobs/JOB_ID/result | jq .
```

Template fields: `id`, `name`, `description`, `system_prompt`, `user_prompt`,
`context_schema` (object definitions `{type, description, …}` — bare type
strings are rejected), `output_schema` (JSON Schema 2020-12), `model`,
`max_output_tokens` (optional 1–32768 generation cap; `0`/unset sends no
explicit cap so `INFERENCE_DEFAULT_MAX_OUTPUT_TOKENS` and the routed model's
limits apply; `EXTRACT_MAX_OUTPUT_TOKENS` sets the server-side fallback for
templates that declare none), `is_active`, `is_default`.

Job list rows carry `validation_status` (`needs_review` when the stored
result failed schema/parse validation, `valid` otherwise) and
`validation_error_count`; the job still completes terminally. The result
payload itself carries `validation_errors`/`validation_warnings`. Rows also
carry `updated_at` (last write) and, on `unknown` jobs, `outcome_reason`
(`interrupted` — the request reached the provider before the worker was
lost) and `reservation_id` (the billing hold that stayed liable; usage may
already have been billed). Such a job is never auto-replayed:
`POST /api/v1/extract/jobs/{id}/retry` — an explicit owner call only —
enqueues a NEW job reusing the same input and returns `202` with the fresh
job row; retrying can bill again.

Completed results can be copied into knowledge with
`POST /api/v1/knowledge/imports` `{"collection_id":…,"result_ref":JOB_ID}`.
A result with `validation_errors` is refused (422 `validation_ack_required`)
until the request adds `"acknowledge_validation": true`.

## 5. Chat (OpenAI-compatible)

```bash
curl -s -H "authorization: Bearer $KEY" -H 'content-type: application/json' \
  $BASE/v1/chat/completions \
  -d '{"model":"dsv41","messages":[{"role":"user","content":"hi"}]}' | jq .choices[0].message
```

Streaming requests whose upstream returns a non-event-stream response or
invalid initial framing fail with HTTP 502 and an OpenAI error envelope with
code `upstream_protocol_error`; the upstream body is not exposed.

## 6. Own conversations

`GET /api/v1/conversations` lists the caller's own conversations,
newest-first (`?limit=` ≤50, `?cursor=` opaque keyset), and
`GET /api/v1/conversations/{id}` returns one conversation with its
authorized messages (revoked-evidence turns are suppressed). Two mutations
round out the dashboard's history controls — both are owner-only, so
another member's id is one indistinguishable 404:

```bash
# rename (title is trimmed, 1–200 chars; 400 invalid_conversation otherwise)
curl -s -H "authorization: Bearer $KEY" -H 'content-type: application/json' \
  -X PATCH $BASE/api/v1/conversations/CONV_ID -d '{"title":"Renewal review"}'

# delete: removes the conversation and its messages; usage/billing records stay
curl -s -H "authorization: Bearer $KEY" -X DELETE $BASE/api/v1/conversations/CONV_ID
```

Agent runs created by `POST /api/v1/agent-runs` return `202` with persisted
`created_at` and `updated_at`. Run steps include terminal errors and end
times; model-step input summaries include the durable `job_id` and
`attempt_id` associated with their billing reservation. A wall-clock timeout
is reported explicitly even while the detached upstream call settles.
Configured tools that cannot be used report the reason on the run and step.

`DELETE /api/v1/prompt-templates/{id}` returns `409 template_in_use` when an
assistant still references the instructions; its message names assistants
visible to the caller. MCP server discovery records the last probe outcome,
including failures, for the Tools dashboard.

## 7. Endpoints and capability discovery

Endpoint capabilities and model lists are read from the model server
itself — `POST /api/v1/providers/discover` (scope `pricing.manage`)
returns the server's `/v1/models` plus 1-token probes:

```bash
curl -s -b jar.txt -H 'content-type: application/json' \
  $BASE/api/v1/providers/discover \
  -d '{"protocol":"openai_compatible","base_origin":"http://spark02:8000","authorized_private":true}' | jq .discovered
# {"models":["dsv41"],"probe_model":"dsv41",
#  "capabilities":{"chat/completions":"yes","embeddings":"unknown","responses":"yes",
#                  "vision":"unknown","responses/compact":"unknown","mcp":"unknown"},
#  "probes":[{"capability":"chat/completions","model":"dsv41","result":"yes","error":""}]}
```

Each probe reports `yes`, `no`, or `unknown` for one model; endpoint-wide
capabilities are `yes` if any suitable model succeeded and `unknown`
otherwise. At most three candidates are tried per capability. The Models
drawer's **Discover from server** button calls this endpoint. Persist an
endpoint with `POST /api/v1/providers` using the discovered values
(`capabilities` includes `model:<alias>=<upstream-id>` bindings).
For a dedicated Tinfoil enclave, set `base_origin` to its HTTPS origin and
`enclave_repo` to the trusted repository reference. The default router origin
`https://inference.tinfoil.sh` uses automatic router configuration.

`request_defaults` (create and update; returned under `config.request_defaults`)
adds top-level fields to chat requests routed to the endpoint that do not set
them — for example a local server's sampling and chat-template switches.
Caller-set fields win, `{}` clears the defaults and an update without the field
keeps them; gateway-owned fields (`model`, `messages`, `stream`,
`stream_options`, `n`, `max_tokens`, `max_completion_tokens`, `tools`, …) are
rejected:

```bash
curl -s -H "authorization: Bearer $KEY" -H 'content-type: application/json' \
  $BASE/api/v1/providers -d '{"id":"spark01-qwen","protocol":"openai_compatible",
  "base_origin":"http://spark01:8888","authorized_private":true,
  "capabilities":["chat/completions","model:Qwen3.8-Flash-Next=Qwen3.8-Flash-Next"],
  "request_defaults":{"temperature":0.5,"chat_template_kwargs":{"enable_thinking":false}}}'
```

## Identity management

`DELETE /api/v1/identities/users/{id}` requires `platform.admin` and returns
`200` with `{"deleted":true,"id":"..."}`. It revokes the person's sessions and
API keys immediately, removes role memberships, and hides the deleted account
from people lists and sign-in. Their email can be used for a new account; past
usage and audit history remain. Deleting yourself or the last active
administrator returns `409`; a user outside the caller's tenant returns `404`.
The action is recorded as `identity.user.deleted` in the security audit log.

## 8. Security and operations

- Sign-in is throttled per process: 10 attempts/minute per IP and
  5 attempts per 15 minutes per account (the account limiter is capped at
  4,096 tracked tenant+email buckets); throttled attempts return 429 and a
  failed-password check still runs a dummy bcrypt compare.
- Changing a password rewrites the hash and revokes the user's other
  sessions atomically — only the session performing the change survives.
- `GET /api/v1/admin/security-audit` (scope `platform.admin`) lists the
  append-only security audit log: `limit` 1–200 (default 100), `before` an
  opaque cursor, response `{"data":[…]}` newest first. Recorded actions
  include `auth.login.succeeded` / `auth.login.failed` /
  `auth.login.throttled` / `auth.logout`, `auth.password.changed`,
  `auth.sessions.revoked`, `apikey.created` / `apikey.revoked`,
  `identity.user.*`, `identity.role.*`, `endpoint.*`,
  `connector.source.*` and `mcp.server.*` / `mcp.tool.*`.
- `GET /metrics` exposes Prometheus metrics; require
  `Authorization: Bearer $METRICS_TOKEN`. With no token configured the
  endpoint fails closed (401) unless `METRICS_AUTH_DISABLED=true` is set.
- `GET /readyz` reports inference readiness and now includes a
  `dependencies` object (`inference`, `vector_store`) alongside `ready`;
  it is still inference-gated only — a vector-store outage never flips it.
  `GET /readyz/knowledge` answers the knowledge dependency alone: 503 while
  the configured vector store is unreachable (bounded probe: 2 s timeout,
  result cached 10 s), 200 otherwise. Unknown `/api/*` and `/v1/*` routes
  return the JSON error envelope (404 `not_found`); dashboard paths keep
  their HTML 404.
- API key issuance stays administrator-only. A signed-in member can file a
  request from Account → Developer:
  `POST /dashboard/account/api-access-request` (session + CSRF, optional
  `{"note":"…"}`) stores at most one open request per user and returns
  `{"status":"open","created":…}`; `POST
  /dashboard/access-requests/{id}/dismiss` (session + CSRF, `platform.admin`)
  closes one without granting anything. Open requests surface on the Home
  attention feed for administrators.

## 9. Billing reconciliation

`POST /api/v1/admin/reservations/{id}/resolve` (scope `platform.admin`)
settles a reservation in the `held` or `unknown` state — or writes the hold
off with all-zero counts — and returns the settled `usage_record`:

```bash
curl -s -H "authorization: Bearer $KEY" -H 'content-type: application/json' \
  $BASE/api/v1/admin/reservations/res_abc/resolve \
  -d '{"input_tokens":12,"cached_input_tokens":4,"output_tokens":8}'
```

`input_tokens` is the provider-reported input total and INCLUDES its cached
subset, exactly like every upstream usage object: the service settles
`input_tokens - cached_input_tokens` as uncached input plus
`cached_input_tokens` as cached input, so cached tokens are never billed
twice. `cached_input_tokens` must not exceed `input_tokens` and all three
fields must be non-negative integers (400 `invalid_request_error`
otherwise). A nonexistent or cross-tenant id is one indistinguishable 404; a
reservation not in the `held` or `unknown` state returns 409, except an
already-settled one which returns the existing `usage_record` (200).

The dashboard's Usage → Reservations page requires `platform.admin` and lists
held and unknown reservations, with state filters and pagination. Both can
be resolved from its drawer using the reconciliation operation above. Resolving
a held reservation while its upstream request is still in flight can settle it
before actual usage arrives; reconcile only after verifying the request's outcome.

`GET /api/v1/admin/billing-audit` (scope `platform.admin`) lists billing
events, optionally filtered by `subject_type` and `subject_id`. The
`request.refused` action records inference admission denials with detail
`reason` (`price_missing`, `budget_exceeded`, or `budget_currency_mismatch`),
resolved `model`, and `capability`. Its subject is the API key for key-authenticated requests,
otherwise the user when available; the actor identifies the requesting principal.
It contains no prompts or credential secrets. Successful requests do not
produce refusal events. The Usage → Audit view offers a kind chip for each
recorded event, including `request.refused`.

## 10. Pricing

`POST /api/v1/admin/pricing` (scope `pricing.manage`) writes immutable price
rows. The single-entry body is `{subject_type, subject_id, capability,
unit, rate_micros, unit_quantity, currency?, effective_from?}` — `unit` is
`input`, `cached_input` or `output` and the rate is `rate_micros`
currency-micros per `unit_quantity` tokens. A `prices` array of the same
entries writes every row in ONE transaction — the dashboard's Set-price
form sends all entered categories this way, so a rejected entry leaves no
partial price set:

```bash
curl -s -H "authorization: Bearer $KEY" -H 'content-type: application/json' \
  $BASE/api/v1/admin/pricing -d '{
    "prices":[
      {"subject_type":"model","subject_id":"dsv41","capability":"chat/completions",
       "unit":"input","rate_micros":2500000,"unit_quantity":1000000,"currency":"USD"},
      {"subject_type":"model","subject_id":"dsv41","capability":"chat/completions",
       "unit":"output","rate_micros":5000000,"unit_quantity":1000000,"currency":"USD"}
    ]}'
# → 201 {"ids":["price_…","price_…"],"count":2}
```

An empty or >12-entry `prices` array, any invalid entry, and a repeated
(subject, capability, unit) pair all return 400 before anything is written.
