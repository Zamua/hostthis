#!/usr/bin/env bash
# Stands up a throwaway celld fleet for the conformance + live-room suites:
# MinIO, the fleet bucket, the worker deploy, and the runtime, all on localhost.
# CI runs this before the celld-gated tests; a dev with docker can run it
# locally and then `CELLD_TEST_ENDPOINT=http://127.0.0.1:8087 go test ...`.
set -euo pipefail

CELLD_IMAGE="ghcr.io/denoland/celld@sha256:f47d97c2980aa98aef1d9c42205a313442f48acb606c5987dbb9b32983a23aaf"
BUCKET=hostthis-cells-ci
# Overridable so a dev box already running something on 9000 (a dev MinIO,
# say) can pick free ports; CI uses the defaults.
MINIO_PORT=${MINIO_PORT:-9000}
CELLD_PORT=${CELLD_PORT:-8087}
CELLD_PEER_PORT=${CELLD_PEER_PORT:-8088}
export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin AWS_REGION=us-east-1

docker rm -f ci-minio ci-celld >/dev/null 2>&1 || true
docker run -d --name ci-minio --net=host -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin minio/minio server --address ":$MINIO_PORT" /data >/dev/null
for i in $(seq 1 30); do
  curl -sf http://127.0.0.1:$MINIO_PORT/minio/health/ready >/dev/null && break
  sleep 1
done

docker run --rm --net=host --entrypoint sh minio/mc -c \
  "mc alias set m http://127.0.0.1:$MINIO_PORT minioadmin minioadmin >/dev/null && mc mb -p m/$BUCKET" >/dev/null

# celld's deploy bundles the worker with an ESBUILD binary mounted into the
# container. npm's platform package carries the native binary (--force lets a
# mac host install the linux package it only ever mounts into a container); the
# `esbuild` package only carries a JS shim that cannot run inside the image.
case "$(uname -m)" in
  arm64|aarch64) ESBUILD_PKG=@esbuild/linux-arm64 ;;
  *)             ESBUILD_PKG=@esbuild/linux-x64 ;;
esac
# Under the CHECKOUT, not mktemp: a mac host runs docker in a VM that mounts
# $HOME but not /var/folders, and a -v source the daemon cannot see is silently
# created as an empty directory - which then "runs" as exec format/permission
# errors inside the container.
ESBUILD_DIR="$PWD/.ci-esbuild"
npm install --prefix "$ESBUILD_DIR" --no-save --force "$ESBUILD_PKG" >/dev/null 2>&1
ESBUILD_BIN="$ESBUILD_DIR/node_modules/$ESBUILD_PKG/bin/esbuild"
[ -f "$ESBUILD_BIN" ] || { echo "esbuild binary not found at $ESBUILD_BIN"; exit 1; }
# A cross-platform --force install can land without the exec bit.
chmod +x "$ESBUILD_BIN"

docker run --rm --net=host \
  -v "$(pwd)/celld:/app" -v "$ESBUILD_BIN:/usr/local/bin/esbuild:ro" \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_REGION \
  -w /app "$CELLD_IMAGE" \
  deploy . --bucket "s3://$BUCKET" --endpoint http://127.0.0.1:$MINIO_PORT

docker run -d --name ci-celld --net=host \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_REGION \
  "$CELLD_IMAGE" \
  --bucket "s3://$BUCKET" --endpoint http://127.0.0.1:$MINIO_PORT \
  --listen 0.0.0.0:$CELLD_PORT --internal-listen 127.0.0.1:$CELLD_PEER_PORT --advertise 127.0.0.1:$CELLD_PEER_PORT >/dev/null

for i in $(seq 1 30); do
  curl -sf http://127.0.0.1:$CELLD_PORT/healthz >/dev/null && { echo "celld ready on :$CELLD_PORT"; exit 0; }
  sleep 1
done
echo "celld never became ready"; docker logs ci-celld | tail -20; exit 1
