#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)

[[ ! -e "$root/Dockerfile.slatedb" ]] || { printf 'obsolete Dockerfile.slatedb returned\n' >&2; exit 1; }
[[ ! -e "$root/scripts/ci-celld-up.sh" ]] || { printf 'obsolete custom celld runner returned\n' >&2; exit 1; }
for dead_file in internal/celld/intentlog.go internal/durable/intent.go internal/durable/memlog.go internal/storage/conformance_intentlog_test.go internal/storage/conformance_intentlog_celld_test.go; do
  [[ ! -e "$root/$dead_file" ]] || { printf 'obsolete Go intent log returned: %s\n' "$dead_file" >&2; exit 1; }
done
grep -q 'file: Dockerfile' "$root/.github/workflows/image.yml"
! grep -q 'Dockerfile\.slatedb' "$root/.github/workflows/image.yml"
# CI runs upstream celld in the production shape (deploy + node against an
# object store), never the removed custom runner and never the Linux dev
# watcher, whose own state writes restart-storm the application.
grep -q 'celld deploy ./celld --bucket' "$root/.github/workflows/ci.yml"
grep -q -- '--listen 127.0.0.1:8087' "$root/.github/workflows/ci.yml"
! grep -q 'celld dev' "$root/.github/workflows/ci.yml"
grep -q '"no_bundle": true' "$root/celld/wrangler.jsonc"
! grep -q 'esbuild@' "$root/.github/workflows/ci.yml"
! grep -q 'hostthis-metadata' "$root/deploy/dev/docker-compose.yml"
for dead in BlobUnit IntentSweeper Readiness Close; do
  ! grep -q "^[[:space:]]*$dead" "$root/cmd/hostthisd/metadata.go" || {
    printf 'obsolete metadata bundle field returned: %s\n' "$dead" >&2
    exit 1
  }
done
! grep -q 'runIntentSweep' "$root/cmd/hostthisd/main.go"

for dead in BlobHandle BlobID blob_id 'StageStream(' 'UnbindOnDelete' 'BeginUpload(' EncodedStream EncodeCompressedStream EncodeCompressedTo PreClaimSlug ReleaseSlugClaim SlugAbandoner AbandonSlug; do
  ! grep -R --include='*.go' -F "$dead" "$root/internal" "$root/cmd" || {
    printf 'obsolete blob surface returned: %s\n' "$dead" >&2
    exit 1
  }
done
! grep -Fq 'io.ReadAll(r)' "$root/internal/service/blobunit_standalone.go"
! grep -Eq 'case "(claim|unclaim)"|async (claim|unclaim)\(' "$root/celld/src/index.js"
[[ ! -e "$root/internal/service/deploy_site_claim_release_test.go" ]] || {
  printf 'obsolete slug claim compensation test returned\n' >&2
  exit 1
}
