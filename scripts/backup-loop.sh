#!/usr/bin/env bash
# Runs inside the `backup` service of docker-compose.prod.yml.
# Once a day at BACKUP_HOUR_UTC: pg_dump -Fc into /backups, then delete dumps
# older than BACKUP_KEEP_DAYS. Connection comes from PGHOST/PGUSER/PGPASSWORD/
# PGDATABASE. A dump is written to a .partial file first and renamed only when
# complete, so a crash never leaves a truncated file that looks valid.
set -uo pipefail

KEEP_DAYS="${BACKUP_KEEP_DAYS:-14}"
HOUR="${BACKUP_HOUR_UTC:-2}"

backup_once() {
  local stamp file
  stamp="$(date -u +%Y%m%d-%H%M%S)"
  file="/backups/rideshare-${stamp}.dump"
  if pg_dump -Fc --no-owner -f "${file}.partial"; then
    mv "${file}.partial" "$file"
    echo "backup: wrote $file ($(du -h "$file" | cut -f1))"
    # Profile photos live outside the database; archive them alongside.
    if [ -d /photos ] && tar -czf "/backups/photos-${stamp}.tar.gz.partial" -C /photos . ; then
      mv "/backups/photos-${stamp}.tar.gz.partial" "/backups/photos-${stamp}.tar.gz"
    else
      rm -f "/backups/photos-${stamp}.tar.gz.partial"
    fi
    find /backups -name 'rideshare-*.dump' -mtime "+${KEEP_DAYS}" -print -delete
    find /backups -name 'photos-*.tar.gz' -mtime "+${KEEP_DAYS}" -print -delete
  else
    rm -f "${file}.partial"
    echo "backup: pg_dump FAILED at ${stamp}" >&2
  fi
}

# One immediately at start-up, so a fresh deploy is never without a backup.
backup_once
while true; do
  now=$(date -u +%s)
  next=$(date -u -d "today ${HOUR}:00" +%s)
  if [ "$next" -le "$now" ]; then
    next=$(date -u -d "tomorrow ${HOUR}:00" +%s)
  fi
  sleep $((next - now))
  backup_once
done
