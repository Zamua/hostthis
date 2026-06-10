.PHONY: help build test smoke dev run docker-build docker-up docker-down \
        dev-minio-up dev-minio-down test-s3 blob-migrate blob-verify \
        fmt vet clean data-dir-perms

# Default goal: show the help text rather than silently no-op.
.DEFAULT_GOAL := help

# Developer-facing targets only. Operator deploy targets (deploy-*,
# logs-*, promote, etc.) live in the operator's private infra repo
# at infra/hostthis/Makefile and are run from there. This file ships
# in the public repo and stays clean of operator paths / ssh / sudo.

help:
	@echo "Developer targets:"
	@echo "  make build         build ./bin/hostthisd (local Go)"
	@echo "  make test          run all unit + integration tests"
	@echo "  make smoke         exercise the verb surface against a live URL"
	@echo "                     (HOSTTHIS_HOST=… ; defaults to hostthis.dev)"
	@echo "  make dev           hot-iterate locally (no container)"
	@echo "  make run           alias for 'make dev'"
	@echo "  make docker-build  build the container image (tag hostthis:dev)"
	@echo "  make docker-up     bring up local compose stack"
	@echo "  make docker-down   tear it down"
	@echo "  make dev-minio-up  start local MinIO for the S3 backend test"
	@echo "  make test-s3       run the S3 round-trip integration test"
	@echo "  make blob-migrate  copy disk blobs into the configured S3 backend"
	@echo "  make blob-verify   verify every disk blob round-trips against S3"
	@echo "  make fmt / vet     gofmt / go vet"
	@echo "  make clean         remove ./bin and ./data"
	@echo
	@echo "Deploy targets live in the operator's private infra repo:"
	@echo "  make -C ~/Dropbox/workspace/macmini/infra/hostthis <target>"

# -- local Go ----------------------------------------------------------------

build:
	go build -o bin/hostthisd ./cmd/hostthisd

test:
	go test ./...

# Run locally (no container) - useful for fast iteration. Defaults to
# path mode so wildcard DNS isn't required.
dev run:
	HOSTTHIS_URL_MODE=path \
	HOSTTHIS_PUBLIC_SCHEME=http \
	HOSTTHIS_APEX_DOMAIN=localhost:8080 \
	HOSTTHIS_DATA_DIR=./data \
	HOSTTHIS_LANDING=./web/landing.html \
	go run ./cmd/hostthisd

# Standalone smoke target - runs against whatever HOSTTHIS_HOST is set
# to (defaults to hostthis.dev). Useful for ad-hoc verification + run
# by the operator's deploy as a post-deploy check.
smoke:
	HOSTTHIS_HOST="$(or $(HOSTTHIS_HOST),hostthis.dev)" ./scripts/smoke.sh

fmt:
	gofmt -s -w .

vet:
	go vet ./...

# -- containers (local dev) --------------------------------------------------

docker-build:
	docker build -t hostthis:dev .

docker-up: data-dir-perms
	docker compose up --build -d
	@echo "ssh: localhost:12222  http: http://localhost:18080"

docker-down:
	docker compose down

# -- Dev MinIO (for the S3 backend integration test) ------------------------

dev-minio-up:
	docker compose -f deploy/dev/docker-compose.yml up -d
	@echo "minio: http://localhost:9000 (s3 api)  http://localhost:9001 (console: admin/supersecret)"
	@echo "bucket 'hostthis-blobs' is auto-created by the init container"

dev-minio-down:
	docker compose -f deploy/dev/docker-compose.yml down -v

# Runs the S3 round-trip test against the local MinIO. Assumes
# dev-minio-up has already been run.
test-s3:
	MINIO_TEST_ENDPOINT=http://localhost:9000 \
	MINIO_TEST_BUCKET=hostthis-blobs \
	MINIO_TEST_ACCESS_KEY=admin \
	MINIO_TEST_SECRET_KEY=supersecret \
	go test -v -count=1 ./internal/storage -run TestS3BlobStore

# -- Blob migration helpers (disk → s3) -------------------------------------

# One-shot: copy every blob under HOSTTHIS_DATA_DIR/blobs into the configured
# S3 backend. Reads the same HOSTTHIS_S3_* env vars hostthisd does.
blob-migrate:
	go run ./cmd/hostthis-blob-migrate

# Verify: every disk blob is present in S3 with identical bytes.
# Exits non-zero on any mismatch; safe to run before flipping the
# HOSTTHIS_BLOB_BACKEND env var.
blob-verify:
	go run ./cmd/hostthis-blob-verify

# Compose mounts ./data into the container under distroless's nonroot uid
# (65532). Make sure the host dir is writable by that uid.
data-dir-perms:
	@mkdir -p ./data
	@if [ "$$(uname)" = "Linux" ]; then sudo chown -R 65532:65532 ./data; fi

clean:
	rm -rf bin data
