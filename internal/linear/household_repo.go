package linear

import (
	"context"
	"fmt"

	"github.com/dwellingtw/backend/internal/viewer"
)

const spanColumns = `household, n, scope, requested, source, starts_at, seq_offset, item_offset, created_at`

// Spans implements Store.
func (r *Repository) Spans(ctx context.Context, h viewer.ID) ([]Span, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+spanColumns+` FROM household_spans WHERE household = $1 ORDER BY n`, h[:])
	if err != nil {
		return nil, fmt.Errorf("spans: %w", err)
	}
	defer rows.Close()
	var out []Span
	for rows.Next() {
		var sp Span
		var hh []byte
		if err := rows.Scan(&hh, &sp.N, &sp.Scope, &sp.Requested, &sp.Source, &sp.StartsAt, &sp.SeqOffset, &sp.ItemOffset, &sp.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan span: %w", err)
		}
		copy(sp.Household[:], hh)
		out = append(out, sp)
	}
	return out, rows.Err()
}

// InsertSpan implements Store. The primary key settles a race between two
// submits for one household: the loser sees ok=false and re-reads.
func (r *Repository) InsertSpan(ctx context.Context, sp *Span) (bool, error) {
	const q = `
INSERT INTO household_spans (household, n, scope, requested, source, starts_at, seq_offset, item_offset)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (household, n) DO NOTHING`
	tag, err := r.pool.Exec(ctx, q, sp.Household[:], sp.N, sp.Scope, sp.Requested, sp.Source, sp.StartsAt, sp.SeqOffset, sp.ItemOffset)
	if err != nil {
		return false, fmt.Errorf("insert span %d: %w", sp.N, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ReplaceSpan implements Store.
func (r *Repository) ReplaceSpan(ctx context.Context, sp *Span) error {
	const q = `
UPDATE household_spans
   SET scope = $3, requested = $4, source = $5, starts_at = $6, seq_offset = $7, item_offset = $8, created_at = now()
 WHERE household = $1 AND n = $2`
	tag, err := r.pool.Exec(ctx, q, sp.Household[:], sp.N, sp.Scope, sp.Requested, sp.Source, sp.StartsAt, sp.SeqOffset, sp.ItemOffset)
	if err != nil {
		return fmt.Errorf("replace span %d: %w", sp.N, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("replace span %d: no such span", sp.N)
	}
	return nil
}

// ListCities implements Store: the same current-clip predicate ListClips
// uses, grouped by city.
func (r *Repository) ListCities(ctx context.Context, min int) ([]City, error) {
	const q = `
SELECT lower(p.city), lower(p.state), count(*)
  FROM video_hls h
  JOIN properties p ON p.zpid = h.zpid
 WHERE p.video_status = 'ready' AND p.video_content_hash = h.content_hash
   AND p.city <> '' AND p.state <> ''
 GROUP BY 1, 2
HAVING count(*) >= $1
 ORDER BY 2, 1`
	rows, err := r.pool.Query(ctx, q, min)
	if err != nil {
		return nil, fmt.Errorf("list cities: %w", err)
	}
	defer rows.Close()
	var out []City
	for rows.Next() {
		var c City
		if err := rows.Scan(&c.City, &c.State, &c.Clips); err != nil {
			return nil, fmt.Errorf("scan city: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
