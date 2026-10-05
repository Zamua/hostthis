#!/usr/bin/env bash
# Runs hostthisd against the celld Worker under Miniflare, both on this machine.
# Cells and payloads persist under $HOSTTHIS_DATA_DIR/celld. The caller supplies
# the rest of the HOSTTHIS_* config (see the Makefile's run target).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
data=${HOSTTHIS_DATA_DIR:-$root/data}
log=$(mktemp)

PERSIST_DIR="$data/celld" node "$root/celld/localrt/serve.mjs" >"$log" &
rt=$!
trap 'kill "$rt" 2>/dev/null || true; rm -f "$log"' EXIT

url=
until [ -n "$url" ]; do
  kill -0 "$rt" 2>/dev/null || { echo "celld runtime exited before serving" >&2; exit 1; }
  sleep 0.1
  url=$(sed -n 's/^LISTENING //p' "$log")
done
echo "celld runtime: $url"

cd "$root"
HOSTTHIS_CELLD_ENDPOINT=$url go run ./cmd/hostthisd
