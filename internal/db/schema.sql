CREATE TABLE IF NOT EXISTS properties (
    id              BIGSERIAL PRIMARY KEY,
    zpid            TEXT UNIQUE NOT NULL,
    sale_price      BIGINT,
    address         TEXT NOT NULL,
    city            TEXT,
    state           TEXT,
    zip             TEXT,
    home_size_sqft  INTEGER,
    lot_size_sqft   INTEGER,
    bedrooms        INTEGER,
    bathrooms       NUMERIC(4,1),
    image_urls      TEXT[] NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_properties_city_state ON properties (city, state);
CREATE INDEX IF NOT EXISTS idx_properties_sale_price ON properties (sale_price);

-- Video / Roku feed columns (idempotent so this file can run on every startup).
ALTER TABLE properties ADD COLUMN IF NOT EXISTS detail_url          TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS video_url           TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS video_status        TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE properties ADD COLUMN IF NOT EXISTS video_content_hash  TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS video_rendered_at   TIMESTAMPTZ;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS video_duration_secs INTEGER;

CREATE INDEX IF NOT EXISTS idx_properties_video_status ON properties (video_status);

-- Enrichment columns: fetched once per property from the Zillow
-- property-details API, then served from the DB forever (see
-- docs/superpowers/specs/2026-07-14-public-listings-api-design.md).
ALTER TABLE properties ADD COLUMN IF NOT EXISTS property_type      TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS description        TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS year_built         INTEGER;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS heating            TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS cooling            TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS garage             TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS hoa_fee_monthly    INTEGER;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS mls_number         TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS listing_status     TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS agent_name         TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS agent_phone        TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS agent_brokerage    TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS latitude           DOUBLE PRECISION;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS longitude          DOUBLE PRECISION;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS details_raw        JSONB;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS details_fetched_at TIMESTAMPTZ;

-- Static property map (LocationIQ), generated on demand by the detail endpoint
-- (see docs/superpowers/specs/2026-07-29-property-maps-locationiq-design.md).
-- map_generated_at set with a NULL map_image_url means the address could not
-- be geocoded; clearing it makes the row eligible again.
ALTER TABLE properties ADD COLUMN IF NOT EXISTS map_image_url      TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS map_generated_at   TIMESTAMPTZ;
-- Dark-style counterpart of map_image_url; filled lazily for rows that only
-- had a light map when it was added.
ALTER TABLE properties ADD COLUMN IF NOT EXISTS map_image_dark_url TEXT;

-- Public listing API filter/sort indexes.
CREATE INDEX IF NOT EXISTS idx_properties_zip           ON properties (zip);
CREATE INDEX IF NOT EXISTS idx_properties_property_type ON properties (property_type);
CREATE INDEX IF NOT EXISTS idx_properties_created_at    ON properties (created_at DESC, id DESC);

-- ZIP rotation table for nationwide collection (see
-- docs/superpowers/specs/2026-08-10-nationwide-zip-rotation-design.md).
-- Seeded from an embedded CSV at startup when empty; last_searched_at is the
-- rotation cursor (NULL = never searched).
CREATE TABLE IF NOT EXISTS zip_codes (
    zip                TEXT PRIMARY KEY,
    city               TEXT NOT NULL DEFAULT '',
    state              TEXT NOT NULL DEFAULT '',
    county             TEXT NOT NULL DEFAULT '',
    population         INTEGER NOT NULL DEFAULT 0,
    last_searched_at   TIMESTAMPTZ,
    last_listing_count INTEGER
);

CREATE INDEX IF NOT EXISTS idx_zip_codes_rotation
    ON zip_codes (last_searched_at ASC NULLS FIRST, population DESC);

-- Linear channels (see docs/superpowers/specs/2026-08-25-linear-channels-design.md).
-- video_hls: one row per (listing, render); segments live at base_url on the
-- CDN. Rows are never deleted so lineups that reference an old render keep
-- resolving. A row is "current" when properties.video_content_hash matches.
CREATE TABLE IF NOT EXISTS video_hls (
    id           BIGSERIAL PRIMARY KEY,
    zpid         TEXT NOT NULL REFERENCES properties(zpid),
    content_hash TEXT NOT NULL,
    base_url     TEXT NOT NULL,
    segment_ms   INTEGER[] NOT NULL,
    total_ms     INTEGER NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (zpid, content_hash)
);

-- channel_lineups: the deterministic air schedule of a channel, as a chain of
-- versions. Version N starts where N-1 ended and continues its counters.
CREATE TABLE IF NOT EXISTS channel_lineups (
    channel_key TEXT NOT NULL,
    version     INTEGER NOT NULL,
    scope       TEXT NOT NULL,
    starts_at   TIMESTAMPTZ NOT NULL,
    ends_at     TIMESTAMPTZ NOT NULL,
    start_seq   BIGINT NOT NULL,
    start_item  BIGINT NOT NULL,
    item_ids    BIGINT[] NOT NULL,
    item_ms     INTEGER[] NOT NULL,
    item_segs   INTEGER[] NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_key, version)
);

-- The EPG asks for the versions overlapping a bounded window
-- (ends_at >= from AND starts_at < to), so a long-running channel's history
-- never has to be scanned.
CREATE INDEX IF NOT EXISTS idx_channel_lineups_key_ends
    ON channel_lineups (channel_key, ends_at);

-- Viewer tracking (see docs/superpowers/specs/2026-08-26-viewer-tracking-design.md).
-- viewer_heartbeats: one row per (pseudonymous viewer, channel, minute) in
-- which the viewer polled the live playlist. viewer_hash is a salted hash;
-- the raw address is kept only in viewer_clients. Rows are purged after the
-- retention period.
CREATE TABLE IF NOT EXISTS viewer_heartbeats (
    viewer_hash BYTEA NOT NULL,
    channel_key TEXT NOT NULL,
    minute      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (viewer_hash, channel_key, minute)
);
CREATE INDEX IF NOT EXISTS idx_viewer_heartbeats_channel_minute
    ON viewer_heartbeats (channel_key, minute);
CREATE INDEX IF NOT EXISTS idx_viewer_heartbeats_minute
    ON viewer_heartbeats (minute);

-- viewer_last_channel: what each viewer tuned to most recently, for the
-- /channels/resolve default.
CREATE TABLE IF NOT EXISTS viewer_last_channel (
    viewer_hash BYTEA PRIMARY KEY,
    channel_key TEXT NOT NULL,
    seen_at     TIMESTAMPTZ NOT NULL
);

-- viewer_clients: the raw public client address and user agent behind the
-- heartbeats, one row per (ip, user agent, channel), listed by the
-- key-protected /admin/viewers. Purged on the heartbeats' retention.
CREATE TABLE IF NOT EXISTS viewer_clients (
    ip          TEXT NOT NULL,
    user_agent  TEXT NOT NULL,
    channel_key TEXT NOT NULL,
    first_seen  TIMESTAMPTZ NOT NULL,
    last_seen   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (ip, user_agent, channel_key)
);
CREATE INDEX IF NOT EXISTS idx_viewer_clients_last_seen
    ON viewer_clients (last_seen);

-- Multi-instance workers (see
-- docs/superpowers/specs/2026-09-19-multi-instance-workers-design.md).
-- schema_meta: the hash of this file as last applied, so Migrate can skip the
-- whole script (and its table locks) on a routine restart.
CREATE TABLE IF NOT EXISTS schema_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- ZIP claims: a ZIP is claimable when claimed_until is NULL or in the past.
-- A kept lease doubles as the retry backoff of a failed or deferred ZIP.
-- resume_page is where a search cut short (budget, error) continues, so pages
-- already paid for are not bought again.
ALTER TABLE zip_codes ADD COLUMN IF NOT EXISTS claimed_by    TEXT;
ALTER TABLE zip_codes ADD COLUMN IF NOT EXISTS claimed_until TIMESTAMPTZ;
ALTER TABLE zip_codes ADD COLUMN IF NOT EXISTS failures      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE zip_codes ADD COLUMN IF NOT EXISTS resume_page   INTEGER NOT NULL DEFAULT 0;

-- listing_queue: discovered listings waiting for the media pipeline. A row is
-- deleted when its listing is done; the properties row is the record. The 3
-- below is workqueue.MaxAttempts: a row that has used them all is dead and
-- stays for inspection until re-discovery or backfill-videos revives it.
CREATE TABLE IF NOT EXISTS listing_queue (
    zpid          TEXT PRIMARY KEY,
    payload       JSONB NOT NULL,
    source_zip    TEXT NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0,
    available_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_by    TEXT,
    claimed_until TIMESTAMPTZ,
    last_error    TEXT,
    enqueued_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_listing_queue_claimable
    ON listing_queue (available_at) WHERE attempts < 3;

-- api_budget: provider requests spent per budget window, fleet-wide. One
-- request is reserved immediately before every paid HTTP attempt.
CREATE TABLE IF NOT EXISTS api_budget (
    window_start TIMESTAMPTZ NOT NULL,
    kind         TEXT NOT NULL,
    spent        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (window_start, kind)
);

-- Details claims. The 5 below is property.MaxDetailsAttempts; rows that have
-- used them all drop out of the index and are never claimed again.
ALTER TABLE properties ADD COLUMN IF NOT EXISTS details_claimed_until TIMESTAMPTZ;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS details_attempts      INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_properties_details_todo
    ON properties (created_at) WHERE details_fetched_at IS NULL AND details_attempts < 5;
