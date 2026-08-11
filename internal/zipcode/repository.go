// Package zipcode persists the ZIP rotation state that drives nationwide
// collection: which ZIP codes to search next and when each was last searched.
package zipcode

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository reads and updates ZIP rotation state in PostgreSQL.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository creates a ZIP rotation repository backed by the given pool.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// NextBatch returns up to limit ZIP codes in rotation order: never-searched
// ZIPs first (most populous first, so dense markets are indexed early), then
// stalest-searched first.
func (r *Repository) NextBatch(ctx context.Context, limit int) ([]string, error) {
	const q = `
SELECT zip FROM zip_codes
 ORDER BY last_searched_at ASC NULLS FIRST, population DESC
 LIMIT $1`
	rows, err := r.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("next zip batch: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var zip string
		if err := rows.Scan(&zip); err != nil {
			return nil, fmt.Errorf("scan zip: %w", err)
		}
		out = append(out, zip)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate zip rows: %w", err)
	}
	return out, nil
}

// MarkSearched stamps a ZIP as searched now with the number of listings its
// search returned. Only called after a successful search, so failed ZIPs
// stay at the front of the rotation and retry next cycle.
func (r *Repository) MarkSearched(ctx context.Context, zip string, listingCount int) error {
	const q = `
UPDATE zip_codes
   SET last_searched_at = now(), last_listing_count = $2
 WHERE zip = $1`
	if _, err := r.pool.Exec(ctx, q, zip, listingCount); err != nil {
		return fmt.Errorf("mark zip searched zip=%s: %w", zip, err)
	}
	return nil
}
