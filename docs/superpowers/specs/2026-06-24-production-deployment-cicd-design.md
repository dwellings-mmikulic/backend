# Production Deployment (CI/CD) — Dwellings Backend

**Date:** 2026-06-24
**Status:** Approved design — ready for implementation planning
**Author:** Claude + marko

## Goal

Deploy the Dwellings backend to the existing Hetzner production infrastructure and
establish a continuous-delivery pipeline: pushing to `main` builds a container
image, publishes it to GitHub Container Registry (GHCR), and auto-deploys it to the
web server. This first pass gets the app **live on the public IP**
(`http://87.99.154.101`). Domain (`dwellings.tv`) + HTTPS is a deferred follow-up.

## Background — what already exists

Provisioned earlier (verified live on Hetzner Cloud, Ashburn `ash`):

| Server | Role | Type | Public IP | Private IP | State |
|---|---|---|---|---|---|
| dwellings-web | nginx | cpx31 (4vCPU/8GB) | `87.99.154.101` | `10.0.0.2` | nginx 1.24.0 running (default page); **no Docker yet** |
| dwellings-db | postgres | cpx21 (3vCPU/4GB) | `87.99.154.111` | `10.0.0.3` | `postgres:16` in Docker, **private-only (public 5432 closed)** |

- Private network `dwellings-net` `10.0.0.0/16`; web→db `10.0.0.3:5432` reachable.
- Firewalls: web `22/80/443`; db `22` only.
- SSH: `ssh -i ~/.ssh/dwellings_tv root@87.99.154.101`.
- DB credentials saved locally at `~/.config/dwellings/db-credentials` (user `dwellings`, db `dwellings`).
- Hetzner API token at `~/.config/hetzner/cloud-token`.

### The application
- Go monolith (`./cmd/server`), multi-stage Docker build (`Dockerfile`) on `alpine`
  with `ffmpeg` + DejaVu font for video overlays.
- Serves HTTP on `:8080` — Roku Direct Publisher feed at `/roku/feed.json` and
  `/healthz`.
- Runs an **internal cron scheduler** that fetches Zillow data (OpenWebNinja),
  uploads images/videos to Bunny CDN, and upserts into Postgres.
- DB schema is **auto-migrated on startup** from an embedded `schema.sql`
  (idempotent) — no separate migration step.
- Config via env vars (see `.env.example`). All secrets (`ZILLOW_API_KEY`,
  `BUNNY_API_KEY`, etc.) are populated in the local `.env`, which **is gitignored**.

### Current repo state
- **Not a git repository** and no remote. `.env` is gitignored ✅.
- Local tooling: Docker 29.4 + buildx 0.33 present; `gh` CLI **not** installed;
  Mac is `arm64` (CI must build `linux/amd64`).

## Decisions (locked)

| Decision | Choice |
|---|---|
| Build & publish | **GitHub Actions** (not local machine) |
| Registry | **GHCR** (`ghcr.io`) |
| Image visibility | **Private** |
| Deploy scope | **Full CD** — Actions auto-deploys to the server on push to `main` |
| First-pass scope | **App live on the public IP**; domain + TLS deferred |

## Target architecture

```
git push main ─▶ GitHub Actions
                  ├─ job 1  build-and-push
                  │     buildx --platform linux/amd64
                  │     push ─▶ ghcr.io/<owner>/dwellings  (private)
                  │     tags: latest + <git-sha>
                  │     auth: built-in GITHUB_TOKEN (packages: write)
                  └─ job 2  deploy   (needs: build-and-push)
                        ssh root@87.99.154.101  (appleboy/ssh-action)
                        cd /opt/dwellings
                        docker compose -f compose.prod.yml pull
                        docker compose -f compose.prod.yml up -d
                        docker image prune -f

runtime (web box):
  Internet ─▶ nginx :80 ─▶ 127.0.0.1:8080 (app container)
                                  ├─▶ Postgres 10.0.0.3:5432 (db box, private net)
                                  └─▶ outbound: OpenWebNinja Zillow API + Bunny CDN
                                      internal cron scheduler runs inside the container
```

## Components

### 1. Git + GitHub repository (one-time)
- Add `.idea/` to `.gitignore` (currently only `workspace.xml` is ignored).
- `git init`, initial commit of the source. Confirm `git status`/`git check-ignore`
  shows `.env` excluded **before** the first commit.
- Create a **private** GitHub repo (proposed name: `dwellings-backend`), add remote,
  push `main`.
- `gh` is not installed. Two paths (user picks at execution time):
  - User creates an empty private repo on github.com and provides the URL; we add
    the remote and push, or
  - Install + `gh auth login` (interactive — user runs via `!`), then `gh repo create`.

### 2. CI/CD workflow — `.github/workflows/publish-deploy.yml`
- **Trigger:** `push` to `main` (plus manual `workflow_dispatch`).
- **Job `build-and-push`:**
  - `permissions: { contents: read, packages: write }`
  - `docker/login-action` → `ghcr.io` with `${{ github.actor }}` / `GITHUB_TOKEN`
  - `docker/metadata-action` → tags `latest` and `sha-<short>`
  - `docker/build-push-action` → `platforms: linux/amd64`, `push: true`,
    `cache-from/to: type=gha`
- **Job `deploy`** (`needs: build-and-push`):
  - `appleboy/ssh-action` using repo secrets `SSH_HOST`, `SSH_USER`, `SSH_PRIVATE_KEY`
  - Runs on the server: `cd /opt/dwellings && docker compose -f compose.prod.yml pull
    && docker compose -f compose.prod.yml up -d && docker image prune -f`

**GitHub repo secrets:**
| Secret | Value |
|---|---|
| `SSH_HOST` | `87.99.154.101` |
| `SSH_USER` | `root` |
| `SSH_PRIVATE_KEY` | private half of a **dedicated CI deploy key** (generated for this; not the personal `dwellings_tv` key) |

### 3. Server one-time prep (performed via SSH)
- Install Docker Engine + compose plugin on the web box (`apt` / official convenience script).
- `docker login ghcr.io` on the server with a **read-only PAT** (`read:packages`) so it
  can pull the private image. (Stored in `/root/.docker/config.json` on the box.)
- Create `/opt/dwellings/` containing `compose.prod.yml` and the production `.env`
  (`chmod 600`).
- Append the CI deploy key's public half to `/root/.ssh/authorized_keys`.
- Install the nginx reverse-proxy server block; disable the default site; reload nginx.

### 4. Production config files (committed to the repo)
- **`compose.prod.yml`** — app service **only** (Postgres is the separate db box):
  - `image: ghcr.io/<owner>/dwellings:latest`
  - `env_file: .env`
  - `ports: ["127.0.0.1:8080:8080"]` (bind localhost; nginx proxies; Hetzner firewall
    also blocks 8080 externally)
  - `restart: unless-stopped`
- **`.env.prod.example`** — template (no secret values), documenting the prod
  `DATABASE_URL=postgres://dwellings:<password>@10.0.0.3:5432/dwellings?sslmode=disable`
  and all required Zillow/Bunny/search/video vars.
- **`deploy/nginx-dwellings.conf`** — `listen 80;` → `proxy_pass http://127.0.0.1:8080;`
  with standard proxy headers; forwards `/roku/feed.json` and `/healthz`.
- **`deploy/README.md`** — documents server layout, secret locations, manual
  pull/restart, and rollback.

### 5. Production `.env` on the server (not committed)
- Copy of the working local config with the prod `DATABASE_URL` (private IP, password
  from `~/.config/dwellings/db-credentials`) and all populated secrets. Lives at
  `/opt/dwellings/.env`, `chmod 600`.

## Data flow

1. Developer pushes to `main`.
2. Actions builds `linux/amd64` image, pushes `ghcr.io/<owner>/dwellings:{latest,sha}`.
3. Deploy job SSHes to the web box, pulls the new `latest`, recreates the app container.
4. App starts, connects to Postgres over the private net, auto-applies the schema, and
   begins serving `:8080` + running the cron scheduler.
5. nginx proxies public `:80` traffic to the container.

## Error handling & operations
- `restart: unless-stopped` recovers the container across crashes and reboots; nginx
  runs independently as a system service.
- **Rollback:** on the server, `docker compose -f compose.prod.yml pull` a previous
  `sha-` tag (edit image tag or `docker tag`), then `up -d`. Documented in
  `deploy/README.md`.
- Schema migration is idempotent and safe to re-run on every deploy.
- If the deploy SSH step fails, the image is still published; deploy can be retried via
  `workflow_dispatch` or a manual server pull.

## Verification (first deploy)
- `curl -fsS http://87.99.154.101/healthz` → `200`.
- `curl -fsS http://87.99.154.101/roku/feed.json` → valid feed JSON.
- `docker compose -f compose.prod.yml logs app` shows: DB connection, schema applied,
  and a scheduler cycle (or next scheduled run).
- GitHub Actions run is green (both jobs).

## Memory (record after success — locations only, never secret values)
- **Production infrastructure**: server topology, IPs, private network, firewalls, SSH
  access, DB credential location.
- **Deployment pipeline**: GHCR image, CI/CD workflow, repo secrets, server layout
  (`/opt/dwellings`), redeploy + rollback procedure.
- **Pending**: `dwellings.tv` DNS + HTTPS.

## Secret locations (referenced, never stored in repo or memory body)
- Hetzner API token: `~/.config/hetzner/cloud-token`
- DB credentials: `~/.config/dwellings/db-credentials`
- Production runtime env: `/opt/dwellings/.env` (server)
- GHCR read PAT + CI deploy key: GitHub repo secrets / server `docker login`

## Needed from the user at execution time
1. GitHub username + repo name (or a pre-created empty private repo URL).
2. Whether to install `gh` or to create the repo manually.
3. A GHCR **read-only** PAT (`read:packages`) for the server's `docker login`.
4. Confirmation of SSH access to `root@87.99.154.101`.

## Out of scope (deferred)
- `dwellings.tv` DNS records and Let's Encrypt/TLS.
- A dedicated non-root `deploy` user (using `root` for the first pass).
- Staging environment / blue-green deploys / health-gated rollout.
