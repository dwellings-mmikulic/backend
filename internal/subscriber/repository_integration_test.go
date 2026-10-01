package subscriber

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/db"
)

// Skipped unless TEST_DATABASE_URL points at a database it may write to.
func TestRepository_Integration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the subscriber repository integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE platform_subscribers`); err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(pool)

	if err := repo.Upsert(ctx, "a@example.com", json.RawMessage(`{"device":"roku"}`)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(ctx, "b@example.com", nil); err != nil {
		t.Fatal(err)
	}
	// Same email again: one row, metadata replaced, updated_at bumped.
	if err := repo.Upsert(ctx, "a@example.com", json.RawMessage(`{"device":"web"}`)); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM platform_subscribers`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rows = %d, want 2", n)
	}
	var device *string
	var bumped bool
	err = pool.QueryRow(ctx, `SELECT metadata->>'device', updated_at > created_at FROM platform_subscribers WHERE email = 'a@example.com'`).Scan(&device, &bumped)
	if err != nil {
		t.Fatal(err)
	}
	if device == nil || *device != "web" || !bumped {
		t.Errorf("a: device=%v bumped=%v", device, bumped)
	}
	var meta []byte
	if err := pool.QueryRow(ctx, `SELECT metadata FROM platform_subscribers WHERE email = 'b@example.com'`).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if meta != nil {
		t.Errorf("b: metadata = %s, want NULL", meta)
	}
}
