.PHONY: help build bundle test parity smoke dev run docker-build docker-up docker-down \
        e2e e2e-ci check require-node \
        fmt fmt-check vet fix-check vuln clean data-dir-perms rebuild-site-fixtures

# Default goal: show the help text rather than silently no-op.
.DEFAULT_GOAL := help

# Developer-facing targets only. Operator deploy targets (deploy-*,
# logs-*, promote, etc.) live in the operator's private deploy repo.
# This file ships in the public repo and stays clean of operator
# paths / ssh / sudo.

help:
	@echo "Developer targets:"
	@echo "  make build         build ./bin/hostthisd (local Go)"
	@echo "  make bundle        build the Worker bundle (worker/dist)"
	@echo "  make check         everything CI's main job gates on: fmt-check, vet,"
	@echo "                     fix-check, test, vuln"
	@echo "  make test          run all unit + integration tests"
	@echo "  make parity        diff hostthisd against the Worker's public surface"
	@echo "  make e2e           run the browser suite (needs Chrome)"
	@echo "  make e2e-ci        same, plus JUnit + screenshots for the PR report"
	@echo "  make smoke         exercise the verb surface against a live URL"
	@echo "                     (HOSTTHIS_HOST=… ; defaults to hostthis.dev)"
	@echo "  make dev           hot-iterate locally (no container)"
	@echo "  make run           alias for 'make dev'"
	@echo "  make docker-build  build the container image (tag hostthis:dev)"
	@echo "  make docker-up     bring up local compose stack"
	@echo "  make docker-down   tear it down"
	@echo "  make fmt / vet     gofmt / go vet"
	@echo "  make rebuild-site-fixtures  rebuild the vite SPA test fixtures (needs npm)"
	@echo "  make clean         remove ./bin, ./data, the Worker bundle and the e2e output"
	@echo
	@echo "Deploy targets live in the operator's private deploy repo."

# -- local Go ----------------------------------------------------------------

build:
	go build -o bin/hostthisd ./cmd/hostthisd

# Every runtime, local or deployed, runs the one bundled module worker/dist/index.js,
# built with the static assets beside it from worker/src and web/shells.
WORKER_SRC := $(shell find worker/src worker/build worker/vendor web/shells -type f)

bundle: worker/dist/index.js

worker/dist/index.js: $(WORKER_SRC) worker/package.json worker/node_modules | require-node
	npm --prefix worker run build

# Every target that runs Node goes through this first.
require-node:
	@./scripts/require-node.sh

worker/node_modules: worker/package-lock.json | require-node
	npm ci --prefix worker
	@touch $@

# Go tests run the Worker bundle locally (internal/workertest), so they
# need it built and its runtime installed. The parity suite runs as its own
# target, once.
test: require-node bundle worker/localrt/node_modules
	go test $$(go list ./... | grep -v '/internal/parity$$')
	$(MAKE) parity
	npm --prefix worker test
	./scripts/test-repo-contracts.sh

# The response parity suite (internal/parity): every request to hostthisd and
# to the Worker's public surface, diffed. Locally by default; against a
# deployed environment when PARITY_HOSTTHISD_URL, PARITY_WORKER_URL,
# PARITY_TOKEN_FILE and PARITY_SLUGS are set (internal/parity/external_test.go).
parity: require-node bundle worker/localrt/node_modules
	go test -count=1 -v -run '^TestParity' ./internal/parity/

# Run locally (no container) - useful for fast iteration. Defaults to
# path mode so wildcard DNS isn't required. The Worker runs beside the
# daemon under Miniflare, persisting under ./data/worker.
dev run: bundle worker/localrt/node_modules
	HOSTTHIS_URL_MODE=path \
	HOSTTHIS_PUBLIC_SCHEME=http \
	HOSTTHIS_APEX_DOMAIN=localhost:8080 \
	HOSTTHIS_DATA_DIR=./data \
	HOSTTHIS_LANDING=./web/landing.html \
	./scripts/dev.sh

worker/localrt/node_modules: worker/localrt/package-lock.json
	cd worker/localrt && npm ci
	@touch $@

# Standalone smoke target - runs against whatever HOSTTHIS_HOST is set
# to (the script defaults it to hostthis.dev). Useful for ad-hoc
# verification + run by the operator's deploy as a post-deploy check.
smoke:
	./scripts/smoke.sh

# -- e2e (browser) -----------------------------------------------------------

# The browser suite is a Playwright project under e2e/, run in Chromium and
# WebKit. E2E_FLAGS passes through to `playwright test`, e.g.
# make e2e E2E_FLAGS='tests/mermaid.spec.ts --project webkit'.
# E2E_READS=worker reads every paste from the Worker's public surface
# instead of hostthisd, into e2e/playwright-report-worker.
e2e: require-node e2e/node_modules bundle worker/localrt/node_modules
	cd e2e && npx playwright test $(E2E_FLAGS)

# CI installs the browsers with their system libraries first. The HTML report
# lands in e2e/playwright-report, and a failing suite still writes it before
# make fails. PLAYWRIGHT_DEPS= skips the system libraries (apt) where they are
# already installed, as in the Playwright image the PR workflow runs in; the
# install is then a no-op for browsers the image already holds.
PLAYWRIGHT_DEPS ?= --with-deps
e2e-ci: require-node e2e/node_modules bundle worker/localrt/node_modules
	cd e2e && npx playwright install $(PLAYWRIGHT_DEPS) chromium webkit
	cd e2e && npx playwright test $(E2E_FLAGS)

e2e/node_modules: e2e/package-lock.json
	cd e2e && npm ci
	@touch $@

fmt:
	gofmt -s -w .

# Every file must be gofmt-clean.
fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then \
		echo "These files are not gofmt-clean (run 'gofmt -w .'):"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

# 'go fix -diff' prints pending modernizations without applying them; any
# output means the tree has idioms 'go fix ./...' would rewrite.
fix-check:
	@out=$$(go fix -diff ./... 2>/dev/null); if [ -n "$$out" ]; then \
		echo "Pending modernizations (run 'go fix ./...' and commit):"; echo "$$out"; exit 1; fi

# What CI's main job gates on, in one command. CI runs these same targets.
# The browser suite (make e2e), the zstd module rebuild and the celld
# contracts are separate CI jobs.
check: fmt-check vet fix-check test vuln

# Fails when a known vulnerability is reachable from this module's code.
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

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

# Compose mounts ./data into the container under distroless's nonroot uid
# (65532). Make sure the host dir is writable by that uid.
data-dir-perms:
	@mkdir -p ./data
	@if [ "$$(uname)" = "Linux" ]; then sudo chown -R 65532:65532 ./data; fi

# Screenshots accumulate across runs, so a renamed flow leaves a directory the
# report would list as unmatched. Removing them is how a local run starts clean.
clean:
	rm -rf bin data worker/dist e2e/playwright-report e2e/test-results e2e/playwright-report-worker e2e/test-results-worker
