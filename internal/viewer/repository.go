package viewer

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the PostgreSQL Store.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository creates a Repository.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// InsertHeartbeats implements Store. Heartbeats and the last-channel upsert
// go in one transaction so a viewer's "last channel" never points at a
// minute that was not recorded.
func (r *Repository) InsertHeartbeats(ctx context.Context, hb []Heartbeat) error {
	if len(hb) == 0 {
		return nil
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	hashes := make([][]byte, len(hb))
	channels := make([]string, len(hb))
	minutes := make([]time.Time, len(hb))
	for i, b := range hb {
		hashes[i] = b.Viewer[:]
		channels[i] = b.Channel
		minutes[i] = b.Minute
	}
	const ins = `
INSERT INTO viewer_heartbeats (viewer_hash, channel_key, minute)
SELECT * FROM unnest($1::bytea[], $2::text[], $3::timestamptz[])
ON CONFLICT DO NOTHING`
	if _, err := tx.Exec(ctx, ins, hashes, channels, minutes); err != nil {
		return fmt.Errorf("insert heartbeats: %w", err)
	}
	// Latest minute per viewer wins; ties keep whichever unnest yields last,
	// which only matters when one viewer polled two channels in one minute.
	const last = `
INSERT INTO viewer_last_channel (viewer_hash, channel_key, seen_at)
SELECT DISTINCT ON (h) h, c, m
  FROM unnest($1::bytea[], $2::text[], $3::timestamptz[]) AS t(h, c, m)
 ORDER BY h, m DESC
ON CONFLICT (viewer_hash) DO UPDATE
   SET channel_key = EXCLUDED.channel_key, seen_at = EXCLUDED.seen_at
 WHERE EXCLUDED.seen_at >= viewer_last_channel.seen_at`
	if _, err := tx.Exec(ctx, last, hashes, channels, minutes); err != nil {
		return fmt.Errorf("upsert last channel: %w", err)
	}
	return tx.Commit(ctx)
}

// LastChannel implements Store.
func (r *Repository) LastChannel(ctx context.Context, id ID, since time.Time) (string, bool, error) {
	const q = `SELECT channel_key FROM viewer_last_channel WHERE viewer_hash = $1 AND seen_at >= $2`
	var ch string
	err := r.pool.QueryRow(ctx, q, id[:], since).Scan(&ch)
	if err == pgx.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("last channel: %w", err)
	}
	return ch, true, nil
}

// Stats implements Store.
func (r *Repository) Stats(ctx context.Context, channel string, now time.Time) (Stats, error) {
	const q = `
SELECT count(DISTINCT viewer_hash) FILTER (WHERE minute >= $2::timestamptz - interval '2 minutes'),
       count(DISTINCT viewer_hash) FILTER (WHERE minute >= $2::timestamptz - interval '24 hours'),
       count(DISTINCT viewer_hash)
  FROM viewer_heartbeats
 WHERE channel_key = $1 AND minute >= $2::timestamptz - interval '7 days'`
	var s Stats
	if err := r.pool.QueryRow(ctx, q, channel, now).Scan(&s.Concurrent, &s.Unique24h, &s.Unique7d); err != nil {
		return Stats{}, fmt.Errorf("stats %s: %w", channel, err)
	}
	return s, nil
}

// Purge implements Store. viewer_last_channel is trimmed on the same cutoff
// so a stale default cannot outlive the evidence for it.
func (r *Repository) Purge(ctx context.Context, before time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM viewer_heartbeats WHERE minute < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("purge heartbeats: %w", err)
	}
	if _, err := r.pool.Exec(ctx, `DELETE FROM viewer_last_channel WHERE seen_at < $1`, before); err != nil {
		return tag.RowsAffected(), fmt.Errorf("purge last channel: %w", err)
	}
	return tag.RowsAffected(), nil
}
