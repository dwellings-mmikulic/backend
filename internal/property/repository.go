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

// SetVideoFailed marks a listing's video render as failed — unless a ready
// video is already stored. A failed re-render (a revisit, or another instance
// working from an older claim) must not take a working video off the feed, so
// matching no row is not an error.
func (r *Repository) SetVideoFailed(ctx context.Context, zpid string) error {
	const q = `
UPDATE properties SET video_status = 'failed', updated_at = now()
 WHERE zpid = $1 AND video_status IS DISTINCT FROM 'ready'`
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
       details_fetched_at, COALESCE(map_image_url,''), COALESCE(map_image_dark_url,''),
       map_generated_at, created_at, updated_at
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
		&p.DetailsFetchedAt, &p.MapImageURL, &p.MapImageDarkURL, &p.MapGeneratedAt,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get property zpid=%s: %w", zpid, err)
	}
	return &p, nil
}

// NeedsVideo reports whether the stored listing lacks a ready video. It is
// what lets the collection cycle revisit a listing whose render failed:
// SkipExisting would otherwise return before the render step and strand that
// listing without a video forever.
func (r *Repository) NeedsVideo(ctx context.Context, zpid string) (bool, error) {
	const q = `
SELECT video_status IS DISTINCT FROM 'ready' OR video_url IS NULL OR video_url = ''
  FROM properties WHERE zpid = $1`
	var needs bool
	err := r.pool.QueryRow(ctx, q, zpid).Scan(&needs)
	if errors.Is(err, pgx.ErrNoRows) {
		// Not stored yet, so the normal new-listing path renders it.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("needs video zpid=%s: %w", zpid, err)
	}
	return needs, nil
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

// VideoStates answers NeedsVideo for a whole search result in one query, so
// the discovery loop's enqueue filter costs one round trip per ZIP instead of
// one per listing. The map holds only the zpids that are stored, each mapped
// to whether it still lacks a ready video; a zpid missing from the map is a
// new listing. The predicate is NeedsVideo's, which the media worker re-checks
// per listing, so the two must never drift apart.
func (r *Repository) VideoStates(ctx context.Context, zpids []string) (map[string]bool, error) {
	states := make(map[string]bool, len(zpids))
	if len(zpids) == 0 {
		return states, nil
	}
	const q = `
SELECT zpid, video_status IS DISTINCT FROM 'ready' OR video_url IS NULL OR video_url = ''
  FROM properties WHERE zpid = ANY($1)`
	rows, err := r.pool.Query(ctx, q, zpids)
	if err != nil {
		return nil, fmt.Errorf("video states: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			zpid  string
			needs bool
		)
		if err := rows.Scan(&zpid, &needs); err != nil {
			return nil, fmt.Errorf("scan video state: %w", err)
		}
		states[zpid] = needs
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate video state rows: %w", err)
	}
	return states, nil
}

// MaxDetailsAttempts is how many row-specific failures (FailDetails) a listing
// gets before ClaimMissingDetails stops offering it. It must equal the literal
// in the predicate of idx_properties_details_todo (schema.sql); an integration
// test compares the two.
const MaxDetailsAttempts = 5

// claimMissingDetailsSQL leases the oldest un-enriched rows that nobody holds.
//
// The attempts limit is written into the statement as a literal, not bound as
// a parameter: PostgreSQL only uses the partial index
// idx_properties_details_todo when it can prove the WHERE implies the index
// predicate, and it cannot prove that about $n. Without the index every claim
// would sort all of properties.
//
// SKIP LOCKED makes concurrent claimers pass over each other's rows instead
// of queueing behind them, and under READ COMMITTED the lock step re-checks
// the lease qual on the newest row version, so a row leased a moment ago by
// another instance is dropped rather than claimed twice. The lease runs on
// the database clock: the instances' own clocks never meet. RETURNING has no
// defined order, hence the final ORDER BY.
//
// Neither details_attempts nor updated_at is touched: a claim is bookkeeping,
// not a failure and not a change to the listing the API reports.
var claimMissingDetailsSQL = fmt.Sprintf(`
WITH picked AS MATERIALIZED (
    SELECT id FROM properties
     WHERE details_fetched_at IS NULL
       AND details_attempts < %d
       AND (details_claimed_until IS NULL OR details_claimed_until <= now())
     ORDER BY created_at ASC
     LIMIT $1
       FOR UPDATE SKIP LOCKED
), leased AS (
    UPDATE properties p
       SET details_claimed_until = now() + make_interval(secs => $2)
      FROM picked
     WHERE p.id = picked.id
 RETURNING p.zpid, p.created_at, p.id
)
SELECT zpid FROM leased ORDER BY created_at ASC, id ASC`, MaxDetailsAttempts)

// ClaimMissingDetails leases up to limit zpids that have never been enriched,
// oldest first, for the given duration, and returns them. It replaces
// ListZPIDsMissingDetails for a fleet: that one hands the same oldest rows to
// every instance, and each would pay for the same details call.
//
// The claim has no owner by design. What keeps a stale holder harmless is
// SetDetails' details_fetched_at IS NULL guard, and the caller working under
// a deadline shorter than the lease. A crashed holder's rows free themselves
// when the lease runs out.
//
// No attempt is counted here — only FailDetails does that — so a provider
// outage or a deploy in the middle of a batch cannot use up a healthy row's
// attempts. Every claimed row must end in SetDetails, FailDetails or
// ReleaseDetails.
func (r *Repository) ClaimMissingDetails(ctx context.Context, limit int, lease time.Duration) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	if lease <= 0 {
		// Such a lease has expired by the time it is written: it would claim
		// nothing and every instance would fetch the same rows.
		return nil, fmt.Errorf("claim missing details: lease must be positive, got %s", lease)
	}
	rows, err := r.pool.Query(ctx, claimMissingDetailsSQL, limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim missing details: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var zpid string
		if err := rows.Scan(&zpid); err != nil {
			return nil, fmt.Errorf("scan claimed zpid: %w", err)
		}
		out = append(out, zpid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed zpid rows: %w", err)
	}
	return out, nil
}

// ReleaseDetails gives claimed rows back without counting an attempt, so they
// are claimable again at once. It is for rows whose fetch never reached a
// verdict about the row itself: the budget ran out, the provider is down, or
// the process is shutting down.
func (r *Repository) ReleaseDetails(ctx context.Context, zpids []string) error {
	if len(zpids) == 0 {
		return nil
	}
	const q = `
UPDATE properties SET details_claimed_until = NULL
 WHERE zpid = ANY($1) AND details_fetched_at IS NULL`
	if _, err := r.pool.Exec(ctx, q, zpids); err != nil {
		return fmt.Errorf("release details of %d zpids: %w", len(zpids), err)
	}
	return nil
}

// FailDetails counts one row-specific failure (undecodable response, a 4xx
// other than 429, a store error). The lease is deliberately left in place: it
// is the backoff before the next try. At MaxDetailsAttempts the row drops out
// of ClaimMissingDetails for good, so one poisonous listing cannot be paid for
// on every pass.
func (r *Repository) FailDetails(ctx context.Context, zpid string) error {
	const q = `UPDATE properties SET details_attempts = details_attempts + 1 WHERE zpid = $1`
	if _, err := r.pool.Exec(ctx, q, zpid); err != nil {
		return fmt.Errorf("fail details zpid=%s: %w", zpid, err)
	}
	return nil
}

// SetDetails stores the enrichment fields and raw API response, and stamps
// details_fetched_at so the row is never enriched again. raw may be nil
// (e.g. a definitive not-found still marks the row as fetched).
//
// It only ever writes a row that has not been enriched yet. The details claim
// has no owner, so after an expired lease two instances can hold the same
// zpid; without the guard the later one's empty or not-found result would wipe
// the record the first one stored. Matching no row is therefore not an error.
// Storing also ends the claim, so details_claimed_until is cleared.
//
// latitude/longitude use COALESCE so a details response with no coordinates
// (a NULL here) does not null out coordinates a map geocode already wrote
// back via SetCoordinates — enrichment can run after map generation, and
// without this it would silently regress the detail endpoint's
// latitude/longitude to null even though the map itself is unaffected.
func (r *Repository) SetDetails(ctx context.Context, zpid string, d *Details, raw []byte) error {
	const q = `
UPDATE properties SET
    property_type = $2, description = $3, year_built = $4, heating = $5,
    cooling = $6, garage = $7, hoa_fee_monthly = $8, mls_number = $9,
    listing_status = $10, agent_name = $11, agent_phone = $12,
    agent_brokerage = $13, latitude = COALESCE($14, latitude),
    longitude = COALESCE($15, longitude),
    details_raw = $16, details_fetched_at = now(),
    details_claimed_until = NULL, updated_at = now()
 WHERE zpid = $1 AND details_fetched_at IS NULL`
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

// SetMapImage records the property's static map URL and stamps
// map_generated_at. An empty url records a permanently unmappable row (the
// address could not be geocoded) so it is never retried.
func (r *Repository) SetMapImage(ctx context.Context, zpid string, m MapURLs) error {
	const q = `
UPDATE properties SET
    map_image_url = NULLIF($2, ''), map_image_dark_url = NULLIF($3, ''),
    map_generated_at = now(), updated_at = now()
 WHERE zpid = $1`
	if _, err := r.pool.Exec(ctx, q, zpid, m.Light, m.Dark); err != nil {
		return fmt.Errorf("set map image zpid=%s: %w", zpid, err)
	}
	return nil
}

// SetCoordinates writes geocoded coordinates back to the row. It deliberately
// touches only latitude/longitude — SetDetails writes the whole enrichment
// block and would clobber the other fields with nils.
func (r *Repository) SetCoordinates(ctx context.Context, zpid string, lat, lon float64) error {
	const q = `
UPDATE properties SET
    latitude = $2, longitude = $3, updated_at = now()
 WHERE zpid = $1`
	if _, err := r.pool.Exec(ctx, q, zpid, lat, lon); err != nil {
		return fmt.Errorf("set coordinates zpid=%s: %w", zpid, err)
	}
	return nil
}
