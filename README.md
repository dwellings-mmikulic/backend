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
internal/locationiq     LocationIQ geocoding + static map client
internal/propertymap    on-demand property map generation
internal/server         HTTP server: /roku/feed.json, /api/v1/properties*, /healthz
internal/scheduler      cron ticker; orchestrates the full collection cycle
internal/hls            ffmpeg remux of a listing video into TS segments
internal/linear         24/7 linear channels: lineups, live HLS playlist, EPG
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
- Per-listing failures are logged + marked `failed`, never fatal. A listing with
  no `ready` video is re-rendered on a later cycle even when `SKIP_EXISTING` is
  set — the revisit renders only, and does not re-upload photos or refresh the
  row. To clear an existing backlog in one pass (a listing only gets revisited
  when it resurfaces in search results), run `cmd/backfill-videos`:

  ```bash
  go run ./cmd/backfill-videos -dry-run   # report what would render
  go run ./cmd/backfill-videos            # render, upload, record
  ```

  It renders from the stored CDN photos, so it costs no Zillow API quota.
- Needs the `ffmpeg` binary + a TTF font (both in the Docker image).

### Property maps

The detail endpoint returns `map_image_url` (light, `streets` base map) and
`map_image_dark_url` (`dark` base map): 1200×800 static maps from
[LocationIQ](https://locationiq.com) centred on the home (no pin — the client draws its own at the image centre), stored at
`maps/<version>/<zpid>.png` and `maps/<version>/<zpid>-dark.png` on Bunny CDN.
Both are generated on first view; a row that predates dark maps gets only its
dark map fetched, the stored light one is kept.

The framing is zoom 15 at 1200×800: wide enough to show a named arterial road
or landmark near the property, not just its own residential block, and sharp
enough not to soften when a Roku upscales it to 1080p. Zoom and size are chosen
**together** — doubling the size at a fixed zoom doubles the ground area
covered, so changing one alone reframes the map rather than just resizing it.

Restyling means changing the constants in `internal/locationiq`, bumping
`StyleVersion` there so restyled maps get a fresh CDN path instead of a cached
stale object, and clearing the stored maps so they regenerate:

```sql
UPDATE properties SET map_image_url = NULL, map_image_dark_url = NULL, map_generated_at = NULL;
```

Maps are generated **on demand** — the first request for a listing that has no
map triggers generation, so listings nobody views cost nothing. If the property
has no coordinates yet, its address is geocoded first (with any trailing unit
designator such as `#4E` or `Apt 5` stripped — LocationIQ cannot resolve those)
and the coordinates are saved back to the row.

The request waits up to 1.5s. If generation takes longer it finishes in the
background, the response carries `map_image_url: null` with a shortened
`max-age=30`, and the next request serves the finished map. Addresses that
cannot be geocoded are recorded once and not geocoded again — but once the
scheduler's details enrichment stores Zillow's own coordinates for the row, the
map is generated from those on the next view.

Generation is also subject to a per-zpid cooldown after a transient failure
and an hourly, process-wide generation budget. Either can make an otherwise
mappable listing return `map_image_url: null` on an enabled deployment — this
is expected backpressure, not a bug, and a later request will retry.

Set `LOCATIONIQ_API_KEY` to enable maps; without it `map_image_url` is always
`null`. Maps also require Bunny CDN configuration (`BUNNY_STORAGE_ZONE`,
`BUNNY_API_KEY`, `BUNNY_CDN_BASE_URL`) — `config.Load` enforces this
whenever `LOCATIONIQ_API_KEY` is set, even if `IMAGES_ENABLED=false`.

### Linear channels

The listing videos double as 24/7 "TV channels": a live HLS stream plus an
EPG, with no running encoder. Every rendered MP4 is remuxed once into TS
segments (`hls/v1/<zpid>/<hash8>/` on Bunny CDN, immutable) and a channel is a
deterministic lineup materialised in `channel_lineups` in 6-hour versions.
The playlist a viewer fetches is computed from the wall clock, so two
instances — or a restart mid-stream — serve byte-identical playlists.

```
GET /channels/master.m3u8[?zip=77494 | ?city=Katy&state=TX | ?state=TX]
GET /channels/live.m3u8   (same filters; the master playlist points here)
GET /channels/epg.json    (same filters; 30-minute programme blocks)
```

No filter is the national channel. A ZIP with fewer than
`LINEAR_MIN_SCOPE_CLIPS` (20) videos falls back to its city, then state, then
national; `epg.json` reports the `scope` actually aired. New listings enter
the rotation at the next lineup version (at most `LINEAR_LINEUP_HOURS` later).

Segments for new renders are produced by the scheduler. To segment the
existing library (and retry failures):

```bash
go run ./cmd/backfill-hls -dry-run
go run ./cmd/backfill-hls -concurrency 4 -upload-concurrency 8
```

In production it ships inside the same image as the server (it needs the same
`DATABASE_URL` and `BUNNY_*` environment, and the same ffmpeg), so run it
through compose rather than installing Go on the box:

```bash
docker compose -f compose.prod.yml run --rm --entrypoint /app/backfill-hls app -dry-run
docker compose -f compose.prod.yml run --rm --entrypoint /app/backfill-hls app -concurrency 4
```

It logs progress every 100 videos and exits non-zero if any video failed, so
a wrapped run does not look successful while part of the library is unairable.

With `PUBLIC_BASE_URL` set, `/roku/feed.json` also lists the national channel
as a Roku `liveFeeds` entry. Its poster is `LINEAR_LIVE_THUMBNAIL_URL`; when
that is empty the first ready listing's image is used, and when there is no
image either the entry is omitted (Roku requires a thumbnail). The entry is
also left out while nothing has been segmented yet, so the feed never points
at an empty stream. Set `LINEAR_ENABLED=false` to turn all of this off.

At startup the server warns when the channels are enabled but cannot work:
`VIDEO_ENABLED=false` (nothing will be segmented), an empty `PUBLIC_BASE_URL`
(no Roku live entry), no music tracks found (silent renders are rejected by
the segmenter), or no segmented clip in the database (run the backfill).

### Viewer tracking and channel resolve

A viewer is a salted SHA-256 of the `sid` query parameter if present — an
app sends its device/advertising id (Roku RIDA) — else of the client IP;
the raw value is never stored. Viewers are recorded once per minute per
channel in `viewer_heartbeats`, batched every 30 s and purged after
`VIEWER_RETENTION_DAYS`.

```
GET /channels/beat?state=TX&sid=… → 204   (heartbeat; call every ~60 s while playing)
GET /channels/resolve             → {"scope","name","master","epg","source"}
GET /channels/stats?state=TX      → {"scope","concurrent","unique_24h","unique_7d"}
```

Playlists are cached per scope at the edge, so audience size never reaches
the app; players report themselves with `beat` (same filters as the
playlist they are on). Polls of `live.m3u8` that do reach the app are
counted too, so anonymous third-party players still show up as a sample.

`resolve` picks a channel for the caller: the one they last watched (within
the retention period), else the ZIP/city/state of their IP (MaxMind
GeoLite2-City at `GEOIP_DB_PATH`), else national — each candidate goes
through the usual thin-area fallback, so the answer always has content.
`source` says which rule won. Pass `?sid=` to identify a device instead of
an IP. `concurrent` is distinct viewers in the last two minutes.

### HTTP endpoints

- `GET /roku/feed.json` — Roku Direct Publisher feed of all `ready` videos.
- `GET /api/v1/properties`, `GET /api/v1/properties/{zpid}` — public listings API, see [API](#api) below.
- `GET /swagger/index.html` — interactive Swagger UI for the public API (spec at `/swagger/doc.json`).
- `GET /channels/master.m3u8`, `GET /channels/live.m3u8`, `GET /channels/epg.json` — linear channels, see [Linear channels](#linear-channels).
- `GET /channels/resolve`, `GET /channels/stats` — see [Viewer tracking and channel resolve](#viewer-tracking-and-channel-resolve).
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
  scheduler's details-enrichment step fills them in, plus `map_image_url` — a
  static LocationIQ map centred on the home (no pin), hosted on Bunny CDN. Returns
  `404` with `{"error":"not found"}` for an unknown `zpid`.

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
cp .env.example .env      # fill in ZILLOW_API_KEY and BUNNY_*
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
`BUNNY_CDN_BASE_URL`.

`CRON_SCHEDULE` is a standard 5-field cron expression (default `0 */12 * * *`,
every 12 hours). A cycle also runs once immediately on startup.

`DETAILS_PER_CYCLE` caps how many properties get a one-time details-API
enrichment call per cycle (default `50`; `0` disables enrichment entirely).

`API_BUDGET_PER_CYCLE` (default `150`) caps the total OpenWebNinja requests
one cycle may spend — search pages plus details calls. ZIP codes are not
configured: a built-in table of all 29,670 US residential ZIPs is seeded on
first startup, and each cycle works through it in rotation order
(never-searched first, most populous first, then stalest), stopping when the
budget is spent and resuming from the cursor next cycle. A cycle is skipped
entirely when the provider reports the monthly quota is exhausted, or when a
previous cycle is still running.

`PUBLIC_BASE_URL` (e.g. `https://api.dwellings.tv`) is this server's public
origin, used for the absolute URLs in the Roku feed; empty leaves the live
channel out of the feed. The linear channels read `LINEAR_ENABLED` (`true`),
`LINEAR_LINEUP_HOURS` (`6`), `LINEAR_MIN_SCOPE_CLIPS` (`20`),
`LINEAR_EPG_HORIZON_HOURS` (`24`) and `LINEAR_LIVE_THUMBNAIL_URL` (empty —
the poster of the Roku live entry; see [Linear channels](#linear-channels)).
Viewer tracking reads `VIEWER_TRACKING_ENABLED` (`true`), `VIEWER_SALT`
(required while on), `VIEWER_SALT_ROTATE_DAILY` (`false`),
`VIEWER_RETENTION_DAYS` (`30`) and `GEOIP_DB_PATH` (empty = no geo).

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
- `SearchPages` pages until `SEARCH_MAX_RESULTS` is reached (`0` = uncapped)
  or the per-cycle API budget runs out (hard cap 20 pages per ZIP). Price and
  bedroom criteria are applied client-side.

## Notes

- Image upload/download failures for individual images are logged and skipped;
  they don't abort the rest of the cycle.

## Commands

```bash
make build   # compile to bin/server
make test    # run tests
make vet     # go vet
```
