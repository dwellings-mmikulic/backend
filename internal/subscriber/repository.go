package subscriber

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the PostgreSQL Store.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository creates a Repository.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Upsert implements Store. A nil metadata is stored as NULL, and a repeat
// call replaces whatever metadata the row had, like the Cineplex endpoint.
func (r *Repository) Upsert(ctx context.Context, email string, metadata json.RawMessage) error {
	const q = `
INSERT INTO platform_subscribers (email, metadata)
VALUES ($1, $2)
ON CONFLICT (email) DO UPDATE
   SET metadata = EXCLUDED.metadata, updated_at = now()`
	var meta []byte
	if len(metadata) > 0 {
		meta = metadata
	}
	if _, err := r.pool.Exec(ctx, q, email, meta); err != nil {
		return fmt.Errorf("upsert platform subscriber %s: %w", email, err)
	}
	return nil
}
