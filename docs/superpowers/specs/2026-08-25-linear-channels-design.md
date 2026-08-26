# Linear Channels (virtual 24/7 HLS from listing videos) — Design

## Goal

Turn the library of per-listing MP4s (~40k ready videos, ~980 h, one uniform
encode profile) into 24/7 linear "TV channels" that a Roku/TV app or a FAST
distributor can play as a live HLS stream with an EPG. Channels are filterable
by ZIP, city or state, with automatic fallback to a wider area when a scope has
too little content.

Approach: **pseudo-live channel assembly**. Every clip is cut into HLS segments
once and stored immutably on Bunny CDN. The Go server never encodes at runtime;
it computes a sliding live playlist as a pure function of
`(channel, wall-clock)` over a deterministic, DB-materialised lineup. Two
instances, or an instance restarted mid-stream, emit byte-identical playlists.
This is the pattern behind AWS MediaTailor Channel Assembly and
nginx-vod-module's mapped-live mode.

Non-goals for this pass: re-encoding the library (fixed GOP, 48 kHz), captions,
ad slates / SCTE-35 payloads, the Roku app itself, per-viewer sessions.
Content-rights questions are explicitly out of scope.

## Measured inputs (prod, 2026-08-25)

- 40,178 ready videos, avg 87 s; 187 city/state pairs, 89 with ≥50 clips;
  median city 38 clips; many ZIPs have only a handful.
- Every MP4: H.264 High 1920×1080 @30 CFR, yuv420p, AAC-LC 44.1 kHz stereo,
  `+faststart`. Keyframes are scene-cut driven with x264's default
  `keyint=250` cap, so the gap between keyframes is **≤ 8.33 s**.

Because all clips share one profile, segments can be produced by remuxing
(`-c copy`) — no decode/encode — and clips can be chained with
`EXT-X-DISCONTINUITY` (timestamps restart per clip) without re-encoding.

## Components

```
internal/hls       ffmpeg remux → TS segments + parsed durations (pure I/O)
internal/linear    channels: scope resolution, lineup chain, playlist + EPG
                   writers, HTTP handlers, repository (video_hls, channel_lineups)
internal/scheduler renderVideo → also segments + uploads the new clip
cmd/backfill-hls   segments the existing backlog from the CDN MP4s
internal/server    mounts the linear handlers
internal/feed      adds a liveFeeds entry for the national channel
```

### 1. Segmentation (`internal/hls`)

`Segmenter.Segment(ctx, mp4Path, outDir) (Clip, error)` runs

```
ffmpeg -y -i in.mp4 -map 0:v:0 -map 0:a:0 -c copy
       -f hls -hls_time 2 -hls_list_size 0 -hls_flags independent_segments
       -hls_segment_type mpegts -hls_segment_filename <outDir>/seg-%03d.ts
       <outDir>/index.m3u8
```

- `-hls_time 2` with `-c copy` cuts at **every** keyframe (spacing is always
  ≥ 2 s), giving 3–8.33 s segments.
- `-map 0:a:0` makes a clip without an audio track fail — a silent clip would
  break audio continuity in the channel, so it is excluded rather than aired.
- `Clip{SegmentMS []int, TotalMS int}` is parsed from the emitted `index.m3u8`
  (`#EXTINF` values, rounded to ms). A clip whose longest segment exceeds
  `TargetDuration` (10 s) is rejected with an error.

The scheduler and backfill upload `seg-NNN.ts` and `index.m3u8` to
`hls/v1/<zpid>/<hash8>/` where `hash8` is the first 8 chars of
`video_content_hash`. Paths are immutable: a re-rendered listing gets a new
prefix, and lineups that already reference the old segments keep playing.
The CDN caches segments indefinitely. A given (zpid, content_hash) always
segments to the same layout, so recording it a second time is a no-op.

### 2. Storage

```sql
CREATE TABLE IF NOT EXISTS video_hls (
    id           BIGSERIAL PRIMARY KEY,
    zpid         TEXT NOT NULL REFERENCES properties(zpid),
    content_hash TEXT NOT NULL,
    base_url     TEXT NOT NULL,          -- https://cdn/hls/v1/<zpid>/<hash8>
    segment_ms   INTEGER[] NOT NULL,
    total_ms     INTEGER NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (zpid, content_hash)
);

CREATE TABLE IF NOT EXISTS channel_lineups (
    channel_key TEXT NOT NULL,            -- "us" | "state:tx" | "city:katy|tx" | "zip:77494"
    version     INTEGER NOT NULL,
    scope       TEXT NOT NULL,            -- effective scope after fallback, same syntax
    starts_at   TIMESTAMPTZ NOT NULL,
    ends_at     TIMESTAMPTZ NOT NULL,     -- starts_at + sum(item_ms)
    start_seq   BIGINT NOT NULL,          -- media sequence of the first segment
    start_item  BIGINT NOT NULL,          -- global index of item 0 (discontinuity base)
    item_ids    BIGINT[] NOT NULL,        -- video_hls.id, in air order
    item_ms     INTEGER[] NOT NULL,
    item_segs   INTEGER[] NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_key, version)
);
```

A clip is **current** when `properties.video_status = 'ready'` and
`properties.video_content_hash = video_hls.content_hash`. Only current clips
enter new lineups; old rows are never deleted so old lineups stay resolvable.

`video_hls` rows are **immutable**: the write is
`ON CONFLICT (zpid, content_hash) DO NOTHING`, never `DO UPDATE`. Stored
lineups record each item's segment count in `item_segs`, and every later
segment's media sequence number is derived from it, so rewriting `segment_ms`
under a live lineup would desynchronise the whole channel.

### 3. Channels and scope fallback

Channel key from query params (`zip`, or `city`+`state`, or `state`, or none):
normalised lowercase, e.g. `zip:77494`, `city:katy|tx`, `state:tx`, `us`.
Invalid combinations (city without state, zip plus state) → 400, as is any raw
parameter longer than 64 characters.

The endpoints are unauthenticated, and every distinct key is a cache entry and
potentially a stored lineup chain, so a filter is validated against real
geography **before** anything is created for it:

- `state` must be a real two-letter US state code (the `stateNames` table);
- `zip` must exist in `zip_codes`;
- `city`+`state` must be a pair some property is in.

An area that fails → 404 `{"error":"unknown area"}` and no version row. A real
but empty area (a ZIP with no listings yet) is not an unknown area: it falls
back up the hierarchy like any thin scope.

When a lineup version is created the key's scope is resolved against the
eligible clip count with fallback up the hierarchy until a scope has at least
`MinScopeClips` (default 20) current clips:

```
zip → city|state (looked up from the properties in that ZIP) → state → us
```

The effective scope is recorded on the version and reported in the EPG. It is
re-evaluated once per request that has to build versions — not once per
version, since one cold EPG request can materialise a dozen of them and each
resolution costs a `ListClips` scan per fallback level — so a ZIP that gains
content gets its own lineup from the next version onwards.

### 4. Lineup chain

A channel's timeline is a chain of versions. Version N covers
`[starts_at, ends_at)`, has `start_seq` = media sequence number of its first
segment and `start_item` = global 0-based index of its first item.

Creating version N (lazily, on the first request that needs it):

- eligible = current clips in the resolved scope;
- `seed = fnv64(channel_key + "|" + version)`; shuffle eligible (sorted by id
  first so the input order is canonical) with `math/rand` seeded by `seed`;
- take the prefix whose total ≤ `LineupHours` (default 6 h); a scope with less
  content than that airs everything once per version, and the next version is
  a fresh shuffle (so thin scopes loop with variation);
- `starts_at`: version 1 → `now − 1 h` truncated to the minute, so the channel
  has history to serve a full window immediately; version N>1 →
  `prev.ends_at`, unless the channel was idle past `prev.ends_at + 5 min`, in
  which case `now − 1 h` (nobody could have been watching, so continuity is
  irrelevant; sequence counters still continue from prev);
- `start_seq = prev.start_seq + sum(prev.item_segs)`,
  `start_item = prev.start_item + len(prev.item_ids)`;
- `INSERT … ON CONFLICT (channel_key, version) DO NOTHING`, then re-read: a
  concurrent instance may have won, and its row is canonical.

Serving a request at `t` means finding the version with the greatest
`starts_at ≤ t` (`VersionsAt`, which also returns its predecessor) — never the
chain's tip. The EPG extends the tip up to a day into the future, so the tip
is routinely not the version on air. A new version is appended only when the
whole chain ends at or before `t`, so extending the chain can never collide
with a version number that already exists; a `t` before version 1 is an error,
and a `t` inside the hole an idle restart leaves behind is served from the
version that ended most recently.

The service caches versions in memory (per channel) so a request costs at most
one small query for the items in the window. A cached entry is served only
while it is younger than 30 s **and** its `cur` still covers the requested
time; the map is capped at 4096 entries, evicting expired entries first and
then the oldest, so unauthenticated traffic cannot grow it without bound.
Concurrent misses on one key are collapsed with `singleflight`: a miss costs a
full scope resolution plus an insert, and a version rollover makes every
in-flight request for that channel miss at once.

### 5. Live playlist

`GET /channels/master.m3u8?…` → one `EXT-X-STREAM-INF` (BANDWIDTH 1,400,000,
AVERAGE-BANDWIDTH 1,100,000, `CODECS="avc1.640028,mp4a.40.2"`,
`RESOLUTION=1920x1080`, `FRAME-RATE=30.000`) pointing at `live.m3u8` with the
scope re-encoded from the parsed filter (never the request's raw query).

`GET /channels/live.m3u8?…` at wall-clock `now`:

- the **live edge** is `now`; a segment is listed only if its end ≤ `now`;
- window = the newest such segments spanning **at least 40 s** (4 ×
  `TargetDuration`) of listed duration, and never fewer than **6** segments.
  RFC 8216 §6.2.2 requires a live playlist to span ≥ 3 × target duration, and
  segments are 3–8.33 s, so a fixed six-segment window can be as little as
  18 s. The window may span a version boundary (the two most recent versions
  are loaded), and the items fetched for it are bounded by the same rule, not
  by a fixed items-back count;
- `#EXTM3U`, `#EXT-X-VERSION:6` (INDEPENDENT-SEGMENTS,
  DISCONTINUITY-SEQUENCE, AVERAGE-BANDWIDTH and FRAME-RATE are all past
  version 3), `#EXT-X-TARGETDURATION:10`,
  `#EXT-X-INDEPENDENT-SEGMENTS`, `#EXT-X-MEDIA-SEQUENCE`,
  `#EXT-X-DISCONTINUITY-SEQUENCE`, then per segment: `#EXT-X-DISCONTINUITY`
  before the first segment of every item except the channel's very first
  item (global index 0), `#EXT-X-PROGRAM-DATE-TIME` on the first segment of
  the window and after each discontinuity, `#EXTINF:<sec>,`, absolute segment
  URL;
- media sequence of a segment = `version.start_seq + segments before it in the
  version`; the discontinuity sequence = number of discontinuity tags belonging
  to segments that precede the window: with `g` = global item index of the
  window's first segment, `removed = 0` if `g = 0`, else `g − 1 + (1 if the
  first listed segment is not item g's first)`;
- headers: `Content-Type: application/vnd.apple.mpegurl`,
  `Cache-Control: public, max-age=2`, `Access-Control-Allow-Origin: *`.

No `EXT-X-ENDLIST`, ever. Wall-clock arithmetic is integer milliseconds.

### 6. EPG

`GET /channels/epg.json?…` returns

```json
{
  "channel": {"key": "zip:77494", "scope": "city:katy|tx", "name": "Homes for sale in Katy, TX"},
  "programs": [
    {"start": "…", "end": "…", "title": "Homes for sale in Katy, TX",
     "description": "12 listings · $285,000–$1,150,000", "listings": 12}
  ]
}
```

Programs are 30-minute wall-clock blocks covering
`[now − 1 block, now + EPGHorizonHours)` (default horizon 24 h) — the block
already airing plus the horizon, never the channel's whole history, so the
response and its queries stay bounded however long the channel has run. Only
the versions overlapping that window are loaded
(`ListVersionsBetween`, indexed on `(channel_key, ends_at)`). The description
is computed from the properties of the items that start in that block.
`Cache-Control: public, max-age=300`.

A request extends the version chain by at most a fixed budget of versions
(`epgChainBudget`), so a thin scope — whose versions are only minutes long —
returns partial coverage that grows across successive polls (the 5-minute
cache paces them) instead of building dozens of lineups synchronously or
failing. Blocks with nothing on air are omitted.

`channel.scope` and `channel.name` come from the version **on air now**, not
from the chain's tip (a version up to a day ahead, whose scope may have been
re-resolved to a different area). A block's `title` comes from the first item
that starts in it.

### 7. Roku feed

`/roku/feed.json` gains `liveFeeds: [{id: "dwellingtv-live", title:
"DwellingTV Live", content: {dateAdded, videos: [{url:
"<PublicBaseURL>/channels/master.m3u8", quality: "HD", videoType: "HLS"}]},
thumbnail, shortDescription, tags}]` when `PublicBaseURL` is configured.

The thumbnail is `LINEAR_LIVE_THUMBNAIL_URL`. Roku Direct Publisher requires
a non-empty thumbnail on every `liveFeeds` entry, so when none is configured
the first ready listing's first image is borrowed, and when there is no image
either **the entry is omitted** — an empty thumbnail, or a branding URL that
was never uploaded, would fail ingestion. The entry is also omitted while the
channels have nothing to play (`Service.HasContent`, a count of current clips
cached for 60 s), so the feed never advertises a stream that answers 503.

### 8. Scheduler and backfill

- `renderVideo` after `SetVideoReady`: segment the local MP4, upload, record
  `video_hls`. Failures are logged and non-fatal; the backfill picks them up.
- `cmd/backfill-hls`: lists ready videos with no `video_hls` row for their
  current hash (oldest first), downloads `video_url`, segments, uploads
  (`-concurrency`, default 4; `-upload-concurrency`, default 8; `-limit`;
  `-dry-run`). Costs no Zillow quota. It only considers properties that
  actually have a `video_content_hash`, reports progress every 100 videos,
  stops on SIGINT/SIGTERM and exits non-zero if any video failed. It ships in
  the server image and is run with
  `docker compose -f compose.prod.yml run --rm --entrypoint /app/backfill-hls app …`.

### 9. Configuration

| Env | Default | Purpose |
|---|---|---|
| `LINEAR_ENABLED` | `true` | mount the channel endpoints and segment new renders |
| `PUBLIC_BASE_URL` | `` | absolute base for the Roku liveFeeds URL (e.g. `https://api.dwellings.tv`); empty = no liveFeeds entry |
| `LINEAR_LINEUP_HOURS` | `6` | max content per lineup version |
| `LINEAR_MIN_SCOPE_CLIPS` | `20` | fallback threshold |
| `LINEAR_EPG_HORIZON_HOURS` | `24` | how far ahead the EPG materialises |
| `LINEAR_LIVE_THUMBNAIL_URL` | `` | poster of the Roku live entry; empty borrows the first ready listing's image, and with neither the entry is omitted |

At startup the server warns when `LINEAR_ENABLED` is on but `VIDEO_ENABLED` is
off (routes mounted, nothing will ever be segmented), when `PUBLIC_BASE_URL`
is empty (no Roku live entry), when the renderer found no music tracks (silent
renders cannot be segmented), and when no clip has been segmented yet (run
`cmd/backfill-hls`).

Constants: `TargetDuration = 10 s`, live window ≥ 40 s of ended segments with
a minimum of 6, `HLSVersion = "v1"`.

## Error handling

- Unknown/invalid filter params → 400 JSON `{"error": …}` (same shape as the
  public API). A scope that resolves all the way to `us` with zero current
  clips → 503 `{"error":"no content"}`.
- Segmentation errors never fail a render; the MP4 is still ready for the VOD
  feed. The clip is simply absent from channels until the backfill succeeds.
- A lineup that disagrees with the clips it references — a missing
  `video_hls` row, or a row whose segment count differs from the version's
  `item_segs` (both impossible unless rows were changed by hand) → the
  request fails with `ErrInconsistentLineup`, which the handler answers as
  500 `{"error":"internal error"}` and logs with the channel, version, item
  index and clip id. It is never a panic: one bad row must not take the
  process down.

## Testing

- `internal/hls`: playlist parsing (golden), argument builder (pure), and an
  ffmpeg integration test that remuxes a generated MP4 and checks segment
  durations sum to the clip duration (skipped when ffmpeg is absent, like
  the render test).
- `internal/linear`: fake store; determinism (two services over the same
  store produce identical bytes for the same `now`); window arithmetic across
  a version boundary; media/discontinuity sequence monotonicity as `now`
  advances; scope fallback; lineup creation race (`ON CONFLICT` path); EPG
  block boundaries; playlist golden files; a live playlist served by a second
  Service after an EPG request on the first pushed the chain tip past `now`
  (regression: the current version is not the chain tip).
- `internal/linear` against a real PostgreSQL
  (`repository_integration_test.go`, skipped unless `TEST_DATABASE_URL` is
  set): every Store method, the `INTEGER[]`/`BIGINT[]` round-trips, the
  `video_hls` and `channel_lineups` conflict behaviour, and the window
  queries — the in-memory store reimplements those rather than running them.
- `internal/scheduler`: a segmenter failure still leaves the render ready
  (no `SetVideoFailed`, no `hls/` upload).
- `internal/feed`: liveFeeds golden.
- `internal/server`: routes mounted, headers, 400 paths.
