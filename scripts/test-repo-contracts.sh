#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)

grep -q 'file: Dockerfile' "$root/.github/workflows/image.yml"
# CI runs celld in the production shape (deploy + node against an object
# store), never the Linux dev watcher, whose own state writes restart-storm
# the application.
grep -q 'celld deploy ./celld --bucket' "$root/.github/workflows/ci.yml"
grep -q -- '--listen 127.0.0.1:8087' "$root/.github/workflows/ci.yml"
! grep -q 'celld dev' "$root/.github/workflows/ci.yml"
grep -q '"no_bundle": true' "$root/celld/wrangler.jsonc"
! grep -q 'esbuild@' "$root/.github/workflows/ci.yml"
! grep -Fq 'io.ReadAll(r)' "$root/internal/service/blobunit_standalone.go"
