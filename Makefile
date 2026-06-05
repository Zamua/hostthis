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

# -- VPS deploy --------------------------------------------------------------
# Assumes:
#  - `ssh vps` alias in ~/.ssh/config reaches admin@<vps>:2222
#  - Working tree on the VPS lives at /home/apps/projects/hostthis/
#  - deploy/vps/compose.yml maps host :22→container :2222 and
#    127.0.0.1:8080→container :8080
#  - nginx already terminates TLS for hostthis.dev and proxies /p/ to :8080
#
# Typical flow: deploy (rsync + rebuild + restart) → deploy-logs to verify.

VPS_HOST    ?= vps
VPS_PATH    ?= /home/apps/projects/hostthis
VPS_COMPOSE ?= $(VPS_PATH)/deploy/vps/compose.yml

deploy: deploy-sync deploy-build deploy-restart
	@echo "deployed; check with 'make deploy-logs'"

# Push the local working tree to the VPS, excluding build/data artifacts.
# Re-chowns the checkout to apps and the data dir to distroless nonroot.
deploy-sync:
	rsync -az --delete \
	  --exclude='/data' --exclude='/bin' --exclude='/.git/objects' --exclude='*.log' \
	  ./ $(VPS_HOST):/tmp/hostthis-staging/
	ssh $(VPS_HOST) "sudo rsync -a --delete /tmp/hostthis-staging/ $(VPS_PATH)/ --exclude=/data && sudo chown -R apps:apps $(VPS_PATH) && sudo chown -R 65532:65532 $(VPS_PATH)/data"

deploy-build:
	ssh $(VPS_HOST) "cd $(VPS_PATH) && sudo docker compose -f deploy/vps/compose.yml build"

deploy-restart:
	ssh $(VPS_HOST) "cd $(VPS_PATH) && sudo docker compose -f deploy/vps/compose.yml up -d"

deploy-logs:
	ssh $(VPS_HOST) "sudo docker logs -f --tail 60 hostthis"

deploy-down:
	ssh $(VPS_HOST) "cd $(VPS_PATH) && sudo docker compose -f deploy/vps/compose.yml down"

clean:
	rm -rf bin data
