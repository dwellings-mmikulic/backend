package db

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

const (
	defaultMaxConns = 10
	// minMaxConns keeps a worker's loops from starving each other on a pool
	// that was configured too small.
	minMaxConns = 4
)

// Advisory lock keys. Every instance of the fleet boots against the same
// database, so the startup steps that are not safe to run twice at once take
// one of these first.
const (
	LockMigrate int64 = 0x6477656c6c6d6967 // "dwellmig"
	LockSeed    int64 = 0x6477656c6c736565 // "dwellsee"
)

// schemaHashKey is the schema_meta row holding the hash of the schema as last
// applied.
const schemaHashKey = "schema_hash"

// Connect opens a pgx connection pool of at most maxConns connections and
// verifies connectivity. maxConns <= 0 takes the default. The fleet shares one
// PostgreSQL, so the sum of every instance's pool has to fit max_connections.
func Connect(ctx context.Context, databaseURL string, maxConns int) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	switch {
	case maxConns <= 0:
		maxConns = defaultMaxConns
	case maxConns < minMaxConns:
		maxConns = minMaxConns
	}
	cfg.MaxConns = int32(maxConns)
	cfg.MaxConnLifetime = time.Hour
	// Nothing here legitimately idles inside a transaction. Without this, a
	// worker that vanishes mid-transaction keeps its row locks until TCP
	// keepalive gives up, hours later.
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "30000"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}

// WithXactLock runs fn in a transaction that holds the advisory lock key
// until it commits or rolls back. The lock lives exactly as long as the work:
// there is no unlock to forget, and a dead connection frees it.
func WithXactLock(ctx context.Context, pool *pgxpool.Pool, key int64, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// A no-op after Commit; on the error paths the caller's ctx may already
	// be done, and the rollback must still reach the server.
	defer func() {
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rbCtx)
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Migrate applies the embedded schema. Instances booting together serialise
// on an advisory lock, and the script is skipped entirely when its hash
// matches the one stored by the last run: its ALTER TABLE statements take
// ACCESS EXCLUSIVE on properties even when they change nothing, which a fleet
// restarting on every deploy must not do to the API.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := migrate(ctx, pool, schemaHashKey, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// migrate runs script unless key already holds its hash, and reports whether
// it ran. The script is idempotent, so a changed hash (even a comment edit)
// only costs one full run.
func migrate(ctx context.Context, pool *pgxpool.Pool, key, script string) (bool, error) {
	sum := sha256.Sum256([]byte(script))
	want := hex.EncodeToString(sum[:])

	applied := false
	err := WithXactLock(ctx, pool, LockMigrate, func(tx pgx.Tx) error {
		const meta = `CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`
		if _, err := tx.Exec(ctx, meta); err != nil {
			return fmt.Errorf("create schema_meta: %w", err)
		}
		var have string
		err := tx.QueryRow(ctx, `SELECT value FROM schema_meta WHERE key = $1`, key).Scan(&have)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read schema hash: %w", err)
		}
		if have == want {
			return nil
		}
		// No arguments: pgx sends the multi-statement script over the simple
		// protocol, inside this transaction.
		if _, err := tx.Exec(ctx, script); err != nil {
			return err
		}
		const store = `
INSERT INTO schema_meta (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`
		if _, err := tx.Exec(ctx, store, key, want); err != nil {
			return fmt.Errorf("store schema hash: %w", err)
		}
		applied = true
		return nil
	})
	return applied, err
}
