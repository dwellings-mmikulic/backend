// Command purge-mp4 is a maintenance tool: for every listing whose video is
// ready, still served as an MP4 (video_url = videos/<zpid>.mp4) and also
// available as HLS for the same render, it deletes the MP4 from the CDN and
// points video_url at the HLS playlist instead.
//
// The HLS segments are a remux of the MP4's own bytes, the Roku feed and the
// API already prefer the playlist, and an MP4 can be rebuilt from the
// segments without re-encoding (ffmpeg -i index.m3u8 -c copy), so the MP4
// copy costs storage for nothing. Before an MP4 goes, its playlist is fetched
// from the CDN and must be a complete VOD playlist; a listing whose HLS copy
// is missing keeps its MP4 and is reported.
//
// It is resumable and idempotent: the row is swapped only after the object
// is gone, a run reads only rows that still name an MP4, and deleting an
// object that is already gone is a success. Listings it could not purge are
// reported and left for the next run. Reads DATABASE_URL and BUNNY_* from
// the environment.
//
//	-dry-run        report what would go without deleting or updating anything
//	-settled D      leave alone videos ready for less than D (1h)
//	-limit N        consider at most N listings (0 = all)
//	-batch N        listings read from the database per page (500)
//	-workers N      listings purged side by side (8)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dwellingtw/backend/internal/bunny"
	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/mp4purge"
	"github.com/dwellingtw/backend/internal/property"
)

// toolMaxConns keeps a maintenance run small next to the fleet's pools: they
// all share one PostgreSQL and its max_connections.
const toolMaxConns = 4

// bunnyTimeout bounds one DELETE; probeTimeout one playlist fetch. Bunny
// answers both in well under a second; these only catch a hung connection.
const (
	bunnyTimeout = 60 * time.Second
	probeTimeout = 30 * time.Second
)

func main() {
	var o options
	flag.BoolVar(&o.dryRun, "dry-run", false, "report without deleting or updating")
	flag.IntVar(&o.limit, "limit", 0, "consider at most this many listings (0 = all)")
	flag.IntVar(&o.batch, "batch", 500, "listings read from the database per page")
	flag.IntVar(&o.workers, "workers", 8, "listings purged side by side")
	flag.DurationVar(&o.settled, "settled", time.Hour, "leave alone videos ready for less than this")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, o); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, o options) error {
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
	probe := mp4purge.HTTPProber{Client: &http.Client{Timeout: probeTimeout}}
	purger := mp4purge.New(storage, repo, probe, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	c := sweep(ctx, repo, purger, o, log.Printf)
	log.Printf("done: %d mp4s purged, %d re-rendered meanwhile, %d listings failed (dry-run=%v, interrupted=%v)",
		c.listings, c.raced, c.failed, o.dryRun, c.interrupted)
	if c.err != nil {
		return c.err
	}
	if c.failed > 0 {
		return fmt.Errorf("%d listings could not be purged; run again to retry them", c.failed)
	}
	return nil
}

// swapSource pages through the listings left to purge (property.Repository).
type swapSource interface {
	ListMP4sToPurge(ctx context.Context, after string, limit int, settledFor time.Duration) ([]property.VideoSwap, error)
}

// mp4Purger purges one listing's MP4 (mp4purge.Purger). Check is the refusal
// without the delete.
type mp4Purger interface {
	Check(s property.VideoSwap) error
	Purge(ctx context.Context, s property.VideoSwap) error
}

type options struct {
	dryRun         bool
	limit          int // listings to consider; 0 = all
	batch, workers int
	settled        time.Duration // videos ready for less than this are left alone
}

// counts is what one sweep came to.
type counts struct {
	listings    int // purged (or, in a dry run, that would be)
	raced       int // re-rendered between the page read and the swap
	failed      int // listings left for the next run
	interrupted bool
	err         error // a failure of the sweep itself, not of one listing
}

// sweep pages through the listings left to purge and purges them, workers at a
// time. Pages are keyed on the last zpid seen, never re-read from the top, so
// a listing that fails is reported once and the sweep moves on; a listing
// that succeeds drops out of the query by itself. logf gets one line per
// failed listing and one per page. An interrupted sweep reports ctx's error.
func sweep(ctx context.Context, src swapSource, pg mp4Purger, o options, logf func(string, ...any)) counts {
	batch, workers := max(o.batch, 1), max(o.workers, 1)
	var (
		c     counts
		mu    sync.Mutex // c, once the workers run
		after string
		start = time.Now()
	)
	interrupted := func() counts {
		c.interrupted, c.err = true, ctx.Err()
		return c
	}
	for {
		if ctx.Err() != nil {
			return interrupted()
		}
		want := batch
		if o.limit > 0 {
			remaining := o.limit - c.listings - c.raced - c.failed
			if remaining <= 0 {
				return c
			}
			want = min(want, remaining)
		}
		page, err := src.ListMP4sToPurge(ctx, after, want, o.settled)
		if err != nil {
			if ctx.Err() != nil {
				return interrupted()
			}
			c.err = fmt.Errorf("list mp4s: %w", err)
			return c
		}
		if len(page) == 0 {
			return c
		}
		after = page[len(page)-1].ZPID

		if o.dryRun {
			for _, s := range page {
				if err := pg.Check(s); err != nil {
					c.failed++
					logf("zpid=%s: %v", s.ZPID, err)
					continue
				}
				c.listings++
			}
		} else {
			var wg sync.WaitGroup
			slots := make(chan struct{}, workers)
			for _, s := range page {
				slots <- struct{}{}
				wg.Add(1)
				go func() {
					defer func() { <-slots; wg.Done() }()
					err := pg.Purge(ctx, s)
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err == nil:
						c.listings++
					case errors.Is(err, property.ErrVideoChanged):
						// Re-rendered between the page read and now: the
						// row names the new render, which the next run
						// judges afresh. Done, not failed.
						c.raced++
					case ctx.Err() != nil:
						// Interrupted, not failed: the next run gets it.
					default:
						c.failed++
						logf("zpid=%s: %v", s.ZPID, err)
					}
				}()
			}
			wg.Wait()
		}

		elapsed := time.Since(start).Seconds()
		logf("progress: %d mp4s, %d raced, %d failed, %.1f/s, through zpid %s",
			c.listings, c.raced, c.failed, float64(c.listings)/max(elapsed, 1), after)
	}
}
