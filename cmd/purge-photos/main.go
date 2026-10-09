// Command purge-photos is a maintenance tool: for every listing whose video is
// ready and that still holds more than one photo on the CDN, it deletes every
// photo but the first and trims the row to it. It is the sweep behind the
// workers' own purge (PHOTO_PURGE, run after each render): it covers the
// library rendered before that existed, and whatever a worker's purge left
// behind.
//
// A video is rendered from photos on the worker's disk, never from the CDN
// copies, and only the first photo is used afterwards (the Roku feed and API
// thumbnail), so the rest cost storage for nothing. Listings whose video is
// not ready keep all their photos: a retry renders from them.
//
// It is resumable and idempotent: a listing is trimmed only once all of its
// objects are gone, a run reads only listings that still have more than one
// photo, and deleting an object that is already gone is a success. Listings it
// could not purge are reported and left for the next run. Reads DATABASE_URL
// and BUNNY_* from the environment.
//
//	-dry-run        report what would go without deleting or updating anything
//	-limit N        consider at most N listings (0 = all)
//	-batch N        listings read from the database per page (500)
//	-workers N      listings purged side by side (8)
//	-concurrency N  objects deleted side by side per listing (4)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dwellingtw/backend/internal/bunny"
	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/photopurge"
	"github.com/dwellingtw/backend/internal/property"
)

// toolMaxConns keeps a maintenance run small next to the fleet's pools: they
// all share one PostgreSQL and its max_connections.
const toolMaxConns = 4

// bunnyTimeout bounds one DELETE. Bunny answers in well under a second; this
// only has to catch a hung connection.
const bunnyTimeout = 60 * time.Second

func main() {
	var o options
	flag.BoolVar(&o.dryRun, "dry-run", false, "report without deleting or updating")
	flag.IntVar(&o.limit, "limit", 0, "consider at most this many listings (0 = all)")
	flag.IntVar(&o.batch, "batch", 500, "listings read from the database per page")
	flag.IntVar(&o.workers, "workers", 8, "listings purged side by side")
	concurrency := flag.Int("concurrency", 4, "objects deleted side by side per listing")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, o, *concurrency); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, o options, concurrency int) error {
	pool, err := db.Connect(ctx, os.Getenv("DATABASE_URL"), toolMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	storage := bunny.New(
		os.Getenv("BUNNY_STORAGE_ZONE"),
		os.Getenv("BUNNY_API_KEY"),
		os.Getenv("BUNNY_STORAGE_HOST"),
		os.Getenv("BUNNY_CDN_BASE_URL"),
		bunnyTimeout,
	)
	repo := property.NewRepository(pool)
	purger := photopurge.New(storage, repo, concurrency, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	c := sweep(ctx, repo, purger, o, log.Printf)
	log.Printf("done: %d listings purged, %d photos deleted, %d listings failed (dry-run=%v, interrupted=%v)",
		c.listings, c.photos, c.failed, o.dryRun, c.interrupted)
	if c.err != nil {
		return c.err
	}
	if c.failed > 0 {
		return fmt.Errorf("%d listings could not be purged; run again to retry them", c.failed)
	}
	return nil
}

// gallerySource pages through the listings left to purge (property.Repository).
type gallerySource interface {
	ListGalleriesToPurge(ctx context.Context, after string, limit int) ([]property.Gallery, error)
}

// galleryPurger purges one listing's gallery (photopurge.Purger).
type galleryPurger interface {
	PurgeURLs(ctx context.Context, zpid string, urls []string) (int, error)
}

type options struct {
	dryRun         bool
	limit          int // listings to consider; 0 = all
	batch, workers int
}

// counts is what one sweep came to.
type counts struct {
	listings    int // purged (or, in a dry run, that would be)
	photos      int // objects deleted (or that would be)
	failed      int // listings left for the next run
	interrupted bool
	err         error // a failure of the sweep itself, not of one listing
}

// sweep pages through the listings left to purge and purges them, workers at a
// time. Pages are keyed on the last zpid seen, never re-read from the top, so
// a listing that fails is reported once and the sweep moves on; a listing
// that succeeds drops out of the query by itself. logf gets one line per
// failed listing and one per page.
func sweep(ctx context.Context, src gallerySource, pg galleryPurger, o options, logf func(string, ...any)) counts {
	batch, workers := max(o.batch, 1), max(o.workers, 1)
	var (
		c     counts
		mu    sync.Mutex // c, once the workers run
		after string
		start = time.Now()
	)
	for {
		if ctx.Err() != nil {
			c.interrupted = true
			return c
		}
		want := batch
		if o.limit > 0 {
			remaining := o.limit - c.listings - c.failed
			if remaining <= 0 {
				return c
			}
			want = min(want, remaining)
		}
		page, err := src.ListGalleriesToPurge(ctx, after, want)
		if err != nil {
			if ctx.Err() != nil {
				c.interrupted = true
				return c
			}
			c.err = fmt.Errorf("list galleries: %w", err)
			return c
		}
		if len(page) == 0 {
			return c
		}
		after = page[len(page)-1].ZPID

		if o.dryRun {
			for _, g := range page {
				c.listings++
				c.photos += len(g.URLs) - 1
			}
		} else {
			var wg sync.WaitGroup
			slots := make(chan struct{}, workers)
			for _, g := range page {
				slots <- struct{}{}
				wg.Add(1)
				go func() {
					defer func() { <-slots; wg.Done() }()
					n, err := pg.PurgeURLs(ctx, g.ZPID, g.URLs)
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err == nil:
						c.listings++
						c.photos += n
					case ctx.Err() != nil:
						// Interrupted, not failed: the next run gets it.
					default:
						c.failed++
						logf("zpid=%s: %v", g.ZPID, err)
					}
				}()
			}
			wg.Wait()
		}

		elapsed := time.Since(start).Seconds()
		logf("progress: %d listings, %d photos, %d failed, %.0f photos/s, through zpid %s",
			c.listings, c.photos, c.failed, float64(c.photos)/max(elapsed, 1), after)
	}
}
