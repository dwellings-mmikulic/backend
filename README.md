# Dwellings Backend

A single-purpose Go service that collects Zillow property listings within a
fleet-wide API budget.

Continuously it:

1. Searches the **OpenWebNinja Zillow API**, one ZIP code at a time in
   rotation order, and queues the listings it finds.
2. Downloads each listing's images and uploads them to **Bunny CDN** storage.
3. Upserts the property (with CDN image URLs) into **PostgreSQL**.
4. Renders a 16:9 1080p **listing video** (slideshow + facts overlay + QR + music),
   uploads it to **Bunny CDN**, and serves a **Roku Direct Publisher JSON feed**
   so a Roku channel ("DwellingTV") can play them.

## Architecture

One binary, one container image. A single instance (`ROLE=all`, the default)
is a monolith: worker loops plus a small HTTP server. Any number of instances
can run against the same PostgreSQL — they coordinate through it with
expiring claims and a shared API budget ledger, never doing the same work
twice. See [Running several instances](#running-several-instances).

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
internal/scheduler      the worker loops: discovery (search ZIPs → queue),
                        media (photos → upsert → render → HLS), details
internal/zipcode        ZIP rotation with atomic, expiring claims
internal/workqueue      listing_queue: discovered listings awaiting the media loop
internal/budget         budget windows + the fleet-wide API request ledger
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
- Per-listing failures are logged + marked `failed`, never fatal: the queued
  listing is retried with a backoff (5 min, then 30 min) and parked as dead
  after three attempts. A listing with no `ready` video is re-rendered when it
  is next discovered even when `SKIP_EXISTING` is set — the revisit renders
  only, and does not re-upload photos or refresh the row. To clear an existing
  backlog without waiting for re-discovery, run `cmd/backfill-videos`, which
  **enqueues** those listings for the workers (it renders nothing itself, so
  at least one instance with `ROLE=all` or `ROLE=worker` must be running):

  ```bash
  go run ./cmd/backfill-videos -dry-run   # report what would be enqueued
  go run ./cmd/backfill-videos            # enqueue; also revives dead queue rows
  go run ./cmd/backfill-videos -status    # queue: claimable / claimed / backoff / dead
  ```

  Workers render from the stored CDN photos and segment the result for the
  linear channels, so it costs no Zillow API quota.
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
app sends its device/advertising id (Roku RIDA) — else of the `ip` query
parameter, else of the client IP. `ip` is for platforms that fill a macro
(`master.m3u8?ip={RokuIP}`) because a server of theirs, not the device, may
fetch the stream. A `sid` outside 1–64 of `A-Za-z0-9._:-`, or an `ip` that is
not a public address, is ignored: an unfilled macro, and the LAN address
Roku puts in its macro (`10.0.0.90`), which would hide the public address the
request came from. `master.m3u8` carries both on to its `live.m3u8` reference, so a
player given only the master URL is still identified. Viewers are recorded once per minute per
channel in `viewer_heartbeats`, batched every 30 s and purged after
`VIEWER_RETENTION_DAYS`.

The raw public client address and user agent behind the heartbeats are kept
in `viewer_clients` (one row per ip, user agent and channel, with first and
last seen), purged on the same retention, and listed by an admin endpoint
mounted only when `VIEWER_ADMIN_KEY` is set:

```
GET /admin/viewers?key=…&hours=24&limit=500   (or Authorization: Bearer …)
→ {"since","count","viewers":[{"ip","user_agent","channel","first_seen","last_seen"}]}
```

It lives outside `/channels/` so nginx never caches it. A viewer only shows
up when one of their polls reaches the app: rarely a problem with `sid`/`ip`
in the URL (own cache entry), but at large audiences most scope-only polls
are served from the edge cache.

```
GET /channels/beat?state=TX&sid=… → 204   (heartbeat; call every ~60 s while playing)
GET /channels/resolve             → {"scope","name","master","epg","source"}
GET /channels/stats?state=TX      → {"scope","concurrent","unique_24h","unique_7d"}
```

Playlists are cached per scope at the edge, so audience size never reaches
the app; players report themselves with `beat` (same filters as the
playlist they are on). Polls of `live.m3u8` that do reach the app are
counted too, so anonymous third-party players still show up as a sample;
with `sid` or `ip` in the URL each viewer has its own cache entry, so every
one of their polls is counted.

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
- `GET /healthz` — liveness. On a `ROLE=worker` instance it is the only route,
  and answers 503 while the worker's circuit breaker is open.

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
every 12 hours; `@every 12h` also works), evaluated in UTC. It does not
trigger anything: it defines the **budget windows**. A new window — and a
fresh budget — starts at each activation; an instance that starts mid-window
joins the current one rather than getting a budget of its own.

`API_BUDGET_PER_CYCLE` (default `150`) caps the OpenWebNinja requests the
**whole fleet** may spend per window — search pages plus details calls,
retries included. One request is reserved in PostgreSQL immediately before
every paid HTTP attempt. `DETAILS_PER_CYCLE` (default `50`; `0` disables
enrichment entirely) is the share of it reserved for the one-time
details-API call per property; search gets the rest. ZIP codes are not
configured: a built-in table of all 29,670 US residential ZIPs is seeded on
first startup and worked through in rotation order (never-searched first,
most populous first, then stalest). When the budget runs out in the middle of
a ZIP, the search resumes from the next page in the next window. Nothing is
spent while the provider reports the monthly quota exhausted, or with less
headroom than the unspent part of the window's budget.

### Running several instances

`ROLE` selects what an instance runs: `all` (default), `api` (HTTP only) or
`worker` (the loops plus `/healthz`). `INSTANCE_ID` (default: hostname) names
it in logs and claims, `DB_MAX_CONNS` (default `10`) bounds its PostgreSQL
pool, and `QUEUE_HIGH_WATER` (default `2000`; `<= 0` disables) pauses
discovery while that many listings wait to be rendered. The budget variables,
the search criteria and the video settings must be identical on every
instance. Provisioning, the connection budget, rollout order and an SQL
runbook are in [`deploy/README.md`](deploy/README.md#fleet-worker-boxes).

`PUBLIC_BASE_URL` (e.g. `https://api.dwellings.tv`) is this server's public
origin, used for the absolute URLs in the Roku feed; empty leaves the live
channel out of the feed. The linear channels read `LINEAR_ENABLED` (`true`),
`LINEAR_LINEUP_HOURS` (`6`), `LINEAR_MIN_SCOPE_CLIPS` (`20`),
`LINEAR_EPG_HORIZON_HOURS` (`24`) and `LINEAR_LIVE_THUMBNAIL_URL` (empty —
the poster of the Roku live entry; see [Linear channels](#linear-channels)).
Viewer tracking reads `VIEWER_TRACKING_ENABLED` (`true`), `VIEWER_SALT`
(required while on), `VIEWER_SALT_ROTATE_DAILY` (`false`),
`VIEWER_RETENTION_DAYS` (`30`), `GEOIP_DB_PATH` (empty = no geo) and
`VIEWER_ADMIN_KEY` (empty = no `/admin/viewers`).

`AD_PREROLL_URL` and `AD_MIDROLL_URL` (both empty) are the VAST ad tags from
the ad server. The backend never calls them: `/api/v1/properties`,
`/api/v1/properties/{zpid}` and `/channels/resolve` return them as
`pre_roll_ad` / `mid_roll_ad` (null when unset, like the Cineplex category
ads), and the Roku app requests them through RAF, substituting device macros
such as `ROKU_ADS_TRACKING_ID` itself. A value that is not an absolute
http(s) URL fails startup.

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
- `SearchPages` pages until `SEARCH_MAX_RESULTS` is reached (`0` = uncapped,
  the default) or the window's API budget runs out (hard cap 20 pages per
  ZIP). Network errors, 429 and 5xx are retried up to three times, honouring
  `Retry-After`; every attempt is charged to the budget. Price and bedroom
  criteria are applied client-side.

## Notes

- Image upload/download failures for individual images are logged and skipped
  (Bunny uploads are retried up to three times first). A listing none of whose
  photos could be stored fails and is retried: an empty gallery is never
  persisted.

## Commands

```bash
make build   # compile to bin/server
make test    # run tests
make vet     # go vet
```
