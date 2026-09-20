package zipseed

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/dwellingtw/backend/internal/db"
)

// TestSeed_ConcurrentBootsSeedOnce boots ten "instances" against an empty
// zip_codes table: exactly one may COPY, the rest must see its rows and
// return 0 without error. It never empties the table itself, so it only
// exercises the race on a fresh database (skipped otherwise):
//
//	TEST_DATABASE_URL=postgres://…/scratch go test ./internal/zipseed/
func TestSeed_ConcurrentBootsSeedOnce(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the zipseed integration test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url, 12)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var seeded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM zip_codes)`).Scan(&seeded); err != nil {
		t.Fatalf("check: %v", err)
	}
	if seeded {
		t.Skip("zip_codes is already seeded; the race needs an empty table")
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
