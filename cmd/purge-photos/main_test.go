package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/dwellingtw/backend/internal/property"
)

// fakeSource is the properties table as the sweep sees it: galleries in zpid
// order, minus the ones that have been purged.
type fakeSource struct {
	mu        sync.Mutex
	galleries []property.Gallery // sorted by zpid
	afters    []string           // the cursor of every call
	err       error
	onList    func() // runs at the start of every call
}

func (s *fakeSource) ListGalleriesToPurge(ctx context.Context, after string, limit int) ([]property.Gallery, error) {
	if s.onList != nil {
		s.onList()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.afters = append(s.afters, after)
	var out []property.Gallery
	for _, g := range s.galleries {
		if g.ZPID > after && len(g.URLs) > 1 {
			out = append(out, g)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// fakePurger trims the source's gallery like the real one trims the row.
type fakePurger struct {
	mu      sync.Mutex
	src     *fakeSource
	purged  []string
	fail    map[string]bool // PurgeURLs of these fails on a delete
	changed map[string]bool // PurgeURLs of these finds the row changed underneath
}

// Check refuses a gallery with a URL that is not one of this listing's photos,
// as the real purger does.
func (p *fakePurger) Check(zpid string, urls []string) error {
	for _, u := range urls {
		if !strings.HasPrefix(u, "https://cdn.example/properties/"+zpid+"/") {
			return fmt.Errorf("gallery zpid=%s left alone: %q is not one of its photos", zpid, u)
		}
	}
	return nil
}

func (p *fakePurger) PurgeURLs(ctx context.Context, zpid string, urls []string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := p.Check(zpid, urls); err != nil {
		return 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail[zpid] {
		return 0, fmt.Errorf("bunny delete returned status 400 for %s", zpid)
	}
	if p.changed[zpid] {
		return 0, fmt.Errorf("trim gallery zpid=%s: %w", zpid, property.ErrGalleryChanged)
	}
	p.purged = append(p.purged, zpid)
	p.src.mu.Lock()
	for i := range p.src.galleries {
		if p.src.galleries[i].ZPID == zpid {
			p.src.galleries[i].URLs = p.src.galleries[i].URLs[:1]
		}
	}
	p.src.mu.Unlock()
	return len(urls) - 1, nil
}

func (p *fakePurger) sorted() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]string(nil), p.purged...)
	sort.Strings(out)
	return out
}

func galleries(n, photos int) []property.Gallery {
	out := make([]property.Gallery, n)
	for i := range out {
		zpid := fmt.Sprintf("%04d", i+1)
		urls := make([]string, photos)
		for j := range urls {
			urls[j] = fmt.Sprintf("https://cdn.example/properties/%s/%d.jpg", zpid, j)
		}
		out[i] = property.Gallery{ZPID: zpid, URLs: urls}
	}
	return out
}

func zpidsOf(gs []property.Gallery) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.ZPID
	}
	return out
}

func quiet(string, ...any) {}

func TestSweep_PurgesEveryListingAcrossBatches(t *testing.T) {
	src := &fakeSource{galleries: galleries(7, 4)}
	pg := &fakePurger{src: src}

	c := sweep(context.Background(), src, pg, options{batch: 3, workers: 2}, quiet)

	if want := zpidsOf(galleries(7, 4)); !reflect.DeepEqual(pg.sorted(), want) {
		t.Errorf("purged = %v, want all of %v", pg.sorted(), want)
	}
	if c.listings != 7 || c.photos != 21 || c.failed != 0 || c.interrupted {
		t.Errorf("counts = %+v, want 7 listings, 21 photos, 0 failed", c)
	}
	// Keyset pages: each call continues after the last zpid of the one before,
	// never from the top (a listing that failed would otherwise be retried
	// forever).
	if want := []string{"", "0003", "0006", "0007"}; !reflect.DeepEqual(src.afters, want) {
		t.Errorf("cursors = %v, want %v", src.afters, want)
	}
}

func TestSweep_DryRunCountsAndTouchesNothing(t *testing.T) {
	src := &fakeSource{galleries: galleries(5, 3)}
	pg := &fakePurger{src: src}

	c := sweep(context.Background(), src, pg, options{dryRun: true, batch: 2, workers: 2}, quiet)

	if len(pg.sorted()) != 0 {
		t.Errorf("dry run purged %v", pg.sorted())
	}
	if c.listings != 5 || c.photos != 10 {
		t.Errorf("counts = %+v, want 5 listings and 10 photos that would go", c)
	}
}

func TestSweep_LimitBoundsTheListingsConsidered(t *testing.T) {
	src := &fakeSource{galleries: galleries(10, 2)}
	pg := &fakePurger{src: src}

	c := sweep(context.Background(), src, pg, options{limit: 4, batch: 3, workers: 1}, quiet)

	if want := []string{"0001", "0002", "0003", "0004"}; !reflect.DeepEqual(pg.sorted(), want) {
		t.Errorf("purged = %v, want the first 4", pg.sorted())
	}
	if c.listings != 4 {
		t.Errorf("listings = %d, want 4", c.listings)
	}
}

func TestSweep_AFailedListingIsCountedAndTheRestGoOn(t *testing.T) {
	src := &fakeSource{galleries: galleries(6, 3)}
	pg := &fakePurger{src: src, fail: map[string]bool{"0002": true, "0005": true}}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	c := sweep(context.Background(), src, pg, options{batch: 4, workers: 1}, logf)

	if want := []string{"0001", "0003", "0004", "0006"}; !reflect.DeepEqual(pg.sorted(), want) {
		t.Errorf("purged = %v, want %v", pg.sorted(), want)
	}
	if c.listings != 4 || c.failed != 2 {
		t.Errorf("counts = %+v, want 4 listings and 2 failed", c)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "0002") {
		t.Errorf("failed listing not reported: %q", logged)
	}
}

// A worker can purge a listing between the sweep reading its page and
// purging it: both delete (a missing object is a success), one trims, and the
// other finds the gallery changed. That listing is done, not failed, and must
// not make the run exit non-zero.
func TestSweep_AListingPurgedByAWorkerMeanwhileIsNotAFailure(t *testing.T) {
	src := &fakeSource{galleries: galleries(4, 3)}
	pg := &fakePurger{src: src, changed: map[string]bool{"0002": true}}

	c := sweep(context.Background(), src, pg, options{batch: 4, workers: 1}, quiet)

	if c.failed != 0 || c.raced != 1 || c.listings != 3 {
		t.Errorf("counts = %+v, want 3 listings, 1 raced, 0 failed", c)
	}
}

// Dry-run reports what a real run would do, so a gallery the purger would
// refuse is not counted as one that would go.
func TestSweep_DryRunLeavesOutGalleriesThePurgerWouldRefuse(t *testing.T) {
	gs := galleries(3, 3)
	gs[1].URLs[2] = "https://photos.zillowstatic.com/a.jpg"
	src := &fakeSource{galleries: gs}
	pg := &fakePurger{src: src}

	c := sweep(context.Background(), src, pg, options{dryRun: true, batch: 4, workers: 1}, quiet)

	if c.listings != 2 || c.photos != 4 || c.failed != 1 {
		t.Errorf("counts = %+v, want 2 listings, 4 photos, 1 refused", c)
	}
}

func TestSweep_InterruptStopsAndReports(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := &fakeSource{galleries: galleries(9, 2)}
	src.onList = func() {
		if len(src.afters) == 1 {
			cancel() // after the first batch has been handed out
		}
	}
	pg := &fakePurger{src: src}

	c := sweep(ctx, src, pg, options{batch: 3, workers: 1}, quiet)

	if !c.interrupted {
		t.Error("interrupted = false after a cancelled context")
	}
	if !errors.Is(c.err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled: an interrupted run must not exit 0", c.err)
	}
	if n := len(pg.sorted()); n != 3 {
		t.Errorf("purged %d listings, want the 3 of the first batch", n)
	}
}

func TestSweep_AListFailureIsFatal(t *testing.T) {
	src := &fakeSource{err: errors.New("connection refused")}
	pg := &fakePurger{src: src}

	c := sweep(context.Background(), src, pg, options{batch: 3, workers: 1}, quiet)

	if c.err == nil || !strings.Contains(c.err.Error(), "connection refused") {
		t.Errorf("err = %v, want the list error", c.err)
	}
}
