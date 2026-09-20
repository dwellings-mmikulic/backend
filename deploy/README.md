# Deployment

Production runs on the Hetzner web box `87.99.154.101` (private `10.0.0.2`).
Postgres runs on the db box `10.0.0.3` over the private network `dwellings-net`.

## Pipeline
Push to `main` → GitHub Actions (`.github/workflows/publish-deploy.yml`):
1. Build `linux/amd64`, push `ghcr.io/<owner>/dwellings:{latest,sha-<short>}`.
2. SSH to the web box, `docker compose -f compose.prod.yml pull && up -d`.
3. For every host in the repository variable `WORKER_HOSTS` (skipped while it
   is empty): SSH through the web box, `compose.worker.yml pull && up -d`.
   See "Fleet" below.

## Server layout
- `/opt/dwellings/compose.prod.yml` — app-only compose (concrete image owner).
  Mounts `/opt/dwellings/geoip` read-only at `/geoip` for the MaxMind
  GeoLite2-City database (`GEOIP_DB_PATH=/geoip/GeoLite2-City.mmdb`).
  Refresh it weekly with `geoipupdate` (apt package; `/etc/GeoIP.conf` holds
  the MaxMind AccountID/LicenseKey and `EditionIDs GeoLite2-City`,
  `DatabaseDirectory /opt/dwellings/geoip`). Without the file the app logs
  a warning and `/channels/resolve` skips the geo step.
- `/opt/dwellings/.env` — runtime secrets, `chmod 600`, NOT in git.
- nginx: `/etc/nginx/sites-available/dwellings` → proxies `:80` to `127.0.0.1:8080`.

## Secret locations (never commit values)
- DB password: `~/.config/dwellings/db-credentials` (local) → server `/opt/dwellings/.env`.
- GHCR read PAT: server `docker login ghcr.io` (`/root/.docker/config.json`).
- CI deploy key + SSH host: GitHub repo secrets `SSH_PRIVATE_KEY`, `SSH_HOST`, `SSH_USER`.

## Channel playlist cache (nginx)

`deploy/nginx-dwellings.conf` puts an nginx `proxy_cache`
(`dwellings_channels`, `/var/cache/nginx/dwellings`, 10 MB of keys) in front
of `/channels/`: 2 s for `live.m3u8` and `master.m3u8`, 60 s for `epg.json`,
keyed on `$scheme$host$uri$is_args$args` so each channel filter is its own
entry. `proxy_cache_lock on` collapses a thundering herd on a cold key into
one origin request, and `proxy_cache_use_stale updating error timeout` keeps
serving the last playlist through a restart or a slow query.

**This cache is what makes the design's playlist TTLs real.** A playlist is a
pure function of (channel, wall clock), so every viewer of a channel gets the
same bytes in the same second; without the cache the advertised
`Cache-Control` only binds well-behaved clients, and every player would reach
Go every couple of seconds. Create the directory before reloading nginx:

```bash
mkdir -p /var/cache/nginx/dwellings && chown www-data:www-data /var/cache/nginx/dwellings
nginx -t && systemctl reload nginx
curl -sI https://api.dwellings.tv/channels/live.m3u8 | grep -i x-cache-status
```

## Bunny CDN (HLS segments)

Bunny fronts **only the segments** (`hls/v1/<zpid>/<hash8>/seg-NNN.ts` and
`index.m3u8`) — never the playlists, which are dynamic and served by the
origin above. Segment paths are immutable (a re-render writes a new `hash8`
prefix), so the pull zone should:

- cache `.ts` for a long time (a year is fine; the URL changes when the
  content does);
- send `Access-Control-Allow-Origin: *` if browser-based players are wanted
  (Roku and native players do not need it).

**Retention:** segments under superseded prefixes are kept. Old lineup
versions reference them by `video_hls.id`, and nothing reaps them yet, so
storage grows with the number of re-renders. Deleting a prefix that a stored
lineup still points at makes that channel 500 until the lineup rolls over.

## Fleet (worker boxes)

Any number of instances may run against the one Postgres. They coordinate
through it: a ZIP, a queued listing and a details batch are each taken with an
atomic, expiring claim, and every paid OpenWebNinja request is reserved from a
fleet-wide budget ledger first. Design:
`docs/superpowers/specs/2026-09-19-multi-instance-workers-design.md`.

| Role | Runs | Where |
|---|---|---|
| `all` (default) | API + the three worker loops | the single-box deployment |
| `api` | HTTP only | the web box once workers exist and it should stop rendering |
| `worker` | discovery, media and details loops, `/healthz` only | worker boxes, `compose.worker.yml` |

### Provisioning a worker box

1. Attach it to the private network `dwellings-net`; open the db box firewall
   and `pg_hba.conf` for its `10.0.0.x` address (`DATABASE_URL` points at
   `10.0.0.3` with `sslmode=disable`).
2. Docker + Compose. Put the CI deploy public key (`dwellings_ci_deploy.pub`)
   in `/root/.ssh/authorized_keys`. Workers need no public SSH: CI reaches
   them through the web box as a jump host.
3. `/opt/dwellings/compose.worker.yml` (from this repo),
   `/opt/dwellings/.env.fleet` (**byte-identical on every box** — compare
   `sha256sum`) and `/opt/dwellings/.env.host` (`INSTANCE_ID`,
   `DB_MAX_CONNS`), both `chmod 600`. GHCR credentials are not needed on the
   box: the CI job logs in per run with its own token. For a manual pull,
   `docker login ghcr.io` with a read PAT first.
4. Add its private address to the repository variable `WORKER_HOSTS`, a JSON
   array such as `["10.0.0.11","10.0.0.12"]`. The `deploy-workers` job is
   skipped while the variable is empty.

### Connection budget

Every instance opens up to `DB_MAX_CONNS` connections (default 10; workers
ship with 6; each backfill tool uses 4):

    sum(DB_MAX_CONNS) + 4 per running backfill tool + psql headroom  <=  max_connections - 3

Ten workers at 6 plus the web box at 10 is 70. Raise `max_connections` on the
db box before going past the default 100. PgBouncer in transaction mode is
**not** compatible with the advisory locks used at startup.

### Must-match variables

`CRON_SCHEDULE`, `API_BUDGET_PER_CYCLE`, `DETAILS_PER_CYCLE`, `SEARCH_*`,
`SKIP_EXISTING`, `IMAGES_ENABLED`, `VIDEO_ENABLED`, `VIDEO_SECONDS_PER_PHOTO`,
`LINEAR_ENABLED` and the API/Bunny keys. The budget ledger's windows come from
`CRON_SCHEDULE` and its effective limit is the **largest**
`API_BUDGET_PER_CYCLE` any box passes: a box with a different schedule gets a
second ledger, a box with a stale higher limit keeps the whole fleet spending
at it. Every instance logs one `effective fleet config` line at boot — grep
it across boxes to find drift. While the web box runs `ROLE=all` its `.env`
must carry the same values.

### Rollout order (first time)

1. Deploy the new image to the **existing** box first and copy the new
   `compose.prod.yml` to it (CI only runs `pull` + `up -d`; the file on the
   box is hand-maintained, and the `stop_grace_period` in it matters). It
   migrates the schema. Remove `SEARCH_MAX_RESULTS=50` and the dead
   `SEARCH_LOCATION` from its `.env` if they are still there.
2. Only then start workers. An image from before this change ignores claims
   and the ledger entirely.
3. Optionally switch the web box to `ROLE=api` so it stops rendering.

**Rollback warning:** never run a pre-claims image while workers are running
— it would search the ZIPs they hold, render without claims, and spend
outside the ledger. Stop the workers first, or start the old image with
`API_BUDGET_PER_CYCLE=0` and `DETAILS_PER_CYCLE=0`, which makes it API-only.

### Draining a box

`docker compose -f compose.worker.yml stop` — the worker kills its renders and
hands every claim back within the 60 s grace period — **and** remove the host
from `WORKER_HOSTS`, or the next push to `main` starts it again.

### Runbook (psql on the db box)

```sql
-- Queue at a glance. 3 = workqueue.MaxAttempts.
SELECT count(*) FILTER (WHERE claimed_until > now())                                          AS claimed,
       count(*) FILTER (WHERE (claimed_until IS NULL OR claimed_until <= now())
                          AND attempts < 3 AND available_at <= now())                         AS claimable,
       count(*) FILTER (WHERE (claimed_until IS NULL OR claimed_until <= now())
                          AND attempts < 3 AND available_at >  now())                         AS backoff,
       count(*) FILTER (WHERE (claimed_until IS NULL OR claimed_until <= now())
                          AND attempts >= 3)                                                  AS dead
  FROM listing_queue;
-- (same numbers: docker compose run --rm --entrypoint /app/backfill-videos app -status)

-- Who holds what. A box missing from this list while rows are claimable is
-- broken (check `docker ps` health and its logs for "circuit breaker").
SELECT split_part(claimed_by, '/', 1) AS instance, count(*), min(claimed_until) AS first_expiry
  FROM listing_queue WHERE claimed_until > now() GROUP BY 1 ORDER BY 1;
SELECT zip, claimed_by, claimed_until, failures, resume_page
  FROM zip_codes WHERE claimed_until > now() ORDER BY claimed_until;

-- Why items died.
SELECT left(last_error, 80) AS error, count(*)
  FROM listing_queue
 WHERE attempts >= 3 AND (claimed_until IS NULL OR claimed_until <= now())
 GROUP BY 1 ORDER BY 2 DESC;

-- Requeue dead items after fixing the cause (a Bunny outage, a blocked IP).
UPDATE listing_queue SET attempts = 0, available_at = now(), last_error = NULL
 WHERE attempts >= 3 AND (claimed_until IS NULL OR claimed_until <= now());

-- Budget spent in recent windows, fleet-wide.
SELECT window_start, kind, spent FROM api_budget ORDER BY window_start DESC, kind LIMIT 8;

-- Details rows given up on (5 = property.MaxDetailsAttempts), and how to retry them.
SELECT count(*) FROM properties WHERE details_fetched_at IS NULL AND details_attempts >= 5;
UPDATE properties SET details_attempts = 0, details_claimed_until = NULL
 WHERE details_fetched_at IS NULL AND details_attempts >= 5;

-- ZIPs parked by failures or by a budget that ran out mid-search.
SELECT zip, failures, resume_page, claimed_until FROM zip_codes
 WHERE claimed_until > now() AND claimed_by IS NULL ORDER BY claimed_until LIMIT 50;
```

Each worker also logs one `worker status` line per minute (in-flight,
completed, failed, breaker state, queue depth).

### Before a national pass

The capacity numbers in the design are estimates. Time one long listing
(~190 photos) rendering on a worker box at `LISTING_CONCURRENCY=4`: it must
finish well inside the 60-minute listing deadline. And size the OpenWebNinja
plan first: 1M listings is about 1.05M requests with details enrichment, about
40–70k with `DETAILS_PER_CYCLE=0`.

## Manual deploy / rollback
```bash
ssh -i ~/.ssh/dwellings_tv root@87.99.154.101
cd /opt/dwellings
docker compose -f compose.prod.yml pull          # latest
docker compose -f compose.prod.yml up -d
# rollback to a specific build:
docker pull ghcr.io/<owner>/dwellings:sha-<short>
docker tag  ghcr.io/<owner>/dwellings:sha-<short> ghcr.io/<owner>/dwellings:latest
docker compose -f compose.prod.yml up -d
```
