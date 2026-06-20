.PHONY: help build test smoke dev run docker-build docker-up docker-down \
        dev-minio-up dev-minio-down test-conformance-kv \
        fmt vet clean data-dir-perms rebuild-site-fixtures

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
	@echo "  make dev-minio-up  start local MinIO for the slatedb/shale metadata + shale-blob tests"
	@echo "  make test-conformance-kv  run slatedb+shale conformance (needs -tags slatedb + MinIO)"
	@echo "  make fmt / vet     gofmt / go vet"
	@echo "  make rebuild-site-fixtures  rebuild the vite SPA test fixtures (needs npm)"
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

# Regenerate the committed site-fixture dist/ trees from the demo source
# (npm ci + vite build for the three framework demos; the plain-static
# demo's dist/ is hand-written and left as-is). The validation harness
# (internal/sitevalidation) byte-compares the served bytes against these
# committed snapshots WITHOUT running npm, so CI needs no Node toolchain;
# this target is the developer-side way to refresh the snapshots.
rebuild-site-fixtures:
	./testdata/sitefixtures/rebuild.sh

# -- containers (local dev) --------------------------------------------------

docker-build:
	docker build -t hostthis:dev .

docker-up: data-dir-perms
	docker compose up --build -d
	@echo "ssh: localhost:12222  http: http://localhost:18080"

docker-down:
	docker compose down

# -- Dev MinIO (for the slatedb/shale metadata + shale-blob tests) ----------

dev-minio-up:
	docker compose -f deploy/dev/docker-compose.yml up -d
	@echo "minio: http://localhost:9000 (s3 api)  http://localhost:9001 (console: admin/supersecret)"
	@echo "buckets 'hostthis-metadata' (+ 'hostthis-blobs' for the shale-blob byte plane) are auto-created by the init container"

dev-minio-down:
	docker compose -f deploy/dev/docker-compose.yml down -v

# Runs the slatedb + shale backend conformance suites against the local
# MinIO (hostthis-metadata bucket). Needs the slatedb build tag, cgo, and
# libslatedb_uniffi on the loader path. SLATEDB_LIB_DIR defaults to
# $HOME/.local/lib but is overridable for a different install location.
# Assumes dev-minio-up has already been run (it provisions both buckets).
SLATEDB_LIB_DIR ?= $(HOME)/.local/lib
test-conformance-kv:
	CGO_ENABLED=1 \
	CGO_LDFLAGS="-L$(SLATEDB_LIB_DIR)" \
	DYLD_LIBRARY_PATH="$(SLATEDB_LIB_DIR)" \
	LD_LIBRARY_PATH="$(SLATEDB_LIB_DIR)" \
	MINIO_TEST_ENDPOINT=http://localhost:9000 \
	MINIO_TEST_METADATA_BUCKET=hostthis-metadata \
	MINIO_TEST_ACCESS_KEY=admin \
	MINIO_TEST_SECRET_KEY=supersecret \
	go test -tags slatedb -count=1 ./internal/storage \
		-run 'TestConformance_Slate|TestConformance_Shale'

# Compose mounts ./data into the container under distroless's nonroot uid
# (65532). Make sure the host dir is writable by that uid.
data-dir-perms:
	@mkdir -p ./data
	@if [ "$$(uname)" = "Linux" ]; then sudo chown -R 65532:65532 ./data; fi

clean:
	rm -rf bin data
