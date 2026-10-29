#!/usr/bin/env bash
# dashboard_e2e.sh — one-command deterministic dashboard e2e (OA-40).
#
# Lifecycle, fail-closed under `set -euo pipefail` with a single EXIT trap:
#   isolated PG schema + temp ENCLAVA_DATA_DIR
#   → stub inference/MCP/GitHub-connector servers started BEFORE the app
#   → build + `enclava bootstrap` (tenant "bootstrap", matching the env
#     endpoint's tenant binding in ensureEnvEndpoint)
#   → app binary against the stub fixtures
#   → playwright (per-role storage states produced by global.setup)
#   → run-owned Qdrant collections deleted through $QDRANT_URL, schema
#     dropped, data dir removed.
#
# Environment contract:
#   E2E_DATABASE_URL or DATABASE_URL   PostgreSQL DSN the script may create a
#                                      fresh e2e_<rand> schema on (required;
#                                      postgres://localhost:5432/enclava is
#                                      the dev default — never run against a
#                                      schema with real data)
#   QDRANT_URL                         required (default http://localhost:6333)
#   APP_PORT                           app port (default 18080)
#   ADMIN_EMAIL / ADMIN_PASSWORD       bootstrap admin credentials (defaults
#                                      admin@e2e.local / generated)
#   E2E_PROJECTS                       optional extra playwright args, e.g.
#                                      "--project=setup" to scope a dev run
set -euo pipefail

cd "$(dirname "$0")/.."

BASE_DSN="${E2E_DATABASE_URL:-${DATABASE_URL:-postgres://localhost:5432/enclava}}"
QDRANT_URL="${QDRANT_URL:-http://localhost:6333}"
APP_PORT="${APP_PORT:-18080}"
ADMIN_EMAIL="${ADMIN_EMAIL:-admin@e2e.local}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-}"
E2E_PROJECTS="${E2E_PROJECTS:-}"

SCHEMA="e2e_run_$$"
DATA_DIR="$(mktemp -d /tmp/enclava-e2e.XXXXXX)"
STUB_DIR="$DATA_DIR/stubs"
mkdir -p "$STUB_DIR"

APP_PID=""
INFERENCE_PID=""
MCP_PID=""
CONNECTOR_PID=""
QDRANT_FAILURE=0
PW_CODE=0

pgctl() { go run ./scripts/e2e_pgctl.go "$@"; }

log() { printf '[dashboard-e2e] %s\n' "$*" >&2; }

cleanup() {
  local code=$?
  set +e
  for pid in "$APP_PID" "$INFERENCE_PID" "$MCP_PID" "$CONNECTOR_PID"; do
    if [ -n "$pid" ]; then kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; fi
  done
  if [ -n "${APP_DSN:-}" ]; then
    # Qdrant teardown BEFORE the schema drop — deleting after the metadata
    # is gone would orphan run-owned vector collections.
    local names="${ENCLAVA_DATA_DIR:-$DATA_DIR}/qdrant-collections.txt"
    pgctl query "$APP_DSN" \
      "SELECT qdrant_collection FROM knowledge_collections WHERE qdrant_collection IS NOT NULL" \
      >>"$names" 2>/dev/null
    sort -u "$names" -o "$names" 2>/dev/null
    while IFS= read -r coll; do
      [ -z "$coll" ] && continue
      local out
      out="$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$QDRANT_URL/collections/$coll" 2>&1)"
      if [ "$out" = "404" ]; then
        log "qdrant collection $coll already gone (404 tolerated)"
      elif [ "$out" != "200" ]; then
        log "TEARDOWN FAILURE: DELETE $QDRANT_URL/collections/$coll → $out"
        QDRANT_FAILURE=1
      fi
    done <"$names"
    pgctl drop "$BASE_DSN" "$SCHEMA" || QDRANT_FAILURE=1
  fi
  if [ "$QDRANT_FAILURE" -ne 0 ]; then
    log "teardown reported failures — run artifacts may be orphaned"
    exit 1
  fi
  rm -rf "$DATA_DIR"
  exit "$code"
}
trap cleanup EXIT

# 1. Isolated schema + the app-facing DSN pinned to it (search_path).
log "creating isolated schema $SCHEMA"
APP_DSN="$(pgctl create "$BASE_DSN" "$SCHEMA")"

# 2. Stub fixtures BEFORE the app — ensureEnvEndpoint seeds model bindings
#    against the inference stub at startup.
log "starting stub inference/MCP/connector servers"
node tests/dashboard/stubs.mjs inference "$STUB_DIR/llm.origin" &
INFERENCE_PID=$!
node tests/dashboard/stubs.mjs mcp "$STUB_DIR/mcp.origin" &
MCP_PID=$!
node tests/dashboard/stubs.mjs connector "$STUB_DIR/connector.origin" "$STUB_DIR/connector-ca.pem" &
CONNECTOR_PID=$!

for f in "$STUB_DIR/llm.origin" "$STUB_DIR/mcp.origin" "$STUB_DIR/connector.origin"; do
  tries=0
  while [ ! -s "$f" ]; do
    tries=$((tries + 1))
    if [ "$tries" -gt 100 ]; then
      log "stub did not publish origin file $f"
      exit 1
    fi
    sleep 0.1
  done
done
E2E_LLM_URL="$(cat "$STUB_DIR/llm.origin")"
E2E_MCP_URL="$(cat "$STUB_DIR/mcp.origin")"
E2E_CONNECTOR_URL="$(cat "$STUB_DIR/connector.origin")"
CONNECTOR_CA_FILE="$STUB_DIR/connector-ca.pem"
log "stubs: llm=$E2E_LLM_URL mcp=$E2E_MCP_URL connector=$E2E_CONNECTOR_URL"

# 3. Build + bootstrap the "bootstrap" tenant. The printed password/API key
#    are captured to the data dir, never echoed.
log "building enclava"
# Same command as the Makefile build target; invoked directly so the script
# runs on hosts without make.
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o enclava ./cmd/enclava

export DATABASE_URL="$APP_DSN"
export ENCLAVA_DATA_DIR="$DATA_DIR/app"
mkdir -p "$ENCLAVA_DATA_DIR"
export INFERENCE_PROVIDER=openai_compatible
export OPENAI_COMPATIBLE_BASE_URL="$E2E_LLM_URL"
export OPENAI_COMPATIBLE_API_KEY="e2e-test-key"
export QDRANT_URL
export DEFAULT_EMBEDDING_MODEL="e2e-embed"
export SESSION_TENANT_SLUG="bootstrap"
# The whole 193-test browser run originates from one loopback IP; the
# default pre-auth budget is sized for real multi-client traffic.
export ENCLAVA_PREAUTH_RATE_LIMIT_PER_MINUTE=100000
export ENCLAVA_PREAUTH_RATE_LIMIT_PER_HOUR=1000000
export ENCLAVA_PREAUTH_RATE_LIMIT_PER_DAY=10000000
export CONNECTOR_GITHUB_E2E="e2e-github-token"
export SSL_CERT_FILE="$CONNECTOR_CA_FILE"
export NODE_EXTRA_CA_CERTS="$CONNECTOR_CA_FILE"

log "bootstrapping tenant bootstrap as $ADMIN_EMAIL"
BOOT_OUT="$(./enclava bootstrap --tenant bootstrap --email "$ADMIN_EMAIL" --issue-key)"
echo "$BOOT_OUT" >"$DATA_DIR/bootstrap.out"
if [ -z "$ADMIN_PASSWORD" ]; then
  ADMIN_PASSWORD="$(printf '%s' "$BOOT_OUT" | sed -n 's/^password: //p')"
fi
E2E_ADMIN_API_KEY="$(printf '%s' "$BOOT_OUT" | sed -n 's/^api key: //p')"
[ -n "$ADMIN_PASSWORD" ] && [ -n "$E2E_ADMIN_API_KEY" ] || {
  log "bootstrap did not print both credentials"
  exit 1
}
printf '%s' "$ADMIN_PASSWORD" >"$DATA_DIR/admin-password"

# 4. The connector SECRET_BINDINGS entry: ref|tenant|purpose|origins|env_name
#    — origins must equal the fixture's exact https://… origin (the adapter
#    resolves the secret against scheme://host).
TENANT_ID="$(pgctl query "$APP_DSN" "SELECT id::text FROM tenants WHERE slug='bootstrap'" | head -1)"
[ -n "$TENANT_ID" ] || { log "could not resolve bootstrap tenant id"; exit 1; }
export SECRET_BINDINGS="CONNECTOR_GITHUB_E2E|$TENANT_ID|connector|$E2E_CONNECTOR_URL|CONNECTOR_GITHUB_E2E"

# 5. Start the app binary on a clean port.
# The manual §9.3 walkthrough overrides APP_HOST (e.g. the Tailscale IP) via
# the environment; the automated run stays loopback.
export APP_HOST="${APP_HOST:-127.0.0.1}"
export APP_PORT
BASE_URL="http://$APP_HOST:$APP_PORT"
log "starting app on $BASE_URL"
ready=0
for _ in $(seq 1 300); do
  if curl -fsS "$BASE_URL/livez" >/dev/null 2>&1; then ready=1; break; fi
  if ! kill -0 "$APP_PID" 2>/dev/null; then break; fi
  sleep 0.1
done
if [ "$ready" != "1" ]; then
  log "app did not become ready — last log lines:"
  tail -n 40 "$DATA_DIR/app.log" >&2 || true
  exit 1
fi

# 6. Playwright — its exit status is the suite's status.
export BASE_URL ADMIN_EMAIL ADMIN_PASSWORD E2E_ADMIN_API_KEY
export E2E_LLM_URL E2E_MCP_URL E2E_CONNECTOR_URL
export E2E_QDRANT_URL="$QDRANT_URL"
export ENCLAVA_DATA_DIR

log "running playwright"
set +e
# shellcheck disable=SC2086
npx playwright test --workers=1 $E2E_PROJECTS
PW_CODE=$?
set -e
if [ "$PW_CODE" -ne 0 ] && [ -f "$DATA_DIR/app.log" ]; then
	cp "$DATA_DIR/app.log" test-results/e2e-app.log 2>/dev/null || true
	echo "[dashboard-e2e] app log preserved at test-results/e2e-app.log (tail follows)" >&2
	tail -40 "$DATA_DIR/app.log" >&2 || true
fi

# Teardown runs in the EXIT trap (Qdrant first, then schema drop); a
# teardown failure forces its own nonzero exit via QDRANT_FAILURE.
exit "$PW_CODE"
