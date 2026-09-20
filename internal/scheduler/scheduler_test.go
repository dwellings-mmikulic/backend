package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/property"
)

// --- RunOnce: one synchronous pass through all three loops ---

func TestRunOnce_DiscoversRendersAndEnrichesInOnePass(t *testing.T) {
	img := jpegServer(t)
	var found []property.Property
	for i := 0; i < 8; i++ {
		found = append(found, listing(fmt.Sprintf("ZP%d", i), img.URL+"/a.jpg", img.URL+"/b.jpg"))
	}
	cfg := baseConfig()
	cfg.DetailsPerCycle = 5
	h := newHarness(t, cfg, "33950")
	h.zillow.pages = map[string][][]property.Property{"33950": {found}}
	h.store.missingDetails = []string{"ZP0", "ZP1"}

	h.s.RunOnce(context.Background())

	if got := h.zips.markedZips(); got["33950"] != 8 {
		t.Errorf("marked = %v, want 33950 with 8 listings", got)
	}
	if got := len(h.store.ready()); got != 8 {
		t.Errorf("video ready = %d, want 8: listings discovered in a pass are rendered in it", got)
	}
	if got := len(h.store.videoFailed()); got != 0 {
		t.Errorf("video failed = %d, want 0", got)
	}
	if got := h.store.upsertCount(); got != 8 {
		t.Errorf("upserts = %d, want 8", got)
	}
	if got := len(h.queue.historyOf("complete")); got != 8 || len(h.queue.queued()) != 0 {
		t.Errorf("completed %d items, %v still queued; want 8 and none", got, h.queue.queued())
	}
	if peak := h.render.peak.Load(); peak > 4 {
		t.Errorf("render concurrency exceeded the limit: peak = %d", peak)
	}
	if set, _, _ := h.store.detailsState(); !reflect.DeepEqual(set, []string{"ZP0", "ZP1"}) {
		t.Errorf("details stored for %v, want [ZP0 ZP1]", set)
	}
	if got := h.s.inFlight.Load(); got != 0 {
		t.Errorf("in flight after RunOnce = %d, want 0: it is synchronous", got)
	}
}

func TestRunOnce_SkipsExisting(t *testing.T) {
	img := jpegServer(t)
	var found []property.Property
	for i := 0; i < 5; i++ {
		found = append(found, listing(fmt.Sprintf("ZP%d", i), img.URL+"/a.jpg"))
	}
	cfg := baseConfig()
	cfg.SkipExisting = true
	h := newHarness(t, cfg, "33950")
	h.zillow.pages = map[string][][]property.Property{"33950": {found}}
	// ZP0, ZP1, ZP2 already exist → should be skipped.
	h.store.existing = map[string]bool{"ZP0": true, "ZP1": true, "ZP2": true}

	h.s.RunOnce(context.Background())

	// Only ZP3, ZP4 are new → 2 upserts and 2 videos.
	if got := h.store.upsertCount(); got != 2 {
		t.Errorf("upserts = %d, want 2 (existing should be skipped)", got)
	}
	if got := h.store.ready(); len(got) != 2 {
		t.Errorf("video ready = %v, want 2", got)
	}
	completed := zpidsOf(h.queue.historyOf("complete"))
	sort.Strings(completed)
	if !reflect.DeepEqual(completed, []string{"ZP3", "ZP4"}) {
		t.Errorf("queue items worked on = %v, want only [ZP3 ZP4]: the rest never reach the queue", completed)
	}
}

func TestRunOnce_ExistingListingWithoutVideoIsReRendered(t *testing.T) {
	img := jpegServer(t)
	var found []property.Property
	for i := 0; i < 4; i++ {
		found = append(found, listing(fmt.Sprintf("ZP%d", i), img.URL+"/a.jpg"))
	}
	cfg := baseConfig()
	cfg.SkipExisting = true
	h := newHarness(t, cfg, "33950")
	h.zillow.pages = map[string][][]property.Property{"33950": {found}}
	// All four are stored already.
	h.store.existing = map[string]bool{"ZP0": true, "ZP1": true, "ZP2": true, "ZP3": true}
	// ZP1 and ZP2 have no ready video — only those two get revisited.
	h.store.needsVideo = map[string]bool{"ZP1": true, "ZP2": true}

	h.s.RunOnce(context.Background())

	ready := h.store.ready()
	sort.Strings(ready)
	if !reflect.DeepEqual(ready, []string{"ZP1", "ZP2"}) {
		t.Errorf("video ready = %v, want [ZP1 ZP2] re-rendered", ready)
	}
	if got := h.store.upsertCount(); got != 0 {
		t.Errorf("upserts = %d, want 0 — a video revisit must not rewrite the row", got)
	}
	// Only the rendered videos should be uploaded; no listing photos.
	for _, path := range h.bunny.paths() {
		if !strings.HasPrefix(path, "videos/") {
			t.Errorf("unexpected upload %q — a video revisit must not re-upload photos", path)
		}
	}
}

func TestRunOnce_SegmentsRenderedVideos(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig(), "33950")
	h.zillow.props = []property.Property{listing("ZP1", img.URL+"/a.jpg")}
	seg := &fakeSegmenter{}
	rec := h.enableHLS(seg)

	h.s.RunOnce(context.Background())

	if seg.calls.Load() != 1 {
		t.Errorf("segmenter calls = %d, want 1", seg.calls.Load())
	}
	if base := rec.base("ZP1"); !strings.HasPrefix(base, "https://cdn.example/hls/v1/ZP1/") {
		t.Errorf("recorded base url = %q", base)
	}
	if got := h.store.ready(); !reflect.DeepEqual(got, []string{"ZP1"}) {
		t.Errorf("video ready = %v, want [ZP1]", got)
	}
}

func TestRunOnce_IsBoundedByTheContext(t *testing.T) {
	cfg := baseConfig()
	cfg.DetailsPerCycle = 5
	h := newHarness(t, cfg, "33950")
	h.queue.put(t, listing("ZP1"), false)
	h.store.missingDetails = []string{"Z1"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.s.RunOnce(ctx)
	}()
	recv(t, done, "RunOnce to return under a cancelled context")

	if h.zips.claimCount() != 0 || len(h.queue.limits()) != 0 || len(h.store.claimLimits) != 0 {
		t.Error("a cancelled RunOnce must not claim anything")
	}
}

// --- Start / Stop ---

// parkSleeps replaces the injected sleep with one that reports the wait and
// then parks until the context ends, so every loop does one step and stops.
func (h *harness) parkSleeps() <-chan time.Duration {
	sleeps := make(chan time.Duration, 64)
	h.s.sleep = func(ctx context.Context, d time.Duration) {
		sleeps <- d
		<-ctx.Done()
	}
	return sleeps
}

// settledGoroutines polls until the goroutine count is back at or below
// baseline. Goroutines that called wg.Done are gone a moment later, not at
// once, hence the (tiny, bounded) polling.
func settledGoroutines(baseline int) (int, bool) {
	deadline := time.Now().Add(failAfter)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			return n, true
		}
		if time.Now().After(deadline) {
			return n, false
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStartStop_RunsAllLoopsAndLeavesNoGoroutineBehind(t *testing.T) {
	cfg := baseConfig()
	cfg.SkipExisting = true
	cfg.DetailsPerCycle = 0
	h := newHarness(t, cfg) // no ZIPs: discovery idles
	sleeps := h.parkSleeps()

	// One item that is being worked on when the shutdown comes, and that takes
	// a moment to give up: Stop must wait for it, not merely for the loops.
	blocked := make(chan struct{})
	finish := make(chan struct{})
	h.store.onExists = func(ctx context.Context) {
		close(blocked)
		<-ctx.Done()
		<-finish
	}
	h.queue.put(t, listing("ZP1", "https://photos.example/a.jpg"), false)

	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Start(ctx); err == nil {
		t.Error("a second Start must be refused: it would double every loop")
	}

	// Start is non-blocking: every loop gets to its first sleep on its own.
	recv(t, blocked, "the media loop to pick the item up")
	var got []time.Duration
	for i := 0; i < 4; i++ {
		got = append(got, recv(t, sleeps, "a loop to reach its sleep"))
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	// media: a probe is out → poll; discovery: no ZIP; status; details: off.
	want := []time.Duration{queuePoll, noZipWait, statusInterval, detailsDisabledWait}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("first sleeps = %v, want %v (media, discovery, status, details)", got, want)
	}

	cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		h.s.Stop()
	}()
	// The item goroutine is still winding down. Stop tracks it, so it cannot
	// have returned: a Stop that returns here reports a shutdown that is not
	// over, and the claims of everything in flight are left to their leases.
	select {
	case <-stopped:
		t.Fatal("Stop returned while a listing was still in flight: the item goroutines are not tracked")
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	recv(t, stopped, "Stop to return")

	// Stop waited for the item goroutine too: its release is on record.
	wantQueue := []queueTransition{{kind: "release", zpid: "ZP1", attempts: 1}}
	if got := h.queue.history(); !reflect.DeepEqual(got, wantQueue) {
		t.Errorf("queue transitions when Stop returned = %+v\nwant %+v", got, wantQueue)
	}
	if n, ok := settledGoroutines(baseline); !ok {
		t.Errorf("goroutines = %d after Stop, want <= %d: something leaked", n, baseline)
	}
	claims := len(h.queue.limits())
	h.s.Stop() // idempotent
	if got := len(h.queue.limits()); got != claims {
		t.Error("the media loop is still claiming after Stop")
	}
}

// main cancels the context before it calls Stop, but Stop must not depend on
// it: a Stop that waits for loops nobody told to stop never returns.
func TestStop_StopsTheLoopsByItself(t *testing.T) {
	cfg := baseConfig()
	cfg.DetailsPerCycle = 0
	h := newHarness(t, cfg)
	sleeps := h.parkSleeps()

	if err := h.s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		recv(t, sleeps, "a loop to reach its sleep")
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		h.s.Stop()
	}()
	recv(t, stopped, "Stop to return without the caller cancelling anything")
}

func TestStop_BeforeStartReturnsAtOnce(t *testing.T) {
	h := newHarness(t, baseConfig())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		h.s.Stop()
	}()
	recv(t, stopped, "Stop to return")
}

// Stop can only cancel a context Start has already made. Loops launched after
// Stop returned would therefore never be ended by it, and would go on claiming
// (and a Stop that lost the race could sit in wg.Wait on them). main is not
// exposed to this — it always Starts, waits for the signal, then Stops — but
// the lifecycle must hold for any caller.
func TestStart_AfterStopIsRefusedAndLaunchesNothing(t *testing.T) {
	cfg := baseConfig()
	cfg.DetailsPerCycle = 0
	h := newHarness(t, cfg, "11111")
	h.queue.put(t, listing("ZP1", "https://photos.example/a.jpg"), false)

	h.s.Stop()
	if err := h.s.Start(context.Background()); err == nil {
		t.Fatal("Start after Stop must be refused: Stop cannot end loops it never made")
	}

	// Give anything that was started a chance to reach the database.
	time.Sleep(20 * time.Millisecond)
	if n := len(h.queue.limits()); n != 0 {
		t.Errorf("the media loop claimed %d time(s) after Stop", n)
	}
	if n := h.zips.claimCount(); n != 0 {
		t.Errorf("discovery claimed %d ZIP(s) after Stop", n)
	}
	h.s.Stop() // still idempotent, and still returns
}

// Spec 3.1: "The work done under a claim runs under a context deadline shorter
// than the lease." That is what lets a live worker never work on an expired
// claim, and what makes renewal unnecessary. The margin — lease minus deadline
// — is what the bookkeeping recorded after the work runs in, so it has to be
// longer than the bookkeeping timeout.
func TestClaims_EveryLeaseOutlastsItsWorkDeadlineAndTheBookkeeping(t *testing.T) {
	for _, c := range []struct {
		what            string
		deadline, lease time.Duration
	}{
		{"zip", zipDeadline, zipLease},
		{"listing", listingDeadline, listingLease},
		{"details", detailsDeadline, detailsLease},
	} {
		if c.lease <= c.deadline+bookkeepingTimeout {
			t.Errorf("%s: lease %s leaves no room for %s of work plus %s of bookkeeping: a worker could still be holding the claim after it expired",
				c.what, c.lease, c.deadline, bookkeepingTimeout)
		}
	}
}

// And the constants have to be the ones actually used: the lease every claim
// is taken with, and a deadline on the context the paid or expensive work runs
// under. Without the deadline a stuck ffmpeg or a hung HTTP call would keep
// working on a claim the fleet has already given to somebody else.
func TestClaims_WorkRunsUnderTheDeadlineAndTheClaimTakesTheLease(t *testing.T) {
	t.Run("zip", func(t *testing.T) {
		h := newHarness(t, leanConfig(10, 0), "11111")
		start := time.Now()

		h.s.discoverStep(context.Background())

		if got := h.zips.leases(); !reflect.DeepEqual(got, []time.Duration{zipLease}) {
			t.Errorf("zip claim leases = %v, want [%s]", got, zipLease)
		}
		dl, ok := h.zillow.workDeadline("search")
		assertWorkDeadline(t, "the ZIP search", dl, ok, start, zipDeadline)
	})

	t.Run("listing", func(t *testing.T) {
		img := jpegServer(t)
		h := newHarness(t, serialConfig())
		start := time.Now()
		var renderDL time.Time
		var renderOK bool
		h.render.setHook(func(ctx context.Context, _ *property.Property) error {
			renderDL, renderOK = ctx.Deadline()
			return nil
		})
		h.queue.put(t, listing("ZP1", img.URL+"/a.jpg"), false)

		h.s.drainQueue(context.Background())

		if got := h.queue.leases(); len(got) == 0 || got[0] != listingLease {
			t.Errorf("listing claim leases = %v, want them taken with %s", got, listingLease)
		}
		assertWorkDeadline(t, "the listing pipeline", renderDL, renderOK, start, listingDeadline)
	})

	t.Run("details", func(t *testing.T) {
		h := detailsHarness(t, 10, "Z1")
		start := time.Now()

		h.s.detailsStep(context.Background())

		if got := h.store.detailsLeases(); !reflect.DeepEqual(got, []time.Duration{detailsLease}) {
			t.Errorf("details claim leases = %v, want [%s]", got, detailsLease)
		}
		dl, ok := h.zillow.workDeadline("details")
		assertWorkDeadline(t, "the details batch", dl, ok, start, detailsDeadline)
	})
}

// assertWorkDeadline checks that the work ran under a deadline, and that the
// deadline is the intended one rather than, say, the lease itself.
func assertWorkDeadline(t *testing.T, what string, dl time.Time, ok bool, start time.Time, want time.Duration) {
	t.Helper()
	if !ok {
		t.Fatalf("%s ran without a deadline: it could still be working after its claim expired", what)
	}
	// start is taken just before the step, so the deadline lands a hair later
	// than now+want. The slack is far smaller than the gap between any
	// deadline and its lease, which is what this has to tell apart.
	const slack = 5 * time.Second
	if got := dl.Sub(start); got < want-slack || got > want+slack {
		t.Errorf("%s deadline = %s after the step began, want about %s", what, got, want)
	}
}

func TestLoop_StepsAgainAtOnceOnZeroAndSleepsOtherwise(t *testing.T) {
	h := newHarness(t, baseConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var slept []time.Duration
	h.s.sleep = func(_ context.Context, d time.Duration) { slept = append(slept, d) }

	waits := []time.Duration{0, 0, 3 * time.Second, 0, time.Minute}
	steps := 0
	h.s.loop(ctx, func(context.Context) time.Duration {
		steps++
		if steps == len(waits) {
			cancel()
		}
		return waits[steps-1]
	})

	if steps != len(waits) {
		t.Errorf("steps = %d, want %d: the loop must end with the context", steps, len(waits))
	}
	if want := []time.Duration{3 * time.Second, time.Minute}; !reflect.DeepEqual(slept, want) {
		t.Errorf("slept %v, want %v: a zero wait means go again now", slept, want)
	}
}

func TestSleepCtx_ReturnsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sleepCtx(ctx, time.Hour)
	}()
	recv(t, done, "sleepCtx to notice the cancelled context")

	sleepCtx(context.Background(), time.Nanosecond) // and when the time is up
}

func TestStatusLine(t *testing.T) {
	h := newHarness(t, baseConfig())
	h.queue.put(t, listing("ZP1"), false)
	h.queue.put(t, listing("ZP2"), false)
	h.s.completed.Add(7)
	h.s.failed.Add(2)
	h.s.inFlight.Add(3)

	h.s.logStatus(context.Background())

	recs := h.logs.find("worker status")
	if len(recs) != 1 || recs[0].level != slog.LevelInfo {
		t.Fatalf("want one Info status line, got %+v", recs)
	}
	want := map[string]any{
		"in_flight": int64(3), "completed": int64(7), "failed": int64(2),
		"breaker": "half-open", "queue_depth": int64(2),
	}
	if !reflect.DeepEqual(recs[0].attrs, want) {
		t.Errorf("status attrs = %v\nwant %v", recs[0].attrs, want)
	}
}

func TestStatusLoop_LogsOncePerInterval(t *testing.T) {
	h := newHarness(t, baseConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var slept []time.Duration
	h.s.sleep = func(_ context.Context, d time.Duration) {
		slept = append(slept, d)
		if len(slept) == 3 {
			cancel() // the third sleep is cut short by the shutdown
		}
	}

	h.s.statusLoop(ctx)

	if want := []time.Duration{statusInterval, statusInterval, statusInterval}; !reflect.DeepEqual(slept, want) {
		t.Errorf("slept %v, want %v", slept, want)
	}
	if got := len(h.logs.find("worker status")); got != 2 {
		t.Errorf("status lines = %d, want 2: one per full interval, none on the way out", got)
	}
}
