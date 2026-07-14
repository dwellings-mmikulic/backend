// Command backfill-images is a one-off maintenance tool: for every property
// whose image_urls do not point at the CDN yet, it downloads the stored source
// photos, normalizes them to JPEG/PNG, uploads them to Bunny storage, and
// updates the row with the CDN URLs. Properties whose photos can no longer be
// downloaded keep their original URLs untouched.
//
// It uses the stored URLs only — no Zillow API calls, no quota cost.
// Reads DATABASE_URL and BUNNY_* from the environment. Pass -dry-run to
// report what would change without uploading or updating anything.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/dwellingtw/backend/internal/bunny"
	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/imaging"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "report without uploading or updating")
	flag.Parse()

	ctx := context.Background()
	if err := run(ctx, *dryRun); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, dryRun bool) error {
	pool, err := db.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()

	up := bunny.New(
		os.Getenv("BUNNY_STORAGE_ZONE"),
		os.Getenv("BUNNY_API_KEY"),
		os.Getenv("BUNNY_STORAGE_HOST"),
		os.Getenv("BUNNY_CDN_BASE_URL"),
		300*time.Second,
	)
	httpc := &http.Client{Timeout: 30 * time.Second}

	rows, err := pool.Query(ctx,
		`SELECT zpid, image_urls FROM properties WHERE image_urls[1] NOT LIKE '%b-cdn.net%' ORDER BY zpid`)
	if err != nil {
		return fmt.Errorf("query properties: %w", err)
	}
	type prop struct {
		zpid string
		urls []string
	}
	var props []prop
	for rows.Next() {
		var p prop
		if err := rows.Scan(&p.zpid, &p.urls); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		props = append(props, p)
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	log.Printf("found %d properties without CDN images", len(props))

	var updated, unchanged int
	for _, p := range props {
		var cdn []string
		for _, src := range p.urls {
			data, err := download(ctx, httpc, src)
			if err != nil {
				log.Printf("zpid=%s download failed: %s: %v", p.zpid, src, err)
				continue
			}
			data, ext, ct, err := imaging.Normalize(data)
			if err != nil {
				log.Printf("zpid=%s normalize failed: %s: %v", p.zpid, src, err)
				continue
			}
			if dryRun {
				cdn = append(cdn, "(dry-run)")
				continue
			}
			url, err := up.Upload(ctx, fmt.Sprintf("properties/%s/%d%s", p.zpid, len(cdn), ext), bytes.NewReader(data), ct)
			if err != nil {
				log.Printf("zpid=%s upload failed: %s: %v", p.zpid, src, err)
				continue
			}
			cdn = append(cdn, url)
		}
		if len(cdn) == 0 {
			log.Printf("zpid=%s: no photos recoverable (%d sources), row left untouched", p.zpid, len(p.urls))
			unchanged++
			continue
		}
		if !dryRun {
			if _, err := pool.Exec(ctx,
				`UPDATE properties SET image_urls = $1, updated_at = now() WHERE zpid = $2`, cdn, p.zpid); err != nil {
				return fmt.Errorf("update zpid=%s: %w", p.zpid, err)
			}
		}
		updated++
		log.Printf("zpid=%s: %d/%d photos on CDN", p.zpid, len(cdn), len(p.urls))
	}
	log.Printf("done: %d updated, %d untouched (dry-run=%v)", updated, unchanged, dryRun)
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
