package property

import (
	"context"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/db"
)

// Skipped unless TEST_DATABASE_URL points at a database it may write to. It
// empties search_feed_pins, so the next feed request there pins a fresh set.
func TestListSearchFeed_Integration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the search feed integration test")
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
	var published int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM properties WHERE `+publishedPredicate).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if published > SearchFeedSize-10 {
		t.Skipf("%d published listings already; this test's rows might not make the pinned set", published)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	prefix := zpidPrefix + "pin-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-"
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM properties WHERE zpid LIKE $1`, prefix+"%")
		pool.Exec(ctx, `TRUNCATE search_feed_pins`)
	})
	repo := NewRepository(pool)
	add := func(name, status string) {
		t.Helper()
		if err := repo.Upsert(ctx, &Property{ZPID: prefix + name, Address: name, ImageURLs: []string{"https://cdn/x.jpg"}}); err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE properties SET video_status = $2, video_url = 'https://cdn/v.mp4', video_rendered_at = now() WHERE zpid = $1`,
			prefix+name, status)
	}
	ours := func(props []Property) []string {
		var out []string
		for _, p := range props {
			if len(p.ZPID) > len(prefix) && p.ZPID[:len(prefix)] == prefix {
				out = append(out, p.ZPID[len(prefix):])
			}
		}
		slices.Sort(out)
		return out
	}

	exec(`TRUNCATE search_feed_pins`)
	add("a", "ready")
	add("b", "ready")
	add("pending", "pending")

	got, err := repo.ListSearchFeed(ctx)
	if err != nil {
		t.Fatalf("first ListSearchFeed: %v", err)
	}
	if o := ours(got); !slices.Equal(o, []string{"a", "b"}) {
		t.Fatalf("first call = %v, want the published a and b pinned", o)
	}

	// A listing published later does not join; one re-rendering drops out
	// until it is ready again, and nothing takes its place.
	add("late", "ready")
	exec(`UPDATE properties SET video_status = 'pending' WHERE zpid = $1`, prefix+"b")
	got, err = repo.ListSearchFeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if o := ours(got); !slices.Equal(o, []string{"a"}) {
		t.Fatalf("after changes = %v, want only a (late not pinned, b re-rendering)", o)
	}
	exec(`UPDATE properties SET video_status = 'ready' WHERE zpid = $1`, prefix+"b")
	got, err = repo.ListSearchFeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if o := ours(got); !slices.Equal(o, []string{"a", "b"}) {
		t.Fatalf("after b is ready again = %v, want a and b", o)
	}
}
