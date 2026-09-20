package db

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// integrationPool connects to TEST_DATABASE_URL or skips. These tests create
// and drop their own scratch tables, so they can run against a database that
// already holds data.
func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the db integration tests")
	}
	pool, err := Connect(context.Background(), url, 0)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestConnect_PoolSize(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the db integration tests")
	}
	for _, tc := range []struct {
		name string
		in   int
		want int32
	}{
		{"default", 0, defaultMaxConns},
		{"floor", 1, minMaxConns},
		{"explicit", 7, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := Connect(context.Background(), url, tc.in)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer pool.Close()
			if got := pool.Config().MaxConns; got != tc.want {
				t.Errorf("MaxConns = %d, want %d", got, tc.want)
			}
		})
	}
}

// A transaction left idle must be killed by the server, or a worker that
// vanishes mid-transaction would hold its row locks for hours.
func TestConnect_SetsIdleInTransactionTimeout(t *testing.T) {
	pool := integrationPool(t)
	var v string
	if err := pool.QueryRow(context.Background(), `SHOW idle_in_transaction_session_timeout`).Scan(&v); err != nil {
		t.Fatalf("show: %v", err)
	}
	if v != "30s" {
		t.Errorf("idle_in_transaction_session_timeout = %q, want 30s", v)
	}
}

func TestMigrate_SkipsWhenSchemaUnchanged(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	const key = "schema_hash_test_skip"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM schema_meta WHERE key = $1`, key)
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS mw_migrate_probe`)
	})
	const v1 = `CREATE TABLE IF NOT EXISTS mw_migrate_probe (id INT PRIMARY KEY);
INSERT INTO mw_migrate_probe VALUES (1) ON CONFLICT DO NOTHING;`

	applied, err := migrate(ctx, pool, key, v1)
	if err != nil || !applied {
		t.Fatalf("first migrate: applied=%v err=%v, want applied", applied, err)
	}
	applied, err = migrate(ctx, pool, key, v1)
	if err != nil || applied {
		t.Fatalf("second migrate: applied=%v err=%v, want skipped", applied, err)
	}
	applied, err = migrate(ctx, pool, key, v1+"\n-- changed")
	if err != nil || !applied {
		t.Fatalf("changed schema: applied=%v err=%v, want applied", applied, err)
	}
}

// A failing script must leave no hash behind, or the next boot would skip a
// migration that never happened.
func TestMigrate_FailureStoresNoHash(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	const key = "schema_hash_test_fail"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM schema_meta WHERE key = $1`, key)
	})
	if _, err := migrate(ctx, pool, key, `SELECT * FROM mw_table_that_does_not_exist`); err == nil {
		t.Fatal("migrate of a broken script succeeded")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_meta WHERE key = $1`, key).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("hash stored after a failed migration")
	}
}

// Ten instances booting together must serialise: exactly one runs the script,
// the rest see its hash and skip, and nobody errors.
func TestMigrate_ConcurrentBootsSerialise(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	const key = "schema_hash_test_concurrent"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM schema_meta WHERE key = $1`, key)
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS mw_migrate_race`)
	})
	// Without the lock, concurrent CREATE TABLE IF NOT EXISTS of the same
	// table collide on the catalog's unique index and error.
	const script = `CREATE TABLE IF NOT EXISTS mw_migrate_race (id INT PRIMARY KEY);
ALTER TABLE mw_migrate_race ADD COLUMN IF NOT EXISTS note TEXT;
SELECT pg_sleep(0.2);`

	var ran atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			applied, err := migrate(ctx, pool, key, script)
			if err != nil {
				errs <- err
				return
			}
			if applied {
				ran.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent migrate: %v", err)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("script ran %d times, want exactly 1", got)
	}
}

func TestMigrate_RealSchemaIsIdempotent(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Force a second full run of the real script over an existing schema.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_meta WHERE key = $1`, schemaHashKey); err != nil {
		t.Fatalf("clear hash: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestWithXactLock_ExcludesAndReleases(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	const key int64 = 0x6d775f74657374 // scratch key, not used by the app

	var inside, maxInside atomic.Int64
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := WithXactLock(ctx, pool, key, func(pgx.Tx) error {
				n := inside.Add(1)
				for {
					m := maxInside.Load()
					if n <= m || maxInside.CompareAndSwap(m, n) {
						break
					}
				}
				time.Sleep(30 * time.Millisecond)
				inside.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("with lock: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := maxInside.Load(); got != 1 {
		t.Errorf("%d holders inside the lock at once, want 1", got)
	}
}
