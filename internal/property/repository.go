package property

import (
	"context"
	"fmt"
	"time"

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
