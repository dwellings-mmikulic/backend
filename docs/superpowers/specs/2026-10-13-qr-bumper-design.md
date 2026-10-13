# QR bumper for the FAST channel — design

Direction agreed in chat 2026-10-13 (Daniel's pivot, relayed by Marko).
Builds on the linear channels (`2026-08-25-linear-channels-design.md`) and
the personal feeds (`2026-10-09-personal-feeds-qr-design.md`).

## Goal

The customization QR is for the FAST channel that goes out on other
platforms, not for the Dwellings app (those app changes are aborted). The
channel itself must carry the invitation, independent of the listing
videos already rendered: every 5 minutes it airs a short *bumper*, a clip
of its own showing a QR and the caption "Looking for something more
specific? Customize your channel through the QR code below".

Scanning the QR opens the existing mobile page (`dwellings.tv/tv/`). On a
platform whose player fetches our playlist per viewer and passes the
viewer's public IP as `ip=`, the TV then switches to the household's
personal feed, as in the app demo. On a platform that fetches from its
own servers, only the phone changes; the channel stays shared.

Non-goals: a designer-made bumper (we have no designer; the bumper is
generated), a logo (no logo file exists yet; it can be added as a
generator input later), per-viewer EPG, auto-localising a platform viewer
by IP before they scan (ruled out: the platform's EPG would describe a
channel the viewer is not seeing, and every viewer IP would create a
household row).

## The bumper asset

`cmd/make-bumper` renders, segments, uploads and registers one bumper. It
is run by hand, in the server image (`docker compose run --entrypoint`,
the way `backfill-hls` is), and again whenever a new bumper is wanted.

Inputs (flags, all with defaults):

| flag | default |
|---|---|
| `-url` | `https://dwellings.tv/tv/` (what the QR encodes) |
| `-caption` | Daniel's text above |
| `-seconds` | 12 |
| `-bg` | a solid colour (`0x101418`); `-bg-image` overrides it with a 16:9 image, scaled and cropped like listing photos |
| `-music` | none; a path to an MP3 from `assets/music` gives a music bed, faded in and out |
| `-name` | a label stored with the row (`default`) |

Render: the QR PNG comes from `internal/qrcode` (same package the
`/feed/qr.png` endpoint uses). One ffmpeg command composes a 1920x1080,
30 fps canvas: background, the caption in the DejaVu font (the same
`VIDEO_FONT_PATH` the listing renderer uses, two lines, centred, above
the QR), the QR centred and large (about 560 px, on a white rounded box
so it scans from the couch), the URL in small text under it, and a 0.5 s
fade in and out. Audio is the music bed or, without one, a silent AAC
track generated from `anullsrc`: the segmenter maps `0:a:0`, so a bumper
must always have an audio stream. Codec settings are the listing encode's
(`libx264`, `yuv420p`, `crf 20`, `preset veryfast`, `aac 160k`, `-r 30`,
`+faststart`), so it splices into a channel without a profile change and
the master playlist's codec string stays true. The argument builder is a
pure function in `internal/bumper` (`buildFFmpegArgs(spec)`), unit-tested
like the listing renderer's.

Content hash: SHA-256 of the inputs (url, caption, seconds, bg, the bytes
of bg-image and music, the font path, and a render version prefix `b1|`).
It names the CDN prefix `hls/bumpers/<hash8>/` (via a `hls.Prefix`-style
helper) and makes the command idempotent: if a `bumpers` row with that
hash exists, the command prints its id and stops.

Pipeline after the render: `hls.Segmenter.Segment` → `hls.Upload` to
Bunny → `Repository.InsertBumper`. A failed render uploads nothing; a
failed upload inserts nothing; a rerun redoes the work from the render
(uploads overwrite by path, as with listings).

Storage:

```sql
CREATE TABLE IF NOT EXISTS bumpers (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    content_hash TEXT NOT NULL UNIQUE,
    base_url     TEXT NOT NULL,
    segment_ms   INTEGER[] NOT NULL,
    total_ms     INTEGER NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Rows are never deleted: lineups that reference an old bumper keep
resolving, exactly as `video_hls` rows do. "The current bumper" is the row
with the highest id. There is no activation flag and no admin endpoint; to
change the bumper, run the command.

## Scheduling the bumper

The bumper is a lineup item. Nothing else in the system (playlists,
counters, EPG, personal splices) knows about time except through the
version chain, so that is where it goes.

Item ids: `channel_lineups.item_ids` stays `BIGINT[]`. A bumper with id
`b` is stored as `-b`; clip ids are always positive. Two helpers in
`internal/linear` are the only code allowed to know this:

```go
func bumperItem(id int64) int64   // -id
func isBumper(item int64) bool    // item < 0
func bumperID(item int64) int64   // -item
```

Building a version (`newVersion`/`buildItems`):

- `lineupSource.resolve` loads the current bumper once per request
  (`Store.CurrentBumper`, nil when the table is empty) next to the clips.
  With no bumper, or with the interval set to 0, the item list is exactly
  what it is today, byte for byte.
- After the seeded shuffle and the cut to `LineupHours`, the builder walks
  the clips and inserts the bumper in front of every clip whose start
  would fall `BumperInterval` or more after the end of the previous bumper
  (for the first one: after the version's start). A version never starts
  with the bumper and never ends with one. With 1.5–3 min clips the gap
  between bumpers is 5 to about 7 minutes, never less than the interval.
- The bumper's `TotalMS` and segment count enter `ItemMS`/`ItemSegs`, so
  `EndsAt`, `StartSeq` and `StartItem` of the next version are computed
  exactly as before. The cut to `LineupHours` is applied to clips before
  bumpers are added; a version therefore runs slightly over `LineupHours`
  by the bumper time, which nothing depends on.
- Determinism: a version is built once and stored; concurrent builders
  race on the primary key as today. The bumper chosen is whatever is
  current at build time, baked into the stored row, so a new bumper airs
  from the next version on (up to `LineupHours` later), and a channel
  restarting after idle picks it up immediately.

## Playing it

`Store.ClipsByID` resolves mixed ids: positive ones from `video_hls`,
negative ones from `bumpers`, returned in one map keyed by the item id as
stored (so the bumper's entry is keyed `-b`). `window`, `expandItems`,
`checkClips` and `writePlaylist` do not change: the bumper is an item with
a base URL and segment lengths, it gets its `EXT-X-DISCONTINUITY` and
`PROGRAM-DATE-TIME` like any clip boundary, and `checkClips` verifies its
recorded segment count the same way. A missing bumper row is an
`InconsistentLineupError` like a missing clip.

Personal feeds: `PersonalPlaylist` splices channel windows, and bumpers
are inside them, so a customized household sees the bumper at the
channel's cadence. `endOfAiring` (the earliest switch point) works for a
bumper segment as for any other. No change.

EPG: in `EPG`, bumper items are skipped when collecting `aired` items, so
they are not counted in `Listings`, not sent to `ListingsByClipID`, and
never title a block. A block containing only a bumper (impossible in
practice, bumpers never start a version and are 12 s) would be an idle
block and is skipped like a gap.

Stats and viewer tracking count playlist requests, not items. Unchanged.

## The TV switch on platforms that pass `ip=`

Today `/channels/live.m3u8?ip=<public ip>` uses the IP for viewer stats
only, and `/channels/master.m3u8` already carries a valid `ip=` into the
media playlist URI. The personal-feeds spec deliberately ignores `ip=` on
the `/feed/` routes; the channel routes are different: a platform fetching
on the viewer's behalf has no other way to tell us who is watching.

### When the channel follows the household

New rule in the `live` handler, applied only when personal feeds are
enabled (`h.feeds != nil`) and `viewer.QueryIP(q)` is non-empty:

1. `id := h.viewers.Hasher.Household(ip)` — the same hash the phone page
   computes from its own client IP, so a phone on the TV's Wi-Fi lands on
   the same household.
2. `h.svc.Household(ctx, id)`. `ErrUnknownHousehold` (no spans) means
   nobody has opened the page from that IP: serve the channel exactly as
   today. Any other error fails the request as today.
3. Otherwise serve the *anchored* personal playlist described below, with
   the channel playlist's headers, and track the request against the
   channel scope as today.

The household is created only by the phone (`/feed/me`), never by the
channel request. Viewers who never scan see the channel they picked, and
the platform's EPG stays truthful for them. Once the phone has opened the
page the TV follows the phone (first the IP's city, then whatever the
viewer picks), within one playlist poll plus a segment, as in the app
demo.

### Anchoring: keeping the player's counters continuous

The platform's player has been playing channel `C` with `C`'s media
sequence and discontinuity counters. The household's personal playlist
has its own counters (continued from whichever channel its span 0 was
defaulted to). Serving the personal playlist as-is would make the
counters jump, possibly backwards, which RFC 8216 forbids and which a
Roku player may treat as a fatal error. The switch therefore works like
a span switch: the stream served on the channel route is a two-part
chain, `C` up to a switch time `S`, the household's feed from `S` on,
relabelled so the counters continue.

Storage:

```sql
CREATE TABLE IF NOT EXISTS channel_anchors (
    household   BYTEA NOT NULL,      -- viewer hash bytes, as in household_spans
    channel_key TEXT NOT NULL,       -- the channel the platform requested
    starts_at   TIMESTAMPTZ NOT NULL, -- S: the household's feed airs from here
    seq_offset  BIGINT NOT NULL,     -- served seq  = personal seq  + seq_offset
    item_offset BIGINT NOT NULL,     -- served item = personal item + item_offset
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (household, channel_key)
);
```

Creating the anchor, on the first channel request that finds the
household (`Service.Anchor(ctx, id, sc, now)`):

- `S := endOfAiring(C, now)`: the end of the channel segment on air, the
  earliest moment the switch can take effect.
- `last` := the last segment of `C`'s window that ends by `S`;
  `first` := the first segment of the household's personal feed starting
  at or after `S` (the personal counterpart of `firstAtOrAfter`, read
  from the spans' contributions).
- `seq_offset = last.Seq + 1 − first.Seq`,
  `item_offset = last.Item + 1 − first.Item`: the first personal segment
  carries the counters the channel's next segment would have carried, and
  it is an item start, so the playlist emits a discontinuity there and
  the item numbering stays consistent with the tag count (the same
  invariant `splice` relies on).
- Insert-if-absent on the primary key, then re-read: concurrent requests
  for one household (the player polls every few seconds, and a platform
  may fetch from more than one edge) all serve the stored row.

Serving (`Service.AnchoredPlaylist(ctx, id, sc, now)`): `C`'s window
segments that end by `S` (none once `S` is older than the window), then
the personal feed's segments from `S` on, shifted by the anchor and with
the first one marked as an item start; trimmed to `liveWindow`. It is
`PersonalPlaylist` with one more relabelling in front and a channel
prefix, and shares its code.

The anchor is permanent for the (household, channel) pair. The counters
it produces stay monotonic for as long as the household's feed exists,
whatever the viewer picks later, because every later span already
continues the personal counters and the anchor only adds constants. A
viewer who opens the platform's channel a week later, with no player
state, gets a consistent playlist whose absolute counter values do not
matter. Anchors are never purged, like a household's current span: one
small row per (household, channel).

### Caching and limits

Caching: nginx keys the playlist cache on the full URL, so each `ip=` has
its own short-lived entry. No change.

Limits (to state to platforms): the switch needs the viewer's real public
IP in `ip=` on our playlist URL, fetched by the viewer's player. A
platform that proxies from its own servers, or passes a LAN or CGNAT
address, gets the shared channel for every viewer; the bumper's QR still
customizes the phone page. Shared public IPs (an office, carrier NAT) are
one household, as before.

## Config

One new setting: `LINEAR_BUMPER_INTERVAL_MINUTES`, default 5. `0` disables
bumpers for versions built from then on, without touching data. Nothing
else: prod already has `LINEAR_ENABLED`, `VIEWER_SALT` and the geo DB the
`ip=` switch relies on.

## Errors

- Generator: render failure → nothing uploaded, nothing inserted, non-zero
  exit with ffmpeg's stderr tail. Upload failure → nothing inserted.
  Existing hash → prints the id, exit 0.
- Play time: bumper row missing → `InconsistentLineupError`, same path as
  a missing clip (503, logged).
- `ip=` switch: store error on the household or anchor lookup → 503 as
  any other playlist failure; unknown household → the channel; a
  household whose feed has nothing within the window → `ErrNoContent`
  (503), the same as the `/feed/` route.

## Testing

Unit, with the in-memory store (`memstore`):

- `buildItems` with a bumper: inserted at the right positions, never
  first, never last, gaps ≥ interval; with no bumper or interval 0 the
  output equals today's; `ItemMS`/`ItemSegs` include the bumper.
- The version chain: `EndsAt`, `StartSeq`, `StartItem` account for the
  bumper.
- `ClipsByID` (memstore and Postgres) resolves mixed ids; `window` renders
  a bumper boundary as a discontinuity.
- EPG: a block's `Listings` and price range ignore bumper items; the
  title comes from the first listing.
- `PersonalPlaylist` across a bumper: offsets and discontinuities still
  monotonic.
- Anchor: created once per (household, channel) with `S` at the end of
  the segment on air; the served playlist's seq and item counters
  continue the channel's across `S`, with a discontinuity at the first
  personal segment; later polls reuse the anchor; a later `Choose` keeps
  the served counters monotonic.
- Handler: `/channels/live.m3u8?ip=` serves the anchored personal
  playlist only when the household has spans; without `ip=`, with a
  private `ip=`, or with feeds disabled it serves the channel; the
  request is tracked.
- `internal/bumper.buildFFmpegArgs`: pure, asserted for the codec profile,
  the silent-audio and music variants, and the hash inputs.

Integration (Postgres harness): the `bumpers` table, `InsertBumper`
idempotency on `content_hash`, `CurrentBumper`, mixed `ClipsByID`; the
`channel_anchors` table and insert-if-absent.

Smoke before shipping (local Docker stack, then prod): run the generator,
let a channel build a version, pull ~10 minutes of `live.m3u8` with ffmpeg
and confirm zero decode errors and the bumper at the expected offsets;
then open `/tv/` from a phone-sized browser, pick a city, and confirm
`/channels/live.m3u8?ip=<that public ip>` switches with continuous
counters (ffmpeg plays across the switch with zero errors).

## Rollout

1. Deploy (schema is applied by the server on start, as for other tables).
2. On web-01: `docker compose -f compose.prod.yml run --rm --entrypoint
   /app/make-bumper app -music /app/assets/music/03-calm-piano.mp3` (the
   music flag is optional). Note the printed id.
3. Within `LineupHours` every channel's next version carries the bumper;
   check a channel playlist and the EPG counts.
4. Tell platforms the playlist URL format with `ip=` and the limit above.
   Nothing changes in the Dwellings Roku app.
