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
