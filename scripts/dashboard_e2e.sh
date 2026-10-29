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
#   E2E_WALKTHROUGH=1                  manual §9.3 walkthrough: seed with the
#                                      setup project only, print the role
#                                      credentials and keep the stack up until
#                                      interrupted (teardown still runs). Pair
#                                      with APP_HOST=0.0.0.0 to reach it over
#                                      the tailnet.
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
  if [ "${E2E_KEEP_STACK:-}" = "1" ] && [ "${PW_CODE:-0}" != "0" ]; then
    log "E2E_KEEP_STACK=1 and playwright failed — stack left up for diagnosis"
    log "  data dir: $DATA_DIR   app pid: $APP_PID   base: http://127.0.0.1:$APP_PORT"
    log "  stop with: kill $APP_PID $INFERENCE_PID $MCP_PID $CONNECTOR_PID; pgctl drop \"$BASE_DSN\" \"$SCHEMA\""
    exit "$code"
  fi
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
# The manual §9.3 walkthrough overrides APP_HOST (e.g. 0.0.0.0 for the
# tailnet) via the environment; the automated run stays loopback.
export APP_HOST="${APP_HOST:-127.0.0.1}"
export APP_PORT
CLIENT_HOST="$APP_HOST"
[ "$CLIENT_HOST" = "0.0.0.0" ] && CLIENT_HOST=127.0.0.1
BASE_URL="http://$CLIENT_HOST:$APP_PORT"
log "starting app on $APP_HOST:$APP_PORT"
./enclava >"$DATA_DIR/app.log" 2>&1 &
APP_PID=$!

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

if [ -n "${E2E_WALKTHROUGH:-}" ]; then
  log "seeding walkthrough data (setup project)"
  npx playwright test --workers=1 --project=setup

  # Optional bring-your-own local model (walkthrough only): e.g.
  #   LOCAL_MODEL_ORIGIN=http://spark02:8000 LOCAL_MODEL_MODEL=dsv41
  # Registers the endpoint (plain HTTP needs authorized_private — tailnet
  # internal), near-zero prices so billing admits it, and a tenant-wide
  # assistant. Skipped silently when the origin is unset or unreachable.
  if [ -n "${LOCAL_MODEL_ORIGIN:-}" ] && [ -n "${LOCAL_MODEL_MODEL:-}" ]; then
    if curl -fsS -m 5 "$LOCAL_MODEL_ORIGIN/v1/models" >/dev/null 2>&1; then
      AUTH="authorization: Bearer $E2E_ADMIN_API_KEY"
      JSON="content-type: application/json"
      LM_ID="local-$LOCAL_MODEL_MODEL"
      curl -s -m 10 -H "$AUTH" -H "$JSON" -X POST "http://$CLIENT_HOST:$APP_PORT/api/v1/providers" \
        -d "{\"id\":\"$LM_ID\",\"protocol\":\"openai_compatible\",\"base_origin\":\"$LOCAL_MODEL_ORIGIN\",\"authorized_private\":true,\"capabilities\":[\"chat/completions\",\"model:$LOCAL_MODEL_MODEL=$LOCAL_MODEL_MODEL\"]}" >/dev/null || true
      for U in input output cached_input; do
        curl -s -m 10 -H "$AUTH" -H "$JSON" -X POST "http://$CLIENT_HOST:$APP_PORT/api/v1/admin/pricing" \
          -d "{\"subject_type\":\"model\",\"subject_id\":\"$LOCAL_MODEL_MODEL\",\"capability\":\"chat/completions\",\"unit\":\"$U\",\"rate_micros\":1,\"unit_quantity\":1000000,\"currency\":\"USD\"}" >/dev/null || true
      done
      curl -s -m 10 -H "$AUTH" -H "$JSON" -X POST "http://$CLIENT_HOST:$APP_PORT/api/v1/chatbots" \
        -d "{\"name\":\"Local · $LOCAL_MODEL_MODEL\",\"model\":\"$LOCAL_MODEL_MODEL\",\"rag_collection_ids\":[],\"audience\":[\"*\"]}" >/dev/null || true
      # Default extraction templates pin to the real local model — the
      # stub-backed aliases return canned text and fail schema validation.
      LOCAL_MODEL_MODEL="$LOCAL_MODEL_MODEL" node -e '
        const B = "http://127.0.0.1:" + process.env.APP_PORT;
        const H = { authorization: "Bearer " + process.env.E2E_ADMIN_API_KEY, "content-type": "application/json" };
        (async () => {
          const j = await (await fetch(B + "/api/v1/extract/templates", { headers: H })).json();
          for (const t of (j.templates || j || [])) {
            if (t.model) continue;
            await fetch(B + "/api/v1/extract/templates/" + encodeURIComponent(t.id), {
              method: "PUT", headers: H,
              body: JSON.stringify({ name: t.name, description: t.description || "", system_prompt: t.system_prompt, user_prompt: t.user_prompt, model: process.env.LOCAL_MODEL_MODEL, is_active: t.is_active, is_default: !!t.is_default, context_schema: t.context_schema || {}, output_schema: t.output_schema || {} }),
            });
          }
        })().catch(() => {});
      ' || true
      log "local model registered: $LOCAL_MODEL_MODEL on $LOCAL_MODEL_ORIGIN (assistant \"Local · $LOCAL_MODEL_MODEL\")"
    else
      log "LOCAL_MODEL_ORIGIN unreachable — skipping local model registration"
    fi
  fi

  log "walkthrough ready: http://<this host>:$APP_PORT/dashboard"
  log "  admin    $ADMIN_EMAIL / $ADMIN_PASSWORD"
  log "  manager  manager@e2e.local / E2e-pass-1234!"
  log "  member   member@e2e.local / E2e-pass-1234!"
  log "stop with Ctrl-C — the EXIT trap drops the schema and collections"
  wait "$APP_PID"
  exit 0
fi

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
