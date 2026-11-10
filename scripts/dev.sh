#!/usr/bin/env bash
# Local development backend: PostGIS in Docker + the Go server on the host.
#
#   ./scripts/dev.sh            # server on http://localhost:4040
#
# The Android emulator reaches it at http://10.0.2.2:4040 — debug builds of
# the app use that address by default (see lib/core/api_client.dart), so no
# ngrok tunnel is needed for emulator testing. A physical phone still needs a
# tunnel or the machine's LAN IP via --dart-define=API_HOST=...
#
# Overridable: PORT (4040), DB_HOST_PORT (55432 — 5432 is often taken by a
# native Postgres).
set -euo pipefail
cd "$(dirname "$0")/.."

export PORT="${PORT:-4040}"
export DB_HOST_PORT="${DB_HOST_PORT:-55432}"

docker compose -f docker/docker-compose.yml up -d db

echo "waiting for PostGIS on 127.0.0.1:${DB_HOST_PORT}…"
for _ in $(seq 1 60); do
  if docker exec rideshare-db pg_isready -U postgres -d rideshare >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

export APP_ENV=dev
export DB_HOST=127.0.0.1
export DB_PORT="$DB_HOST_PORT"
export DB_USER=postgres
export DB_PASSWORD=password
export DB_NAME=rideshare
export JWT_SECRET="${JWT_SECRET:-local-dev-secret-not-for-prod}"
# Same launch areas as production (Kraków + ~30 km, Poltava oblast).
export SERVICE_AREAS="${SERVICE_AREAS-krakow:49.80,19.45,50.33,20.40;poltava:48.70,32.00,50.95,35.60}"

echo "backend → http://localhost:${PORT} (emulator: http://10.0.2.2:${PORT})"
exec go run .
