# Production Deployment (CI/CD) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up a GitHub Actions → GHCR → Hetzner pipeline so pushing to `main` builds the Dwellings backend image, publishes it privately, and auto-deploys it to the web server, reachable at `http://87.99.154.101`.

**Architecture:** A CI job builds a `linux/amd64` image and pushes it to a private GHCR package. A dependent CD job SSHes into the web box and runs `docker compose pull && up -d`. On the box, nginx reverse-proxies port 80 to the app container on `127.0.0.1:8080`; the app connects to the existing Postgres box over the private network (`10.0.0.3:5432`) and auto-migrates its schema on startup.

**Tech Stack:** Go monolith (Docker multi-stage, alpine+ffmpeg), GitHub Actions, `docker/build-push-action`, `appleboy/ssh-action`, GHCR, Hetzner Cloud (Ubuntu 24.04), nginx, Postgres 16.

## Global Constraints

- Production must build for **`linux/amd64`** (servers are amd64; dev Mac is arm64) — CI handles this, never build/push from the Mac.
- **`.env` is never committed** — it is gitignored; verify before every commit that touches tracking.
- Image is **private** on GHCR; the server authenticates with a **read-only** PAT (`read:packages`).
- App listens on `:8080`; in prod the container binds **`127.0.0.1:8080`** only (nginx proxies; Hetzner firewall also blocks 8080 externally).
- Postgres is the **separate db box** at `10.0.0.3:5432` over the private network — the production compose file runs the **app only**, never its own Postgres.
- DB schema auto-migrates on startup (embedded idempotent `schema.sql`) — no separate migration step.
- Deploy user for this pass is **`root`**; SSH via a **dedicated CI deploy key**, not the personal `~/.ssh/dwellings_tv` key.
- Secrets (PATs, keys, passwords) are referenced by location only — never pasted into committed files, the plan, or memory bodies.

### Execution variables (set once at the start, used throughout)

```bash
export GH_OWNER="<your-github-username>"     # GHCR namespace, e.g. markomikulic
export REPO="dwellings-backend"              # GitHub repo name
export IMAGE="ghcr.io/${GH_OWNER}/dwellings" # lowercased; GHCR requires lowercase
export WEB_IP="87.99.154.101"
export DB_PRIV_IP="10.0.0.3"
```

> `GH_OWNER` must be lowercase in the image path. If your username has capitals, lowercase it for `IMAGE` only.

---

### Task 1: Initialize git repo with secrets excluded

**Files:**
- Modify: `.gitignore`
- Create: `.git/` (via `git init`)

**Interfaces:**
- Produces: a clean local git repo on branch `main` whose tracked set excludes `.env`, `bin/`, `tmp/`, and the JetBrains `.idea/` clutter.

- [ ] **Step 1: Harden `.gitignore`** — append IDE + OS noise so the first commit is clean.

Append these lines to `.gitignore` (keep existing lines):

```gitignore
.idea/
.DS_Store
```

- [ ] **Step 2: Initialize the repo on `main`**

Run:
```bash
git init -b main
```
Expected: `Initialized empty Git repository in /Users/marko/Projects/Dwellings/backend/.git/`

- [ ] **Step 3: Verify `.env` and secrets are ignored BEFORE staging**

Run:
```bash
git add -A --dry-run | grep -E '(^|/)\.env$' && echo "DANGER: .env would be committed" || echo "OK: .env excluded"
git check-ignore .env .idea/workspace.xml bin/ || true
```
Expected: prints `OK: .env excluded`, and `check-ignore` lists `.env` (and any present `.idea`/`bin` paths). If `.env` shows as "would be committed", STOP and fix `.gitignore`.

- [ ] **Step 4: Stage and make the initial commit**

Run:
```bash
git add -A
git status --short | grep -E '\.env$' && echo "ABORT" || git commit -m "chore: initial commit of Dwellings backend"
```
Expected: a commit is created; `git status` is clean; `git ls-files | grep '^\.env$'` returns nothing.

- [ ] **Step 5: Confirm `.env.example` (template) IS tracked but `.env` is NOT**

Run:
```bash
git ls-files | grep -E '\.env'
```
Expected: shows `.env.example` only (not `.env`).

---

### Task 2: Add production config files

**Files:**
- Create: `compose.prod.yml`
- Create: `.env.prod.example`
- Create: `deploy/nginx-dwellings.conf`
- Create: `deploy/README.md`

**Interfaces:**
- Produces: `compose.prod.yml` referencing image `ghcr.io/<owner>/dwellings:latest`, service name `app`, reading `/opt/dwellings/.env`. Consumed by Task 6 (server file placement) and Task 9 (deploy job runs `docker compose -f compose.prod.yml`).

- [ ] **Step 1: Create `compose.prod.yml`** (app-only; no Postgres)

```yaml
# Production compose — app only. Postgres runs on the separate db box
# (10.0.0.3) reached over the private network. Used on the web box at
# /opt/dwellings/compose.prod.yml.
services:
  app:
    image: ghcr.io/OWNER_PLACEHOLDER/dwellings:latest
    env_file:
      - .env
    ports:
      - "127.0.0.1:8080:8080"   # localhost only; nginx proxies public :80
    restart: unless-stopped
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
```

> `OWNER_PLACEHOLDER` is replaced with the real lowercased owner when the file is placed on the server in Task 6 (`sed`). It is committed with the placeholder so the repo carries no assumption about the username; the server copy is concrete.

- [ ] **Step 2: Create `.env.prod.example`** (template, NO secret values)

```bash
# Production environment template for the web box (/opt/dwellings/.env).
# Copy to .env on the server and fill in real secret values. NEVER commit the real .env.

# Database — the private-network Postgres box. Password is in ~/.config/dwellings/db-credentials (local) / Hetzner.
DATABASE_URL=postgres://dwellings:REPLACE_WITH_DB_PASSWORD@10.0.0.3:5432/dwellings?sslmode=disable

# Scheduler — standard 5-field cron expression.
CRON_SCHEDULE=0 * * * *

# OpenWebNinja Real-Time Zillow Data API
ZILLOW_BASE_URL=https://api.openwebninja.com/realtime-zillow-data
ZILLOW_API_KEY=REPLACE_WITH_ZILLOW_API_KEY

SKIP_EXISTING=true
IMAGES_ENABLED=true

# Bunny CDN storage
BUNNY_STORAGE_ZONE=REPLACE
BUNNY_API_KEY=REPLACE
BUNNY_STORAGE_HOST=storage.bunnycdn.com
BUNNY_CDN_BASE_URL=https://your-zone.b-cdn.net

# Search criteria
SEARCH_LOCATION=Punta Gorda, FL
SEARCH_HOME_STATUS=FOR_SALE
SEARCH_MAX_RESULTS=50

# Video rendering
VIDEO_ENABLED=true
VIDEO_SECONDS_PER_PHOTO=4

# HTTP server
HTTP_PORT=8080

# Concurrency / timeouts
LISTING_CONCURRENCY=
IMAGE_CONCURRENCY=8
HTTP_TIMEOUT_SECONDS=30
BUNNY_TIMEOUT_SECONDS=300
```

- [ ] **Step 3: Create `deploy/nginx-dwellings.conf`**

```nginx
# Dwellings backend reverse proxy. Installed at
# /etc/nginx/sites-available/dwellings and symlinked into sites-enabled.
server {
    listen 80 default_server;
    listen [::]:80 default_server;
    server_name _;

    # Roku Direct Publisher feed + health check + everything else → app container.
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 300s;
    }
}
```

- [ ] **Step 4: Create `deploy/README.md`**

````markdown
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
````

- [ ] **Step 5: Commit**

```bash
git add compose.prod.yml .env.prod.example deploy/
git commit -m "build: add production compose, env template, nginx config, deploy docs"
```

---

### Task 3: Add the CI/CD workflow

**Files:**
- Create: `.github/workflows/publish-deploy.yml`

**Interfaces:**
- Consumes: GitHub repo secrets `SSH_HOST`, `SSH_USER`, `SSH_PRIVATE_KEY` (created in Task 8); the server-side `/opt/dwellings` (Task 6) and GHCR login (Task 6).
- Produces: on push to `main`, image `ghcr.io/<owner>/dwellings:{latest,sha-<short>}` and a live container on the web box.

- [ ] **Step 1: Create `.github/workflows/publish-deploy.yml`**

```yaml
name: Publish & Deploy

on:
  push:
    branches: [main]
  workflow_dispatch:

concurrency:
  group: deploy-main
  cancel-in-progress: true

jobs:
  build-and-push:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write
    steps:
      - name: Checkout
        uses: actions/checkout@v4

      - name: Log in to GHCR
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Image metadata
        id: meta
        uses: docker/metadata-action@v5
        with:
          images: ghcr.io/${{ github.repository_owner }}/dwellings
          tags: |
            type=raw,value=latest
            type=sha,prefix=sha-

      - name: Set up Buildx
        uses: docker/setup-buildx-action@v3

      - name: Build and push
        uses: docker/build-push-action@v6
        with:
          context: .
          platforms: linux/amd64
          push: true
          tags: ${{ steps.meta.outputs.tags }}
          labels: ${{ steps.meta.outputs.labels }}
          cache-from: type=gha
          cache-to: type=gha,mode=max

  deploy:
    needs: build-and-push
    runs-on: ubuntu-latest
    steps:
      - name: Deploy over SSH
        uses: appleboy/ssh-action@v1
        with:
          host: ${{ secrets.SSH_HOST }}
          username: ${{ secrets.SSH_USER }}
          key: ${{ secrets.SSH_PRIVATE_KEY }}
          script: |
            set -e
            cd /opt/dwellings
            docker compose -f compose.prod.yml pull
            docker compose -f compose.prod.yml up -d
            docker image prune -f
```

- [ ] **Step 2: Lint the YAML locally**

Run:
```bash
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/publish-deploy.yml')); print('YAML OK')"
```
Expected: `YAML OK`

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/publish-deploy.yml
git commit -m "ci: add GHCR build-and-push + SSH auto-deploy workflow"
```

---

### Task 4: Generate the dedicated CI deploy key

**Files:**
- Create: `~/.ssh/dwellings_ci_deploy` + `~/.ssh/dwellings_ci_deploy.pub` (local, NOT in repo)

**Interfaces:**
- Produces: a passwordless ed25519 keypair. The **public** half goes into the web box `authorized_keys` (Task 6). The **private** half becomes GitHub secret `SSH_PRIVATE_KEY` (Task 8).

- [ ] **Step 1: Generate a passphrase-free deploy key**

Run:
```bash
ssh-keygen -t ed25519 -C "github-actions-deploy-dwellings" -f ~/.ssh/dwellings_ci_deploy -N ""
```
Expected: creates `~/.ssh/dwellings_ci_deploy` and `~/.ssh/dwellings_ci_deploy.pub`.

- [ ] **Step 2: Confirm both halves exist and are not in the repo**

Run:
```bash
ls -l ~/.ssh/dwellings_ci_deploy ~/.ssh/dwellings_ci_deploy.pub
git -C /Users/marko/Projects/Dwellings/backend check-ignore -v ~/.ssh/dwellings_ci_deploy 2>/dev/null; echo "(key lives outside the repo — nothing to commit)"
```
Expected: both files listed; the key path is outside the project tree.

---

### Task 5: Install Docker Engine on the web box

**Files:** none (remote server state)

**Interfaces:**
- Consumes: SSH access `root@87.99.154.101` with `~/.ssh/dwellings_tv`.
- Produces: `docker` + `docker compose` available on the web box.

- [ ] **Step 1: Confirm SSH access**

Run:
```bash
ssh -i ~/.ssh/dwellings_tv -o StrictHostKeyChecking=accept-new root@$WEB_IP 'echo connected; lsb_release -ds'
```
Expected: prints `connected` and `Ubuntu 24.04 ...`.

- [ ] **Step 2: Install Docker via the official convenience script**

Run:
```bash
ssh -i ~/.ssh/dwellings_tv root@$WEB_IP 'curl -fsSL https://get.docker.com | sh'
```
Expected: Docker installs; ends without error.

- [ ] **Step 3: Verify Docker + compose plugin**

Run:
```bash
ssh -i ~/.ssh/dwellings_tv root@$WEB_IP 'docker --version && docker compose version && systemctl is-enabled docker'
```
Expected: prints Docker version, `Docker Compose version v2.x`, and `enabled`.

---

### Task 6: Configure GHCR auth + place app files on the web box

**Files:**
- Create (server): `/opt/dwellings/compose.prod.yml`, `/opt/dwellings/.env`
- Modify (server): `/root/.ssh/authorized_keys`

**Interfaces:**
- Consumes: the CI deploy public key (Task 4), `compose.prod.yml` + `.env.prod.example` (Task 2), DB password from `~/.config/dwellings/db-credentials`, local `.env` secrets, and a GHCR **read-only PAT** (provided by the user).
- Produces: a server able to pull the private image and a complete `/opt/dwellings` ready for `docker compose up`.

- [ ] **Step 1: Add the CI deploy key to the web box `authorized_keys`**

Run:
```bash
ssh-copy-id -i ~/.ssh/dwellings_ci_deploy.pub -o IdentityFile=~/.ssh/dwellings_tv root@$WEB_IP \
  || cat ~/.ssh/dwellings_ci_deploy.pub | ssh -i ~/.ssh/dwellings_tv root@$WEB_IP 'mkdir -p ~/.ssh && cat >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys'
```
Expected: key appended.

- [ ] **Step 2: Verify the deploy key can log in (this is exactly what CI will do)**

Run:
```bash
ssh -i ~/.ssh/dwellings_ci_deploy -o IdentitiesOnly=yes root@$WEB_IP 'echo ci-key-ok'
```
Expected: prints `ci-key-ok`.

- [ ] **Step 3: Log the server into GHCR with the read-only PAT**

Run (user supplies the `read:packages` PAT; piped via stdin so it is not stored in shell history on the server):
```bash
read -rsp "Paste GHCR read-only PAT: " GHCR_PAT; echo
echo "$GHCR_PAT" | ssh -i ~/.ssh/dwellings_tv root@$WEB_IP "docker login ghcr.io -u $GH_OWNER --password-stdin"
unset GHCR_PAT
```
Expected: `Login Succeeded` on the server.

- [ ] **Step 4: Create `/opt/dwellings` and place a concrete `compose.prod.yml`**

Run (substitutes the real owner into the committed placeholder):
```bash
ssh -i ~/.ssh/dwellings_tv root@$WEB_IP 'mkdir -p /opt/dwellings'
sed "s/OWNER_PLACEHOLDER/${GH_OWNER}/" compose.prod.yml | \
  ssh -i ~/.ssh/dwellings_tv root@$WEB_IP 'cat > /opt/dwellings/compose.prod.yml'
```
Expected: file written; verify with `ssh ... 'grep image /opt/dwellings/compose.prod.yml'` showing the real owner.

- [ ] **Step 5: Build the production `.env` from local secrets + DB password and upload it**

Run (reads the DB password from the saved credentials file; adjust the grep key if the file format differs):
```bash
DB_PW=$(grep -iE 'password' ~/.config/dwellings/db-credentials | head -1 | sed -E 's/.*[:=][[:space:]]*//')
# Start from the working local .env, but override DATABASE_URL for the private DB box.
{ grep -v '^DATABASE_URL=' .env; \
  echo "DATABASE_URL=postgres://dwellings:${DB_PW}@${DB_PRIV_IP}:5432/dwellings?sslmode=disable"; } \
  | ssh -i ~/.ssh/dwellings_tv root@$WEB_IP 'umask 077; cat > /opt/dwellings/.env && chmod 600 /opt/dwellings/.env'
unset DB_PW
```
Expected: `/opt/dwellings/.env` exists with `chmod 600`.

- [ ] **Step 6: Verify the `.env` is correct without printing secrets**

Run:
```bash
ssh -i ~/.ssh/dwellings_tv root@$WEB_IP 'cd /opt/dwellings && \
  grep -q "@'"$DB_PRIV_IP"':5432" .env && echo "DATABASE_URL points at db box OK"; \
  awk -F= "/^[A-Z]/{print \$1\" set\"}" .env'
```
Expected: prints `DATABASE_URL points at db box OK` and the list of set keys (no values).

---

### Task 7: Configure the nginx reverse proxy on the web box

**Files:**
- Create (server): `/etc/nginx/sites-available/dwellings`
- Modify (server): `/etc/nginx/sites-enabled/` (symlink; remove `default`)

**Interfaces:**
- Consumes: `deploy/nginx-dwellings.conf` (Task 2).
- Produces: nginx forwarding public `:80` → `127.0.0.1:8080`.

- [ ] **Step 1: Upload the server block and enable it**

Run:
```bash
scp -i ~/.ssh/dwellings_tv deploy/nginx-dwellings.conf root@$WEB_IP:/etc/nginx/sites-available/dwellings
ssh -i ~/.ssh/dwellings_tv root@$WEB_IP 'ln -sf /etc/nginx/sites-available/dwellings /etc/nginx/sites-enabled/dwellings && rm -f /etc/nginx/sites-enabled/default'
```
Expected: symlink created, default removed.

- [ ] **Step 2: Test config and reload (do NOT break a working nginx)**

Run:
```bash
ssh -i ~/.ssh/dwellings_tv root@$WEB_IP 'nginx -t && systemctl reload nginx'
```
Expected: `nginx: configuration file ... test is successful` then a clean reload.

- [ ] **Step 3: Confirm nginx now proxies (502 is expected — app not deployed yet)**

Run:
```bash
curl -s -o /dev/null -w '%{http_code}\n' http://$WEB_IP/healthz
```
Expected: `502` (nginx is up and proxying; the upstream app container does not exist yet — that is Task 9).

---

### Task 8: Create the GitHub repo and configure secrets

**Files:** none local (GitHub remote state)

**Interfaces:**
- Consumes: the local commits (Tasks 1–3), CI deploy private key (Task 4).
- Produces: a private GitHub repo with `main` pushed and secrets `SSH_HOST`, `SSH_USER`, `SSH_PRIVATE_KEY` set. Pushing `main` will trigger the workflow (Task 9).

- [ ] **Step 1: Create the private repo and add the remote**

If `gh` is installed and authenticated:
```bash
gh repo create "$GH_OWNER/$REPO" --private --source=. --remote=origin --disable-issues=false
```
Otherwise (manual): create an empty **private** repo named `$REPO` at github.com, then:
```bash
git remote add origin "git@github.com:$GH_OWNER/$REPO.git"
```
Expected: `origin` remote configured. Verify: `git remote -v`.

- [ ] **Step 2: Add the three repo secrets**

If `gh` is available:
```bash
gh secret set SSH_HOST --body "$WEB_IP" -R "$GH_OWNER/$REPO"
gh secret set SSH_USER --body "root"    -R "$GH_OWNER/$REPO"
gh secret set SSH_PRIVATE_KEY < ~/.ssh/dwellings_ci_deploy -R "$GH_OWNER/$REPO"
```
Otherwise (manual): in GitHub → repo → Settings → Secrets and variables → Actions → New repository secret, add:
- `SSH_HOST` = `87.99.154.101`
- `SSH_USER` = `root`
- `SSH_PRIVATE_KEY` = full contents of `~/.ssh/dwellings_ci_deploy` (including BEGIN/END lines)

- [ ] **Step 3: Verify secrets exist (names only)**

Run (if `gh`):
```bash
gh secret list -R "$GH_OWNER/$REPO"
```
Expected: lists `SSH_HOST`, `SSH_USER`, `SSH_PRIVATE_KEY`.

---

### Task 9: First deploy and verify

**Files:** none (triggers the pipeline)

**Interfaces:**
- Consumes: everything above — server prepped (Tasks 5–7), repo + secrets (Task 8), workflow (Task 3).
- Produces: a live app at `http://87.99.154.101`.

- [ ] **Step 1: Push `main` to trigger the pipeline**

Run:
```bash
git push -u origin main
```
Expected: push succeeds; GitHub Actions starts the `Publish & Deploy` run.

- [ ] **Step 2: Watch the workflow to green**

Run (if `gh`):
```bash
gh run watch -R "$GH_OWNER/$REPO" --exit-status
```
Otherwise: open the Actions tab and confirm both `build-and-push` and `deploy` jobs succeed.
Expected: both jobs green. If `build-and-push` fails on platform, confirm `platforms: linux/amd64`. If `deploy` fails on auth, recheck `SSH_PRIVATE_KEY` and that the deploy key is in the box `authorized_keys` (Task 6 Step 2).

- [ ] **Step 3: Confirm the container is running on the box**

Run:
```bash
ssh -i ~/.ssh/dwellings_tv root@$WEB_IP 'cd /opt/dwellings && docker compose -f compose.prod.yml ps'
```
Expected: the `app` service is `Up`.

- [ ] **Step 4: Health check through nginx**

Run:
```bash
curl -fsS -o /dev/null -w 'healthz: %{http_code}\n' http://$WEB_IP/healthz
```
Expected: `healthz: 200`.

- [ ] **Step 5: Confirm the Roku feed serves**

Run:
```bash
curl -fsS http://$WEB_IP/roku/feed.json | head -c 400; echo
```
Expected: valid JSON (a feed object; may be empty/few items until the first scheduler cycle completes).

- [ ] **Step 6: Confirm DB connectivity + scheduler in the logs**

Run:
```bash
ssh -i ~/.ssh/dwellings_tv root@$WEB_IP 'cd /opt/dwellings && docker compose -f compose.prod.yml logs --tail=80 app'
```
Expected: logs show schema applied / DB connected and the scheduler registered (no DB dial errors to `10.0.0.3:5432`). If you see connection refused, recheck the private-network reachability and `DATABASE_URL`.

---

### Task 10: Record production facts in memory

**Files:**
- Create: `~/.claude/projects/-Users-marko-Projects-Dwellings-backend/memory/production-infrastructure.md`
- Create: `~/.claude/projects/-Users-marko-Projects-Dwellings-backend/memory/production-deployment.md`
- Modify: `~/.claude/projects/-Users-marko-Projects-Dwellings-backend/memory/MEMORY.md`

**Interfaces:**
- Consumes: verified state from Tasks 5–9.
- Produces: persistent memory of the prod topology and CI/CD pipeline (secret **locations** only), plus the deferred domain/TLS note.

- [ ] **Step 1: Write `production-infrastructure.md`** — `type: project`. Server table (roles, public/private IPs, sizes, region `ash`), private network `dwellings-net` `10.0.0.0/16`, firewalls, SSH (`~/.ssh/dwellings_tv`), Hetzner token + DB credential **locations**. Link `[[production-deployment]]`, `[[architecture-decisions]]`.

- [ ] **Step 2: Write `production-deployment.md`** — `type: project`. GHCR private image `ghcr.io/<owner>/dwellings`, the `publish-deploy.yml` pipeline (push `main` → build+push → SSH deploy), repo secrets, server layout `/opt/dwellings`, nginx proxy, redeploy + rollback commands, and **Pending:** `dwellings.tv` DNS + HTTPS. Link `[[production-infrastructure]]`.

- [ ] **Step 3: Add two index lines to `MEMORY.md`**

```markdown
- [Production infrastructure](production-infrastructure.md) — Hetzner web+db boxes, IPs, private net, firewalls, SSH
- [Production deployment](production-deployment.md) — GHCR + GitHub Actions CI/CD to the web box; domain/TLS pending
```

- [ ] **Step 4: Verify memory files are valid and indexed**

Run:
```bash
ls ~/.claude/projects/-Users-marko-Projects-Dwellings-backend/memory/production-*.md
grep -c "production-" ~/.claude/projects/-Users-marko-Projects-Dwellings-backend/memory/MEMORY.md
```
Expected: both files listed; grep count `>= 2`.

---

## Self-Review

**Spec coverage:**
- Git/GitHub repo setup → Task 1 (git) + Task 8 (remote/secrets). ✅
- CI workflow (build/push) → Task 3. ✅
- Full CD (auto-deploy) → Task 3 `deploy` job + Task 9. ✅
- Private GHCR image + server read-PAT → Task 6 Step 3. ✅
- Server prep (Docker, /opt/dwellings, .env, authorized_keys, nginx) → Tasks 5, 6, 7. ✅
- Dedicated CI deploy key (not personal key) → Task 4. ✅
- App-only prod compose pointing at `10.0.0.3` → Task 2 + Task 6. ✅
- Verify (healthz 200, feed, logs) → Task 9. ✅
- Memory (locations only) → Task 10. ✅
- Deferred domain/TLS → noted in Task 10 Step 2 and out-of-scope. ✅

**Placeholder scan:** The only `_PLACEHOLDER` token is `OWNER_PLACEHOLDER` in the committed `compose.prod.yml`, intentionally `sed`-substituted in Task 6 Step 4; `REPLACE_*` tokens live only in `.env.prod.example` (a template). Execution variables (`GH_OWNER`, etc.) are real user inputs set at the top. No vague "add error handling" steps.

**Type/name consistency:** Service name `app`, file `compose.prod.yml`, image `ghcr.io/<owner>/dwellings`, secrets `SSH_HOST`/`SSH_USER`/`SSH_PRIVATE_KEY`, path `/opt/dwellings` — used identically across Tasks 2, 3, 6, 7, 9.
