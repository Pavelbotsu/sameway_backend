#!/usr/bin/env bash
# Restore a backup made by the `backup` service (scripts/backup-loop.sh).
#
#   scripts/restore.sh drill  backups/rideshare-YYYYMMDD-HHMMSS.dump
#       Restore drill: loads the dump into a THROWAWAY container, prints row
#       counts and the time it took, then removes the container. Touches
#       nothing in production. Run it monthly — a backup you have never
#       restored is not a backup.
#
#   RESTORE_CONFIRM=yes scripts/restore.sh restore backups/rideshare-….dump
#       Real restore into the running prod database (docker-compose.prod.yml).
#       Replaces the current data. Stop the app first:
#         docker compose -f docker-compose.prod.yml stop app
set -euo pipefail

mode="${1:-}"
dump="${2:-}"
if [[ -z "$mode" || -z "$dump" || ! -f "$dump" ]]; then
  echo "usage: $0 drill|restore <dump file>" >&2
  exit 2
fi
image="postgis/postgis:15-3.3"
counts_sql="SELECT 'users', COUNT(*) FROM users
UNION ALL SELECT 'ride_requests', COUNT(*) FROM ride_requests
UNION ALL SELECT 'completed_trips', COUNT(*) FROM completed_trips
UNION ALL SELECT 'messages', COUNT(*) FROM messages;"

case "$mode" in
  drill)
    name="sameway-restore-drill-$$"
    trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
    start=$(date +%s)
    docker run -d --name "$name" -e POSTGRES_PASSWORD=drill -e POSTGRES_DB=rideshare "$image" >/dev/null
    for _ in $(seq 1 60); do
      docker exec "$name" pg_isready -U postgres -d rideshare >/dev/null 2>&1 && break
      sleep 1
    done
    sleep 2 # the image restarts Postgres once after init
    for _ in $(seq 1 30); do
      docker exec "$name" pg_isready -U postgres -d rideshare >/dev/null 2>&1 && break
      sleep 1
    done
    # Restore into a database made from template0: the postgis image's
    # default database already has the extension schemas (tiger, topology…),
    # which the dump recreates itself.
    docker exec "$name" createdb -U postgres -T template0 restore_drill
    docker exec -i "$name" pg_restore -U postgres -d restore_drill --no-owner --exit-on-error < "$dump"
    docker exec "$name" psql -U postgres -d restore_drill -At -F ' ' -c "$counts_sql"
    echo "restore drill OK in $(( $(date +%s) - start )) s — record the date and duration in the runbook"
    ;;
  restore)
    if [[ "${RESTORE_CONFIRM:-}" != "yes" ]]; then
      echo "refusing: this REPLACES production data. Re-run with RESTORE_CONFIRM=yes" >&2
      exit 1
    fi
    cd "$(dirname "$0")/.."
    # Recreate the database empty (template0) and load the dump into it — the
    # dump carries the schema, extensions and data.
    docker compose -f docker-compose.prod.yml exec -T db \
      psql -U postgres -d postgres -v ON_ERROR_STOP=1 \
      -c 'DROP DATABASE IF EXISTS rideshare WITH (FORCE)' \
      -c 'CREATE DATABASE rideshare TEMPLATE template0'
    docker compose -f docker-compose.prod.yml exec -T db \
      pg_restore -U postgres -d rideshare --no-owner --exit-on-error < "$dump"
    docker compose -f docker-compose.prod.yml exec -T db \
      psql -U postgres -d rideshare -At -F ' ' -c "$counts_sql"
    echo "restored. Start the app: docker compose -f docker-compose.prod.yml start app"
    ;;
  *)
    echo "unknown mode: $mode (use drill or restore)" >&2
    exit 2
    ;;
esac
