# Listing Videos for Roku (DwellingTV) — Design

Date: 2026-06-20

## Goal

Automatically generate a 16:9 1080p MP4 video for each real-estate listing, with
listing-info overlays and background music, host it on Bunny CDN, and expose a
Roku Direct Publisher JSON feed so a Roku channel can play them.

## Decisions (from brainstorming)

- **Destination:** Roku TV channel (16:9 landscape, 1080p, H.264 MP4).
- **Structure:** one video per listing (Roku shows a catalog).
- **Style:** simple slideshow + overlays, rendered server-side with ffmpeg.
- **Trigger:** automatically after ingestion (a render stage in the collection cycle).
- **Overlays:** core listing facts + background music + a QR code linking to the Zillow listing.
- **Roku scope:** render + host MP4s AND serve a Roku Direct Publisher JSON feed.
- **Pacing:** all photos, fixed seconds per photo (configurable; default 4s).
- **Photo fit:** scale-to-cover + center-crop to 16:9.
- **Music:** 10 bundled CC0 tracks (FreePD mirror on archive.org) in `assets/music/`,
  one chosen deterministically by zpid (stable across re-renders).

## Rendering approach

Single ffmpeg invocation per listing (`internal/video`):
photos → `concat` demuxer → 1080p H.264 stream → filter graph draws the persistent
facts overlay (`drawtext`) and composites the QR PNG (`overlay`), with a music
track mixed in (`-shortest`). One process, one output file.

## Architecture changes

The service was scheduler-only. This adds:

1. `internal/video` — ffmpeg orchestration: builds args, renders MP4, selects music, builds facts text, computes content hash.
2. `internal/qrcode` — QR PNG from a URL (`github.com/skip2/go-qrcode`).
3. `internal/feed` + HTTP server — serves the Roku Direct Publisher feed; started alongside the scheduler in `main.go`.

Infra:
- Runtime Docker image switches from `distroless/static` to an image with the
  `ffmpeg` binary + a TTF font (alpine + `ffmpeg`, `ttf-dejavu`).
- Capture `detailUrl` from the Zillow API (already returned) for the QR + feed.
- ~65 MB of music baked into the image (acceptable; can move to Bunny later).

## Data flow

```
search → download photos → upload photos to Bunny      (existing)
      → render video (ffmpeg) → upload MP4 to Bunny → store video_url + status
```

Runs after image upload, idempotent via a content hash (price + photo set + facts):
re-render only when no `ready` video exists or content changed.

## Schema additions (`properties`, idempotent ALTER ... ADD COLUMN IF NOT EXISTS)

| column | purpose |
|---|---|
| `detail_url` | Zillow listing URL (QR + feed link) |
| `video_url` | Bunny CDN URL of the MP4 |
| `video_status` | `pending` / `ready` / `failed` |
| `video_content_hash` | detect when re-render is needed |
| `video_rendered_at` | timestamp |
| `video_duration_secs` | needed by the Roku feed |

## Video spec

1920×1080, H.264 (`libx264`, `yuv420p`), 30fps, AAC audio, `+faststart`.
Seconds-per-photo configurable (default 4s); duration = `photos × secs`.
Photos scaled to cover + center-cropped to 16:9.

Overlays (persistent for the whole video):
- Lower-third semi-transparent bar: price (large), address, facts row
  (`3 bd · 2 ba · 1,779 sqft · lot 9,999 sqft`).
- QR code bottom-right with a "Scan for details" caption → `detail_url`.

Music: a track from `assets/music/` chosen deterministically by zpid, looped/
trimmed to length, fixed volume. Empty dir → silent video (logged).

## Roku feed

`GET /roku/feed.json` — Roku Direct Publisher format. Each `ready` listing →
a `shortFormVideo`: `id` (zpid), `title` (`"$310,000 · 23265 McBurney Ave"`),
`content` (MP4 URL, `duration`, `videos[].quality=HD`), `thumbnail` (first photo),
`releaseDate`, `shortDescription` (beds/baths/sqft). Built from
`SELECT ... WHERE video_status='ready'`. Also `GET /healthz`. Port via `HTTP_PORT`
(default 8080).

## Error handling

Per-listing, non-fatal — one bad listing never aborts the cycle:
- Some photo downloads fail → render with what we have; zero photos → `failed`, skip.
- ffmpeg non-zero exit → capture stderr, mark `failed`, continue (retried next cycle).
- Bunny upload failure → `failed`, continue.
- Temp working dir always cleaned up (`defer`).

## Testing

- Unit: facts-overlay formatting; music selection (deterministic by zpid); QR
  payload = `detail_url`; content-hash change detection; ffmpeg args builder
  (assert args without running ffmpeg); Roku feed JSON (golden test).
- Integration (skipped if `ffmpeg` absent or `-short`): render a ~3s MP4 from 2
  test images; assert ffprobe reports 1920×1080, H.264, expected duration, audio stream.

## New config

| env | default | meaning |
|---|---|---|
| `VIDEO_ENABLED` | `true` | master switch for the render stage |
| `VIDEO_SECONDS_PER_PHOTO` | `4` | per-photo display time |
| `MUSIC_DIR` | `assets/music` | bundled CC0 tracks |
| `VIDEO_FONT_PATH` | `/usr/share/fonts/.../DejaVuSans-Bold.ttf` | drawtext font |
| `HTTP_PORT` | `8080` | Roku feed + health server |
