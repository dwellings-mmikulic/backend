# Personal feeds + dynamic QR — design

Direction agreed in chat 2026-10-09 (Daniel's model, relayed by Marko).
Builds on the linear channels (`2026-08-25-linear-channels-design.md`) and
viewer tracking (`2026-08-26-viewer-tracking-design.md`).

## Goal

Every TV gets its own feed URL. Behind it the server decides which channel
airs: the household's choice if it made one, else the city its IP is in,
else national. Every 5 minutes the Dwellings Roku app shows a QR for one
minute. Scanning it opens a mobile page where the viewer enters a ZIP or
picks a city; on submit the server reroutes that household's feed, and the
TV changes within seconds without the app doing anything.

Purpose right now: a working example Marko can demo to platforms. Thin
coverage per ZIP is expected and handled by the existing scope fallback.

Non-goals: a personal EPG, price/beds filters, several TVs in one home
choosing differently (one household, one feed), the Roku app itself (its
required changes are listed at the end).

## Identity: the household

`Hasher.Household(ip)` = the existing viewer hash with `kind = "ip"`,
ignoring any `sid`. The TV and a phone on the home Wi-Fi share the public
IP, so they hash to the same household. Raw IPs are never stored.

The hex household id is the only thing that identifies a feed. It appears
in the QR URL and in the personal media-playlist URL. Knowing an id is the
capability to change that TV's feed, exactly as Daniel's raw-IP scheme,
except the id cannot be guessed from an address.

`VIEWER_SALT_ROTATE_DAILY` must stay off (prod default): a rotating salt
would make every household forget its choice at midnight UTC.

## Endpoints

All under `/feed/`, served by the API process, CORS `*`.

| Route | Purpose |
|---|---|
| `GET /feed/master.m3u8` | Personal master. Household from the connection IP (or a public `ip=` param, same precedence as today). Creates the household's first span if none. Points at `live.m3u8?hh=<id>`. `Cache-Control: no-store`. |
| `GET /feed/live.m3u8?hh=<id>` | Personal media playlist (below). Cached 2 s per URL like other playlists. Records a viewer heartbeat on the effective scope so `/channels/stats` keeps counting. |
| `GET /feed/qr.png` | QR PNG of `<MOBILE_BASE_URL>/tv/<id>` for the connecting household, built on the fly with `internal/qrcode`. `no-store`. Optional `size` (default 400, max 1000). |
| `GET /feed/me` | Same JSON as `GET /feed/{id}` for the connecting household (creates span 0 if needed), so a typed-in `/tv/` URL finds its feed by IP alone. `no-store`. |
| `GET /feed/{id}` | JSON for the mobile page: `{id, scope, name, requested, source, updated_at, live}`. `source` is `choice`, `geo` or `default`; `live` is the personal media playlist URL for an in-page player. |
| `POST /feed/{id}` | Body `{zip}` or `{city, state}`. Validated with `ParseScope` + `AreaExists` (400/404 as `/channels/`). Resolves the thin-scope fallback, appends a span, returns the same JSON as GET. 404 for an unknown household id. |
| `GET /feed/areas` | `[{city, state, name, clips}]` for every city with at least `MinScopeClips` current clips, cached 10 min. Feeds the mobile page's city picker. |
| `GET /tv/` and `GET /tv/{id}` | The mobile page (one embedded HTML file). |

`/channels/resolve` gains `feed: "<PUBLIC_BASE_URL>/feed/master.m3u8"`
and `qr: {url, show_seconds: 60, every_seconds: 300}` so Daniel can tune
the cadence without an app release (`QR_SHOW_SECONDS`, `QR_EVERY_SECONDS`).

## Storage

```sql
CREATE TABLE IF NOT EXISTS household_spans (
    household   BYTEA       NOT NULL,   -- 16-byte viewer hash
    n           INTEGER     NOT NULL,   -- 0, 1, 2… in order
    scope       TEXT        NOT NULL,   -- effective channel key after fallback
    requested   TEXT        NOT NULL,   -- what the viewer asked for ("" = none)
    source      TEXT        NOT NULL,   -- choice | geo | default
    starts_at   TIMESTAMPTZ NOT NULL,   -- S: wall-clock the span begins airing
    seq_offset  BIGINT      NOT NULL,   -- personal seq = channel seq + seq_offset
    item_offset BIGINT      NOT NULL,   -- personal item = channel item + item_offset
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (household, n)
);
```

A household's feed is its chain of spans. The latest span is its current
choice. Rows are never updated except the one case below; old spans are
purged with the viewer retention loop once older than `VIEWER_RETENTION_DAYS`
and no longer the latest.

## Default channel (span 0)

Created by the first `/feed/master.m3u8` request, `starts_at = now`,
offsets 0, scope from the existing resolver order minus "last watched":
the IP's ZIP, city, then state (`geoCandidates`, each through
`resolveScope`), else national. `source` records which. Without the
GeoLite2 file every household starts national.

## Switching: the personal playlist

The scope channels keep their deterministic lineup chains and sequence
counters, untouched. A personal playlist is a splice of scope-channel
windows:

- Span k contributes the segments of channel `scope_k` that **start at or
  after `S_k`** and have **ended by `min(now, S_{k+1})`**, in channel
  order. (Span 0 contributes from `S_0 − 1 h`, so a fresh feed has a full
  window at once, like a fresh channel.)
- Each segment's `Seq` and `Item` are shifted by the span's offsets. The
  first segment of a span k > 0 is marked `FirstOfItem` so the writer emits
  `EXT-X-DISCONTINUITY` there, whatever position in its clip it has.
- The spans' segments are concatenated oldest first and trimmed with the
  existing `windowSpec` (≥ 40 s, ≥ 6 segments). `writePlaylist` is reused
  unchanged; `DISCONTINUITY-SEQUENCE` still falls out of the first listed
  segment's `Item`.

Appending span k+1 (on POST), inside one transaction holding the
household's latest span `FOR UPDATE`:

1. `S = end` of the segment of `scope_k` airing at `now` (the first segment
   with `end > now`), so the old channel finishes the segment the viewer is
   in. If the latest span has not started yet (`starts_at > now`, a second
   submit within seconds), it is replaced in place instead and `S` stays.
2. Ensure `scope_{k+1}`'s lineup chain covers `S` (`current(key, S)`, which
   creates versions lazily: this is where a never-watched city or ZIP is
   "created" on first request).
3. `first` = the first segment of `scope_{k+1}` with `start ≥ S`;
   `last` = the last segment of span k (`end ≤ S`), both with span k's
   offsets applied.
4. `seq_offset = last.Seq + 1 − first.channelSeq`,
   `item_offset = last.Item + 1 − first.channelItem`.

This keeps `MEDIA-SEQUENCE` and `DISCONTINUITY-SEQUENCE` strictly
increasing across the switch, and the player sees a boundary identical to
the clip-to-clip boundaries the channels already contain. Media timestamps
restart per segment already (`independent_segments` + discontinuity), so a
mid-clip entry into the new channel decodes cleanly.

Gap at the splice: between `S` and `first.end` (≤ one target duration) no
new segment is listed; the player's buffer covers it. The viewer sees the
new city roughly 10–20 s after submit, after the segments already buffered
play out.

Window cost: at most the spans intersecting the last ~60 s, each a cached
version lookup plus one clips query, per poll per TV. The same in-memory
version cache and singleflight as the channels apply.

## Mobile page (`/tv/{id}`)

Single HTML file embedded in the binary (no build step), phone-first.
Calls `/feed/{id}` on load and shows "Your TV is showing: Los Angeles,
CA". A ZIP field and a searchable city list (from `/feed/areas`). Submit
posts, then shows the effective area ("Showing homes around Katy, TX" when
a ZIP fell back) and a "Watch here" button that plays
`/feed/live.m3u8?hh=<id>` in-page with hls.js (cdnjs, pinned), so the
demo works with no Roku in the room. `/tv/` with no id uses the phone's own
household (the page calls `/feed/me` and rewrites its URL to `/tv/<id>`),
which is Daniel's pure-IP path for a typed-in URL.

nginx on `dwellings.tv` proxies `/tv/` to the API box so the QR URL is
short; `api.dwellings.tv` serves `/feed/` with `/feed/live.m3u8` cached 2 s
by full URI and everything else uncached.

## Config

`MOBILE_BASE_URL` (default `https://dwellings.tv`), `QR_SHOW_SECONDS`
(60), `QR_EVERY_SECONDS` (300). Personal feeds are on whenever
`LINEAR_ENABLED` and viewer tracking are (they need the salt).

## Errors

Unknown household id → 404. Unknown area → 404 `unknown area`; the page
says "We don't have that area yet". No content at all → 503 like the
channels. A span whose scope channel hits `InconsistentLineupError` fails
the request loudly, as channels do.

## Testing

- `viewer`: `Household` ignores sid, equals `IDAt("", ip)`.
- `linear` (fake clock, in-memory store, as the existing tests): splicing
  two channels at `S` yields strictly increasing `Seq`, a discontinuity at
  the splice, a correct `DISCONTINUITY-SEQUENCE` as the window slides past
  it, and the same bytes from two service instances; replace-in-place for
  a second submit before `S`; span 0 window at cold start; purge keeps the
  latest span.
- Handlers: master/live/qr/GET/POST/areas status codes, headers, 404s,
  validation; resolve carries `feed` and `qr`.
- Repository integration test for `household_spans` (same pattern as the
  lineups one).
- Manual: play `/feed/master.m3u8` in hls.js and VLC, submit from the page,
  confirm a clean switch; then on a Roku with the updated app.

## Roku app changes (outside this repo)

1. Play `resolve.feed` (the personal master) instead of a scope master.
2. Every `qr.every_seconds`, overlay `qr.url` (fetch fresh each time) for
   `qr.show_seconds`, bottom-right, with "Scan to pick your city".
3. Keep calling `/channels/beat` as today. Nothing else: the switch is
   server-side.

## Rollout

Schema applies on start. nginx: `/feed/` location on the API host, `/tv/`
proxy on the apex. `.env`: `MOBILE_BASE_URL`. GeoLite2 file on the box for
the city default (pending MaxMind credentials since August; national until
then).
