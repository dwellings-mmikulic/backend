// Command backfill-hls segments every ready listing video that the linear
// channels cannot air yet: properties whose current render has no video_hls
// row. It downloads the MP4 from the CDN, remuxes it into TS segments,
// uploads them under hls/v1/<zpid>/<hash8>/ and records the layout.
//
// The scheduler does the same for every new render; this tool clears the
// backlog that existed before channels shipped and retries clips whose
// segmentation failed. It costs no Zillow API quota.
//
// Reads DATABASE_URL and BUNNY_* from the environment. Flags: -dry-run,
// -limit N, -concurrency N (default 4).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/dwellingtw/backend/internal/bunny"
	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/hls"
	"github.com/dwellingtw/backend/internal/linear"
)

type pending struct {
	zpid, videoURL, hash string
}

func main() {
	dryRun := flag.Bool("dry-run", false, "report without downloading, segmenting, uploading, or updating")
	limit := flag.Int("limit", 0, "process at most this many videos (0 = all)")
	concurrency := flag.Int("concurrency", 4, "videos processed in parallel")
	flag.Parse()

	if err := run(context.Background(), *dryRun, *limit, *concurrency); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, dryRun bool, limit, concurrency int) error {
	pool, err := db.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()

	up := bunny.New(
		os.Getenv("BUNNY_STORAGE_ZONE"),
		os.Getenv("BUNNY_API_KEY"),
		envStr("BUNNY_STORAGE_HOST", "storage.bunnycdn.com"),
		os.Getenv("BUNNY_CDN_BASE_URL"),
		300*time.Second,
	)
	repo := linear.NewRepository(pool)
	seg := hls.NewSegmenter()
	httpc := &http.Client{Timeout: 120 * time.Second}

	todo, err := listPending(ctx, pool, limit)
	if err != nil {
		return err
	}
	log.Printf("found %d ready videos without segments", len(todo))
	if dryRun {
		for _, p := range todo {
			log.Printf("zpid=%s: would segment %s", p.zpid, p.videoURL)
		}
		return nil
	}

	var done, failed atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(concurrency, 1))
	for _, p := range todo {
		g.Go(func() error {
			if err := segmentOne(gctx, httpc, seg, up, repo, p); err != nil {
				log.Printf("zpid=%s failed: %v", p.zpid, err)
				failed.Add(1)
				return nil // keep going; the next run retries it
			}
			done.Add(1)
			return nil
		})
	}
	_ = g.Wait()
	log.Printf("done: %d segmented, %d failed", done.Load(), failed.Load())
	return nil
}

// listPending returns ready videos whose current render has no video_hls
// row, oldest first so the backlog drains deterministically.
func listPending(ctx context.Context, pool *pgxpool.Pool, limit int) ([]pending, error) {
	q := `
SELECT p.zpid, p.video_url, COALESCE(p.video_content_hash, '')
  FROM properties p
  LEFT JOIN video_hls h ON h.zpid = p.zpid AND h.content_hash = COALESCE(p.video_content_hash, '')
 WHERE p.video_status = 'ready' AND p.video_url IS NOT NULL AND p.video_url <> '' AND h.id IS NULL
 ORDER BY p.created_at ASC`
	if limit > 0 {
		q += " LIMIT " + strconv.Itoa(limit)
	}
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query pending: %w", err)
	}
	defer rows.Close()
	var out []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.zpid, &p.videoURL, &p.hash); err != nil {
			return nil, fmt.Errorf("scan pending: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func segmentOne(ctx context.Context, httpc *http.Client, seg *hls.Segmenter, up *bunny.Client, repo *linear.Repository, p pending) error {
	workDir, err := os.MkdirTemp("", "backfill-hls-"+p.zpid+"-")
	if err != nil {
		return fmt.Errorf("work dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	mp4 := filepath.Join(workDir, "video.mp4")
	if err := download(ctx, httpc, p.videoURL, mp4); err != nil {
		return fmt.Errorf("download %s: %w", p.videoURL, err)
	}
	dir := filepath.Join(workDir, "hls")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	clip, err := seg.Segment(ctx, mp4, dir)
	if err != nil {
		return err
	}
	base, err := hls.Upload(ctx, up, dir, hls.Prefix(p.zpid, p.hash), clip, 8)
	if err != nil {
		return err
	}
	if err := repo.SetVideoHLS(ctx, p.zpid, p.hash, base, clip); err != nil {
		return err
	}
	log.Printf("zpid=%s: %d segments, %d ms → %s", p.zpid, len(clip.SegmentMS), clip.TotalMS, base)
	return nil
}

func download(ctx context.Context, c *http.Client, src, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", res.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, res.Body)
	return err
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
