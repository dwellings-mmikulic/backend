package viewer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/db"
)

// Skipped unless TEST_DATABASE_URL points at a database it may write to.
func TestRepository_Integration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the viewer repository integration test")
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
	if _, err := pool.Exec(ctx, `TRUNCATE viewer_heartbeats, viewer_last_channel`); err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(pool)

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	a, b := ID{1}, ID{2}
	beats := []Heartbeat{
		{a, "us", now.Add(-40 * time.Hour)},
		{a, "us", now.Add(-time.Minute)},
		{a, "state:tx", now}, // latest → last channel
		{b, "us", now.Add(-5 * time.Hour)},
	}
	if err := repo.InsertHeartbeats(ctx, beats); err != nil {
		t.Fatal(err)
	}
	if err := repo.InsertHeartbeats(ctx, beats[:1]); err != nil { // duplicate is fine
		t.Fatal(err)
	}
	if ch, ok, err := repo.LastChannel(ctx, a, now.Add(-time.Hour)); err != nil || !ok || ch != "state:tx" {
		t.Errorf("last channel a = %q %v %v, want state:tx", ch, ok, err)
	}
	if _, ok, _ := repo.LastChannel(ctx, b, now.Add(-time.Hour)); ok {
		t.Error("b was seen 5h ago; since=1h must not match")
	}
	// An older batch must not roll last_channel backwards.
	if err := repo.InsertHeartbeats(ctx, []Heartbeat{{a, "zip:77494", now.Add(-10 * time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	if ch, _, _ := repo.LastChannel(ctx, a, now.Add(-time.Hour)); ch != "state:tx" {
		t.Errorf("older batch rolled last channel back to %q", ch)
	}

	s, err := repo.Stats(ctx, "us", now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Concurrent != 1 || s.Unique24h != 2 || s.Unique7d != 2 {
		t.Errorf("stats = %+v, want concurrent 1, 24h 2, 7d 2", s)
	}

	n, err := repo.Purge(ctx, now.Add(-30*time.Hour))
	if err != nil || n != 1 {
		t.Errorf("purge = %d, %v; want 1 row", n, err)
	}
}
