#!/bin/sh
# Restore a backup made by backup.sh into this directory's stack:
#   ./restore.sh backups/<UTC time>
# On a new server: extract the bundle, copy the backup directory next to it
# and run this before setup.py; it installs the backup's .env. If a different
# .env already exists here, restoring is refused: the data is encrypted with
# the backup's key.
set -eu
cd "$(dirname "$0")"
src=${1:?usage: ./restore.sh <backup directory>}
[ -s "$src/janus.dump" ] || { echo "restore.sh: $src/janus.dump is missing or empty" >&2; exit 1; }
[ -f "$src/env" ] || { echo "restore.sh: $src/env is missing" >&2; exit 1; }

key() { sed -n 's/^JANUS_ENCRYPTION_KEY=//p' "$1"; }
if [ -f .env ]; then
  if [ "$(key .env)" != "$(key "$src/env")" ]; then
    echo "restore.sh: .env here has a different encryption key than the backup." >&2
    echo "  Restore on a fresh copy: docker compose down -v && rm .env, then run this again." >&2
    exit 1
  fi
else
  (umask 077 && cp "$src/env" .env)
  echo "Installed the backup's .env."
fi

docker compose stop janus 2>/dev/null || true
docker compose up -d --wait postgres
docker compose exec -T postgres psql -U janus -d postgres -v ON_ERROR_STOP=1 -q \
  -c 'DROP DATABASE IF EXISTS janus WITH (FORCE)' -c 'CREATE DATABASE janus OWNER janus'
docker compose exec -T postgres pg_restore -U janus -d janus --no-owner --exit-on-error < "$src/janus.dump"
docker compose up -d
echo "Restored $src. Check: docker compose ps"
