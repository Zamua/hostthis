.PHONY: help build test smoke dev run docker-build docker-up docker-down \
        dev-minio-up dev-minio-down test-conformance-kv e2e e2e-ci \
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
	@echo "  make e2e           run the browser suite (needs Chrome)"
	@echo "  make e2e-ci        same, plus JUnit + screenshots for the PR report"
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
	@echo "  make clean         remove ./bin, ./data and the e2e output"
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

# -- e2e (browser) -----------------------------------------------------------

# Where the flows write their screenshots. CI overrides it; see
# docs/E2E-REPORTS.md for the layout the report consumes.
E2E_ARTIFACTS ?= $(CURDIR)/artifacts
# Pinned and fetched on demand, so the target needs nothing installed first.
GOTESTSUM_VERSION ?= v1.13.0

# The e2e suite is behind a build tag, so `make test` never picks it up and a
# checkout without Chrome still tests clean. Set E2E_CHROME_PATH if Chrome is
# installed somewhere the driver does not look. E2E_FLAGS scopes a run, e.g.
# make e2e E2E_FLAGS='-run TestMermaidRender -v'.
e2e:
	E2E_ARTIFACTS="$(E2E_ARTIFACTS)" go test -tags e2e -count=1 $(E2E_FLAGS) ./e2e/...

# Emits the two things the pull-request report expects: JUnit at results.xml
# and one screenshot directory per flow under $(E2E_ARTIFACTS). A failing suite
# is the case the report matters most for, and gotestsum writes the report
# before exiting non-zero, so both survive a failure and make still fails.
e2e-ci:
	@mkdir -p "$(E2E_ARTIFACTS)"
	E2E_ARTIFACTS="$(E2E_ARTIFACTS)" \
	go run gotest.tools/gotestsum@$(GOTESTSUM_VERSION) \
		--junitfile results.xml -- -tags e2e -count=1 ./e2e/...

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
# Build and test against the go.mod PINS, ignoring any local go.work.
#
# The workspace redirects shale to an on-disk checkout, so an ordinary `make
# test` validates whatever that tree happens to be - not the version a release
# image is built from. CI never sees this (go.work is gitignored, so a fresh
# checkout has none), which is why the pinned combination stayed correct while
# local runs quietly diverged. This target is how to get CI's answer locally.
test-pinned:
	GOWORK=off go build ./...
	GOWORK=off go test ./...

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

# Screenshots accumulate across runs, so a renamed flow leaves a directory the
# report would list as unmatched. Removing them is how a local run starts clean.
clean:
	rm -rf bin data artifacts results.xml
