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

# upload a document (txt md html csv json jsonl pdf docx images epub)
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
```

Uploads dedupe by content hash; a re-upload of the same bytes returns the
existing document. On failure, `documents[].error_message` carries the
parser's reason (e.g. malformed document).

A collection embeds with the deployment default (`DEFAULT_EMBEDDING_MODEL`,
`DEFAULT_EMBEDDING_DIMENSIONS`) unless the create body sets
`embedding_model` and/or `embedding_dimensions` (0 = the model's full output;
otherwise 32–8192, vectors truncated and re-normalized — only for
Matryoshka-trained models such as Qwen3-Embedding). A shortened size is
checked with one test embedding at create time (400 if the model can't
produce it). Changing either with `PUT` is checked the same way, then every
document re-embeds into a new index in the background; search keeps reading
each document from its previous index until it is done, and the old index is
dropped once no document uses it.

`embed_titles` (default `true` for new collections) embeds each chunk behind
its document's name — better across many documents, worse inside a single
long one such as a book. Changing it with `PUT` re-embeds in the background.

## 3. Structured extraction

```bash
# list templates (walkthrough ships Invoice and Contract defaults)
curl -s -H "authorization: Bearer $KEY" $BASE/api/v1/extract/templates | jq '.templates[].id'

# run a job: multipart, same formats as knowledge upload
curl -s -H "authorization: Bearer $KEY" \
  -F "template_id=detailed_invoice" -F "file=@invoice.epub" \
  $BASE/api/v1/extract/jobs | jq .job.id

# poll, then read the typed result
curl -s -H "authorization: Bearer $KEY" $BASE/api/v1/extract/jobs | jq '.jobs[0]'
curl -s -H "authorization: Bearer $KEY" \
  $BASE/api/v1/extract/jobs/JOB_ID/result | jq .result
```

Completed results can be copied into knowledge with
`POST /api/v1/knowledge/imports` `{"collection_id":…,"result_job_id":JOB_ID}`.

## 4. Chat (OpenAI-compatible)

```bash
curl -s -H "authorization: Bearer $KEY" -H 'content-type: application/json' \
  $BASE/v1/chat/completions \
  -d '{"model":"dsv41","messages":[{"role":"user","content":"hi"}]}' | jq .choices[0].message
```

## 5. Endpoints and capability discovery

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
