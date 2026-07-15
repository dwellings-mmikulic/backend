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

-- Public listing API filter/sort indexes.
CREATE INDEX IF NOT EXISTS idx_properties_zip           ON properties (zip);
CREATE INDEX IF NOT EXISTS idx_properties_property_type ON properties (property_type);
CREATE INDEX IF NOT EXISTS idx_properties_created_at    ON properties (created_at DESC, id DESC);
