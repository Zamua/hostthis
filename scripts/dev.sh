#!/usr/bin/env bash
# Runs hostthisd against the Worker under Miniflare, both on this machine.
# The Worker serves every paste, site and room in path mode at
# localhost:$HOSTTHIS_DEV_READ_PORT (default 8787), the host the URLs hostthisd
# prints name. Cells and payloads persist under $HOSTTHIS_DATA_DIR/worker. The
# caller supplies the rest of the HOSTTHIS_* config (see the Makefile's run
# target).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
data=${HOSTTHIS_DATA_DIR:-$root/data}
port=${HOSTTHIS_DEV_READ_PORT:-8787}
log=$(mktemp)
token=$(openssl rand -hex 16)

PERSIST_DIR="$data/worker" PORT="$port" HOSTTHIS_WORKER_TOKEN="$token" \
  HOSTTHIS_LOCALRT_APEX_DOMAIN=localhost HOSTTHIS_LOCALRT_URL_MODE=path HOSTTHIS_LOCALRT_PUBLIC_SCHEME=http \
  node "$root/worker/localrt/serve.mjs" >"$log" &
rt=$!
trap 'kill "$rt" 2>/dev/null || true; rm -f "$log"' EXIT

url=
until [ -n "$url" ]; do
  kill -0 "$rt" 2>/dev/null || { echo "worker runtime exited before serving" >&2; exit 1; }
  sleep 0.1
  url=$(sed -n 's/^LISTENING //p' "$log")
done
echo "worker runtime: $url (reads at http://localhost:$port/p/<slug>)"

cd "$root"
HOSTTHIS_WORKER_ENDPOINT=$url HOSTTHIS_WORKER_TOKEN=$token HOSTTHIS_APEX_DOMAIN=localhost:$port \
  go run ./cmd/hostthisd
