package linear

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const bumperColumns = `id, kind, name, content_hash, base_url, segment_ms, total_ms, created_at`

func scanBumper(row pgx.Row) (*Bumper, error) {
	var b Bumper
	var ms []int32
	if err := row.Scan(&b.ID, &b.Kind, &b.Name, &b.ContentHash, &b.BaseURL, &ms, &b.TotalMS, &b.CreatedAt); err != nil {
		return nil, err
	}
	b.SegmentMS = ints(ms)
	return &b, nil
}

// InsertBumper stores b unless a bumper with its content hash exists; it
// returns the stored row's id either way, and whether this call created it.
// An empty Kind is stored as KindBumper.
func (r *Repository) InsertBumper(ctx context.Context, b *Bumper) (int64, bool, error) {
	const q = `
INSERT INTO bumpers (kind, name, content_hash, base_url, segment_ms, total_ms)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (content_hash) DO NOTHING
RETURNING id`
	kind := b.Kind
	if kind == "" {
		kind = KindBumper
	}
	var id int64
	err := r.pool.QueryRow(ctx, q, kind, b.Name, b.ContentHash, b.BaseURL, int32s(b.SegmentMS), b.TotalMS).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("insert bumper: %w", err)
	}
	existing, err := r.BumperByHash(ctx, b.ContentHash)
	if err != nil {
		return 0, false, err
	}
	if existing == nil {
		return 0, false, fmt.Errorf("insert bumper: conflict on %s but no row", b.ContentHash)
	}
	return existing.ID, false, nil
}

// BumperByHash returns the bumper with content hash hash, or nil.
func (r *Repository) BumperByHash(ctx context.Context, hash string) (*Bumper, error) {
	b, err := scanBumper(r.pool.QueryRow(ctx, `SELECT `+bumperColumns+` FROM bumpers WHERE content_hash = $1`, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bumper by hash: %w", err)
	}
	return b, nil
}

// CurrentBumper implements Store: the newest row of kind.
func (r *Repository) CurrentBumper(ctx context.Context, kind string) (*Bumper, error) {
	b, err := scanBumper(r.pool.QueryRow(ctx, `SELECT `+bumperColumns+` FROM bumpers WHERE kind = $1 ORDER BY id DESC LIMIT 1`, kind))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("current bumper: %w", err)
	}
	return b, nil
}
