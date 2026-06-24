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
