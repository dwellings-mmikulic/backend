# Image Format Normalization — Design

**Date:** 2026-07-14
**Status:** Approved

## Problem

Property images must be stored in Bunny CDN storage as JPEG or PNG only, at the
highest resolution available. Today:

- Production has `IMAGES_ENABLED=false` (set when the Bunny free trial lapsed in
  June 2026 and uploads returned 401). Billing is active again — a test upload
  from the prod box returns 201 — but the flag was never flipped back.
- `SKIP_EXISTING=true` means the 89 already-stored properties are never
  reprocessed, so re-enabling the flag alone would not backfill them.
- The pipeline stores whatever bytes the source returns. Zillow currently serves
  `image/jpeg` at `cc_ft_1536` (verified byte-identical to the original `o_a`
  variant — this is the maximum size Zillow publishes), but nothing guarantees
  JPEG/PNG if a source ever returns WebP, GIF, etc.

## Decision

Add `internal/imaging` with a single entry point:

```go
// Normalize returns image bytes guaranteed to be JPEG or PNG, with the
// matching file extension (".jpg" or ".png") and content type.
func Normalize(data []byte) (out []byte, ext, contentType string, err error)
```

Behavior:

- Format is sniffed from the **bytes** (magic numbers via `image.DecodeConfig`),
  never from URL extension or Content-Type headers.
- JPEG and PNG input is returned **unchanged** — no re-encode, no quality loss,
  resolution preserved.
- WebP, GIF, BMP, and TIFF are decoded (`golang.org/x/image` decoders +
  stdlib GIF) and re-encoded: PNG when the image carries transparency,
  otherwise JPEG at quality 95.
- Undecodable data returns an error; the caller skips that photo with a warning
  log (the listing still processes with its remaining photos).

Wiring: `scheduler.downloadPhotos` normalizes right after download, before
writing the local file, and names the file by the sniffed extension. Local
files are therefore always compliant before both the Bunny upload and the
video render. The URL-based `imageExt` guessing and the `.webp` branches in
`contentTypeForExt` become dead and are removed.

Resolution is already handled by `hiResZillow` (upgrades to `cc_ft_1536`) and
stays as-is.

## Alternatives considered

- **Normalize at upload time only** — leaves non-compliant bytes in the video
  render path and needs a second read of the file. Rejected: normalizing once
  at download covers both consumers.
- **Always re-encode to JPEG** — simpler but recompresses already-compliant
  JPEGs (quality loss) and destroys transparency. Rejected.

## Rollout / backfill (ops, after merge)

1. Push to `main`; CI builds and deploys the image.
2. On the web box: set `IMAGES_ENABLED=true` and temporarily
   `SKIP_EXISTING=false`, restart the app.
3. One collection cycle reprocesses all listings: uploads every photo to
   `properties/<zpid>/<n>.jpg|png`, and — because stored image URLs switch to
   CDN URLs — the video content hash changes, so all videos re-render once
   (this also retries the 24 currently-failed videos).
4. Verify objects in Bunny storage, then set `SKIP_EXISTING=true` back and
   restart.

## Testing

- Unit tests in `internal/imaging` with real fixture bytes per format:
  JPEG/PNG passthrough (bytes identical), WebP→JPEG, GIF→JPEG,
  transparent PNG stays PNG, transparent WebP→PNG, garbage → error.
- Scheduler tests updated: a fake source serving WebP results in a `.jpg`
  local file and a `image/jpeg` upload.
