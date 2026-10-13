#!/usr/bin/env bash
# Runs hostthisd against the Worker under Miniflare, both on this machine.
# Cells and payloads persist under $HOSTTHIS_DATA_DIR/worker. The caller supplies
# the rest of the HOSTTHIS_* config (see the Makefile's run target).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
data=${HOSTTHIS_DATA_DIR:-$root/data}
log=$(mktemp)

PERSIST_DIR="$data/worker" node "$root/worker/localrt/serve.mjs" >"$log" &
rt=$!
trap 'kill "$rt" 2>/dev/null || true; rm -f "$log"' EXIT

url=
until [ -n "$url" ]; do
  kill -0 "$rt" 2>/dev/null || { echo "worker runtime exited before serving" >&2; exit 1; }
  sleep 0.1
  url=$(sed -n 's/^LISTENING //p' "$log")
done
echo "worker runtime: $url"

cd "$root"
HOSTTHIS_WORKER_ENDPOINT=$url go run ./cmd/hostthisd
