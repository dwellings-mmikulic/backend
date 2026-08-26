package linear

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/hls"
)

// Repository is the PostgreSQL Store.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository creates a Repository.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// scopeWhere renders the scope as extra AND-conditions on the properties
// alias p, with 1-based positional args. Empty for the national scope.
func scopeWhere(s Scope) (string, []any) {
	switch {
	case s.Zip != "":
		return " AND p.zip = $1", []any{s.Zip}
	case s.City != "":
		return " AND lower(p.city) = $1 AND lower(p.state) = $2", []any{s.City, s.State}
	case s.State != "":
		return " AND lower(p.state) = $1", []any{s.State}
	}
	return "", nil
}

// SetVideoHLS records (or refreshes) the segment layout of a render.
func (r *Repository) SetVideoHLS(ctx context.Context, zpid, contentHash, baseURL string, clip hls.Clip) error {
	const q = `
INSERT INTO video_hls (zpid, content_hash, base_url, segment_ms, total_ms)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (zpid, content_hash) DO UPDATE SET
    base_url = EXCLUDED.base_url, segment_ms = EXCLUDED.segment_ms, total_ms = EXCLUDED.total_ms`
	if _, err := r.pool.Exec(ctx, q, zpid, contentHash, baseURL, int32s(clip.SegmentMS), clip.TotalMS); err != nil {
		return fmt.Errorf("set video hls zpid=%s: %w", zpid, err)
	}
	return nil
}

// ListClips implements Store: current clips (the render the property row
// points at) in scope, ordered by id.
func (r *Repository) ListClips(ctx context.Context, s Scope) ([]ClipRef, error) {
	where, args := scopeWhere(s)
	q := `
SELECT h.id, h.total_ms, cardinality(h.segment_ms)
  FROM video_hls h
  JOIN properties p ON p.zpid = h.zpid
 WHERE p.video_status = 'ready' AND p.video_content_hash = h.content_hash` + where + `
 ORDER BY h.id`
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list clips: %w", err)
	}
	defer rows.Close()
	var out []ClipRef
	for rows.Next() {
		var c ClipRef
		if err := rows.Scan(&c.ID, &c.TotalMS, &c.Segments); err != nil {
			return nil, fmt.Errorf("scan clip: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CityOfZip implements Store.
func (r *Repository) CityOfZip(ctx context.Context, zip string) (string, string, error) {
	const q = `
SELECT lower(city), lower(state)
  FROM properties
 WHERE zip = $1 AND city IS NOT NULL AND city <> '' AND state IS NOT NULL AND state <> ''
 GROUP BY 1, 2
 ORDER BY count(*) DESC
 LIMIT 1`
	var city, state string
	err := r.pool.QueryRow(ctx, q, zip).Scan(&city, &state)
	if err == pgx.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("city of zip %s: %w", zip, err)
	}
	return city, state, nil
}

const versionColumns = `channel_key, version, scope, starts_at, ends_at, start_seq, start_item, item_ids, item_ms, item_segs`

func scanVersions(rows pgx.Rows) ([]Version, error) {
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		var ms, segs []int32
		if err := rows.Scan(&v.Key, &v.Version, &v.Scope, &v.StartsAt, &v.EndsAt, &v.StartSeq, &v.StartItem, &v.ItemIDs, &ms, &segs); err != nil {
			return nil, fmt.Errorf("scan lineup: %w", err)
		}
		v.ItemMS, v.ItemSegs = ints(ms), ints(segs)
		out = append(out, v)
	}
	return out, rows.Err()
}

// LatestVersions implements Store.
func (r *Repository) LatestVersions(ctx context.Context, key string, n int) ([]Version, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+versionColumns+` FROM channel_lineups WHERE channel_key = $1 ORDER BY version DESC LIMIT $2`, key, n)
	if err != nil {
		return nil, fmt.Errorf("latest lineups %s: %w", key, err)
	}
	return scanVersions(rows)
}

// VersionsAt implements Store.
func (r *Repository) VersionsAt(ctx context.Context, key string, t time.Time) ([]Version, error) {
	const q = `SELECT ` + versionColumns + ` FROM channel_lineups
 WHERE channel_key = $1 AND starts_at <= $2
 ORDER BY version DESC LIMIT 2`
	rows, err := r.pool.Query(ctx, q, key, t)
	if err != nil {
		return nil, fmt.Errorf("lineups at %s for %s: %w", t, key, err)
	}
	return scanVersions(rows)
}

// ListVersions implements Store.
func (r *Repository) ListVersions(ctx context.Context, key string) ([]Version, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+versionColumns+` FROM channel_lineups WHERE channel_key = $1 ORDER BY version ASC`, key)
	if err != nil {
		return nil, fmt.Errorf("list lineups %s: %w", key, err)
	}
	return scanVersions(rows)
}

// InsertVersion implements Store. The primary key settles creation races.
func (r *Repository) InsertVersion(ctx context.Context, v *Version) (bool, error) {
	const q = `
INSERT INTO channel_lineups (` + versionColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (channel_key, version) DO NOTHING`
	tag, err := r.pool.Exec(ctx, q, v.Key, v.Version, v.Scope, v.StartsAt, v.EndsAt, v.StartSeq, v.StartItem,
		v.ItemIDs, int32s(v.ItemMS), int32s(v.ItemSegs))
	if err != nil {
		return false, fmt.Errorf("insert lineup %s v%d: %w", v.Key, v.Version, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ClipsByID implements Store.
func (r *Repository) ClipsByID(ctx context.Context, ids []int64) (map[int64]ClipSegments, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, base_url, segment_ms FROM video_hls WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("clips by id: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]ClipSegments, len(ids))
	for rows.Next() {
		var c ClipSegments
		var ms []int32
		if err := rows.Scan(&c.ID, &c.BaseURL, &ms); err != nil {
			return nil, fmt.Errorf("scan clip segments: %w", err)
		}
		c.SegmentMS = ints(ms)
		out[c.ID] = c
	}
	return out, rows.Err()
}

// ListingsByClipID implements Store.
func (r *Repository) ListingsByClipID(ctx context.Context, ids []int64) ([]Listing, error) {
	const q = `
SELECT h.id, p.zpid, COALESCE(p.sale_price, 0), COALESCE(p.city, ''), COALESCE(p.state, '')
  FROM video_hls h JOIN properties p ON p.zpid = h.zpid
 WHERE h.id = ANY($1)`
	rows, err := r.pool.Query(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("listings by clip: %w", err)
	}
	defer rows.Close()
	var out []Listing
	for rows.Next() {
		var l Listing
		if err := rows.Scan(&l.ClipID, &l.ZPID, &l.Price, &l.City, &l.State); err != nil {
			return nil, fmt.Errorf("scan listing: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func int32s(in []int) []int32 {
	out := make([]int32, len(in))
	for i, v := range in {
		out[i] = int32(v)
	}
	return out
}

func ints(in []int32) []int {
	out := make([]int, len(in))
	for i, v := range in {
		out[i] = int(v)
	}
	return out
}
