#!/usr/bin/env bash
# Prints a snapshot of the production listing pipeline (discovery -> details -> render).
# Runs psql on web-01 against the private-net Postgres. Read-only.
#
# Usage: bash .claude/skills/processing-stats/stats.sh [--errors]
#   --errors   also print the distinct error texts behind stuck queue items
set -euo pipefail

WEB_HOST="${DWELLINGS_WEB_HOST:-87.99.154.101}"
SSH_KEY="${DWELLINGS_SSH_KEY:-$HOME/.ssh/dwellings_tv}"
SHOW_ERRORS=0
[[ "${1:-}" == "--errors" ]] && SHOW_ERRORS=1

# NOTE: do NOT `source /opt/dwellings/.env` on the box - line 2 is a cron
# expression (*/12 ...) and bash tries to execute it. grep the one key instead.
read -r -d '' REMOTE <<'REMOTE_EOF' || true
DBURL=$(grep "^DATABASE_URL=" /opt/dwellings/.env | cut -d= -f2-)
P() { docker run --rm --network host -e PGCONNECT_TIMEOUT=10 postgres:16 psql "$DBURL" -X "$@"; }

echo "== containers on web-01 =="
docker ps --format "{{.Names}}  {{.Status}}"

P \
 -c "select now() as db_now" \
 -c "select count(*) total,
            count(*) filter (where video_status='ready')   ready,
            count(*) filter (where video_status='pending') pending,
            count(*) filter (where video_status='failed')  failed,
            count(*) filter (where details_fetched_at is not null) details_done
     from properties" \
 -c "select count(*) filter (where video_rendered_at  > now()-interval '1 hour')  renders_1h,
            count(*) filter (where video_rendered_at  > now()-interval '24 hours') renders_24h,
            count(*) filter (where created_at         > now()-interval '24 hours') new_24h,
            count(*) filter (where details_fetched_at > now()-interval '24 hours') details_24h,
            max(video_rendered_at) last_render,
            max(created_at)        last_new
     from properties" \
 -c "select date_trunc('day', video_rendered_at)::date as render_day, count(*) renders
     from properties where video_rendered_at > now()-interval '7 days'
     group by 1 order by 1" \
 -c "select count(*) depth,
            count(*) filter (where claimed_until > now()) claimed_now,
            count(*) filter (where attempts >= 3) stuck,
            min(enqueued_at) filter (where attempts < 3)  oldest_live,
            min(enqueued_at) filter (where attempts >= 3) oldest_stuck
     from listing_queue" \
 -c "select claimed_by, count(*) claims from listing_queue
     where claimed_until > now() group by 1 order by 1" \
 -c "select window_start, kind, spent from api_budget
     order by window_start desc, kind limit 6" \
 -c "select count(*) total,
            count(*) filter (where last_searched_at is not null) searched,
            count(*) filter (where claimed_until > now()) claimed_now,
            count(*) filter (where failures > 0) failing,
            max(last_searched_at) last_search
     from zip_codes"

if [ "$SHOW_ERRORS" = "1" ]; then
  echo "== stuck queue errors (attempts >= 3) =="
  P -c "select attempts, left(regexp_replace(regexp_replace(last_error, 'zpid=[0-9]+', 'zpid=N'), 'encode [0-9]+ into', 'encode N into'), 110) err, count(*)
        from listing_queue where attempts >= 3 group by 1,2 order by 3 desc"
fi
REMOTE_EOF

ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no -o ConnectTimeout=15 "root@$WEB_HOST" \
  "SHOW_ERRORS=$SHOW_ERRORS bash -s" <<<"$REMOTE"
