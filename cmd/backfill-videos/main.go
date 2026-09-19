// Command backfill-videos is a one-off maintenance tool: for every property
// that has photos but no ready video, it downloads the stored CDN photos,
// renders the listing video, uploads it to Bunny storage, and records the
// result on the row.
//
// It exists because a render failure used to be permanent. Under
// SKIP_EXISTING=true the collection cycle returned before the render step for
// any listing it had already stored, so a listing whose video failed once was
// never retried. The cycle now revisits those listings by itself, but only as
// they resurface in search results — this tool clears the existing backlog in
// one pass.
//
// It uses the stored CDN URLs only — no Zillow API calls, no quota cost.
// Reads DATABASE_URL, BUNNY_* and the VIDEO_* settings from the environment.
// Pass -dry-run to report what would change without rendering or uploading.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/dwellingtw/backend/internal/bunny"
	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/imaging"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/video"
	"github.com/jackc/pgx/v5/pgxpool"
)

// toolMaxConns keeps a maintenance run small next to the fleet's pools: they
// all share one PostgreSQL and its max_connections.
const toolMaxConns = 4

func main() {
	dryRun := flag.Bool("dry-run", false, "report without rendering, uploading, or updating")
	limit := flag.Int("limit", 0, "process at most this many properties (0 = all)")
	flag.Parse()

	ctx := context.Background()
	if err := run(ctx, *dryRun, *limit); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, dryRun bool, limit int) error {
	pool, err := db.Connect(ctx, os.Getenv("DATABASE_URL"), toolMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	repo := property.NewRepository(pool)
	up := bunny.New(
		os.Getenv("BUNNY_STORAGE_ZONE"),
		os.Getenv("BUNNY_API_KEY"),
		os.Getenv("BUNNY_STORAGE_HOST"),
		os.Getenv("BUNNY_CDN_BASE_URL"),
		300*time.Second,
	)
	httpc := &http.Client{Timeout: 60 * time.Second}

	vcfg := config.VideoConfig{
		Enabled:         true,
		SecondsPerPhoto: envInt("VIDEO_SECONDS_PER_PHOTO", 4),
		MusicDir:        envStr("MUSIC_DIR", "assets/music"),
		FontPath:        envStr("VIDEO_FONT_PATH", "/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf"),
	}
	renderer, err := video.New(vcfg)
	if err != nil {
		return fmt.Errorf("init renderer: %w", err)
	}
	log.Printf("renderer ready: %d music tracks, %ds per photo", renderer.TrackCount(), vcfg.SecondsPerPhoto)

	zpids, err := listZPIDsNeedingVideo(ctx, pool, limit)
	if err != nil {
		return err
	}
	log.Printf("found %d properties without a ready video", len(zpids))

	var rendered, skipped, failed int
	for _, zpid := range zpids {
		p, err := repo.GetByZPID(ctx, zpid)
		if err != nil {
			log.Printf("zpid=%s load failed: %v", zpid, err)
			failed++
			continue
		}
		if len(p.ImageURLs) == 0 {
			log.Printf("zpid=%s: no photos, nothing to render", zpid)
			skipped++
			continue
		}
		if dryRun {
			log.Printf("zpid=%s: would render from %d photos", zpid, len(p.ImageURLs))
			rendered++
			continue
		}
		if err := renderOne(ctx, httpc, renderer, up, repo, p, vcfg.SecondsPerPhoto); err != nil {
			log.Printf("zpid=%s render failed: %v", zpid, err)
			// Leave the row marked failed so a later run retries it.
			_ = repo.SetVideoFailed(ctx, zpid)
			failed++
			continue
		}
		rendered++
	}
	log.Printf("done: %d rendered, %d skipped, %d failed (dry-run=%v)", rendered, skipped, failed, dryRun)
	return nil
}

// listZPIDsNeedingVideo returns properties that have photos but no usable
// video, oldest first so the backlog drains deterministically.
func listZPIDsNeedingVideo(ctx context.Context, pool *pgxpool.Pool, limit int) ([]string, error) {
	q := `
SELECT zpid FROM properties
 WHERE (video_status IS DISTINCT FROM 'ready' OR video_url IS NULL OR video_url = '')
   AND image_urls IS NOT NULL AND cardinality(image_urls) > 0
 ORDER BY created_at ASC`
	if limit > 0 {
		q += " LIMIT " + strconv.Itoa(limit)
	}
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query properties: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var zpid string
		if err := rows.Scan(&zpid); err != nil {
			return nil, fmt.Errorf("scan zpid: %w", err)
		}
		out = append(out, zpid)
	}
	return out, rows.Err()
}

// renderOne downloads the property's photos, renders its video, uploads it,
// and records the result.
func renderOne(
	ctx context.Context,
	httpc *http.Client,
	renderer *video.Renderer,
	up *bunny.Client,
	repo *property.Repository,
	p *property.Property,
	secondsPerPhoto int,
) error {
	workDir, err := os.MkdirTemp("", "backfill-"+p.ZPID+"-")
	if err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	var photos []string
	for i, src := range p.ImageURLs {
		data, err := download(ctx, httpc, src)
		if err != nil {
			log.Printf("zpid=%s photo download failed: %s: %v", p.ZPID, src, err)
			continue
		}
		data, ext, _, err := imaging.Normalize(data)
		if err != nil {
			log.Printf("zpid=%s photo normalize failed: %s: %v", p.ZPID, src, err)
			continue
		}
		dest := filepath.Join(workDir, strconv.Itoa(i)+ext)
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return fmt.Errorf("write photo: %w", err)
		}
		photos = append(photos, dest)
	}
	if len(photos) == 0 {
		return fmt.Errorf("no photos could be downloaded (%d sources)", len(p.ImageURLs))
	}

	outPath := filepath.Join(workDir, "video.mp4")
	dur, err := renderer.Render(ctx, p, photos, workDir, outPath)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}

	f, err := os.Open(outPath)
	if err != nil {
		return fmt.Errorf("open rendered video: %w", err)
	}
	defer f.Close()

	cdnURL, err := up.Upload(ctx, path.Join("videos", p.ZPID+".mp4"), f, "video/mp4")
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	// Same content hash the scheduler uses, so the next cycle sees this video
	// as current and does not re-render it.
	hash := video.ContentHash(p, secondsPerPhoto)
	if err := repo.SetVideoReady(ctx, p.ZPID, cdnURL, hash, dur); err != nil {
		return fmt.Errorf("record video: %w", err)
	}
	log.Printf("zpid=%s: video ready (%d photos, %ds) %s", p.ZPID, len(photos), dur, cdnURL)
	return nil
}

func download(ctx context.Context, c *http.Client, src string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", res.StatusCode)
	}
	return io.ReadAll(res.Body)
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
