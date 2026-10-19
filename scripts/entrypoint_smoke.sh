#!/bin/sh
set -eu

root_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

config_dir="$tmp_dir/config"
mkdir -p "$config_dir"
printf 'ready\n' >"$config_dir/CUSTOM_FLAG"

app_bin="$tmp_dir/fake-app"
cat >"$app_bin" <<'APP'
#!/bin/sh
set -eu

count=0
if [ -f "$ENTRYPOINT_TEST_COUNT" ]; then
	count="$(cat "$ENTRYPOINT_TEST_COUNT")"
fi
count=$((count + 1))
printf '%s\n' "$count" >"$ENTRYPOINT_TEST_COUNT"

if [ "$CUSTOM_FLAG" != "ready" ]; then
	echo "CUSTOM_FLAG was not loaded from config dir" >&2
	exit 64
fi

if [ "$count" -eq 1 ]; then
	exit 17
fi

printf '%s\n' "$APP_HOST:$APP_PORT:$DATABASE_URL:$TRUST_PROXY_HEADERS" >"$ENTRYPOINT_TEST_ENV"
APP
chmod 0755 "$app_bin"

env_file="$tmp_dir/env"
count_file="$tmp_dir/count"
database_url="postgres://postgres:postgres@db:5432/enclava"

ENCLAVA_CONFIG_DIR="$config_dir" \
ENCLAVA_REQUIRED_CONFIG_KEYS="CUSTOM_FLAG" \
ENCLAVA_APP_BIN="$app_bin" \
ENCLAVA_APP_RESTART_LIMIT=3 \
ENCLAVA_APP_RESTART_DELAY_SECONDS=0 \
DATABASE_URL="$database_url" \
ENTRYPOINT_TEST_COUNT="$count_file" \
ENTRYPOINT_TEST_ENV="$env_file" \
	"$root_dir/docker/enclava-go-cap-entrypoint"

if [ "$(cat "$count_file")" != "2" ]; then
	echo "expected fake app to run twice" >&2
	exit 1
fi

expected="0.0.0.0:8080:$database_url:true"
actual="$(cat "$env_file")"
if [ "$actual" != "$expected" ]; then
	echo "unexpected default env: $actual" >&2
	exit 1
fi
