# Property Maps via LocationIQ — Design

**Date:** 2026-07-29
**Status:** Approved

## Goal

Give every property a static map image with a pin on the home, hosted on Bunny
CDN and exposed through the public detail API as `map_image_url`.

Maps are generated **on demand**: when a detail request hits a property that has
no map yet, the map is generated during that request. There is no scheduled
map-generation pass — a listing nobody looks at costs nothing.

## Scope

In scope:

- A LocationIQ client (forward geocoding + static maps).
- On-demand map generation wired into `GET /api/v1/properties/{zpid}`.
- Two additive `properties` columns and a repository method to write them.
- `map_image_url` on the detail DTO and its Swagger annotation.

Out of scope:

- Maps on browse cards (`GET /api/v1/properties`) — detail endpoint only.
- A map slide in the rendered listing video.
- Tunable map styling (zoom, size, marker) — hardcoded until there is a reason.

## Components

### `internal/locationiq` — API client

Mirrors the shape of `internal/zillow`: a struct holding base URLs, an API key,
and an `*http.Client`, constructed in `cmd/server/main.go`.

**`Geocode(ctx, addr Address) (lat, lon float64, err error)`**

`GET https://us1.locationiq.com/v1/search/structured` with `key`, `format=json`,
`country=us`, and `street` / `city` / `state` / `postalcode` from the property.

Returns a sentinel `ErrNoMatch` when the response is an empty result set, so
callers can distinguish "this address is not geocodable" from a transient
failure. Non-2xx and transport errors return a wrapped error.

**`StaticMap(ctx, lat, lon float64) ([]byte, error)`**

```
GET https://maps.locationiq.com/v3/staticmap
    ?key=<key>
    &center=<lat>,<lon>
    &zoom=16
    &size=600x400
    &format=png
    &maptype=streets
    &markers=icon:large-red-cutout|<lat>,<lon>
```

Returns the raw PNG bytes. `zoom`, `size`, `maptype`, and the marker icon are
package constants.

### `internal/propertymap` — generation service

One job: given a property, return its map URL, creating it if needed.

```go
// Ensure returns the property's map URL, generating it when absent.
//
//	url != ""              — the map is ready
//	url == "", pending      — generation is still running in the background
//	url == "", !pending     — no map, and none is coming (unmappable,
//	                          disabled, or denied by a guard)
Ensure(ctx context.Context, p *property.Property) (url string, pending bool)
```

The `pending` flag is what the caching rule below keys on, so the handler can
tell "still working on it" apart from "there will never be one".

Logic:

1. `p.MapImageURL != ""` → return it.
2. `p.MapGeneratedAt != nil && p.MapImageURL == ""` → permanently unmappable,
   return `("", false)` without any API call.
3. Coordinates nil → `locationiq.Geocode`, then persist the coordinates back to
   the row. This is a one-time cost per property and also fills in the detail
   endpoint's `latitude` / `longitude`.
4. `locationiq.StaticMap` → `bunny.Upload(ctx, "maps/<zpid>.png", …, "image/png")`
   → `repo.SetMapImage(ctx, zpid, url)`.

The service depends on narrow interfaces (a geocoder/mapper, the existing
`uploader`, and a small store interface) so it can be tested with fakes, matching
how `internal/scheduler` declares its dependencies.

**Single-flight.** All of `Ensure` runs inside a `singleflight.Group` keyed by
zpid (`golang.org/x/sync` is already a dependency). Concurrent views of the same
listing produce exactly one geocode and one map fetch.

### Detail handler wiring

The handler loads the property as it does today, then calls `Ensure` with a
deadline:

- Generation runs on a goroutine with `context.WithoutCancel(reqCtx)` plus its
  own ~20s timeout, so a viewer navigating away does not kill a half-finished
  map.
- The handler `select`s on the result channel against a 1.5s timer.
  - Result first → the response carries the populated `map_image_url`.
  - Timer first → the response carries `null`; generation continues in the
    background and the next viewer reads the finished URL from the DB.

When LocationIQ is not configured, `Ensure` is a no-op returning
`("", false)` and the handler skips the goroutine entirely.

## Failure semantics

Mirrors the existing `details_fetched_at` convention.

| Case | Persisted | Retried? |
| --- | --- | --- |
| Map generated | `map_image_url`, `map_generated_at` | No |
| Geocode returns `ErrNoMatch` | `map_generated_at` only, URL stays NULL | No — permanently unmappable |
| Transient failure (LocationIQ 5xx, timeout, Bunny error) | nothing | Yes, on the next request |

Clearing `map_generated_at` for a row is enough to make it eligible again, so
the permanent-failure decision is cheap to reverse.

**Abuse and outage guards.** The detail endpoint is public and unauthenticated,
so on-demand generation is a lever anyone can pull on the LocationIQ quota, and
an outage would otherwise turn every request into a retry. The service holds two
in-process, memory-only guards (cleared on restart):

- A per-zpid cooldown after a transient failure.
- A global token-bucket ceiling on generations per hour.

When either guard denies a generation, `Ensure` returns `("", false)`
immediately — the response is simply a null map.

## Caching

The detail endpoint currently sends `Cache-Control: public, max-age=300`. A
`map_image_url: null` response returned while generation is still running would
be cached for five minutes.

Detail responses whose map is null **and** whose generation is still pending send
`max-age=30` instead. Every other response keeps `max-age=300`.

## Schema

Two additive columns, in the existing idempotent style of `internal/db/schema.sql`:

```sql
ALTER TABLE properties ADD COLUMN IF NOT EXISTS map_image_url    TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS map_generated_at TIMESTAMPTZ;
```

`property.Property` gains `MapImageURL string` and `MapGeneratedAt *time.Time`.
`Repository.GetByZPID` selects both. Two new repository methods:

- `SetMapImage(ctx, zpid, url string)` — sets `map_image_url` and stamps
  `map_generated_at`. Called with an empty URL to record a permanently
  unmappable row.
- `SetCoordinates(ctx, zpid string, lat, lon float64)` — writes just
  `latitude` / `longitude` for the geocoding write-back. Deliberately **not**
  `SetDetails`, which writes the whole enrichment block and would clobber
  those fields with nils.

## API

`map_image_url` (`string|null`) is added to the detail DTO in `internal/api/dto.go`
and to the handler's Swagger annotations. `make swagger` is run and the
regenerated `docs/` committed, per the project's existing workflow.

Browse cards are unchanged.

## Configuration

One new environment variable:

```
# LocationIQ (static property maps on the detail endpoint). Unset = maps disabled.
LOCATIONIQ_API_KEY=
```

Added to `.env.example` as an empty placeholder — the real key goes in the
operator's `.env` and the production secrets file, never in git.

The key is **optional**. When it is unset, `config.Load` leaves maps disabled,
`map_image_url` is always null, and existing deployments and local runs are
unaffected. Map generation also requires the Bunny CDN configuration that
`IMAGES_ENABLED` already validates.

## Testing

- **`internal/locationiq`** — against a fixture-pinned `httptest` server:
  geocoding URL construction and response parsing, `ErrNoMatch` on an empty
  result set, static-map URL construction (particularly the `markers` syntax),
  and non-2xx handling.
- **`internal/propertymap`** — with fake client, uploader, and store, covering
  the four paths: already has a URL, permanently unmappable, geocode-then-
  generate, coordinates already present. Plus a concurrent test asserting
  single-flight collapses duplicate work into one geocode and one map fetch,
  and tests for the cooldown and token-bucket guards.
- **`internal/api`** — a handler test asserting the deadline path returns
  `map_image_url: null` without blocking, and one asserting the fast path
  returns the populated URL. Existing detail tests must keep passing with maps
  disabled.

## Rollout

Additive and backward-compatible. Deploying without `LOCATIONIQ_API_KEY` changes
nothing but the presence of a null `map_image_url` field. Setting the key turns
generation on; maps then accumulate naturally as listings are viewed.
