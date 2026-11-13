# enclava-go

Enclava is a company knowledge and AI platform delivered as a single Go
binary with an embedded dashboard. It stores metadata in PostgreSQL,
vector search in Qdrant, and routes OpenAI-compatible inference to
customer-local or confidential-compute endpoints (privatemode, tinfoil,
openai_compatible, redpill). Connectors ingest content from Slack, Teams,
GitHub, GitLab, Jira, Linear, Notion, and Google Drive; an extraction
pipeline parses and OCRs documents for retrieval-augmented chat and search.

## Features

- OpenAI-compatible inference API backed by local or confidential providers
- Retrieval-augmented chat and search (Qdrant, configurable embedding models)
- Document extraction with sandboxed native tools (poppler, tesseract, bubblewrap)
- Connectors for Slack, Teams, GitHub, GitLab, Jira, Linear, Notion, Google Drive
- Embedded web dashboard with session auth, roles, and scoped API keys

## Quickstart

    cp .env.example .env
    docker compose up --build

Set `PRIVATEMODE_API_KEY` in `.env` first; compose routes inference through
a bundled `privatemode-proxy`, which the provider requires. Set
`ENCLAVA_SECRETS_KEY` (`openssl rand -base64 32`) to store connector
credentials from the dashboard.

The dashboard is at http://localhost:8080/dashboard. Create the first
tenant and admin user from inside the running container:

    docker compose exec enclava /usr/local/bin/app bootstrap \
      --tenant bootstrap --email admin@example.com --issue-key

Without `--password`, a generated admin password is printed exactly once
along with the bootstrap API key. There is no HTTP signup; database
access authorizes bootstrap.

## Local development

    make build   # CGO_ENABLED=0 go build -> ./enclava
    make test    # go test ./... + entrypoint smoke test
    make ci      # fmt-check, vet, test, lint

Unit tests run offline. The PostgreSQL and Qdrant integration suites
fail hard unless real services are provided:

    ENCLAVA_TEST_DATABASE_URL=postgres://enclava:enclava@localhost:5432/enclava_test
    QDRANT_INTEGRATION_URL=http://localhost:6333

Native dependencies for extraction: poppler, tesseract, and bubblewrap
(used to sandbox parsing and OCR subprocesses).

API reference: docs/api.md

## Observability

Prometheus metrics are served at `GET /metrics` on the main port, protected
by `METRICS_TOKEN` (or `METRICS_AUTH_DISABLED=true`); it includes HTTP
request totals and latency histograms by route plus runtime/process metrics.
Set `OTEL_EXPORTER_OTLP_ENDPOINT` (optionally `OTEL_SERVICE_NAME`) to enable
OTLP/HTTP trace export; outbound inference client calls are traced too.
