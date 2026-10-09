package linear

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/viewer"
)

// Skipped unless TEST_DATABASE_URL points at a database it may write to.
// Rows are written under a random household and removed again.
func TestHouseholdRepository_Integration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the household repository integration test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var hh viewer.ID
	if _, err := rand.Read(hh[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM household_spans WHERE household = $1`, hh[:]) })
	repo := NewRepository(pool)

	if spans, err := repo.Spans(ctx, hh); err != nil || spans != nil {
		t.Fatalf("unknown household: %+v, %v", spans, err)
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	s0 := &Span{Household: hh, N: 0, Scope: "us", Source: SourceDefault, StartsAt: now}
	if ok, err := repo.InsertSpan(ctx, s0); err != nil || !ok {
		t.Fatalf("insert span 0: %v %v", ok, err)
	}
	if ok, err := repo.InsertSpan(ctx, s0); err != nil || ok {
		t.Fatalf("duplicate insert: ok=%v err=%v", ok, err)
	}
	s1 := &Span{Household: hh, N: 1, Scope: "city:katy|tx", Requested: "zip:77494", Source: SourceChoice, StartsAt: now.Add(time.Minute), SeqOffset: 1234, ItemOffset: -7}
	if ok, err := repo.InsertSpan(ctx, s1); err != nil || !ok {
		t.Fatal(err)
	}
	s1.Scope = "state:tx"
	if err := repo.ReplaceSpan(ctx, s1); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceSpan(ctx, &Span{Household: hh, N: 5}); err == nil {
		t.Error("replacing a missing span must fail")
	}
	spans, err := repo.Spans(ctx, hh)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 || spans[1].Scope != "state:tx" || spans[1].SeqOffset != 1234 || spans[1].ItemOffset != -7 ||
		!spans[1].StartsAt.Equal(now.Add(time.Minute)) || spans[1].CreatedAt.IsZero() || spans[0].Household != hh {
		t.Errorf("spans = %+v", spans)
	}

	// Purge trims superseded spans older than the cutoff and keeps the latest.
	if _, err := pool.Exec(ctx, `UPDATE household_spans SET created_at = now() - interval '40 days' WHERE household = $1`, hh[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.NewRepository(pool).Purge(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	spans, _ = repo.Spans(ctx, hh)
	if len(spans) != 1 || spans[0].N != 1 {
		t.Errorf("after purge: %+v", spans)
	}

	cities, err := repo.ListCities(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cities {
		if c.City == "" || c.State == "" || c.Clips < 1 {
			t.Errorf("bad city row %+v", c)
		}
	}
}
