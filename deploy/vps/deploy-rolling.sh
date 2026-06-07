#!/usr/bin/env bash
# Rolling deploy for hostthis on the VPS. Restarts one color at a time,
# waiting for /healthz on each replica before flipping to the next.
# Traefik's active healthcheck routes only to healthy replicas, so HTTP
# traffic is uninterrupted during the deploy.
#
# SSH (port 22) is held by blue only — restarting blue causes a brief
# (~5s) SSH outage during which new ssh connections fail. HTTP path is
# zero-downtime. See infra/TODO.md "SSH cutover for hostthis" for the
# follow-up that fixes this.
#
# Run from the project root ON THE VPS (admin):
#   sudo bash deploy/vps/deploy-rolling.sh
#
# Exit codes:
#   0 — both replicas now on new image, healthy
#   1 — build failed
#   2 — replica failed to become healthy within timeout
set -euo pipefail

COMPOSE="docker compose --env-file deploy/vps/.env -f deploy/vps/compose.yml"
HEALTH_TIMEOUT_S=60
HEALTH_INTERVAL_S=2

cycle_color() {
  local color="$1"
  local container="hostthis-${color}"
  echo
  echo "==> rotating ${color}"
  echo "    rebuilding + recreating ${container}"
  $COMPOSE up -d --no-deps --force-recreate --build "${container}"

  echo "    waiting for /healthz with X-Backend-Color=${color}"
  local elapsed=0
  while true; do
    # Probe via traefik's docker network to the specific replica's container IP.
    # We use docker inspect to get the IP then curl /healthz, asserting that the
    # X-Backend-Color header confirms we hit the right replica.
    local ip
    ip=$(docker inspect "${container}" --format '{{.NetworkSettings.Networks.traefik_proxy.IPAddress}}')
    if [ -n "$ip" ]; then
      local resp
      resp=$(docker run --rm --network traefik_proxy alpine sh -c "wget -q -O- --server-response 'http://${ip}:8080/healthz' 2>&1" || true)
      if echo "$resp" | grep -q "200 OK" && echo "$resp" | grep -qi "X-Backend-Color: ${color}"; then
        echo "    ${color} is healthy at ${ip}"
        break
      fi
    fi
    if (( elapsed >= HEALTH_TIMEOUT_S )); then
      echo "    TIMEOUT: ${color} did not become healthy within ${HEALTH_TIMEOUT_S}s" >&2
      exit 2
    fi
    sleep "$HEALTH_INTERVAL_S"
    elapsed=$(( elapsed + HEALTH_INTERVAL_S ))
  done
}

main() {
  cd "$(dirname "$0")/../.."
  echo "==> building image once"
  $COMPOSE build hostthis-blue >/dev/null

  # Rotate green FIRST. While green restarts, blue handles all traffic
  # (it's the SSH owner so it MUST stay up between cycles to keep SSH).
  # Then rotate blue — during blue's restart, traefik routes HTTP to
  # green and SSH gets the brief outage on :22.
  cycle_color green
  cycle_color blue

  echo
  echo "==> rolling deploy complete"
}

main "$@"
