# XMLTV guide for the FAST channel — design

Requested by Marko 2026-10-20: distributors need an EPG in the format of
Freebie TV's feeds (`…/feeds/epg/shema_freebietvus/AMG00864C4.xml`, an
XMLTV document). Builds on the linear channels
(`2026-08-25-linear-channels-design.md`), whose JSON EPG (§6) already
computes the schedule; this adds a second rendering of it.

## Why a guide is possible for a channel that changes by the minute

A distributor's guide needs programmes days ahead. Our lineup is only
materialised `LINEAR_EPG_HORIZON_HOURS` (24) ahead, and on purpose: a
lineup built today for next week would not contain next week's listings.

The resolution is that the guide schedules *blocks*, not listings. Every
30-minute block of a channel is the same show — the area's homes for sale —
so its title, length and category are known arbitrarily far ahead; only the
listings inside it are decided close to air time. Blocks the lineup already
covers carry the listing count and price range (as `epg.json` does); blocks
beyond it carry a standing description. Nothing is built for the far blocks,
so the guide horizon is independent of the lineup horizon.

## Endpoint

`GET /channels/epg.xml[?zip=… | ?city=…&state=… | ?state=…]`, the same
filters and scope fallback as `epg.json`, errors mapped the same way
(400 / 404 unknown area / 503 no content). `Content-Type:
application/xml; charset=utf-8`, `Cache-Control: public, max-age=300`,
CORS `*`. nginx caches it like `epg.json` (60 s, per filter).

The document mirrors the sample's elements:

- `<tv date="YYYY-MM-DD" source-info-name="Dwellings"
  generator-info-name="dwellings-backend">` with the XMLTV DOCTYPE;
- one `<channel id="<channel key>">` with the viewer-facing name
  (`id` is the scope key: `us`, `state:tx`, `city:katy|tx`, `zip:77494`;
  the channel key, not the effective scope, so the id is stable across
  fallback);
- one `<programme>` per 30-minute block from the block on air
  (`now − 1 block`) up to `now + LINEAR_EPG_GUIDE_DAYS` (7) days, with no
  gaps: `start`/`stop` as `2026-10-19T09:00:00.000+0000` (the sample's
  shape, not the DTD's `20261019090000 +0000`), `title` = channel name,
  `desc` as above (plus the "Customize this channel at …/tv" note when the
  personal feeds are on, as in `epg.json`), categories Lifestyle and Real
  Estate, `language` English, `length` 30 minutes, `episode-num
  system="assetID"` = `<key>@<YYYYMMDDTHHMM>` (unique per block), and the
  sample's `video`/`audio` blocks (1920x1080, stereo).

Left out on purpose: `icon` (no channel artwork URL is configured yet),
`rating`, `credits`, `previously-shown`. Add them when a distributor asks.

## Config

`LINEAR_EPG_GUIDE_DAYS` (default 7) → `linear.Options.GuideDays`.

## Testing

Unit tests on the renderer (continuous blocks to the horizon, timestamp
shape, summary vs standing description, unique asset ids, escaping of the
note) and the handler (headers, prologue, 7-day default, 404 for an unknown
area, note present only with feeds on). Config test for the new setting.
