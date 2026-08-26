# Deployment

Production runs on the Hetzner web box `87.99.154.101` (private `10.0.0.2`).
Postgres runs on the db box `10.0.0.3` over the private network `dwellings-net`.

## Pipeline
Push to `main` → GitHub Actions (`.github/workflows/publish-deploy.yml`):
1. Build `linux/amd64`, push `ghcr.io/<owner>/dwellings:{latest,sha-<short>}`.
2. SSH to the web box, `docker compose -f compose.prod.yml pull && up -d`.

## Server layout
- `/opt/dwellings/compose.prod.yml` — app-only compose (concrete image owner).
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
