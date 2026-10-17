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
	"time"

	"github.com/dwellingtw/backend/internal/property"
)

// fakeSource is the properties table as the sweep sees it: swaps in zpid
// order, minus the ones that have been purged.
type fakeSource struct {
	mu      sync.Mutex
	swaps   []property.VideoSwap // sorted by zpid
	done    map[string]bool
	afters  []string        // the cursor of every call
	settled []time.Duration // the settle margin of every call
	err     error
	onList  func() // runs at the start of every call
}

func (s *fakeSource) ListMP4sToPurge(ctx context.Context, after string, limit int, settledFor time.Duration) ([]property.VideoSwap, error) {
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
	s.settled = append(s.settled, settledFor)
	var out []property.VideoSwap
	for _, v := range s.swaps {
		if v.ZPID > after && !s.done[v.ZPID] {
			out = append(out, v)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// fakePurger swaps the source's row like the real one swaps the table's.
type fakePurger struct {
	mu      sync.Mutex
	src     *fakeSource
	purged  []string
	fail    map[string]bool // Purge of these fails
	changed map[string]bool // Purge of these finds the row re-rendered
}

// Check refuses a swap whose MP4 is not the listing's own, as the real one.
func (p *fakePurger) Check(s property.VideoSwap) error {
	if s.MP4URL != "https://cdn.example/videos/"+s.ZPID+".mp4" {
		return fmt.Errorf("mp4 zpid=%s left alone: %q is not its MP4", s.ZPID, s.MP4URL)
	}
	return nil
}

func (p *fakePurger) Purge(ctx context.Context, s property.VideoSwap) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.Check(s); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail[s.ZPID] {
		return fmt.Errorf("delete videos/%s.mp4: bunny delete returned status 400", s.ZPID)
	}
	if p.changed[s.ZPID] {
		return fmt.Errorf("swap video url zpid=%s: %w", s.ZPID, property.ErrVideoChanged)
	}
	p.purged = append(p.purged, s.ZPID)
	p.src.mu.Lock()
	p.src.done[s.ZPID] = true
	p.src.mu.Unlock()
	return nil
}

func (p *fakePurger) sorted() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]string(nil), p.purged...)
	sort.Strings(out)
	return out
}

func swaps(n int) []property.VideoSwap {
	out := make([]property.VideoSwap, n)
	for i := range out {
		zpid := fmt.Sprintf("%04d", i+1)
		out[i] = property.VideoSwap{
			ZPID:        zpid,
			MP4URL:      "https://cdn.example/videos/" + zpid + ".mp4",
			HLSURL:      "https://cdn.example/hls/v1/" + zpid + "/aaaaaaaa/index.m3u8",
			ContentHash: "aaaaaaaa",
		}
	}
	return out
}

func source(sw []property.VideoSwap) *fakeSource {
	return &fakeSource{swaps: sw, done: map[string]bool{}}
}

func zpidsOf(sw []property.VideoSwap) []string {
	out := make([]string, len(sw))
	for i, s := range sw {
		out[i] = s.ZPID
	}
	return out
}

func quiet(string, ...any) {}

func TestSweep_PurgesEveryListingAcrossBatches(t *testing.T) {
	src := source(swaps(7))
	pg := &fakePurger{src: src}

	c := sweep(context.Background(), src, pg, options{batch: 3, workers: 2, settled: time.Hour}, quiet)

	if want := zpidsOf(swaps(7)); !reflect.DeepEqual(pg.sorted(), want) {
		t.Errorf("purged = %v, want all of %v", pg.sorted(), want)
	}
	for i, d := range src.settled {
		if d != time.Hour {
			t.Errorf("page %d asked with settled = %v, want 1h", i, d)
		}
	}
	if c.listings != 7 || c.failed != 0 || c.interrupted {
		t.Errorf("counts = %+v, want 7 listings, 0 failed", c)
	}
	if want := []string{"", "0003", "0006", "0007"}; !reflect.DeepEqual(src.afters, want) {
		t.Errorf("cursors = %v, want %v", src.afters, want)
	}
}

func TestSweep_DryRunCountsAndTouchesNothing(t *testing.T) {
	src := source(swaps(5))
	pg := &fakePurger{src: src}

	c := sweep(context.Background(), src, pg, options{dryRun: true, batch: 2, workers: 2}, quiet)

	if len(pg.sorted()) != 0 {
		t.Errorf("dry run purged %v", pg.sorted())
	}
	if c.listings != 5 {
		t.Errorf("counts = %+v, want 5 listings that would go", c)
	}
}

func TestSweep_LimitBoundsTheListingsConsidered(t *testing.T) {
	src := source(swaps(10))
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
	src := source(swaps(6))
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

// A listing re-rendered between the page read and the swap is done, not
// failed: its row names the new render, and must not make the run exit
// non-zero.
func TestSweep_AListingReRenderedMeanwhileIsNotAFailure(t *testing.T) {
	src := source(swaps(4))
	pg := &fakePurger{src: src, changed: map[string]bool{"0002": true}}

	c := sweep(context.Background(), src, pg, options{batch: 4, workers: 1}, quiet)

	if c.failed != 0 || c.raced != 1 || c.listings != 3 {
		t.Errorf("counts = %+v, want 3 listings, 1 raced, 0 failed", c)
	}
}

// Dry-run reports what a real run would do, so a row the purger would refuse
// is not counted as one that would go.
func TestSweep_DryRunLeavesOutRowsThePurgerWouldRefuse(t *testing.T) {
	sw := swaps(3)
	sw[1].MP4URL = "https://photos.zillowstatic.com/a.mp4"
	src := source(sw)
	pg := &fakePurger{src: src}

	c := sweep(context.Background(), src, pg, options{dryRun: true, batch: 4, workers: 1}, quiet)

	if c.listings != 2 || c.failed != 1 {
		t.Errorf("counts = %+v, want 2 listings, 1 refused", c)
	}
}

func TestSweep_InterruptStopsAndReports(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := source(swaps(9))
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
	src := &fakeSource{err: errors.New("connection refused"), done: map[string]bool{}}
	pg := &fakePurger{src: src}

	c := sweep(context.Background(), src, pg, options{batch: 3, workers: 1}, quiet)

	if c.err == nil || !strings.Contains(c.err.Error(), "connection refused") {
		t.Errorf("err = %v, want the list error", c.err)
	}
}
