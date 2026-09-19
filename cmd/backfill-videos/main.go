// Command backfill-videos is a maintenance tool: it finds every property that
// has photos but no ready video and ENQUEUES it for the workers, which render
// it from the stored CDN photos, upload it, segment it for the linear channels
// and record the result — the same pipeline, claims and retries as a freshly
// discovered listing. It renders nothing itself, so at least one instance
// with ROLE=all or ROLE=worker has to be running for the backlog to drain.
//
// It exists because a listing without a video is otherwise only revisited
// when it resurfaces in search results, which for most of the country is a
// full ZIP rotation away. It also revives dead queue rows (listings that used
// up their attempts), which is how to retry them after fixing the cause.
//
// It uses the stored rows only — no Zillow API calls, no quota cost. Reads
// DATABASE_URL from the environment.
//
//	-dry-run  report what would be enqueued without touching the queue
//	-limit N  consider at most N properties (0 = all)
//	-status   print the queue's state and exit
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/scheduler"
	"github.com/dwellingtw/backend/internal/workqueue"
)

// toolMaxConns keeps a maintenance run small next to the fleet's pools: they
// all share one PostgreSQL and its max_connections.
const toolMaxConns = 4

// enqueueBatch bounds one INSERT. Each payload carries a listing's photo
// URLs, so a batch is a few hundred kilobytes.
const enqueueBatch = 500

func main() {
	dryRun := flag.Bool("dry-run", false, "report without enqueuing")
	limit := flag.Int("limit", 0, "consider at most this many properties (0 = all)")
	status := flag.Bool("status", false, "print the queue's state and exit")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *dryRun, *limit, *status); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, dryRun bool, limit int, statusOnly bool) error {
	pool, err := db.Connect(ctx, os.Getenv("DATABASE_URL"), toolMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	// The queue table arrives with this version's schema, and the tool may be
	// the first thing run after an upgrade. A no-op when the schema is current.
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	queue := workqueue.NewRepository(pool)
	if statusOnly {
		return printStatus(ctx, queue)
	}

	zpids, err := listZPIDsNeedingVideo(ctx, pool, limit)
	if err != nil {
		return err
	}
	log.Printf("found %d properties with photos but no ready video", len(zpids))

	repo := property.NewRepository(pool)
	var offered, enqueued, failed int
	batch := make([]workqueue.NewItem, 0, enqueueBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := queue.Enqueue(ctx, batch)
		// ErrUnstorable: everything storable in the batch WAS enqueued and n
		// is valid; the message names the listings PostgreSQL refused.
		if errors.Is(err, workqueue.ErrUnstorable) {
			log.Printf("warning: %v", err)
			err = nil
		}
		if err != nil {
			return fmt.Errorf("enqueue: %w", err)
		}
		offered += len(batch)
		enqueued += n
		batch = batch[:0]
		return nil
	}

	for _, zpid := range zpids {
		if ctx.Err() != nil {
			break
		}
		p, err := repo.GetByZPID(ctx, zpid)
		if err != nil {
			log.Printf("zpid=%s load failed: %v", zpid, err)
			failed++
			continue
		}
		if dryRun {
			log.Printf("zpid=%s: would enqueue (%d photos)", zpid, len(p.ImageURLs))
			offered++
			continue
		}
		// revisit: render only. The stored row and its CDN photos are left
		// exactly as they are, whatever SKIP_EXISTING says on the worker.
		payload, err := scheduler.EncodeListing(p, true)
		if err != nil {
			log.Printf("zpid=%s encode failed: %v", zpid, err)
			failed++
			continue
		}
		batch = append(batch, workqueue.NewItem{ZPID: p.ZPID, Payload: payload, SourceZip: p.Zip})
		if len(batch) == enqueueBatch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}

	if dryRun {
		log.Printf("dry run: %d would be offered to the queue, %d failed to load", offered, failed)
		return ctx.Err()
	}
	// Enqueue leaves rows that are already waiting or being rendered alone;
	// dead rows are revived and count as enqueued.
	log.Printf("done: %d enqueued (new or revived), %d already queued or not storable, %d failed to load or encode",
		enqueued, offered-enqueued, failed)
	if err := printStatus(ctx, queue); err != nil {
		return err
	}
	return ctx.Err()
}

func printStatus(ctx context.Context, queue *workqueue.Repository) error {
	st, err := queue.Stats(ctx)
	if err != nil {
		return fmt.Errorf("queue stats: %w", err)
	}
	log.Printf("queue: %d claimable, %d claimed, %d in backoff, %d dead",
		st.Claimable, st.Claimed, st.Backoff, st.Dead)
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
