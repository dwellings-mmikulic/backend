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

// InsertHeartbeats implements Store. Heartbeats, the last-channel upsert and
// the client rows go in one transaction so a viewer's "last channel" never
// points at a minute that was not recorded.
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
	ips := make([]string, len(hb))
	agents := make([]string, len(hb))
	for i, b := range hb {
		hashes[i] = b.Viewer[:]
		channels[i] = b.Channel
		minutes[i] = b.Minute
		ips[i] = b.Client.IP
		agents[i] = b.Client.UserAgent
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
	// Only the seen range is kept, so a batch replayed after a failed flush
	// changes nothing.
	const clients = `
INSERT INTO viewer_clients (ip, user_agent, channel_key, first_seen, last_seen)
SELECT ip, ua, c, min(m), max(m)
  FROM unnest($1::text[], $2::text[], $3::text[], $4::timestamptz[]) AS t(ip, ua, c, m)
 WHERE ip <> ''
 GROUP BY ip, ua, c
ON CONFLICT (ip, user_agent, channel_key) DO UPDATE
   SET first_seen = LEAST(viewer_clients.first_seen, EXCLUDED.first_seen),
       last_seen  = GREATEST(viewer_clients.last_seen, EXCLUDED.last_seen)`
	if _, err := tx.Exec(ctx, clients, ips, agents, channels, minutes); err != nil {
		return fmt.Errorf("upsert clients: %w", err)
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

// Clients implements Store.
func (r *Repository) Clients(ctx context.Context, since time.Time, limit int) ([]ClientSeen, error) {
	const q = `
SELECT ip, user_agent, channel_key, first_seen, last_seen
  FROM viewer_clients
 WHERE last_seen >= $1
 ORDER BY last_seen DESC, ip, channel_key
 LIMIT $2`
	rows, err := r.pool.Query(ctx, q, since, limit)
	if err != nil {
		return nil, fmt.Errorf("clients: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ClientSeen, error) {
		var c ClientSeen
		err := row.Scan(&c.IP, &c.UserAgent, &c.Channel, &c.FirstSeen, &c.LastSeen)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("clients: %w", err)
	}
	return out, nil
}

// Purge implements Store. viewer_last_channel and viewer_clients are trimmed
// on the same cutoff so neither a stale default nor a raw address outlives
// the evidence for it.
func (r *Repository) Purge(ctx context.Context, before time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM viewer_heartbeats WHERE minute < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("purge heartbeats: %w", err)
	}
	if _, err := r.pool.Exec(ctx, `DELETE FROM viewer_last_channel WHERE seen_at < $1`, before); err != nil {
		return tag.RowsAffected(), fmt.Errorf("purge last channel: %w", err)
	}
	if _, err := r.pool.Exec(ctx, `DELETE FROM viewer_clients WHERE last_seen < $1`, before); err != nil {
		return tag.RowsAffected(), fmt.Errorf("purge clients: %w", err)
	}
	return tag.RowsAffected(), nil
}
