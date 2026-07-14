package property

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository persists properties to PostgreSQL.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository creates a property repository backed by the given pool.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Upsert inserts a property or updates it if a row with the same zpid exists.
// Video columns are intentionally not touched here — the render stage owns them,
// so re-ingesting a listing preserves its existing video state.
func (r *Repository) Upsert(ctx context.Context, p *Property) error {
	const q = `
INSERT INTO properties (
    zpid, sale_price, address, city, state, zip,
    home_size_sqft, lot_size_sqft, bedrooms, bathrooms, detail_url, image_urls
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (zpid) DO UPDATE SET
    sale_price     = EXCLUDED.sale_price,
    address        = EXCLUDED.address,
    city           = EXCLUDED.city,
    state          = EXCLUDED.state,
    zip            = EXCLUDED.zip,
    home_size_sqft = EXCLUDED.home_size_sqft,
    lot_size_sqft  = EXCLUDED.lot_size_sqft,
    bedrooms       = EXCLUDED.bedrooms,
    bathrooms      = EXCLUDED.bathrooms,
    detail_url     = EXCLUDED.detail_url,
    image_urls     = EXCLUDED.image_urls,
    updated_at     = now()
RETURNING id, video_status, COALESCE(video_content_hash, '')`

	err := r.pool.QueryRow(ctx, q,
		p.ZPID, p.SalePrice, p.Address, p.City, p.State, p.Zip,
		p.HomeSizeSqft, p.LotSizeSqft, p.Bedrooms, p.Bathrooms, p.DetailURL, p.ImageURLs,
	).Scan(&p.ID, &p.VideoStatus, &p.VideoContentHash)
	if err != nil {
		return fmt.Errorf("upsert property zpid=%s: %w", p.ZPID, err)
	}
	return nil
}

// SetVideoReady records a successfully rendered and uploaded video.
func (r *Repository) SetVideoReady(ctx context.Context, zpid, videoURL, contentHash string, durationSecs int) error {
	const q = `
UPDATE properties
   SET video_url = $2, video_status = 'ready', video_content_hash = $3,
       video_duration_secs = $4, video_rendered_at = now(), updated_at = now()
 WHERE zpid = $1`
	if _, err := r.pool.Exec(ctx, q, zpid, videoURL, contentHash, durationSecs); err != nil {
		return fmt.Errorf("set video ready zpid=%s: %w", zpid, err)
	}
	return nil
}

// SetVideoFailed marks a listing's video render as failed.
func (r *Repository) SetVideoFailed(ctx context.Context, zpid string) error {
	const q = `UPDATE properties SET video_status = 'failed', updated_at = now() WHERE zpid = $1`
	if _, err := r.pool.Exec(ctx, q, zpid); err != nil {
		return fmt.Errorf("set video failed zpid=%s: %w", zpid, err)
	}
	return nil
}

// ListReadyForFeed returns listings whose video is ready, newest first, for the
// Roku feed.
func (r *Repository) ListReadyForFeed(ctx context.Context) ([]Property, error) {
	const q = `
SELECT zpid, sale_price, address, city, state, zip,
       home_size_sqft, lot_size_sqft, bedrooms, bathrooms, detail_url,
       image_urls, COALESCE(video_url,''), COALESCE(video_duration_secs,0),
       video_rendered_at
  FROM properties
 WHERE video_status = 'ready' AND video_url IS NOT NULL
 ORDER BY video_rendered_at DESC NULLS LAST`

	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list ready for feed: %w", err)
	}
	defer rows.Close()

	var out []Property
	for rows.Next() {
		var p Property
		var renderedAt *time.Time
		if err := rows.Scan(
			&p.ZPID, &p.SalePrice, &p.Address, &p.City, &p.State, &p.Zip,
			&p.HomeSizeSqft, &p.LotSizeSqft, &p.Bedrooms, &p.Bathrooms, &p.DetailURL,
			&p.ImageURLs, &p.VideoURL, &p.VideoDurationSecs, &renderedAt,
		); err != nil {
			return nil, fmt.Errorf("scan feed row: %w", err)
		}
		p.VideoStatus = VideoReady
		p.VideoRenderedAt = renderedAt
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feed rows: %w", err)
	}
	return out, nil
}

// Exists reports whether a property with the given zpid is already stored.
func (r *Repository) Exists(ctx context.Context, zpid string) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM properties WHERE zpid = $1)`
	var exists bool
	if err := r.pool.QueryRow(ctx, q, zpid).Scan(&exists); err != nil {
		return false, fmt.Errorf("check property exists zpid=%s: %w", zpid, err)
	}
	return exists, nil
}

// ErrNotFound is returned when a requested property does not exist.
var ErrNotFound = errors.New("property not found")

// List returns one page of properties matching f, the total number of rows
// matching the filter (ignoring pagination), and whether another page exists.
func (r *Repository) List(ctx context.Context, f Filter) ([]Property, int, bool, error) {
	countQ, countArgs := buildCountQuery(f)
	var total int
	if err := r.pool.QueryRow(ctx, countQ, countArgs...).Scan(&total); err != nil {
		return nil, 0, false, fmt.Errorf("count properties: %w", err)
	}

	q, args := buildListQuery(f)
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, false, fmt.Errorf("list properties: %w", err)
	}
	defer rows.Close()

	var out []Property
	for rows.Next() {
		var p Property
		if err := rows.Scan(
			&p.ID, &p.ZPID, &p.SalePrice, &p.Address, &p.City, &p.State, &p.Zip,
			&p.Bedrooms, &p.Bathrooms, &p.HomeSizeSqft, &p.PropertyType,
			&p.ImageURLs, &p.CreatedAt,
		); err != nil {
			return nil, 0, false, fmt.Errorf("scan property row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("iterate property rows: %w", err)
	}

	hasMore := false
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
		hasMore = true
	}
	return out, total, hasMore, nil
}

// GetByZPID returns the full property record, or ErrNotFound.
func (r *Repository) GetByZPID(ctx context.Context, zpid string) (*Property, error) {
	const q = `
SELECT id, zpid, COALESCE(sale_price,0), address, COALESCE(city,''),
       COALESCE(state,''), COALESCE(zip,''), COALESCE(bedrooms,0),
       COALESCE(bathrooms,0), COALESCE(home_size_sqft,0),
       COALESCE(lot_size_sqft,0), COALESCE(detail_url,''), image_urls,
       COALESCE(video_url,''),
       property_type, description, year_built, heating, cooling, garage,
       hoa_fee_monthly, mls_number, listing_status,
       agent_name, agent_phone, agent_brokerage, latitude, longitude,
       details_fetched_at, created_at, updated_at
  FROM properties WHERE zpid = $1`

	var p Property
	err := r.pool.QueryRow(ctx, q, zpid).Scan(
		&p.ID, &p.ZPID, &p.SalePrice, &p.Address, &p.City, &p.State, &p.Zip,
		&p.Bedrooms, &p.Bathrooms, &p.HomeSizeSqft,
		&p.LotSizeSqft, &p.DetailURL, &p.ImageURLs,
		&p.VideoURL,
		&p.PropertyType, &p.Description, &p.YearBuilt, &p.Heating, &p.Cooling, &p.Garage,
		&p.HOAFeeMonthly, &p.MLSNumber, &p.ListingStatus,
		&p.AgentName, &p.AgentPhone, &p.AgentBrokerage, &p.Latitude, &p.Longitude,
		&p.DetailsFetchedAt, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get property zpid=%s: %w", zpid, err)
	}
	return &p, nil
}

// ListZPIDsMissingDetails returns up to limit zpids that have never been
// enriched, oldest first (so backfill drains deterministically).
func (r *Repository) ListZPIDsMissingDetails(ctx context.Context, limit int) ([]string, error) {
	const q = `
SELECT zpid FROM properties
 WHERE details_fetched_at IS NULL
 ORDER BY created_at ASC
 LIMIT $1`
	rows, err := r.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list zpids missing details: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var zpid string
		if err := rows.Scan(&zpid); err != nil {
			return nil, fmt.Errorf("scan zpid: %w", err)
		}
		out = append(out, zpid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate zpid rows: %w", err)
	}
	return out, nil
}

// SetDetails stores the enrichment fields and raw API response, and stamps
// details_fetched_at so the row is never enriched again. raw may be nil
// (e.g. a definitive not-found still marks the row as fetched).
func (r *Repository) SetDetails(ctx context.Context, zpid string, d *Details, raw []byte) error {
	const q = `
UPDATE properties SET
    property_type = $2, description = $3, year_built = $4, heating = $5,
    cooling = $6, garage = $7, hoa_fee_monthly = $8, mls_number = $9,
    listing_status = $10, agent_name = $11, agent_phone = $12,
    agent_brokerage = $13, latitude = $14, longitude = $15,
    details_raw = $16, details_fetched_at = now(), updated_at = now()
 WHERE zpid = $1`
	_, err := r.pool.Exec(ctx, q, zpid,
		d.PropertyType, d.Description, d.YearBuilt, d.Heating,
		d.Cooling, d.Garage, d.HOAFeeMonthly, d.MLSNumber,
		d.ListingStatus, d.AgentName, d.AgentPhone,
		d.AgentBrokerage, d.Latitude, d.Longitude, raw,
	)
	if err != nil {
		return fmt.Errorf("set details zpid=%s: %w", zpid, err)
	}
	return nil
}
