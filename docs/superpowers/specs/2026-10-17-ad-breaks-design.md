# Cued ad breaks for the FAST channel — design

Requested by Daniel 2026-10-17 (the syndication one-sheet promises
"SCTE-35 ad cues" and "SSAI ready"); scoped by Marko to CUE-OUT/CUE-IN tags
with a slate break between listings. Builds on the linear channels
(`2026-08-25-linear-channels-design.md`) and the QR bumper
(`2026-10-13-qr-bumper-design.md`), whose mechanism it reuses.

## Goal

A FAST platform's server-side ad insertion (SSAI) needs to be told where
the ad breaks are. Our live playlists carried no cue of any kind: only
`EXT-X-DISCONTINUITY` per clip and `EXT-X-PROGRAM-DATE-TIME`. The channel
now airs a break of our own between listings on a fixed cadence and marks
it in every media playlist the way legacy SCTE-35 HLS markers are read
(AWS MediaPackage "SCTE-35 enhanced", MediaTailor, Wurl, Amagi):

```
#EXT-X-DISCONTINUITY
#EXT-X-CUE-OUT:60.000
#EXT-X-PROGRAM-DATE-TIME:…
#EXTINF:5.000,
…/hls/slates/<hash8>/seg-000.ts
#EXT-X-CUE-OUT-CONT:ElapsedTime=5.000,Duration=60.000
#EXTINF:5.000,
…/seg-001.ts
…
#EXT-X-DISCONTINUITY
#EXT-X-CUE-IN
#EXT-X-PROGRAM-DATE-TIME:…
#EXTINF:…
```

Non-goals: binary SCTE-35 payloads (`EXT-X-DATERANGE` with `SCTE35-OUT`,
or a SCTE-35 PID in the transport stream, for partners that ingest SRT/UDP),
ads in our own Roku app during the break, DASH, 4K.

## The slate

A break is filled with a *slate*: a clip of our own that a stitcher
replaces with ads and that any player without SSAI (our Roku app included)
simply shows. It is the QR bumper's render with its own caption ("We'll be
right back / Scan the QR code to customize your channel") and length (60 s),
made with `make-bumper -kind slate`, uploaded under `hls/slates/<hash8>/`
and stored in the `bumpers` table with `kind = 'slate'` (new column, default
`'bumper'`, added with `ALTER TABLE … ADD COLUMN IF NOT EXISTS`). The kind
is part of the content hash only for non-bumper kinds, so every existing
bumper keeps its hash. `-seconds` is 3..180 for a slate; the cue's duration
is the rendered slate's `total_ms`.

"The current slate" is the newest `slate` row, as "the current bumper" is
the newest `bumper` row: `Store.CurrentBumper(ctx, kind)`.

## Scheduling

`LINEAR_AD_BREAK_INTERVAL_MINUTES` (default 10; 0 = off) is to the slate
what `LINEAR_BUMPER_INTERVAL_MINUTES` is to the bumper. The lineup builder's
bumper insertion is generalised to a list of *inserts* (clip + interval):
each is placed before the first clip that would start that interval or more
of air time after the insert last ended, air time counting everything aired
including the other insert. Inserts due at one boundary air in list order:
the slate first, then the bumper, so the invitation follows the ads straight
into content. An insert is never a version's first or last item. With no
slate row or interval 0 the lineup is what it was.

Nothing else changes: the slate enters `ItemMS`/`ItemSegs` like the bumper,
`ClipsByID` resolves it (marked `Break`), the EPG skips it as it skips
bumpers, personal feeds and the anchored `ip=` playlists carry it inside the
channel windows they splice.

## Cues in the playlist

`ClipSegments.Break` marks a slate; `expandItems` stamps each of its
segments with the break length and the elapsed time at the segment's start.
`writePlaylist`, which every playlist route shares, emits per segment, after
the discontinuity and before `PROGRAM-DATE-TIME`:

- first slate segment: `#EXT-X-CUE-OUT:<seconds>`;
- later slate segments: `#EXT-X-CUE-OUT-CONT:ElapsedTime=…,Duration=…`,
  also in place of CUE-OUT when the window opens mid-break so a joining
  stitcher knows where in the break it is;
- first content segment after a slate segment: `#EXT-X-CUE-IN`.

A break whose last segment has left the window takes its CUE-IN with it,
as any tag goes with its segment. A personal-feed switch mid-slate ends the
break early with a CUE-IN at the splice, which is the right signal.

Lead time: the cue enters the playlist when the slate's first segment has
ended, and players sit three target durations behind the listed edge, so a
stitcher sees it 10–25 s before playback reaches it. A platform that needs
an earlier announcement needs `EXT-X-DATERANGE` with a future `START-DATE`
(a follow-up).

## Config, errors, testing

One new setting, above. A missing slate row for a stored item is an
`InconsistentLineupError`, as for a bumper. Unit tests cover the writer
(cues around a slate, a window opening mid-break, none once the break has
left), the builder (slate before bumper at a shared boundary; interval 0),
the memory store (current per kind, `Break` on slates), the service (a
version airs the slate at the cadence), the config default, the hash
(kind-sensitive), the CDN prefix and the command's kind handling. The
Postgres integration test covers the column, current-per-kind and `Break`.

## Rollout

1. Deploy (the column is added on start). Nothing airs yet.
2. On web-01: `docker compose -f compose.prod.yml run --rm --entrypoint
   /app/make-bumper app -kind slate -music /app/assets/music/03-calm-piano.mp3`.
3. Within `LINEAR_LINEUP_HOURS` every channel's next version carries the
   break; pull `live.m3u8` across one and check the CUE tags.
4. Tell platforms: legacy CUE-OUT/CUE-IN markers, 60 s breaks every 10 min
   of air time, single 1080p rendition.
