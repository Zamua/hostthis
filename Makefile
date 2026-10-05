.PHONY: help build test smoke dev run docker-build docker-up docker-down \
        e2e e2e-ci \
        fmt vet clean data-dir-perms rebuild-site-fixtures

# Default goal: show the help text rather than silently no-op.
.DEFAULT_GOAL := help

# Developer-facing targets only. Operator deploy targets (deploy-*,
# logs-*, promote, etc.) live in the operator's private deploy repo.
# This file ships in the public repo and stays clean of operator
# paths / ssh / sudo.

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
	@echo "  make fmt / vet     gofmt / go vet"
	@echo "  make rebuild-site-fixtures  rebuild the vite SPA test fixtures (needs npm)"
	@echo "  make clean         remove ./bin, ./data and the e2e output"
	@echo
	@echo "Deploy targets live in the operator's private deploy repo."

# -- local Go ----------------------------------------------------------------

build:
	go build -o bin/hostthisd ./cmd/hostthisd

# Go tests run the celld Worker locally (internal/celldtest), so they need its
# runtime installed.
test: celld/localrt/node_modules
	go test ./...
	npm --prefix celld test
	./scripts/test-repo-contracts.sh

# Run locally (no container) - useful for fast iteration. Defaults to
# path mode so wildcard DNS isn't required. The celld Worker runs beside the
# daemon under Miniflare, persisting under ./data/celld.
dev run: celld/localrt/node_modules
	HOSTTHIS_URL_MODE=path \
	HOSTTHIS_PUBLIC_SCHEME=http \
	HOSTTHIS_APEX_DOMAIN=localhost:8080 \
	HOSTTHIS_DATA_DIR=./data \
	HOSTTHIS_LANDING=./web/landing.html \
	./scripts/dev.sh

celld/localrt/node_modules: celld/localrt/package-lock.json
	cd celld/localrt && npm ci
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
e2e: e2e/node_modules celld/localrt/node_modules
	cd e2e && npx playwright test $(E2E_FLAGS)

# CI installs the browsers with their system libraries first. The HTML report
# lands in e2e/playwright-report, and a failing suite still writes it before
# make fails.
e2e-ci: e2e/node_modules celld/localrt/node_modules
	cd e2e && npx playwright install --with-deps chromium webkit
	cd e2e && npx playwright test $(E2E_FLAGS)

e2e/node_modules: e2e/package-lock.json
	cd e2e && npm ci
	@touch $@

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

# Compose mounts ./data into the container under distroless's nonroot uid
# (65532). Make sure the host dir is writable by that uid.
data-dir-perms:
	@mkdir -p ./data
	@if [ "$$(uname)" = "Linux" ]; then sudo chown -R 65532:65532 ./data; fi

# Screenshots accumulate across runs, so a renamed flow leaves a directory the
# report would list as unmatched. Removing them is how a local run starts clean.
clean:
	rm -rf bin data e2e/playwright-report e2e/test-results
