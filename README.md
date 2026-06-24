# Dwellings Backend

A single-purpose Go service that collects Zillow property listings on a schedule.

Each cycle it:

1. Searches the **OpenWebNinja Zillow API** using configurable criteria (location, price, beds).
2. Downloads each listing's images and uploads them to **Bunny CDN** storage.
3. Upserts the property (with CDN image URLs) into **PostgreSQL**.
4. Renders a 16:9 1080p **listing video** (slideshow + facts overlay + QR + music),
   uploads it to **Bunny CDN**, and serves a **Roku Direct Publisher JSON feed**
   so a Roku channel ("DwellingTV") can play them.

## Architecture

Monolith with an internal cron scheduler plus a small HTTP server for the feed.
One binary, one container.

```
cmd/server/main.go      entrypoint: config, DB, scheduler, HTTP server
internal/config         typed config from env vars
internal/db             pgx pool + embedded schema (applied on startup)
internal/property       domain model + PostgreSQL repository (upsert by zpid)
internal/zillow         OpenWebNinja Zillow API client
internal/bunny          Bunny CDN storage upload client
internal/qrcode         QR PNG generation (listing detail URL)
internal/video          ffmpeg slideshow renderer (overlays, QR, music)
internal/feed           Roku Direct Publisher feed builder
internal/server         HTTP server: /roku/feed.json, /healthz
internal/scheduler      cron ticker; orchestrates the full collection cycle
```

### Listing videos

After a listing's photos are handled, the scheduler renders a video (unless it
already has a `ready` one with an unchanged content hash):

```
photos → ffmpeg (scale-to-cover 1920×1080, lower-third price/address/facts
overlay, QR to the Zillow listing, music bed) → upload videos/<zpid>.mp4 to
Bunny → store video_url + status.
```

- Pacing: every photo for `VIDEO_SECONDS_PER_PHOTO` seconds (default 4).
- Music: 10 bundled CC0 tracks in `assets/music/` (public domain), chosen
  deterministically by zpid. Empty dir → silent video.
- Per-listing failures are logged + marked `failed`, retried next cycle, never fatal.
- Needs the `ffmpeg` binary + a TTF font (both in the Docker image).

### HTTP endpoints

- `GET /roku/feed.json` — Roku Direct Publisher feed of all `ready` videos.
- `GET /healthz` — liveness.

## Collected fields

Sale price, address, city, state, zip, home size (sq ft), lot size (sq ft),
bedrooms, bathrooms, and image URLs (on Bunny CDN).

## Running locally (Docker)

```bash
cp .env.example .env      # fill in ZILLOW_API_KEY, BUNNY_* and SEARCH_LOCATION
make up                   # starts postgres + app
make logs                 # follow app logs
make down                 # stop everything
```

Postgres data persists in the `pgdata` volume. The schema is applied
automatically on startup (idempotent).

## Running locally (without Docker)

Start a Postgres instance, set `DATABASE_URL` (and the other vars) in `.env` or
your shell, then:

```bash
make run
```

## Configuration

All configuration is via environment variables — see `.env.example`. Required:
`DATABASE_URL`, `ZILLOW_API_KEY`, `BUNNY_STORAGE_ZONE`, `BUNNY_API_KEY`,
`BUNNY_CDN_BASE_URL`, `SEARCH_LOCATION`.

`CRON_SCHEDULE` is a standard 5-field cron expression (default `0 * * * *`,
hourly). A cycle also runs once immediately on startup.

## OpenWebNinja Zillow API

- Endpoint: `GET https://api.openwebninja.com/realtime-zillow-data/search`
  with `location` (required), `home_status` (`FOR_SALE`/`FOR_RENT`/`RECENTLY_SOLD`),
  and `page`. Auth via the `X-API-Key` header.
- Response envelope: `{status, request_id, parameters, data: [ ... ]}` — `data`
  is a flat array of listings. Field mapping lives in `internal/zillow/client.go`
  (`toProperty`) and is verified against a captured response in
  `internal/zillow/testdata/`.
- Full image sets are built from each listing's `carouselPhotosComposable`
  (`baseUrl` + `photoData[].photoKey`), falling back to the `imgSrc` thumbnail.
- `Search` pages until `SEARCH_MAX_RESULTS` is reached (hard cap 20 pages).
  Price and bedroom criteria are applied client-side.

## Notes

- Image upload/download failures for individual images are logged and skipped;
  they don't abort the rest of the cycle.

## Commands

```bash
make build   # compile to bin/server
make test    # run tests
make vet     # go vet
```
