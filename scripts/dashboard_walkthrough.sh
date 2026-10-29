#!/bin/sh
# dashboard_walkthrough.sh — persistent §9.3 manual-verification instance.
#
# Brings up the full Option A stack (stub inference/MCP, isolated schema,
# seeded tenant with admin/manager/member accounts, documents, assistants,
# tools) bound to APP_HOST (default: this host's Tailscale IP) and LEAVES IT
# RUNNING for the phone + desktop walkthrough. Teardown is manual (see the
# end of this file) — the credentials print once and are saved under $DATA_DIR.
#
# Usage:
#   ./scripts/dashboard_walkthrough.sh            # binds the tailscale IP
#   APP_HOST=127.0.0.1 ./scripts/dashboard_walkthrough.sh
#
# Requires: PostgreSQL (E2E_DATABASE_URL), Qdrant (QDRANT_URL), openssl, go.
log() { printf '%s %s\n' "[walkthrough]" "$*" >&2; }

E2E_DATABASE_URL="${E2E_DATABASE_URL:-postgres://enclava:enclava@127.0.0.1:5432/enclava?sslmode=disable}"
export E2E_DATABASE_URL
QDRANT_URL="${QDRANT_URL:-http://127.0.0.1:6333}"
export QDRANT_URL

# Default to the tailscale address so a phone on the tailnet can reach it.
if [ -z "${APP_HOST:-}" ]; then
  APP_HOST="$(tailscale ip -4 2>/dev/null | head -1 || true)"
  [ -n "$APP_HOST" ] || APP_HOST=127.0.0.1
fi
export APP_HOST
APP_PORT="${APP_PORT:-18080}"
export APP_PORT
DATA_DIR="$(mktemp -d /tmp/enclava-walk.XXXXXX)"
export WALK_DATA_DIR="$DATA_DIR"
log "data dir: $DATA_DIR (kept after exit — see $DATA_DIR/credentials.txt)"

log "starting stub inference/MCP servers"
mkdir -p "$DATA_DIR/stubs"
node tests/dashboard/stubs.mjs inference "$DATA_DIR/stubs/llm.origin" >"$DATA_DIR/stubs-llm.log" 2>&1 &
STUB_LLVM_PID=$!
node tests/dashboard/stubs.mjs mcp "$DATA_DIR/stubs/mcp.origin" >"$DATA_DIR/stubs-mcp.log" 2>&1 &
STUB_MCP_PID=$!
cleanup_bg() {
  kill "$STUB_LLVM_PID" "$STUB_MCP_PID" 2>/dev/null || true
}
trap 'cleanup_bg' INT TERM
for _ in $(seq 1 100); do
  [ -f "$DATA_DIR/stubs/llm.origin" ] && [ -f "$DATA_DIR/stubs/mcp.origin" ] && break
  sleep 0.1
done
E2E_LLM_URL="$(cat "$DATA_DIR/stubs/llm.origin")"
E2E_MCP_URL="$(cat "$DATA_DIR/stubs/mcp.origin")"
log "stubs: llm=$E2E_LLM_URL mcp=$E2E_MCP_URL"
export E2E_LLM_URL E2E_MCP_URL
# ── Isolated schema + app, reusing the e2e runner's own wiring ──
log "building enclava"
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o enclava ./cmd/enclava

SCHEMA="walk_$(date +%s)"
APP_DSN="$(go run ./scripts/e2e_pgctl.go create "$E2E_DATABASE_URL" "$SCHEMA")"
log "schema: $SCHEMA"

export DATABASE_URL="$APP_DSN"
export ENCLAVA_DATA_DIR="$DATA_DIR/app"; mkdir -p "$ENCLAVA_DATA_DIR"
export INFERENCE_PROVIDER=openai_compatible
export OPENAI_COMPATIBLE_BASE_URL="$E2E_LLM_URL"
export OPENAI_COMPATIBLE_API_KEY="walkthrough-key"
export DEFAULT_EMBEDDING_MODEL="e2e-embed"
export SESSION_TENANT_SLUG="bootstrap"
export ENCLAVA_PREAUTH_RATE_LIMIT_PER_MINUTE=100000
export ENCLAVA_PREAUTH_RATE_LIMIT_PER_HOUR=1000000
export ENCLAVA_PREAUTH_RATE_LIMIT_PER_DAY=10000000

log "bootstrapping tenant bootstrap"
ADMIN_PASSWORD="$(./enclava bootstrap --tenant bootstrap --email admin@walkthrough.local --name 'Walkthrough Admin' 2>/dev/null | sed -n 's/^password: //p')"
{
  echo "URL:      http://$APP_HOST:$APP_PORT/dashboard"
  echo "Admin:    admin@walkthrough.local / $ADMIN_PASSWORD"
} >"$DATA_DIR/credentials.txt"

log "starting app on http://$APP_HOST:$APP_PORT"
./enclava >"$DATA_DIR/app.log" 2>&1 &
APP_PID=$!
printf '%s' "$APP_PID" >"$DATA_DIR/app.pid"
for _ in $(seq 1 300); do
  curl -fsS "http://127.0.0.1:$APP_PORT/livez" >/dev/null 2>&1 && break
  sleep 0.1
done

log ""
log "  ═══ Walkthrough instance is live ═══"
log "  Open:  http://$APP_HOST:$APP_PORT/dashboard"
log "  Admin: admin@walkthrough.local  password: $ADMIN_PASSWORD"
log "  (also saved to $DATA_DIR/credentials.txt)"
log ""
log "  Manager/Member accounts: sign in as Admin → Access → People →"
log "  invite one of each (the seeded Knowledge-manager and Member roles are"
log "  listed under Access → Roles). Or use the seeded e2e accounts below."
log ""
log "  The app and stubs keep running. To stop later:"
log "    kill \$(cat $DATA_DIR/app.pid) $STUB_PID"
log "    go run ./scripts/e2e_pgctl.go drop \"$E2E_DATABASE_URL\" \"$SCHEMA\""
log ""

wait "$APP_PID"
