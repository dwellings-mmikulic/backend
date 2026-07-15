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
internal/api            public read-only listings API (browse + detail)
internal/server         HTTP server: /roku/feed.json, /api/v1/properties*, /healthz
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
- `GET /api/v1/properties`, `GET /api/v1/properties/{zpid}` — public listings API, see [API](#api) below.
- `GET /swagger/index.html` — interactive Swagger UI for the public API (spec at `/swagger/doc.json`).
- `GET /healthz` — liveness.

## API

Public, read-only listings API. No auth required.

Interactive docs: [https://api.dwellings.tv/swagger/index.html](https://api.dwellings.tv/swagger/index.html). The spec is generated from handler annotations with [swaggo/swag](https://github.com/swaggo/swag) — run `make swagger` after changing the API surface and commit the regenerated `docs/`.

- `GET /api/v1/properties` — paginated browse list. Query params (all optional):
  - `zip`, `property_type` — exact-match filters. `city`, `state` — case-insensitive match.
  - `min_price`, `max_price` — price range (whole dollars, e.g. `500000`).
  - `min_beds`, `min_baths` — minimum bedrooms/bathrooms.
  - `min_sqft`, `max_sqft` — home size range (sq ft).
  - `sort` — `newest` (default), `price_asc`, or `price_desc`.
  - `limit` — page size, `1`–`100` (default `24`).
  - `cursor` — opaque pagination token, see below.

  Response: `{"total": <int>, "results": [...], "next_cursor": <string|null>}`.
  Each result is a browse-card summary (`zpid`, `price`, `address`, `city`,
  `state`, `zip`, `bedrooms`, `bathrooms`, `home_size_sqft`, `property_type`,
  `image_url`).

- `GET /api/v1/properties/{zpid}` — full detail for one listing, including
  enrichment fields (`description`, `year_built`, `heating`, `cooling`,
  `garage`, `hoa_fee_monthly`, `mls_number`, `listing_status`, `agent`,
  `latitude`, `longitude`, `lot_size_acres`) that are `null` until the
  scheduler's details-enrichment step fills them in. Returns `404` with
  `{"error":"not found"}` for an unknown `zpid`.

**Pagination:** when a page has more results, the response includes
`next_cursor`. Pass it back as `cursor` on the next request (with the same
`sort`) to get the following page; `next_cursor` is `null` on the last page.

Both endpoints send `Access-Control-Allow-Origin: *` and
`Cache-Control: public, max-age=300`, and return `400` with
`{"error": "..."}` for invalid query params.

## Collected fields

Sale price, address, city, state, zip, home size (sq ft), lot size (sq ft),
bedrooms, bathrooms, and image URLs (on Bunny CDN). A capped number of
listings per cycle (`DETAILS_PER_CYCLE`) are further enriched via a one-time
details-API call: description, year built, heating/cooling, garage, HOA fee,
MLS number, listing status, agent contact, and lat/long.

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

`DETAILS_PER_CYCLE` caps how many properties get a one-time details-API
enrichment call per cycle (default `50`; `0` disables enrichment entirely).

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
