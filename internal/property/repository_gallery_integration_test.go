package property

import (
	"context"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/db"
)

// TestRepository_GalleryIntegration covers the two gallery methods the photo
// purge uses. Skipped unless TEST_DATABASE_URL points at a database it may
// write to (see TestRepository_Integration). Rows are namespaced per run and
// removed again.
func TestRepository_GalleryIntegration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the property repository integration test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewRepository(pool)
	zpid := "gallery-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE zpid = $1`, zpid)
	})
	full := []string{"https://cdn.example/p/0.jpg", "https://cdn.example/p/1.jpg", "https://cdn.example/p/2.jpg"}
	if _, err := pool.Exec(ctx,
		`INSERT INTO properties (zpid, address, image_urls) VALUES ($1, 'x', $2)`, zpid, full); err != nil {
		t.Fatalf("insert: %v", err)
	}

	t.Run("ImageURLs reads the stored gallery", func(t *testing.T) {
		got, err := repo.ImageURLs(ctx, zpid)
		if err != nil {
			t.Fatalf("ImageURLs: %v", err)
		}
		if !reflect.DeepEqual(got, full) {
			t.Errorf("ImageURLs = %v, want %v", got, full)
		}
	})

	t.Run("ImageURLs of an unknown listing is an error", func(t *testing.T) {
		if _, err := repo.ImageURLs(ctx, zpid+"-missing"); err == nil {
			t.Error("ImageURLs of a missing row = nil error, want one")
		}
	})

	t.Run("TrimImageURLs refuses a row that has changed", func(t *testing.T) {
		stale := []string{"https://cdn.example/p/0.jpg", "https://cdn.example/p/9.jpg"}
		if err := repo.TrimImageURLs(ctx, zpid, stale, stale[:1]); err == nil {
			t.Fatal("TrimImageURLs with a stale gallery = nil, want an error")
		}
		got, _ := repo.ImageURLs(ctx, zpid)
		if !reflect.DeepEqual(got, full) {
			t.Errorf("gallery after refused trim = %v, want untouched %v", got, full)
		}
	})

	t.Run("TrimImageURLs replaces the gallery it was shown", func(t *testing.T) {
		if err := repo.TrimImageURLs(ctx, zpid, full, full[:1]); err != nil {
			t.Fatalf("TrimImageURLs: %v", err)
		}
		got, err := repo.ImageURLs(ctx, zpid)
		if err != nil {
			t.Fatalf("ImageURLs: %v", err)
		}
		if !reflect.DeepEqual(got, full[:1]) {
			t.Errorf("gallery after trim = %v, want %v", got, full[:1])
		}
		var updatedAfterInsert bool
		if err := pool.QueryRow(ctx, `SELECT updated_at > created_at FROM properties WHERE zpid = $1`, zpid).Scan(&updatedAfterInsert); err != nil {
			t.Fatal(err)
		}
		if !updatedAfterInsert {
			t.Error("updated_at not bumped by the trim")
		}
	})
}

// TestRepository_ListGalleriesToPurgeIntegration covers the sweep's query:
// ready listings that still hold more than one photo, in zpid order, after a
// cursor.
func TestRepository_ListGalleriesToPurgeIntegration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the property repository integration test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url, 4)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewRepository(pool)
	// The prefix sorts after every real zpid (digits), so the test rows are
	// the tail of the zpid order whatever else the database holds, and the
	// cursor tests below are exact.
	prefix := "zz-purge-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE zpid LIKE $1`, prefix+"%")
	})
	// renderedAgo < 0 leaves video_rendered_at NULL (rows from before the
	// column existed).
	insert := func(suffix, status string, photos int, renderedAgo time.Duration) string {
		zpid := prefix + suffix
		urls := make([]string, photos)
		for i := range urls {
			urls[i] = "https://cdn.example/properties/" + zpid + "/" + strconv.Itoa(i) + ".jpg"
		}
		var renderedAt *time.Time
		if renderedAgo >= 0 {
			t := time.Now().Add(-renderedAgo)
			renderedAt = &t
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO properties (zpid, address, image_urls, video_status, video_rendered_at) VALUES ($1, 'x', $2, $3, $4)`,
			zpid, urls, status, renderedAt); err != nil {
			t.Fatalf("insert %s: %v", zpid, err)
		}
		return zpid
	}
	const day = 24 * time.Hour
	a := insert("a", "ready", 3, day)
	insert("b", "ready", 1, day)  // already purged
	insert("c", "pending", 3, -1) // no video yet: photos still needed for the render
	insert("d", "failed", 3, -1)  // no video yet: photos still needed for the retry
	e := insert("e", "ready", 2, day)
	f := insert("f", "ready", 5, -1) // ready since before video_rendered_at existed
	g := insert("g", "ready", 3, time.Minute)

	zpids := func(gs []Gallery) []string {
		out := make([]string, len(gs))
		for i, g := range gs {
			out[i] = g.ZPID
		}
		return out
	}

	// A video that went ready a minute ago is still the worker's: its own
	// purge follows the render. The sweep takes only videos that have been
	// ready for settledFor.
	got, err := repo.ListGalleriesToPurge(ctx, prefix, 10, time.Hour)
	if err != nil {
		t.Fatalf("ListGalleriesToPurge: %v", err)
	}
	if want := []string{a, e, f}; !reflect.DeepEqual(zpids(got), want) {
		t.Errorf("purgeable (settled 1h) = %v, want %v", zpids(got), want)
	}
	if len(got) > 0 && len(got[0].URLs) != 3 {
		t.Errorf("gallery of %s = %v, want its 3 photos", a, got[0].URLs)
	}

	got, err = repo.ListGalleriesToPurge(ctx, prefix, 10, 0)
	if err != nil {
		t.Fatalf("ListGalleriesToPurge(settled 0): %v", err)
	}
	if want := []string{a, e, f, g}; !reflect.DeepEqual(zpids(got), want) {
		t.Errorf("purgeable (settled 0) = %v, want %v", zpids(got), want)
	}

	got, err = repo.ListGalleriesToPurge(ctx, prefix, 2, time.Hour)
	if err != nil {
		t.Fatalf("ListGalleriesToPurge(limit 2): %v", err)
	}
	if want := []string{a, e}; !reflect.DeepEqual(zpids(got), want) {
		t.Errorf("first page = %v, want %v", zpids(got), want)
	}
	got, err = repo.ListGalleriesToPurge(ctx, e, 2, time.Hour)
	if err != nil {
		t.Fatalf("ListGalleriesToPurge(after e): %v", err)
	}
	if want := []string{f}; !reflect.DeepEqual(zpids(got), want) {
		t.Errorf("page after %s = %v, want %v", e, zpids(got), want)
	}
}
