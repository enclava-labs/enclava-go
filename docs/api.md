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

Uploads dedupe by content hash; a re-upload of the same bytes returns the
existing document. On failure, `documents[].error_message` carries the
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
to collection membership. Source views report `unmapped_principals` —
provider accounts with no Enclava identity binding — which are skipped and
get no access until an administrator binds the identity and the source
re-syncs.

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
payload itself carries `validation_errors`/`validation_warnings`.

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

## 7. Endpoints and capability discovery

Endpoint capabilities and model lists are read from the model server
itself — `POST /api/v1/providers/discover` (scope `pricing.manage`)
returns the server's `/v1/models` plus 1-token probes:

```bash
curl -s -b jar.txt -H 'content-type: application/json' \
  $BASE/api/v1/providers/discover \
  -d '{"protocol":"openai_compatible","base_origin":"http://spark02:8000","authorized_private":true}' | jq .discovered
# {"models":["dsv41"],"probe_model":"dsv41",
#  "capabilities":{"chat/completions":"yes","embeddings":"no","responses":"yes",
#                  "vision":"unknown","responses/compact":"unknown","mcp":"unknown"}}
```

`yes`/`no` are probe-settled; `unknown` capabilities (vision, compact,
MCP) have no cheap probe and stay operator judgment. The Models drawer's
**Discover from server** button calls this same endpoint. Persist an
endpoint with `POST /api/v1/providers` using the discovered values
(`capabilities` includes `model:<alias>=<upstream-id>` bindings).

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

## 9. Billing reconciliation

`POST /api/v1/admin/reservations/{id}/resolve` (scope `platform.admin`)
settles a reservation stuck in the `unknown` state — or writes the hold off
with all-zero counts — and returns the settled `usage_record`:

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
reservation not in the `unknown` state returns 409, except an
already-settled one which returns the existing `usage_record` (200).
