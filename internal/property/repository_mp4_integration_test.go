package property

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/db"
)

// TestRepository_MP4PurgeIntegration covers the MP4 purge's query and swap.
// Skipped unless TEST_DATABASE_URL points at a database it may write to. Rows
// are namespaced per run and removed again.
func TestRepository_MP4PurgeIntegration(t *testing.T) {
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
	prefix := "mp4-test-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM video_hls WHERE zpid LIKE $1`, prefix+"%")
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE zpid LIKE $1`, prefix+"%")
	})

	// zpid, video state, rendered how long ago, hls clip hash (empty: none).
	type row struct {
		zpid, status, url, hash string
		age                     time.Duration
		clip                    string
	}
	rows := []row{
		{prefix + "a", "ready", "https://cdn.example/videos/a.mp4", "h1", 2 * time.Hour, "h1"},           // to purge
		{prefix + "b", "ready", "https://cdn.example/videos/b.mp4", "h2", 2 * time.Hour, "old"},          // clip of an older render only
		{prefix + "c", "ready", "https://cdn.example/videos/c.mp4", "h3", 2 * time.Hour, ""},             // no clip
		{prefix + "d", "ready", "https://cdn.example/hls/v1/d/h4/index.m3u8", "h4", 2 * time.Hour, "h4"}, // already swapped
		{prefix + "e", "pending", "", "", 2 * time.Hour, ""},                                             // not ready
		{prefix + "f", "ready", "https://cdn.example/videos/f.mp4", "h6", 10 * time.Minute, "h6"},        // too fresh for settled=1h
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx, `
INSERT INTO properties (zpid, address, sale_price, video_status, video_url, video_content_hash, video_rendered_at)
VALUES ($1, 'x', 1, $2, NULLIF($3, ''), NULLIF($4, ''), now() - $5::interval)`,
			r.zpid, r.status, r.url, r.hash, r.age.String()); err != nil {
			t.Fatalf("insert %s: %v", r.zpid, err)
		}
		if r.clip != "" {
			if _, err := pool.Exec(ctx, `
INSERT INTO video_hls (zpid, content_hash, base_url, segment_ms, total_ms)
VALUES ($1, $2, $3, '{5000}', 5000)`,
				r.zpid, r.clip, "https://cdn.example/hls/v1/"+r.zpid+"/"+r.clip); err != nil {
				t.Fatalf("insert clip %s: %v", r.zpid, err)
			}
		}
	}

	got, err := repo.ListMP4sToPurge(ctx, prefix, 10, time.Hour)
	if err != nil {
		t.Fatalf("ListMP4sToPurge: %v", err)
	}
	want := []VideoSwap{{
		ZPID: prefix + "a", MP4URL: "https://cdn.example/videos/a.mp4",
		HLSURL: "https://cdn.example/hls/v1/" + prefix + "a/h1/index.m3u8", ContentHash: "h1",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListMP4sToPurge(settled 1h) = %+v, want %+v", got, want)
	}

	got, err = repo.ListMP4sToPurge(ctx, prefix, 10, 0)
	if err != nil {
		t.Fatalf("ListMP4sToPurge(settled 0): %v", err)
	}
	if len(got) != 2 || got[0].ZPID != prefix+"a" || got[1].ZPID != prefix+"f" {
		t.Errorf("ListMP4sToPurge(settled 0) = %+v, want a and f", got)
	}

	t.Run("SwapVideoURL refuses a re-rendered row", func(t *testing.T) {
		err := repo.SwapVideoURL(ctx, prefix+"a", want[0].MP4URL, want[0].HLSURL, "other-hash")
		if !errors.Is(err, ErrVideoChanged) {
			t.Fatalf("SwapVideoURL with a stale hash = %v, want ErrVideoChanged", err)
		}
		var url string
		if err := pool.QueryRow(ctx, `SELECT video_url FROM properties WHERE zpid = $1`, prefix+"a").Scan(&url); err != nil {
			t.Fatal(err)
		}
		if url != want[0].MP4URL {
			t.Errorf("video_url = %q after a refused swap, want the MP4 untouched", url)
		}
	})

	t.Run("SwapVideoURL replaces the URL it was shown", func(t *testing.T) {
		if err := repo.SwapVideoURL(ctx, prefix+"a", want[0].MP4URL, want[0].HLSURL, "h1"); err != nil {
			t.Fatalf("SwapVideoURL: %v", err)
		}
		var url, status string
		if err := pool.QueryRow(ctx, `SELECT video_url, video_status FROM properties WHERE zpid = $1`, prefix+"a").Scan(&url, &status); err != nil {
			t.Fatal(err)
		}
		if url != want[0].HLSURL || status != "ready" {
			t.Errorf("row = %q/%s, want the playlist URL and still ready", url, status)
		}
		// Swapped, so the next page no longer lists it; it is still published,
		// now with the playlist as both URLs.
		left, err := repo.ListMP4sToPurge(ctx, prefix, 10, time.Hour)
		if err != nil || len(left) != 0 {
			t.Errorf("ListMP4sToPurge after the swap = %+v, %v; want nothing", left, err)
		}
		p, err := repo.GetPublishedByZPID(ctx, prefix+"a")
		if err != nil {
			t.Fatalf("GetPublishedByZPID after the swap: %v", err)
		}
		if p.VideoURL != want[0].HLSURL || p.HLSURL != want[0].HLSURL {
			t.Errorf("published row = %q/%q, want both URLs the playlist", p.VideoURL, p.HLSURL)
		}
	})
}
