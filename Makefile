.PHONY: build test run docker-build docker-up docker-down deploy deploy-build deploy-restart deploy-logs deploy-down clean fmt vet dev-minio-up dev-minio-down test-s3 blob-migrate blob-verify

# -- local Go ----------------------------------------------------------------

build:
	go build -o bin/hostthisd ./cmd/hostthisd

test:
	go test ./...

# Run locally (no container) — useful for fast iteration. Defaults to
# path mode so wildcard DNS isn't required.
run:
	HOSTTHIS_URL_MODE=path \
	HOSTTHIS_PUBLIC_SCHEME=http \
	HOSTTHIS_APEX_DOMAIN=localhost:8080 \
	HOSTTHIS_DATA_DIR=./data \
	HOSTTHIS_LANDING=./web/landing.html \
	go run ./cmd/hostthisd

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

# -- Remote deploy -----------------------------------------------------------
# Targets a single-host install over ssh. Override the VPS_* variables on
# the command line or in a local (untracked) `.env.deploy` you source first.
#
# Assumes:
#  - You can `ssh $(VPS_HOST)` and `sudo` on the far end.
#  - deploy/vps/compose.yml maps host :22→container :2222 and
#    127.0.0.1:8080→container :8080.
#  - A TLS-terminating reverse proxy terminates HTTPS for your apex
#    domain and proxies /p/ to 127.0.0.1:8080.
#
# Typical flow:
#   make deploy VPS_HOST=myvps VPS_PATH=/opt/hostthis VPS_USER=apps
#   make deploy-logs VPS_HOST=myvps

VPS_HOST ?=
VPS_PATH ?= /opt/hostthis
VPS_USER ?= $(shell whoami)

# Distroless 'nonroot' uid the hostthis container runs as.
# Don't change unless you know why.
CONTAINER_UID ?= 65532

deploy: _require-vps-host deploy-sync deploy-build deploy-restart deploy-smoke
	@echo "deployed; check with 'make deploy-logs VPS_HOST=$(VPS_HOST)'"

# Run the verb-level smoke test against the live URL after every
# deploy. Pass HOSTTHIS_HOST to target a non-default host.
deploy-smoke: _require-apex
	@echo
	@echo "=== smoke-testing deployed instance ==="
	@sleep 3   # give the new container a beat to start listening
	HOSTTHIS_HOST="$(HOSTTHIS_APEX_DOMAIN)" ./scripts/smoke.sh

# Standalone smoke target — runs against whatever HOSTTHIS_HOST is set
# to (defaults to hostthis.dev). Useful for ad-hoc verification.
smoke:
	HOSTTHIS_HOST="$(or $(HOSTTHIS_HOST),hostthis.dev)" ./scripts/smoke.sh

# Push the local working tree to the VPS, excluding build/data artifacts.
# Re-chowns the checkout to VPS_USER and the data dir to the container uid.
# Also preserves deploy/vps/.env (operator-managed secrets like MinIO creds)
# and the minio-data volume across rsyncs.
deploy-sync: _require-vps-host
	rsync -az --delete \
	  --exclude='/data' --exclude='/bin' --exclude='/.git/objects' --exclude='*.log' \
	  --exclude='/deploy/vps/.env' \
	  ./ $(VPS_HOST):/tmp/hostthis-staging/
	ssh $(VPS_HOST) "sudo mkdir -p $(VPS_PATH)/data && sudo rsync -a --delete /tmp/hostthis-staging/ $(VPS_PATH)/ --exclude=/data --exclude=/deploy/vps/.env && sudo chown -R $(VPS_USER):$(VPS_USER) $(VPS_PATH) && sudo chown -R $(CONTAINER_UID):$(CONTAINER_UID) $(VPS_PATH)/data"

# Build the env-var prefix once. Sets the runtime config the compose
# file reads (apex, mode, scheme) plus the absolute data path so the
# volume mount doesn't depend on cwd.
DEPLOY_ENV = HOSTTHIS_APEX_DOMAIN='$(HOSTTHIS_APEX_DOMAIN)' \
             HOSTTHIS_URL_MODE='$(or $(HOSTTHIS_URL_MODE),subdomain)' \
             HOSTTHIS_PUBLIC_SCHEME='$(or $(HOSTTHIS_PUBLIC_SCHEME),https)' \
             HOSTTHIS_DATA_PATH='$(VPS_PATH)/data'

deploy-build: _require-vps-host _require-apex
	ssh $(VPS_HOST) "cd $(VPS_PATH) && sudo $(DEPLOY_ENV) docker compose --env-file deploy/vps/.env -f deploy/vps/compose.yml build"

deploy-restart: _require-vps-host _require-apex
	ssh $(VPS_HOST) "cd $(VPS_PATH) && sudo $(DEPLOY_ENV) docker compose --env-file deploy/vps/.env -f deploy/vps/compose.yml up -d --remove-orphans"

deploy-logs: _require-vps-host
	ssh $(VPS_HOST) "sudo docker logs -f --tail 60 hostthis"

deploy-down: _require-vps-host
	ssh $(VPS_HOST) "cd $(VPS_PATH) && sudo docker compose -f deploy/vps/compose.yml down"

_require-vps-host:
	@if [ -z "$(VPS_HOST)" ]; then \
	  echo "VPS_HOST is required. Pass it like: make deploy VPS_HOST=myvps"; \
	  exit 1; \
	fi

_require-apex:
	@if [ -z "$(HOSTTHIS_APEX_DOMAIN)" ]; then \
	  echo "HOSTTHIS_APEX_DOMAIN is required. Pass it like: make deploy HOSTTHIS_APEX_DOMAIN=example.com"; \
	  exit 1; \
	fi

clean:
	rm -rf bin data
