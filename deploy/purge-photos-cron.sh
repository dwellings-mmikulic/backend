#!/usr/bin/env bash
# Daily photo purge sweep on web-01 (root crontab, see deploy/README.md).
#
# Purges the CDN photos but the first of every listing whose video has been
# ready for at least an hour (-settled, the tool's default), which is what the
# workers' own purge after each render leaves behind: a purge cut short, a
# Bunny hiccup. Listings whose video is pending or failed are never read by
# the tool, whatever this script does.
#
# Installed at /opt/dwellings/purge-photos-cron.sh. One run at a time: the
# flock guards against the previous day's run still going, and the pgrep
# against a manual sweep (compose run --entrypoint /app/purge-photos).
set -u
cd /opt/dwellings || exit 1

LOG=/var/log/purge-photos-cron.log
exec >>"$LOG" 2>&1 </dev/null

exec 9>/run/lock/purge-photos.lock
if ! flock -n 9; then
  echo "$(date -u +%FT%TZ) skipped: a sweep is already running"
  exit 0
fi
if pgrep -f '/app/purge-photos' >/dev/null; then
  echo "$(date -u +%FT%TZ) skipped: a manual sweep is running"
  exit 0
fi

echo "$(date -u +%FT%TZ) start"
docker compose -f compose.prod.yml run --rm -T --name purge-photos-cron \
  --entrypoint /app/purge-photos app -workers 16 -concurrency 8
echo "$(date -u +%FT%TZ) end (exit $?)"
