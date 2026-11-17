#!/bin/sh
# Back up Janus: a PostgreSQL dump plus a copy of .env (the dump is useless
# without its JANUS_ENCRYPTION_KEY). Janus keeps running.
#   ./backup.sh                 -> backups/<UTC time>/
#   ./backup.sh /srv/janus-bak  -> /srv/janus-bak/<UTC time>/
# Example cron line (daily at 02:30):
#   30 2 * * * cd /opt/janus-compose && ./backup.sh >/dev/null
set -eu
cd "$(dirname "$0")"
[ -f .env ] || { echo "backup.sh: no .env here; run python3 setup.py first" >&2; exit 1; }
root=${1:-backups}
dir="$root/$(date -u +%Y%m%dT%H%M%SZ)"
umask 077
mkdir -p "$dir"
docker compose exec -T postgres pg_dump -U janus -d janus --format=custom > "$dir/janus.dump"
cp .env "$dir/env"
if [ ! -s "$dir/janus.dump" ]; then
  echo "backup.sh: the dump is empty; see docker compose logs postgres" >&2
  exit 1
fi
echo "Backed up to $dir (janus.dump + env). Keep both private and together."
