.PHONY: build test run docker-build docker-up docker-down deploy deploy-build deploy-restart deploy-logs deploy-down clean fmt vet

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
#  - nginx (or any other front door) terminates TLS for your apex
#    domain and proxies /p/ to 127.0.0.1:8080.
#
# Typical flow:
#   make deploy VPS_HOST=myvps VPS_PATH=/opt/hostthis VPS_USER=apps
#   make deploy-logs VPS_HOST=myvps

VPS_HOST ?=
VPS_PATH ?= /opt/hostthis
VPS_USER ?= $(shell whoami)

# Distroless 'nonroot' uid the container runs as. Don't change unless
# you know why.
CONTAINER_UID ?= 65532

deploy: _require-vps-host deploy-sync deploy-build deploy-restart
	@echo "deployed; check with 'make deploy-logs VPS_HOST=$(VPS_HOST)'"

# Push the local working tree to the VPS, excluding build/data artifacts.
# Re-chowns the checkout to VPS_USER and the data dir to the container uid.
deploy-sync: _require-vps-host
	rsync -az --delete \
	  --exclude='/data' --exclude='/bin' --exclude='/.git/objects' --exclude='*.log' \
	  ./ $(VPS_HOST):/tmp/hostthis-staging/
	ssh $(VPS_HOST) "sudo mkdir -p $(VPS_PATH)/data && sudo rsync -a --delete /tmp/hostthis-staging/ $(VPS_PATH)/ --exclude=/data && sudo chown -R $(VPS_USER):$(VPS_USER) $(VPS_PATH) && sudo chown -R $(CONTAINER_UID):$(CONTAINER_UID) $(VPS_PATH)/data"

deploy-build: _require-vps-host
	ssh $(VPS_HOST) "cd $(VPS_PATH) && sudo docker compose -f deploy/vps/compose.yml build"

deploy-restart: _require-vps-host
	ssh $(VPS_HOST) "cd $(VPS_PATH) && sudo docker compose -f deploy/vps/compose.yml up -d"

deploy-logs: _require-vps-host
	ssh $(VPS_HOST) "sudo docker logs -f --tail 60 hostthis"

deploy-down: _require-vps-host
	ssh $(VPS_HOST) "cd $(VPS_PATH) && sudo docker compose -f deploy/vps/compose.yml down"

_require-vps-host:
	@if [ -z "$(VPS_HOST)" ]; then \
	  echo "VPS_HOST is required. Pass it like: make deploy VPS_HOST=myvps"; \
	  exit 1; \
	fi

clean:
	rm -rf bin data
