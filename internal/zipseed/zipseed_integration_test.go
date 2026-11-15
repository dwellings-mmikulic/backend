package zipseed

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/db"
)

// TestSeed_ConcurrentBootsSeedOnce boots ten "instances" against an empty
// zip_codes table: exactly one may COPY, the rest must see its rows and
// return 0 without error. Other packages seed the shared test database, so
// it creates a database of its own on the same server and drops it again:
//
//	TEST_DATABASE_URL=postgres://… go test ./internal/zipseed/
func TestSeed_ConcurrentBootsSeedOnce(t *testing.T) {
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run the zipseed integration test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, emptyDatabase(ctx, t, baseURL), 12)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	want, err := parseRows()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var wg sync.WaitGroup
	counts := make([]int, 10)
	errs := make([]error, 10)
	for i := range counts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counts[i], errs[i] = Seed(ctx, pool)
		}()
	}
	wg.Wait()

	seeders := 0
	for i := range counts {
		if errs[i] != nil {
			t.Errorf("instance %d: %v", i, errs[i])
		}
		if counts[i] > 0 {
			seeders++
			if counts[i] != len(want) {
				t.Errorf("instance %d inserted %d rows, want %d", i, counts[i], len(want))
			}
		}
	}
	if seeders != 1 {
		t.Errorf("%d instances seeded, want exactly 1", seeders)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM zip_codes`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != len(want) {
		t.Errorf("zip_codes holds %d rows, want %d", n, len(want))
	}
}

// emptyDatabase creates a database on the server behind baseURL, drops it
// when the test ends, and returns its URL.
func emptyDatabase(ctx context.Context, t *testing.T, baseURL string) string {
	t.Helper()
	admin, err := db.Connect(ctx, baseURL, 1)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	name := fmt.Sprintf("zipseed_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	// Registered before the caller's pool, so it runs after that pool closed.
	t.Cleanup(func() {
		defer admin.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}
