package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/workqueue"
)

var errRender = errors.New("ffmpeg exploded")

// failRendersOf makes the renderer fail for the given zpids.
func failRendersOf(h *harness, zpids ...string) {
	bad := map[string]bool{}
	for _, z := range zpids {
		bad[z] = true
	}
	h.render.setHook(func(_ context.Context, p *property.Property) error {
		if bad[p.ZPID] {
			return errRender
		}
		return nil
	})
}

// serialConfig processes one listing at a time, so queue order is call order.
func serialConfig() *config.Config {
	cfg := baseConfig()
	cfg.Concurrency.Listings = 1
	return cfg
}

func zpidsOf(trs []queueTransition) []string {
	var out []string
	for _, tr := range trs {
		out = append(out, tr.zpid)
	}
	return out
}

func TestMedia_SuccessCompletesTheItem(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, serialConfig())
	h.queue.put(t, listing("ZP1", img.URL+"/a.jpg"), false)

	h.s.drainQueue(context.Background())

	want := []queueTransition{{kind: "complete", zpid: "ZP1", attempts: 1}}
	if got := h.queue.history(); !reflect.DeepEqual(got, want) {
		t.Errorf("queue transitions = %+v\nwant %+v", got, want)
	}
	if got := h.queue.queued(); len(got) != 0 {
		t.Errorf("queue still holds %v: a completed item is deleted", got)
	}
	if got := h.store.ready(); !reflect.DeepEqual(got, []string{"ZP1"}) {
		t.Errorf("video ready = %v, want [ZP1]", got)
	}
	if h.s.completed.Load() != 1 || h.s.failed.Load() != 0 || h.s.inFlight.Load() != 0 {
		t.Errorf("counters completed=%d failed=%d in_flight=%d, want 1 0 0",
			h.s.completed.Load(), h.s.failed.Load(), h.s.inFlight.Load())
	}
	if got := h.s.breaker.stateName(); got != "closed" {
		t.Errorf("breaker = %s, want closed after a successful probe", got)
	}
}

func TestMedia_InfrastructureFailureBacksOffThenDies(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, serialConfig())
	h.closeBreaker()
	failRendersOf(h, "BAD")
	h.queue.put(t, listing("BAD", img.URL+"/a.jpg"), false)
	ctx := context.Background()

	h.s.drainQueue(ctx)
	h.s.drainQueue(ctx) // still backing off: nothing to claim
	h.clock.advance(listingRetryFirst)
	h.s.drainQueue(ctx)
	h.clock.advance(listingRetryLater)
	h.s.drainQueue(ctx)
	h.clock.advance(24 * time.Hour)
	h.s.drainQueue(ctx) // dead: never offered again

	fails := h.queue.historyOf("fail")
	if len(fails) != 3 {
		t.Fatalf("fail transitions = %+v, want 3 (then dead)", fails)
	}
	wantRetry := []time.Duration{5 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, f := range fails {
		if f.attempts != i+1 || f.retryAfter != wantRetry[i] || f.refund {
			t.Errorf("fail %d = %+v, want attempt %d, retry %s, no refund (the breaker was closed)",
				i, f, i+1, wantRetry[i])
		}
		if !strings.Contains(f.msg, "ffmpeg exploded") {
			t.Errorf("fail %d message = %q, want the cause kept for inspection", i, f.msg)
		}
	}
	if row, ok := h.queue.row("BAD"); !ok || row.attempts != workqueue.MaxAttempts {
		t.Errorf("row = %+v ok=%v, want it kept, dead, for inspection", row, ok)
	}
	if got := h.s.failed.Load(); got != 3 {
		t.Errorf("failed counter = %d, want 3", got)
	}
	if recs := h.logs.find("listing failed"); len(recs) != 3 || recs[0].level != slog.LevelError {
		t.Errorf("want an Error per failure, got %+v", recs)
	}
}

func TestMedia_ShutdownReleasesTheItem(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, serialConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.render.setHook(func(ctx context.Context, _ *property.Property) error {
		cancel() // SIGTERM while ffmpeg runs
		<-ctx.Done()
		return ctx.Err()
	})
	h.queue.put(t, listing("ZP1", img.URL+"/a.jpg"), false)

	h.s.drainQueue(ctx)

	want := []queueTransition{{kind: "release", zpid: "ZP1", attempts: 1, delay: 0}}
	if got := h.queue.history(); !reflect.DeepEqual(got, want) {
		t.Fatalf("queue transitions = %+v\nwant %+v", got, want)
	}
	if row, _ := h.queue.row("ZP1"); row.attempts != 0 {
		t.Errorf("attempts = %d, want 0: a release refunds the attempt", row.attempts)
	}
	if got := h.s.breaker.stateName(); got != "half-open" {
		t.Errorf("breaker = %s, want it untouched (half-open): a shutdown is no verdict", got)
	}
	if h.s.failed.Load() != 0 || h.s.completed.Load() != 0 {
		t.Error("a released item is neither completed nor failed")
	}
	if got := h.store.videoFailed(); len(got) != 0 {
		t.Errorf("video failed = %v, want none on shutdown", got)
	}
}

// The item is done although the context died under the last step: the
// completion is bookkeeping and must still be recorded.
func TestMedia_CompletionOutlivesACancelledContext(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, serialConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.store.afterReady = cancel
	h.queue.put(t, listing("ZP1", img.URL+"/a.jpg"), false)

	h.s.drainQueue(ctx)

	if got := zpidsOf(h.queue.historyOf("complete")); !reflect.DeepEqual(got, []string{"ZP1"}) {
		t.Errorf("completed = %v, want [ZP1]", got)
	}
}

func TestMedia_PanicInTheRendererFailsTheItemAndTheProcessSurvives(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, serialConfig())
	h.closeBreaker()
	h.render.setHook(func(_ context.Context, p *property.Property) error {
		if p.ZPID == "BOMB" {
			panic("nil map in the overlay code")
		}
		return nil
	})
	h.queue.put(t, listing("BOMB", img.URL+"/a.jpg"), false)
	h.queue.put(t, listing("ZP2", img.URL+"/a.jpg"), false)

	h.s.drainQueue(context.Background())

	fails := h.queue.historyOf("fail")
	if len(fails) != 1 || fails[0].zpid != "BOMB" || !strings.Contains(fails[0].msg, "nil map in the overlay code") {
		t.Errorf("fail transitions = %+v, want BOMB failed with the panic value", fails)
	}
	if got := zpidsOf(h.queue.historyOf("complete")); !reflect.DeepEqual(got, []string{"ZP2"}) {
		t.Errorf("completed = %v, want [ZP2]: the next item must still be processed", got)
	}
	recs := h.logs.find("panicked")
	if len(recs) != 1 || recs[0].level != slog.LevelError {
		t.Fatalf("want one Error about the panic, got %+v", recs)
	}
	if stack, _ := recs[0].attrs["stack"].(string); !strings.Contains(stack, "goroutine") {
		t.Errorf("the panic log should carry the stack, got %q", stack)
	}
}

// processListingSafely's recover only covers the item's own goroutine, but
// the pipeline starts goroutines of its own: the photo downloads run
// imaging.Normalize (the x/image webp/tiff/bmp decoders) and the photo uploads
// run the CDN client inside errgroup closures, and errgroup does not pass a
// panic on to Wait. So "an image the decoder chokes on" used to kill the whole
// worker, leaving every other listing in flight claimed for its 65 min lease
// with an attempt already spent. Run in a child process, because the failure
// mode is the process dying.
func TestMedia_PanicInAPhotoGoroutineFailsTheItemAndTheProcessSurvives(t *testing.T) {
	const childEnv = "SCHED_PHOTO_PANIC_CHILD"
	if side := os.Getenv(childEnv); side != "" {
		img := jpegServer(t)
		h := newHarness(t, serialConfig())
		h.closeBreaker()
		switch side {
		case "download":
			h.s.http = &http.Client{Transport: panicRoundTripper{}}
		case "upload":
			h.bunny.onUpload = func(string) { panic("bug in a photo goroutine") }
		default:
			t.Fatalf("unknown side %q", side)
		}
		h.queue.put(t, listing("ZP1", img.URL+"/a.jpg"), false)

		h.s.drainQueue(context.Background())

		fails := h.queue.historyOf("fail")
		if len(fails) != 1 || fails[0].zpid != "ZP1" {
			t.Fatalf("want the item failed once, got %+v", h.queue.history())
		}
		recs := h.logs.find("panicked, photo dropped")
		if len(recs) != 1 || recs[0].level != slog.LevelError {
			t.Fatalf("want one Error about the panicking photo, got %+v", recs)
		}
		if stack, _ := recs[0].attrs["stack"].(string); !strings.Contains(stack, "goroutine") {
			t.Errorf("the panic log should carry the stack, got %q", stack)
		}
		return
	}
	for _, side := range []string{"download", "upload"} {
		t.Run(side, func(t *testing.T) {
			cmd := exec.Command(os.Args[0],
				"-test.run=^TestMedia_PanicInAPhotoGoroutineFailsTheItemAndTheProcessSurvives$", "-test.count=1")
			cmd.Env = append(os.Environ(), childEnv+"="+side)
			if out, err := cmd.CombinedOutput(); err != nil {
				if len(out) > 1500 {
					out = out[:1500]
				}
				t.Fatalf("the whole process died of one listing's panic (%v):\n%s", err, out)
			}
		})
	}
}

// panicRoundTripper stands in for a photo the image decoder chokes on: the
// panic happens on the download goroutine, where no caller's recover reaches.
type panicRoundTripper struct{}

func (panicRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	panic("bug in a photo goroutine")
}

// hls.Upload runs the segment uploads in errgroup goroutines inside another
// package, so the recover has to travel with the uploader. A panicking segment
// upload is a failed segmentation: the VOD render still goes ready.
func TestMedia_PanicInASegmentUploadIsAnErrorNotACrash(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, serialConfig())
	h.closeBreaker()
	rec := h.enableHLS(&fakeSegmenter{})
	h.bunny.onUpload = func(p string) {
		if strings.HasPrefix(p, "hls/") {
			panic("bug in a segment upload")
		}
	}
	h.queue.put(t, listing("ZP1", img.URL+"/a.jpg"), false)

	h.s.drainQueue(context.Background())

	if got := zpidsOf(h.queue.historyOf("complete")); !reflect.DeepEqual(got, []string{"ZP1"}) {
		t.Fatalf("completed = %v, want [ZP1]: a segment that will not upload is not a failed render", got)
	}
	if got := h.store.ready(); !reflect.DeepEqual(got, []string{"ZP1"}) {
		t.Errorf("ready = %v, want [ZP1]", got)
	}
	if n := rec.count(); n != 0 {
		t.Errorf("recorded %d clips, want none", n)
	}
	if recs := h.logs.find("upload panicked"); len(recs) == 0 {
		t.Error("want the panicking segment upload logged")
	}
}

func TestMedia_UndecodablePayloadIsParkedUntilItDies(t *testing.T) {
	h := newHarness(t, serialConfig())
	h.queue.putRaw(t, "JUNK", []byte(`{"zpid": 12}`))

	h.s.drainQueue(context.Background())

	want := []queueTransition{{kind: "fail", zpid: "JUNK", attempts: 1, retryAfter: poisonRetry, refund: false}}
	got := h.queue.history()
	if len(got) != 1 {
		t.Fatalf("queue transitions = %+v, want one fail", got)
	}
	got[0].msg = ""
	if !reflect.DeepEqual(got, want) {
		t.Errorf("queue transitions = %+v\nwant %+v", got, want)
	}
	if recs := h.logs.find("undecodable"); len(recs) != 1 || recs[0].level != slog.LevelError {
		t.Errorf("want one Error about the payload, got %+v", recs)
	}
	// It says nothing about the box: the boot probe is still to be done.
	if n, _, _ := h.s.breaker.allow(4); n != 1 {
		t.Errorf("breaker allowance = %d, want 1: still half-open, and the probe slot is free again", n)
	}
}

// No photos at the source is terminal and not a failure. It is no proof that
// this box can download, render and upload either, so the breaker stays where
// it was.
func TestMedia_NoSourcePhotosCompletesWithoutTouchingTheBreaker(t *testing.T) {
	h := newHarness(t, serialConfig())
	h.queue.put(t, listing("BARE"), false)

	h.s.drainQueue(context.Background())

	if got := zpidsOf(h.queue.historyOf("complete")); !reflect.DeepEqual(got, []string{"BARE"}) {
		t.Errorf("completed = %v, want [BARE]", got)
	}
	if got := h.s.failed.Load(); got != 0 {
		t.Errorf("failed counter = %d, want 0", got)
	}
	if h.store.upsertCount() != 1 || len(h.store.videoFailed()) != 1 {
		t.Error("the listing should be stored with its video marked failed, as before")
	}
	if got := h.s.breaker.stateName(); got != "half-open" {
		t.Errorf("breaker = %s, want it untouched (half-open)", got)
	}
	if n, _, _ := h.s.breaker.allow(4); n != 1 {
		t.Errorf("breaker allowance = %d, want 1: the probe slot must be free again", n)
	}
}

func TestMedia_SkippedListingCompletesWithoutTouchingTheBreaker(t *testing.T) {
	cfg := serialConfig()
	cfg.SkipExisting = true
	h := newHarness(t, cfg)
	h.store.existing = map[string]bool{"OLD": true} // rendered since it was enqueued
	h.queue.put(t, listing("OLD", "https://photos.example/a.jpg"), false)

	h.s.drainQueue(context.Background())

	if got := zpidsOf(h.queue.historyOf("complete")); !reflect.DeepEqual(got, []string{"OLD"}) {
		t.Errorf("completed = %v, want [OLD]", got)
	}
	if got := h.s.breaker.stateName(); got != "half-open" {
		t.Errorf("breaker = %s, want it untouched: a skip proves only that the database answers", got)
	}
}

func TestMedia_PhotosThatCannotBeStoredFailTheItem(t *testing.T) {
	t.Run("none downloadable", func(t *testing.T) {
		blocked := statusServer(t, http.StatusForbidden)
		h := newHarness(t, serialConfig())
		h.closeBreaker()
		h.queue.put(t, listing("ZP1", blocked.URL+"/a.jpg"), false)

		h.s.drainQueue(context.Background())

		if got := zpidsOf(h.queue.historyOf("fail")); !reflect.DeepEqual(got, []string{"ZP1"}) {
			t.Errorf("failed = %v, want [ZP1]", got)
		}
		if h.store.upsertCount() != 0 {
			t.Error("nothing may be persisted")
		}
	})
	t.Run("none uploadable", func(t *testing.T) {
		img := jpegServer(t)
		h := newHarness(t, serialConfig())
		h.closeBreaker()
		h.bunny.failPrefix = "properties/"
		h.queue.put(t, listing("ZP1", img.URL+"/a.jpg"), false)

		h.s.drainQueue(context.Background())

		if got := zpidsOf(h.queue.historyOf("fail")); !reflect.DeepEqual(got, []string{"ZP1"}) {
			t.Errorf("failed = %v, want [ZP1]", got)
		}
		if h.store.upsertCount() != 0 {
			t.Error("no Upsert may happen: an empty gallery must never be persisted")
		}
	})
}

func TestMedia_ReducedModesStillCompleteItems(t *testing.T) {
	tests := []struct {
		name          string
		images, video bool
	}{
		{"images disabled", false, true},
		{"video disabled", true, false},
		{"both disabled", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img := jpegServer(t)
			cfg := serialConfig()
			cfg.ImagesEnabled, cfg.Video.Enabled = tt.images, tt.video
			h := newHarness(t, cfg)
			h.queue.put(t, listing("ZP1", img.URL+"/a.jpg"), false)
			h.queue.put(t, listing("ZP2", img.URL+"/a.jpg"), false)

			h.s.drainQueue(context.Background())

			if got := zpidsOf(h.queue.historyOf("complete")); !reflect.DeepEqual(got, []string{"ZP1", "ZP2"}) {
				t.Errorf("completed = %v, want [ZP1 ZP2]", got)
			}
			if h.store.upsertCount() != 2 {
				t.Errorf("upserts = %d, want 2", h.store.upsertCount())
			}
			// Storing the listing is all the work there is in this mode, so it
			// has to count as the proof that closes the breaker.
			if got := h.s.breaker.stateName(); got != "closed" {
				t.Errorf("breaker = %s, want closed", got)
			}
		})
	}
}

func TestMedia_RevisitPayload(t *testing.T) {
	t.Run("takes the video-only path even with SKIP_EXISTING off", func(t *testing.T) {
		img := jpegServer(t)
		h := newHarness(t, serialConfig())
		h.store.needsVideo = map[string]bool{"ZP1": true} // stored, still without a video
		h.queue.put(t, listing("ZP1", img.URL+"/a.jpg"), true)

		h.s.drainQueue(context.Background())

		if got := zpidsOf(h.queue.historyOf("complete")); !reflect.DeepEqual(got, []string{"ZP1"}) {
			t.Errorf("completed = %v, want [ZP1]", got)
		}
		if h.store.upsertCount() != 0 || len(h.bunny.pathsWithPrefix("properties/")) != 0 {
			t.Error("a revisit must neither upsert nor upload photos")
		}
		if got := h.store.ready(); !reflect.DeepEqual(got, []string{"ZP1"}) {
			t.Errorf("video ready = %v, want [ZP1]", got)
		}
	})

	// Completing it would lose the backfill request; failing it would burn
	// its attempts on boxes that can never do it.
	t.Run("on a worker without video it is released for another box", func(t *testing.T) {
		cfg := serialConfig()
		cfg.Video.Enabled = false
		h := newHarness(t, cfg)
		h.queue.put(t, listing("ZP1", "https://photos.example/a.jpg"), true)

		h.s.drainQueue(context.Background())

		want := []queueTransition{{kind: "release", zpid: "ZP1", attempts: 1, delay: revisitNoVideoDelay}}
		if got := h.queue.history(); !reflect.DeepEqual(got, want) {
			t.Errorf("queue transitions = %+v\nwant %+v", got, want)
		}
		if got := h.s.breaker.stateName(); got != "half-open" {
			t.Errorf("breaker = %s, want it untouched", got)
		}
		if n, _, _ := h.s.breaker.allow(4); n != 1 {
			t.Errorf("breaker allowance = %d, want 1: the probe slot must be free again", n)
		}
		if h.s.failed.Load() != 0 {
			t.Error("not a failure")
		}
	})
}

// Spec 3.1, for the queue's Fail. TestMedia_ShutdownReleasesTheItem covers the
// shutdown a listing notices; this is the one it does not — SIGTERM landing
// between the verdict and the write. Without the detached context the Fail is
// lost and the item stays claimed by a process that is gone, hidden for the
// whole 65 min lease with an attempt already spent.
func TestMedia_FailedListingIsRecordedOnACancelledContext(t *testing.T) {
	h := newHarness(t, serialConfig())
	h.queue.put(t, listing("ZP1"), false)
	items, err := h.queue.Claim(context.Background(), testOwner, 1, listingLease)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim = (%+v, %v), want one item", items, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h.s.failItem(ctx, items[0], errRender, listingRetryFirst, false)

	fails := h.queue.historyOf("fail")
	if len(fails) != 1 || fails[0].zpid != "ZP1" || fails[0].retryAfter != listingRetryFirst {
		t.Fatalf("fail transitions = %+v, want ZP1 failed with a %s backoff", fails, listingRetryFirst)
	}
	if !strings.Contains(fails[0].msg, errRender.Error()) {
		t.Errorf("fail message = %q, want the cause kept", fails[0].msg)
	}
	if recs := h.logs.find("fail listing failed"); len(recs) != 0 {
		t.Errorf("the transition was lost: %+v", recs)
	}
}

func TestMedia_LostLeaseOnCompleteIsAWarning(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, serialConfig())
	h.render.setHook(func(context.Context, *property.Property) error {
		h.queue.steal("ZP1") // the lease ran out and another box took the item
		return nil
	})
	h.queue.put(t, listing("ZP1", img.URL+"/a.jpg"), false)

	h.s.drainQueue(context.Background())

	if recs := h.logs.find("lease lost"); len(recs) != 1 || recs[0].level != slog.LevelWarn {
		t.Errorf("want one Warn about the lost lease, got %+v", recs)
	}
	if _, ok := h.queue.row("ZP1"); !ok {
		t.Error("the row now belongs to somebody else and must be left alone")
	}
}

// --- breaker + dispatcher ---

func TestMedia_BootProbeThenFullConcurrency(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig()) // 4 slots
	for i := 0; i < 8; i++ {
		h.queue.put(t, listing(fmt.Sprintf("ZP%d", i), img.URL+"/a.jpg", img.URL+"/b.jpg"), false)
	}

	// Renders block until released, so what overlaps is decided here and not
	// by the scheduler of the day.
	entered := make(chan string, 8)
	release := make(chan struct{})
	h.render.setHook(func(ctx context.Context, p *property.Property) error {
		entered <- p.ZPID
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.s.drainQueue(context.Background())
	}()

	// The boot probe: exactly one item, alone.
	if got := recv(t, entered, "the probe's render"); got != "ZP0" {
		t.Errorf("probe = %s, want ZP0 (oldest first)", got)
	}
	if got := h.queue.limits(); !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("claim limits so far = %v, want [1]: a booting worker proves itself with one item", got)
	}
	send(t, release, struct{}{}, "the probe to take its release")

	// Closed now: all four slots fill, and with none released they overlap.
	for i := 0; i < 4; i++ {
		recv(t, entered, "a render to start")
	}
	if got := h.render.cur.Load(); got != 4 {
		t.Errorf("renders in flight = %d, want 4", got)
	}
	close(release)
	recv(t, done, "the drain to finish")

	if got := len(h.store.ready()); got != 8 {
		t.Errorf("video ready = %d, want 8", got)
	}
	if got := h.store.upsertCount(); got != 8 {
		t.Errorf("upserts = %d, want 8", got)
	}
	if peak := h.render.peak.Load(); peak != 4 {
		t.Errorf("peak render concurrency = %d, want exactly the limit of 4", peak)
	}
	limits := h.queue.limits()
	if len(limits) < 2 || limits[1] != 4 {
		t.Errorf("claim limits = %v, want the second claim to ask for all 4 slots", limits)
	}
	for _, l := range limits {
		if l < 1 || l > 4 {
			t.Errorf("claim limits = %v: never more than the slots, never zero", limits)
		}
	}
}

func TestMedia_FailedBootProbeRefundsTheAttemptAndOpensTheBreaker(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, serialConfig())
	failRendersOf(h, "ZP1")
	h.queue.put(t, listing("ZP1", img.URL+"/a.jpg"), false)
	h.queue.put(t, listing("ZP2", img.URL+"/a.jpg"), false)

	h.s.drainQueue(context.Background())

	// A crash-looping or broken box fails its probe on every boot. Without
	// the refund it would dead-letter three healthy listings per three boots.
	want := []queueTransition{{kind: "fail", zpid: "ZP1", attempts: 1, retryAfter: listingRetryFirst, refund: true}}
	got := h.queue.history()
	for i := range got {
		got[i].msg = ""
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("queue transitions = %+v\nwant %+v", got, want)
	}
	if row, _ := h.queue.row("ZP1"); row.attempts != 0 {
		t.Errorf("attempts = %d, want 0 (refunded)", row.attempts)
	}
	if h.s.Healthy() {
		t.Error("Healthy() = true, want false while the breaker is open")
	}
	if got := h.queue.limits(); !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("claim limits = %v, want [1]: an open breaker claims nothing", got)
	}
}

func TestMedia_FiveConsecutiveFailuresOpenTheBreaker(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, serialConfig())
	bad := []string{"B1", "B2", "B3", "B4", "B5"}
	failRendersOf(h, bad...)
	h.queue.put(t, listing("OK0", img.URL+"/a.jpg"), false) // the boot probe
	for _, z := range bad {
		h.queue.put(t, listing(z, img.URL+"/a.jpg"), false)
	}
	h.queue.put(t, listing("LATER1", img.URL+"/a.jpg"), false)
	h.queue.put(t, listing("LATER2", img.URL+"/a.jpg"), false)
	ctx := context.Background()

	h.s.drainQueue(ctx)

	fails := h.queue.historyOf("fail")
	if got := zpidsOf(fails); !reflect.DeepEqual(got, bad) {
		t.Fatalf("failed = %v, want %v", got, bad)
	}
	for _, f := range fails {
		if f.refund {
			t.Errorf("%s was refunded, but the breaker was closed when it failed", f.zpid)
		}
	}
	if h.s.Healthy() {
		t.Error("Healthy() = true, want false: five failures in a row open the breaker")
	}
	for _, z := range []string{"LATER1", "LATER2"} {
		if row, _ := h.queue.row(z); row.attempts != 0 {
			t.Errorf("%s was claimed although the breaker is open", z)
		}
	}
	claims := len(h.queue.limits())

	// While it is open nothing is claimed at all.
	h.s.drainQueue(ctx)
	if got := len(h.queue.limits()); got != claims {
		t.Errorf("claimed %d more times while open", got-claims)
	}

	// After the pause: one probe, and its success restores full service.
	h.clock.advance(breakerBasePause)
	if !h.s.Healthy() {
		t.Error("Healthy() = false after the pause, want true (half-open)")
	}
	h.s.drainQueue(ctx)
	if got := h.queue.limits()[claims]; got != 1 {
		t.Errorf("first claim after the pause asked for %d, want exactly 1", got)
	}
	if got := zpidsOf(h.queue.historyOf("complete")); !reflect.DeepEqual(got, []string{"OK0", "LATER1", "LATER2"}) {
		t.Errorf("completed = %v, want [OK0 LATER1 LATER2]", got)
	}
	if got := h.s.breaker.stateName(); got != "closed" {
		t.Errorf("breaker = %s, want closed", got)
	}
}

func TestDispatch_OpenBreakerWaitsOutThePauseWithoutClaiming(t *testing.T) {
	h := newHarness(t, baseConfig())
	h.queue.put(t, listing("ZP1"), false)
	_, probe, _ := h.s.breaker.allow(4)
	h.s.breaker.failure(probe) // open for 1m
	h.clock.advance(20 * time.Second)
	slots := make(chan struct{}, 4)

	wait := h.s.dispatch(context.Background(), slots, func(workqueue.Item, probeID) { t.Error("item dispatched") })

	if wait != 40*time.Second {
		t.Errorf("wait = %s, want the 40s left of the pause", wait)
	}
	if len(h.queue.limits()) != 0 {
		t.Error("Claim called while the breaker is open")
	}
	if len(slots) != 0 {
		t.Errorf("%d slots still held", len(slots))
	}
}

// A straggler is an item claimed while the breaker was still closed that is
// still running after it opened and half-opened again. Its outcome is not the
// probe's: reporting inconclusive (a skip, a photo-less listing, a shutdown
// release, an undecodable payload) must not free the probe's place, or the
// next dispatch hands out a second probe next to the one still rendering —
// against spec 3.4's "half-open: exactly one item is claimed".
func TestDispatch_StragglerOutcomeDoesNotFreeTheHalfOpenProbe(t *testing.T) {
	img := jpegServer(t)
	cfg := baseConfig() // 4 slots
	cfg.SkipExisting = true
	h := newHarness(t, cfg)
	h.store.existing = map[string]bool{"S": true} // stored, video ready: skipped
	h.closeBreaker()

	var existsCalls atomic.Int64
	sEntered := make(chan struct{})
	releaseS := make(chan struct{})
	h.store.onExists = func(ctx context.Context) {
		if existsCalls.Add(1) == 1 { // the straggler only
			close(sEntered)
			<-releaseS
		}
	}
	entered := make(chan string, 4)
	h.render.setHook(func(ctx context.Context, p *property.Property) error {
		entered <- p.ZPID
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	slots := make(chan struct{}, 4)
	run := func(it workqueue.Item, probe probeID) { h.s.launch(ctx, &wg, slots, it, probe, nil) }

	// 1. The straggler is claimed while closed and hangs in its DB lookup.
	h.queue.put(t, listing("S", img.URL+"/a.jpg"), false)
	h.s.dispatch(ctx, slots, run)
	recv(t, sEntered, "the straggler to start")

	// 2. Five failures elsewhere open the breaker; the pause elapses.
	for i := 0; i < breakerThreshold; i++ {
		h.s.breaker.failure(0)
	}
	h.clock.advance(breakerBasePause)

	// 3. Half-open: one probe.
	h.queue.put(t, listing("P1", img.URL+"/a.jpg"), false)
	h.queue.put(t, listing("P2", img.URL+"/a.jpg"), false)
	h.s.dispatch(ctx, slots, run)
	if got := recv(t, entered, "the probe's render"); got != "P1" {
		t.Fatalf("probe = %s, want P1", got)
	}

	// 4. The straggler finishes as a skip -> inconclusive, under its own id.
	close(releaseS)
	deadline := time.Now().Add(failAfter)
	for len(h.queue.historyOf("complete")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("straggler never completed")
		}
		time.Sleep(time.Millisecond)
	}

	// 5. Still half-open, P1 still out: the dispatcher must not claim again.
	if st := h.s.breaker.stateName(); st != "half-open" {
		t.Fatalf("breaker = %s, want half-open", st)
	}
	h.s.dispatch(ctx, slots, run)
	select {
	case z := <-entered:
		t.Errorf("second half-open probe %s dispatched while the first (P1) is still in flight; claim limits %v", z, h.queue.limits())
	case <-time.After(50 * time.Millisecond):
	}
}

// The same rule for a straggler that fails: it is refunded like any other
// failure on a suspect box, but it is not the probe's verdict, so it neither
// re-opens the breaker nor frees the probe's place.
func TestBreaker_StragglerFailureWhileHalfOpenIsNotTheProbesVerdict(t *testing.T) {
	clock := newFakeClock()
	b := newTestBreaker(clock)
	b.failure(takeProbe(t, b, 4)) // the boot probe fails: open for 1m
	clock.advance(breakerBasePause)
	probe := takeProbe(t, b, 4) // half-open again, one probe out

	if refund := b.failure(0); !refund {
		t.Error("a straggler failing on a suspect box must get its attempt back")
	}
	if got := b.stateName(); got != "half-open" {
		t.Errorf("state = %q, want half-open: a straggler must not re-open the breaker", got)
	}
	if n, p, _ := b.allow(4); n != 0 || p != 0 {
		t.Errorf("allow(4) = (%d, %d), want no second probe while the first is out", n, p)
	}
	// Only the probe's own verdict counts, and it still can be given.
	if refund := b.failure(probe); !refund {
		t.Error("the probe's own failure must refund the attempt")
	}
	if b.healthy() {
		t.Error("the probe's failure must re-open the breaker")
	}
	if _, _, wait := b.allow(4); wait != 2*breakerBasePause {
		t.Errorf("pause = %s, want %s: only the probe's failure counts as an opening", wait, 2*breakerBasePause)
	}
}

func TestDispatch_NothingClaimedHandsEverythingBack(t *testing.T) {
	tests := []struct {
		name     string
		claimErr error
	}{
		{"empty queue", nil},
		{"claim error", errors.New("db down")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, baseConfig())
			h.queue.claimErr = tt.claimErr
			slots := make(chan struct{}, 4)

			wait := h.s.dispatch(context.Background(), slots, func(workqueue.Item, probeID) { t.Error("item dispatched") })

			if wait != queuePoll {
				t.Errorf("wait = %s, want %s", wait, queuePoll)
			}
			if len(slots) != 0 {
				t.Errorf("%d slots still held, want all handed back", len(slots))
			}
			// The probe that found nothing to work on must not block the next.
			if n, _, _ := h.s.breaker.allow(4); n != 1 {
				t.Errorf("breaker allowance = %d, want 1", n)
			}
			if tt.claimErr != nil {
				if recs := h.logs.find("claim listings failed"); len(recs) != 1 {
					t.Errorf("want one Error about the failed claim, got %+v", recs)
				}
			}
		})
	}
}

func TestDispatch_ClaimsOnlyTheFreeSlots(t *testing.T) {
	h := newHarness(t, baseConfig())
	h.closeBreaker()
	for i := 0; i < 6; i++ {
		h.queue.put(t, listing(fmt.Sprintf("ZP%d", i)), false)
	}
	slots := make(chan struct{}, 4)
	slots <- struct{}{} // one listing is still rendering
	var got []string

	wait := h.s.dispatch(context.Background(), slots, func(it workqueue.Item, _ probeID) { got = append(got, it.ZPID) })

	if wait != 0 {
		t.Errorf("wait = %s, want 0 after a claim", wait)
	}
	if want := []int{3}; !reflect.DeepEqual(h.queue.limits(), want) {
		t.Errorf("claim limits = %v, want %v", h.queue.limits(), want)
	}
	if !reflect.DeepEqual(got, []string{"ZP0", "ZP1", "ZP2"}) {
		t.Errorf("dispatched %v, want the three oldest", got)
	}
	if len(slots) != 4 {
		t.Errorf("slots held = %d, want 4 (1 + 3 dispatched)", len(slots))
	}
}

func TestDispatch_FewerItemsThanSlotsReturnsTheRest(t *testing.T) {
	h := newHarness(t, baseConfig())
	h.closeBreaker()
	h.queue.put(t, listing("ZP0"), false)
	slots := make(chan struct{}, 4)

	h.s.dispatch(context.Background(), slots, func(workqueue.Item, probeID) {})

	if len(slots) != 1 {
		t.Errorf("slots held = %d, want 1: unused slots go back", len(slots))
	}
}

func TestDispatch_CancelledWhileWaitingForASlot(t *testing.T) {
	h := newHarness(t, baseConfig())
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h.s.dispatch(ctx, slots, func(workqueue.Item, probeID) { t.Error("item dispatched") })

	if len(h.queue.limits()) != 0 {
		t.Error("Claim called after the context was cancelled")
	}
}

func TestErrorText_IsAlwaysStorable(t *testing.T) {
	long := strings.Repeat("é", maxErrorLen) // 2 bytes each: the cut lands inside a rune
	tests := []struct {
		name string
		in   string
		want func(string) bool
	}{
		{"plain", "render video: exit status 1", func(s string) bool { return s == "render video: exit status 1" }},
		{"nul bytes", "bad \x00 byte", func(s string) bool { return s == "bad  byte" }},
		{"invalid utf-8", "bad \xff byte", func(s string) bool { return s == "bad ? byte" }},
		{"too long", long, func(s string) bool {
			return len(s) <= maxErrorLen+len("…") && strings.HasSuffix(s, "…") && strings.ToValidUTF8(s, "") == s
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorText(errors.New(tt.in)); !tt.want(got) {
				t.Errorf("errorText = %q", got)
			}
		})
	}
}
