# Public Listings API + Details Enrichment — Design

**Date:** 2026-07-14
**Status:** Approved for planning

## Goal

Expose two public (unauthenticated) JSON endpoints backing the Dwellings.TV app screens:

1. **Browse** — a filterable, sorted, cursor-paginated list of properties (browse-listings screen).
2. **Detail** — full information for a single property (property-detail screen).

Both serve exclusively from our PostgreSQL database. The data comes from the existing
OpenWebNinja Zillow scraping pipeline, extended with a one-time-per-property details
enrichment call.

## Background / current state

- The scheduler searches OpenWebNinja (`/search`) every 12h and upserts listings into
  the `properties` table keyed by `zpid`.
- Stored today: price, address, city, state, zip, home/lot sqft, beds, baths,
  image URLs (Bunny CDN), Zillow detail URL, video render state.
- The detail screen needs fields we do not collect: description, property type,
  year built, heating, cooling, garage, HOA fee, MLS #, listing status, agent
  (name/phone/brokerage), and lat/long for the map pin.
- The existing HTTP server (`internal/server`) serves `GET /roku/feed.json` and
  `GET /healthz`. The Roku feed identifies items by `zpid`, so the public API keys
  the detail endpoint by `zpid` too.

## Decisions (made during brainstorming)

- **Full enrichment, cached:** call OpenWebNinja's property-details endpoint **once
  per property**, persist the result, and always serve from our DB afterwards.
- **Enrich at scrape time + backfill:** enrichment runs inside the scheduler cycle,
  never at request time. Existing rows are backfilled by the same mechanism.
- **Hybrid storage:** typed columns for fields the UI/filters need, plus the raw
  details response in a JSONB column so future fields never require re-spending
  API quota.
- **Cursor-based (keyset) pagination**, not offset/page numbers.
- **No "featured" concept yet** (the badge in the mock has no data source; add later).
- **Public endpoints:** no auth, permissive CORS, cacheable responses.

## Endpoints

### `GET /api/v1/properties` — browse

Query parameters (all optional):

| Param | Meaning |
|---|---|
| `zip` | exact ZIP match |
| `city` | case-insensitive city match |
| `state` | case-insensitive state match (e.g. `TX`) |
| `min_price`, `max_price` | whole dollars, inclusive |
| `property_type` | stored Zillow `homeType` value, e.g. `SINGLE_FAMILY`, `CONDO` |
| `min_beds` | "N+" filter |
| `min_baths` | "N+" filter (float, e.g. `2.5`) |
| `min_sqft`, `max_sqft` | home size, inclusive |
| `sort` | `newest` (default) \| `price_asc` \| `price_desc` |
| `limit` | default 24, max 100 |
| `cursor` | opaque token from the previous response; omit for first page |

Response `200`:

```json
{
  "total": 312,
  "results": [
    {
      "zpid": "123456",
      "price": 1850000,
      "address": "1234 Hilltop Drive",
      "city": "Austin",
      "state": "TX",
      "zip": "78746",
      "bedrooms": 4,
      "bathrooms": 3.5,
      "home_size_sqft": 3200,
      "property_type": "SINGLE_FAMILY",
      "image_url": "https://cdn.../0.jpg"
    }
  ],
  "next_cursor": "eyJwIjoxODUwMDAwLCJpZCI6NDJ9"
}
```

- `image_url` is the first stored image (card thumbnail); `null` if none.
- `next_cursor` is `null` when there are no further results.
- `total` is the full count matching the filters (cheap `COUNT(*)` at current scale),
  so the UI can show "312 Results".
- Enrichment-only fields (`property_type`) are `null` on rows not yet enriched;
  such rows are excluded by a `property_type` filter but otherwise appear normally.

### Pagination mechanics (keyset)

- Fetch `limit+1` rows; if `limit+1` came back, a next page exists.
- Cursor = base64url-encoded JSON of the last row's sort-key value + `id`
  tiebreaker + the sort name it belongs to.
- Predicates:
  - `sort=newest` → `WHERE (created_at, id) < ($1, $2) ORDER BY created_at DESC, id DESC`
  - `sort=price_asc` → `WHERE (sale_price, id) > ($1, $2) ORDER BY sale_price ASC, id ASC`
  - `sort=price_desc` → mirror of `price_asc`
- A cursor replayed with a different `sort`, or a garbled token →
  `400 {"error":"invalid cursor"}`.
- Filters are not encoded in the cursor; the client resends them with each page.

### `GET /api/v1/properties/{zpid}` — detail

Response `200`: all browse-card fields plus:

```json
{
  "zpid": "123456",
  "price": 1850000,
  "address": "1234 Hilltop Drive",
  "city": "Austin",
  "state": "TX",
  "zip": "78746",
  "bedrooms": 4,
  "bathrooms": 3.5,
  "home_size_sqft": 3200,
  "lot_size_sqft": 12197,
  "lot_size_acres": 0.28,
  "property_type": "SINGLE_FAMILY",
  "description": "Stunning modern home...",
  "year_built": 2021,
  "heating": "Central",
  "cooling": "Central Air",
  "garage": "2 Car Attached",
  "hoa_fee_monthly": 125,
  "mls_number": "1234567",
  "listing_status": "FOR_SALE",
  "agent": { "name": "Hill Country Dream Realty", "phone": "512-555-0123", "brokerage": "..." },
  "latitude": 30.2672,
  "longitude": -97.7431,
  "image_urls": ["https://cdn.../0.jpg", "..."],
  "video_url": null,
  "detail_url": "https://www.zillow.com/homedetails/...",
  "created_at": "2026-07-01T12:00:00Z",
  "updated_at": "2026-07-14T00:00:00Z"
}
```

- Fields not yet enriched are `null` (the TV app hides empty rows).
- `agent` is `null` when no agent fields are known.
- `video_url` is `null` until the listing video is rendered.
- Unknown zpid → `404 {"error":"not found"}`.

### Common behavior

- `Content-Type: application/json` everywhere, including errors
  (`{"error":"..."}`).
- `Cache-Control: public, max-age=300` on both endpoints.
- CORS: `Access-Control-Allow-Origin: *` (public, read-only API).
- Invalid query values (non-numeric `min_price`, `limit` > 100, unknown `sort`)
  → `400` with a message naming the bad parameter.

## Schema changes

Same idempotent pattern as the video columns
(`ALTER TABLE ... ADD COLUMN IF NOT EXISTS` in the embedded `schema.sql`):

```sql
-- Enrichment (typed columns used by UI/filters)
property_type       TEXT
description         TEXT
year_built          INTEGER
heating             TEXT
cooling             TEXT
garage              TEXT
hoa_fee_monthly     INTEGER
mls_number          TEXT
listing_status      TEXT
agent_name          TEXT
agent_phone         TEXT
agent_brokerage     TEXT
latitude            DOUBLE PRECISION
longitude           DOUBLE PRECISION

-- Enrichment bookkeeping
details_raw         JSONB        -- full property-details API response
details_fetched_at  TIMESTAMPTZ  -- NULL = not yet enriched

-- New indexes
idx_properties_zip            ON properties (zip)
idx_properties_property_type  ON properties (property_type)
idx_properties_created_at     ON properties (created_at DESC, id DESC)
```

## Enrichment flow

1. New client method `zillow.Client.PropertyDetails(ctx, zpid)` calling
   OpenWebNinja `GET /property-details?zpid=...` (same `X-API-Key` auth and
   `{status, data}` envelope as `/search`).
2. Scheduler cycle, after the existing search/upsert step:
   - `SELECT zpid FROM properties WHERE details_fetched_at IS NULL ORDER BY created_at ASC LIMIT 50`
     (per-cycle cap protects API quota; covers both new listings and backfill of
     pre-existing rows).
   - For each: fetch details, map to typed columns, store columns + `details_raw`,
     set `details_fetched_at = now()`.
   - On fetch/decode failure: log and leave the row NULL — it retries next cycle.
     A `404`-style "no such property" response still sets `details_fetched_at`
     (with whatever partial data exists) so dead zpids don't retry forever.
3. **Open item (implementation-time):** exact response field names for
   heating/cooling/garage/HOA/agent are unverified until we hit the live endpoint
   once. The mapping gets pinned by a fixture test in `internal/zillow/testdata/`,
   same as the search mapping. If a field doesn't exist in the response, its
   column simply stays NULL — no API-shape change.

## Code layout

- **`internal/api` (new):** HTTP handlers for both endpoints, query-param
  parsing/validation, cursor encode/decode, response DTOs. Depends on
  `internal/property` via a small consumer-side interface.
- **`internal/server`:** mounts the two `/api/v1/...` routes on the existing mux;
  applies the CORS + Cache-Control headers.
- **`internal/property`:** extend `Property` with the enrichment fields; add
  repository methods `List(ctx, Filter) (rows, total, err)` (parameterized dynamic
  WHERE; sort mapped through a fixed whitelist — never interpolated from input)
  and `GetByZPID(ctx, zpid)`; add `MarkDetails(...)` (or similar) for the
  enrichment writer.
- **`internal/zillow`:** `PropertyDetails` method + `toDetails` mapping.
- **`internal/scheduler`:** enrichment step after ingest.

## Testing

- **`internal/api`:** `httptest` handler tests against a fake repository —
  filter parsing, limit bounds, invalid-cursor 400, sort whitelist, 404 detail,
  null-field rendering, CORS/cache headers.
- **`internal/property`:** List/GetByZPID query tests following the repo's
  existing repository-test pattern; keyset pagination correctness (no skipped or
  duplicated rows across pages, tiebreaker on equal prices).
- **`internal/zillow`:** fixture test pinning the property-details mapping.
- **`internal/scheduler`:** enrichment step — happy path, failure leaves row
  NULL, cap respected.

## Out of scope

- Authentication / API keys / rate limiting.
- "Featured" listings.
- Favorites, search-by-map-bounds, and any write endpoints.
- Changing the Roku feed.
