// Package photopurge removes a listing's photos from the CDN once its video is
// rendered, keeping the first one.
//
// A video is rendered from the photos a worker downloaded to its disk, never
// from the CDN copies, so once it is ready the gallery on the CDN serves one
// purpose only: the first photo is the listing's thumbnail (Roku feed, the
// live channel's poster, the public API's card image). The other photos cost
// storage for nothing; at ~29 per listing they were most of the objects in the
// zone.
//
// The row is trimmed only after every object is gone, so a crash in between
// leaves a row that still lists the objects — which is what the sweep
// (cmd/purge-photos) finds and finishes: an object that is already gone counts
// as deleted.
package photopurge

import (
	"context"
	"fmt"
	"log/slog"

	"golang.org/x/sync/errgroup"
)

// Storage deletes objects by path and knows which CDN URLs are its own
// (bunny.Client).
type Storage interface {
	Delete(ctx context.Context, path string) error
	ObjectPath(cdnURL string) (string, error)
}

// Store reads and trims a listing's gallery (property.Repository).
type Store interface {
	ImageURLs(ctx context.Context, zpid string) ([]string, error)
	// TrimImageURLs replaces the gallery from with to, and fails when the row
	// no longer holds from.
	TrimImageURLs(ctx context.Context, zpid string, from, to []string) error
}

// Purger purges galleries. It is safe for concurrent use.
type Purger struct {
	storage     Storage
	store       Store
	concurrency int
	log         *slog.Logger
}

// New returns a Purger that deletes up to concurrency objects at once.
func New(storage Storage, store Store, concurrency int, log *slog.Logger) *Purger {
	return &Purger{storage: storage, store: store, concurrency: max(concurrency, 1), log: log}
}

// Purge reads the listing's gallery from its row and purges it. It reports
// how many objects were deleted.
//
// The row, not the caller's copy of the property, is what gets purged: a
// worker revisiting a listing holds the provider's source URLs, while the row
// holds the CDN URLs the public reads.
func (p *Purger) Purge(ctx context.Context, zpid string) (int, error) {
	urls, err := p.store.ImageURLs(ctx, zpid)
	if err != nil {
		return 0, fmt.Errorf("load gallery zpid=%s: %w", zpid, err)
	}
	return p.PurgeURLs(ctx, zpid, urls)
}

// PurgeURLs deletes every photo of the gallery but the first and trims the row
// to it. A gallery of one or none is left alone.
//
// Every URL must be an object of our CDN, or nothing is touched: a source URL
// in the row means it was never moved to the CDN (or has just been refreshed
// with new sources), and deleting around it would leave a mixed row. A failed
// delete leaves the row untrimmed and is returned; the objects that did go are
// gone, and the next attempt has only the rest to do.
func (p *Purger) PurgeURLs(ctx context.Context, zpid string, urls []string) (int, error) {
	if len(urls) < 2 {
		return 0, nil
	}
	paths := make([]string, len(urls))
	for i, u := range urls {
		path, err := p.storage.ObjectPath(u)
		if err != nil {
			return 0, fmt.Errorf("gallery zpid=%s left alone: %w", zpid, err)
		}
		paths[i] = path
	}

	// No WithContext: one refused object must not abandon the others, which
	// would be as much as left to do on the next attempt. ctx itself still
	// stops everything when the caller is done.
	var g errgroup.Group
	g.SetLimit(p.concurrency)
	for _, path := range paths[1:] {
		g.Go(func() error {
			if err := p.storage.Delete(ctx, path); err != nil {
				return fmt.Errorf("delete %s: %w", path, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return 0, fmt.Errorf("purge zpid=%s: %w", zpid, err)
	}
	if err := p.store.TrimImageURLs(ctx, zpid, urls, urls[:1]); err != nil {
		return 0, fmt.Errorf("trim gallery zpid=%s: %w", zpid, err)
	}
	p.log.Debug("photos purged", "zpid", zpid, "deleted", len(paths)-1, "kept", urls[0])
	return len(paths) - 1, nil
}
