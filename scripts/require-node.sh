#!/bin/sh
# Fails unless `node` is the major version .nvmrc names, or newer. The Worker
# tests pass or fail by Node version, so every target that runs Node checks it.
set -eu
want=$(cat "$(dirname "$0")/../.nvmrc")
have=$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0)
if [ "$have" -lt "$want" ]; then
  echo "hostthis needs Node $want or newer on PATH; found $(node --version 2>/dev/null || echo none)." >&2
  echo "  Put a newer node first on PATH (nvm use, or your package manager's node)." >&2
  exit 1
fi
