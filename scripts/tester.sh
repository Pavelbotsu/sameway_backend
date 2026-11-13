#!/usr/bin/env bash
# Public tester server: the backend on PORT (7007) behind an ngrok tunnel, so
# testers' phones (Poltava, Krakow) can use a release build of the app.
#
#   ./scripts/tester.sh          # Ctrl-C stops the backend and the tunnel
#
# Differences from scripts/dev.sh, because this one is reachable from the
# internet:
#   - APP_ENV=prod: no debug endpoints, mail bodies (reset codes) not logged,
#     launch service areas (Krakow + ~30 km, Poltava oblast).
#   - A random JWT secret, created once in .tester-jwt-secret (git-ignored).
#     dev.sh's secret is public in the repo: anyone could forge logins.
#   - Self-hosted routing (OSRM) when its data volume exists (built by
#     `docker compose -f docker-compose.prod.yml -p sameway-osrm-trial up
#     osrm-data osrm-build`), otherwise the public OSRM demo.
#
# Release builds of the app use NGROK_DOMAIN by default (lib/core/api_client.dart).
# Run only one backend against the dev database at a time: WebSocket events
# live in the process, so users split across two servers would miss each
# other's offers. The emulator can use this one with
# --dart-define=API_HOST=10.0.2.2:7007.
set -euo pipefail
cd "$(dirname "$0")/.."
export MSYS_NO_PATHCONV=1 # Git Bash on Windows: keep /data paths for docker

export PORT="${PORT:-7007}"
export DB_HOST_PORT="${DB_HOST_PORT:-55432}"
NGROK_DOMAIN="${NGROK_DOMAIN:-ibuprofen-dolly-prison.ngrok-free.dev}"
OSRM_VOLUME="${OSRM_VOLUME:-sameway-osrm-trial_osrm}"
OSRM_IMAGE=ghcr.io/project-osrm/osrm-backend:v6.0.0

# ── database ────────────────────────────────────────────────────────────────
docker compose -f docker/docker-compose.yml up -d db
echo "waiting for PostGIS on 127.0.0.1:${DB_HOST_PORT}..."
for _ in $(seq 1 60); do
  docker exec rideshare-db pg_isready -U postgres -d rideshare >/dev/null 2>&1 && break
  sleep 1
done

# ── routing ─────────────────────────────────────────────────────────────────
start_osrm() { # name port profile
  if docker ps --format '{{.Names}}' | grep -qx "$1"; then return; fi
  if docker ps -a --format '{{.Names}}' | grep -qx "$1"; then
    docker start "$1" >/dev/null
  else
    docker run -d --name "$1" --restart unless-stopped -p "127.0.0.1:$2:5000" \
      -v "$OSRM_VOLUME:/data:ro" "$OSRM_IMAGE" \
      osrm-routed --algorithm mld "/data/$3/region.osrm" >/dev/null
  fi
}
if docker volume inspect "$OSRM_VOLUME" >/dev/null 2>&1; then
  start_osrm sameway-osrm-car 5001 car
  start_osrm sameway-osrm-foot 5002 foot
  for _ in $(seq 1 60); do
    curl -sf -o /dev/null "http://localhost:5001/nearest/v1/driving/19.94,50.06" &&
      curl -sf -o /dev/null "http://localhost:5002/nearest/v1/foot/19.94,50.06" && break
    sleep 1
  done
  export OSRM_BASE_URL=http://localhost:5001
  export OSRM_FOOT_BASE_URL=http://localhost:5002
  echo "routing: self-hosted OSRM (Malopolska + Poltava oblast)"
else
  export OSRM_BASE_URL=http://router.project-osrm.org
  export ALLOW_PUBLIC_OSRM=true
  echo "routing: public OSRM demo (no local map volume $OSRM_VOLUME)"
fi

# ── backend config ──────────────────────────────────────────────────────────
SECRET_FILE=.tester-jwt-secret
if [ ! -s "$SECRET_FILE" ]; then
  head -c 48 /dev/urandom | base64 | tr -d '\n' >"$SECRET_FILE"
  echo "created $SECRET_FILE (keep it: changing it signs every tester out)"
fi
export JWT_SECRET="$(cat "$SECRET_FILE")"
export JWT_TTL_HOURS=168
export APP_ENV=prod
export DB_HOST=127.0.0.1
export DB_PORT="$DB_HOST_PORT"
export DB_USER=postgres
export DB_PASSWORD=password
export DB_NAME=rideshare
unset SERVICE_AREAS # prod default: Krakow + ~30 km, Poltava oblast

# ── tunnel ──────────────────────────────────────────────────────────────────
mkdir -p data
ngrok http --url="$NGROK_DOMAIN" "$PORT" --log=stdout --log-format=logfmt >data/ngrok.log 2>&1 &
NGROK_PID=$!
cleanup() { kill "$NGROK_PID" 2>/dev/null || true; }
trap cleanup EXIT INT TERM

echo "backend -> http://localhost:${PORT}   public -> https://${NGROK_DOMAIN}"
go run .
