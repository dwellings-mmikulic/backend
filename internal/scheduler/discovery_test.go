package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/budget"
	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/workqueue"
	"github.com/dwellingtw/backend/internal/zillow"
)

// discoverUntilWait steps discovery the way RunOnce and the loop do: again at
// once while a step returns 0. It returns the first wait.
func (h *harness) discoverUntilWait(t *testing.T, ctx context.Context) time.Duration {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if wait := h.s.discoverStep(ctx); wait > 0 {
			return wait
		}
		if ctx.Err() != nil {
			return 0
		}
	}
	t.Fatal("discovery never came to rest")
	return 0
}

func props(zpids ...string) []property.Property {
	out := make([]property.Property, 0, len(zpids))
	for _, z := range zpids {
		out = append(out, listing(z))
	}
	return out
}

func TestDiscover_RotatesUntilTheSearchBudgetIsSpent(t *testing.T) {
	// No details reserve: 3 requests, and an empty ZIP costs one.
	h := newHarness(t, leanConfig(3, 0), "11111", "22222", "33333", "44444", "55555")

	wait := h.discoverUntilWait(t, context.Background())

	marked := h.zips.markedZips()
	if len(marked) != 3 {
		t.Fatalf("marked %d zips, want 3 (budget): %v", len(marked), marked)
	}
	for _, z := range []string{"11111", "22222", "33333"} {
		if _, ok := marked[z]; !ok {
			t.Errorf("zip %s not marked; rotation order should win", z)
		}
	}
	if got := h.zips.claimCount(); got != 3 {
		t.Errorf("claimed %d zips, want 3: a spent budget must stop discovery BEFORE it claims", got)
	}
	if wait != maxWindowSleep {
		t.Errorf("wait = %s, want %s (next window, capped)", wait, maxWindowSleep)
	}
	if got := h.ledger.get(h.windowStart(), budget.KindSearch); got != 3 {
		t.Errorf("ledger search spent = %d, want 3", got)
	}
}

func TestDiscover_DetailsReserveShrinksTheSearchBudget(t *testing.T) {
	h := newHarness(t, leanConfig(3, 2), "11111", "22222") // search budget = 3 - 2 = 1

	h.discoverUntilWait(t, context.Background())

	if got := len(h.zips.markedZips()); got != 1 {
		t.Fatalf("marked %d zips, want 1 (search budget 1)", got)
	}
	if got := h.ledger.limitsSeen[budget.KindSearch]; len(got) == 0 || got[0] != 1 {
		t.Errorf("limits passed to the ledger = %v, want 1", got)
	}
}

func TestDiscover_NoSearchBudgetAtAllNeverClaims(t *testing.T) {
	h := newHarness(t, leanConfig(5, 5), "11111")

	if wait := h.s.discoverStep(context.Background()); wait != maxWindowSleep {
		t.Errorf("wait = %s, want %s", wait, maxWindowSleep)
	}
	if got := h.zips.claimCount(); got != 0 {
		t.Errorf("claimed %d zips with a search budget of 0", got)
	}
}

func TestDiscover_SearchesTheClaimedZipUnbounded(t *testing.T) {
	cfg := leanConfig(10, 0)
	cfg.Search = config.SearchCriteria{Location: "ignored", HomeStatus: "FOR_SALE", MaxPages: 7, MaxResults: 50}
	h := newHarness(t, cfg, "33950")

	if wait := h.s.discoverStep(context.Background()); wait != 0 {
		t.Fatalf("wait = %s, want 0 after a complete search", wait)
	}
	calls := h.zillow.searchCalls()
	if len(calls) != 1 {
		t.Fatalf("searches = %v, want 1", calls)
	}
	// MaxPages is 0 on purpose: the permit, not a page cap derived from an
	// in-process budget, is what stops a search now.
	if want := (searchCall{location: "33950", startPage: 0, maxPages: 0}); calls[0] != want {
		t.Errorf("search = %+v, want %+v", calls[0], want)
	}
}

// A multi-page search is charged per request: a ZIP with one page of listings
// costs two (the page and the empty one that ends the search).
func TestDiscover_MultiPageSearchIsChargedPerRequest(t *testing.T) {
	h := newHarness(t, leanConfig(3, 0), "11111", "22222", "33333")
	h.zillow.pages = map[string][][]property.Property{"11111": {props("a", "b")}}

	h.discoverUntilWait(t, context.Background())

	marked := h.zips.markedZips()
	if _, ok := marked["11111"]; !ok {
		t.Error("zip 11111 (2 requests) should be marked")
	}
	if _, ok := marked["22222"]; !ok {
		t.Error("zip 22222 (1 request) should be marked")
	}
	if got := h.zillow.searchedLocations(); !reflect.DeepEqual(got, []string{"11111", "22222"}) {
		t.Errorf("searched %v; 33333 must never be queried — the budget went on 11111+22222 (2+1=3)", got)
	}
}

// One ZIP spends the whole window: the rest of the rotation is left alone.
func TestDiscover_OneZipCanSpendTheWholeBudget(t *testing.T) {
	h := newHarness(t, leanConfig(3, 0), "11111", "22222", "33333")
	h.zillow.pages = map[string][][]property.Property{"11111": {props("a"), props("b")}}

	h.discoverUntilWait(t, context.Background())

	if got := h.zillow.searchedLocations(); !reflect.DeepEqual(got, []string{"11111"}) {
		t.Fatalf("searched = %v, want only [11111]", got)
	}
	if got := h.zips.markedZips(); len(got) != 1 || got["11111"] != 2 {
		t.Errorf("marked = %v, want 11111 with 2 listings", got)
	}
	if got := h.zips.claimCount(); got != 1 {
		t.Errorf("claimed %d zips, want 1", got)
	}
}

func TestDiscover_MarksTheListingCountEvenWhenEveryListingIsSkipped(t *testing.T) {
	cfg := leanConfig(10, 0)
	cfg.SkipExisting = true
	h := newHarness(t, cfg, "33950")
	h.zillow.pages = map[string][][]property.Property{"33950": {props("a", "b")}}
	h.store.existing = map[string]bool{"a": true, "b": true}

	h.discoverUntilWait(t, context.Background())

	if got := h.zips.markedZips()["33950"]; got != 2 {
		t.Errorf("last_listing_count = %d, want 2", got)
	}
	if got := h.queue.queued(); len(got) != 0 {
		t.Errorf("queued %v, want nothing: both listings are stored", got)
	}
}

func TestDiscover_ClosedQuotaGateNeitherClaimsNorSearches(t *testing.T) {
	tests := []struct {
		name  string
		usage *zillow.Usage
	}{
		{"quota exceeded", &zillow.Usage{Status: "exceeded"}},
		{"remaining below the budget", usageWith("ok", 100)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, leanConfig(150, 0), "33950")
			h.zillow.setUsage(tt.usage, nil)

			wait := h.s.discoverStep(context.Background())

			if wait != maxWindowSleep {
				t.Errorf("wait = %s, want %s", wait, maxWindowSleep)
			}
			if got := h.zips.claimCount(); got != 0 {
				t.Errorf("claimed %d zips behind a closed gate", got)
			}
			if got := h.zillow.searchedLocations(); len(got) != 0 {
				t.Errorf("searched %v behind a closed gate", got)
			}
		})
	}
}

func TestDiscover_ProceedsWhenTheUsageCheckFails(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0), "33950")
	h.zillow.setUsage(nil, errors.New("usage endpoint down"))

	h.discoverUntilWait(t, context.Background())

	if _, ok := h.zips.markedZips()["33950"]; !ok {
		t.Error("discovery should proceed (fail-open) when the usage check errors")
	}
}

func TestDiscover_NothingClaimable(t *testing.T) {
	t.Run("empty rotation", func(t *testing.T) {
		h := newHarness(t, leanConfig(10, 0))
		if wait := h.s.discoverStep(context.Background()); wait != noZipWait {
			t.Errorf("wait = %s, want %s", wait, noZipWait)
		}
	})
	t.Run("claim error", func(t *testing.T) {
		h := newHarness(t, leanConfig(10, 0), "33950")
		h.zips.claimErr = errors.New("db down")
		if wait := h.s.discoverStep(context.Background()); wait != noZipWait {
			t.Errorf("wait = %s, want %s", wait, noZipWait)
		}
		if recs := h.logs.find("claim zip failed"); len(recs) != 1 || recs[0].level != slog.LevelError {
			t.Errorf("want one Error about the failed claim, got %+v", recs)
		}
		if got := h.zillow.searchedLocations(); len(got) != 0 {
			t.Errorf("searched %v without a claim", got)
		}
	})
}

func TestDiscover_Backpressure(t *testing.T) {
	fill := func(t *testing.T, h *harness, n int) {
		for i := 0; i < n; i++ {
			h.queue.put(t, listing(fmt.Sprintf("Q%d", i)), false)
		}
	}
	t.Run("at the high-water mark nothing is claimed or searched", func(t *testing.T) {
		cfg := leanConfig(10, 0)
		cfg.QueueHighWater = 2
		h := newHarness(t, cfg, "33950")
		fill(t, h, 2)

		if wait := h.s.discoverStep(context.Background()); wait != backpressureWait {
			t.Errorf("wait = %s, want %s", wait, backpressureWait)
		}
		if h.zips.claimCount() != 0 || len(h.zillow.searchedLocations()) != 0 {
			t.Error("discovery claimed or searched under backpressure")
		}
		if got := h.zillow.usageCallCount(); got != 0 {
			t.Errorf("Usage called %d times: backpressure comes before the quota gate", got)
		}
	})
	t.Run("below the mark discovery runs", func(t *testing.T) {
		cfg := leanConfig(10, 0)
		cfg.QueueHighWater = 3
		h := newHarness(t, cfg, "33950")
		fill(t, h, 2)

		h.discoverUntilWait(t, context.Background())
		if _, ok := h.zips.markedZips()["33950"]; !ok {
			t.Error("zip not searched although the queue is below the high-water mark")
		}
	})
	t.Run("items in backoff do not count: depth is what is claimable", func(t *testing.T) {
		cfg := leanConfig(10, 0)
		cfg.QueueHighWater = 1
		h := newHarness(t, cfg, "33950")
		fill(t, h, 1)
		items, _ := h.queue.Claim(context.Background(), testOwner, 1, time.Hour)
		_ = h.queue.Fail(context.Background(), items[0], testOwner, "x", time.Hour, false)

		h.discoverUntilWait(t, context.Background())
		if _, ok := h.zips.markedZips()["33950"]; !ok {
			t.Error("zip not searched although nothing in the queue is claimable")
		}
	})
	for _, hw := range []int{0, -1} {
		t.Run(fmt.Sprintf("high water %d disables it", hw), func(t *testing.T) {
			cfg := leanConfig(10, 0)
			cfg.QueueHighWater = hw
			h := newHarness(t, cfg, "33950")
			fill(t, h, 5)

			h.discoverUntilWait(t, context.Background())
			if _, ok := h.zips.markedZips()["33950"]; !ok {
				t.Error("zip not searched although backpressure is disabled")
			}
			if _, depths := h.queue.calls(); depths != 0 {
				t.Errorf("Depth called %d times with backpressure disabled", depths)
			}
		})
	}
	t.Run("a failed depth query does not stop discovery", func(t *testing.T) {
		cfg := leanConfig(10, 0)
		cfg.QueueHighWater = 1
		h := newHarness(t, cfg, "33950")
		h.queue.depthErr = errors.New("db hiccup")

		h.discoverUntilWait(t, context.Background())
		if _, ok := h.zips.markedZips()["33950"]; !ok {
			t.Error("zip not searched after a failed depth query")
		}
	})
}

// The budget can run out in the middle of a dense ZIP. What was fetched is
// enqueued, the ZIP is handed back for the next window with the first page
// that was not fetched — and it is NOT stamped searched. Before this, a ZIP
// denser than the budget could never be finished.
func TestDiscover_BudgetRunsOutMidZipThenResumesNextWindow(t *testing.T) {
	h := newHarness(t, leanConfig(2, 0), "11111", "22222")
	h.zillow.pages = map[string][][]property.Property{
		"11111": {props("p1"), props("p2"), props("p3")}, // 4 requests to finish
	}
	ctx := context.Background()

	wait := h.discoverUntilWait(t, ctx)

	if wait != maxWindowSleep {
		t.Errorf("wait = %s, want %s (next window, capped)", wait, maxWindowSleep)
	}
	if got := h.zips.markedZips(); len(got) != 0 {
		t.Fatalf("marked %v: a half-searched ZIP must not be stamped searched", got)
	}
	want := []zipTransition{{kind: "defer", zip: "11111", until: h.nextWindow(), resumePage: 3}}
	if got := h.zips.history(); !reflect.DeepEqual(got, want) {
		t.Fatalf("zip transitions = %+v\nwant %+v", got, want)
	}
	if got := h.queue.queued(); !reflect.DeepEqual(got, []string{"p1", "p2"}) {
		t.Errorf("queued %v, want the two pages that were paid for", got)
	}
	if h.s.discoverFailures != 0 {
		t.Error("running out of budget is not a failure")
	}

	// Until the window rolls the ZIP stays hidden and nothing is spent.
	h.discoverUntilWait(t, ctx)
	if got := len(h.zillow.searchCalls()); got != 1 {
		t.Fatalf("searched %d times within the spent window, want 1", got)
	}

	h.clock.advance(testWindow)
	h.discoverUntilWait(t, ctx)

	calls := h.zillow.searchCalls()
	if len(calls) < 2 || calls[1] != (searchCall{location: "11111", startPage: 3}) {
		t.Fatalf("searches = %+v, want the second one to resume 11111 at page 3", calls)
	}
	if _, ok := h.zips.markedZips()["11111"]; !ok {
		t.Error("the resumed ZIP should be marked once its last page is in")
	}
	if got := h.queue.queued(); !reflect.DeepEqual(got, []string{"p1", "p2", "p3"}) {
		t.Errorf("queued %v, want all three pages, none bought twice", got)
	}
}

// Another instance can take the last request between the budget check and the
// first permit (and a ledger that cannot be reached denies as well). Nothing
// was fetched, so the ZIP is handed back exactly as it was claimed: released,
// not deferred, because a ledger that was only briefly unreachable must not
// park a ZIP until the next window (the budget check keeps everyone else off
// it while the budget really is spent).
func TestDiscover_PermitDeniedBeforeTheFirstRequestReleasesTheZip(t *testing.T) {
	for _, resume := range []int{0, 3} {
		t.Run(fmt.Sprintf("resume page %d", resume), func(t *testing.T) {
			h := newHarness(t, leanConfig(10, 0), "11111")
			h.zips.setResume("11111", resume)
			h.ledger.reserveErr = errors.New("ledger unreachable")

			wait := h.s.discoverStep(context.Background())

			want := []zipTransition{{kind: "release", zip: "11111"}}
			if got := h.zips.history(); !reflect.DeepEqual(got, want) {
				t.Errorf("zip transitions = %+v\nwant %+v", got, want)
			}
			if wait != maxWindowSleep {
				t.Errorf("wait = %s, want %s", wait, maxWindowSleep)
			}
		})
	}
}

// A ledger that cannot be read is a database in trouble: the permit, which
// fails closed, would deny the first request anyway. Claiming a ZIP just to
// hand it back is churn, so the step backs off without claiming anything.
func TestDiscover_BudgetLookupErrorClaimsNothing(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0), "11111")
	h.ledger.spentErr = errors.New("db down")

	wait := h.s.discoverStep(context.Background())

	if n := h.zips.claimCount(); n != 0 {
		t.Errorf("claims = %d, want none while the budget cannot be read", n)
	}
	if len(h.zillow.searchCalls()) != 0 {
		t.Error("searched while the budget could not be read")
	}
	if wait != noZipWait {
		t.Errorf("wait = %s, want %s", wait, noZipWait)
	}
}

func TestDiscover_SearchErrorKeepsPartialResultsAndFailsTheZip(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0), "11111", "22222")
	h.zillow.pages = map[string][][]property.Property{"11111": {props("p1"), props("p2"), props("p3")}}
	h.zillow.pageErr = map[string]map[int]error{"11111": {3: errors.New("boom")}}
	ctx := context.Background()

	wait := h.s.discoverStep(ctx)

	if wait != 5*time.Second {
		t.Errorf("wait = %s, want 5s after the first failure", wait)
	}
	if got := h.queue.queued(); !reflect.DeepEqual(got, []string{"p1", "p2"}) {
		t.Errorf("queued %v, want the pages fetched before the failure", got)
	}
	// 12 h window: the next window is later than now+1h, so the ZIP keeps
	// today's "a failed ZIP retries next cycle".
	want := []zipTransition{{kind: "fail", zip: "11111", until: h.nextWindow(), resumePage: 3}}
	if got := h.zips.history(); !reflect.DeepEqual(got, want) {
		t.Fatalf("zip transitions = %+v\nwant %+v", got, want)
	}
	if _, ok := h.zips.markedZips()["11111"]; ok {
		t.Error("a failed ZIP must not be stamped searched")
	}
	if recs := h.logs.find("search failed"); len(recs) != 1 || recs[0].level != slog.LevelError {
		t.Errorf("want one Error about the failed search, got %+v", recs)
	}

	// This replaces the old in-memory "tried" map: the failed ZIP is hidden,
	// so the rest of the budget goes to the ZIPs behind it, not to retries.
	h.discoverUntilWait(t, ctx)
	if got := h.zillow.searchedLocations(); !reflect.DeepEqual(got, []string{"11111", "22222"}) {
		t.Errorf("searched %v, want 11111 exactly once and then 22222", got)
	}
	if _, ok := h.zips.markedZips()["22222"]; !ok {
		t.Error("zip 22222 should be searched with the remaining budget")
	}
}

func TestDiscover_FailedZipIsHiddenForAtLeastAnHour(t *testing.T) {
	tests := []struct {
		name      string
		window    time.Duration
		wantAfter time.Duration // until - now
	}{
		{"next window is later than an hour", 12 * time.Hour, 12 * time.Hour},
		{"next window is sooner than an hour", 20 * time.Minute, zipFailBackoff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, leanConfig(10, 0), "11111")
			h.windows.length = tt.window
			h.zillow.pageErr = map[string]map[int]error{"11111": {1: errors.New("boom")}}

			h.s.discoverStep(context.Background())

			hist := h.zips.history()
			if len(hist) != 1 || hist[0].kind != "fail" {
				t.Fatalf("zip transitions = %+v, want one fail", hist)
			}
			if got := hist[0].until.Sub(h.clock.now()); got != tt.wantAfter {
				t.Errorf("hidden for %s, want %s", got, tt.wantAfter)
			}
			if hist[0].resumePage != 1 {
				t.Errorf("resume page = %d, want the failing page 1", hist[0].resumePage)
			}
		})
	}
}

func TestFailurePause(t *testing.T) {
	want := map[int]time.Duration{
		0: 5 * time.Second, 1: 5 * time.Second, 2: 10 * time.Second, 3: 20 * time.Second,
		4: 40 * time.Second, 5: 80 * time.Second, 6: 160 * time.Second,
		7: 5 * time.Minute, 8: 5 * time.Minute, 64: 5 * time.Minute, 1 << 20: 5 * time.Minute,
	}
	for k, w := range want {
		if got := failurePause(k); got != w {
			t.Errorf("failurePause(%d) = %s, want %s", k, got, w)
		}
	}
}

func TestDiscover_PauseGrowsWithConsecutiveFailuresAndASuccessResetsIt(t *testing.T) {
	zips := []string{"f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "ok", "f9"}
	h := newHarness(t, leanConfig(1000, 0), zips...)
	h.zillow.pageErr = map[string]map[int]error{}
	for _, z := range zips {
		if z != "ok" {
			h.zillow.pageErr[z] = map[int]error{1: errors.New("boom")}
		}
	}
	ctx := context.Background()

	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute,
	}
	for i, w := range want {
		if got := h.s.discoverStep(ctx); got != w {
			t.Fatalf("pause after failure %d = %s, want %s", i+1, got, w)
		}
	}
	if got := h.s.discoverStep(ctx); got != 0 {
		t.Fatalf("wait after a successful search = %s, want 0", got)
	}
	if got := h.s.discoverStep(ctx); got != 5*time.Second {
		t.Errorf("pause after a success and a new failure = %s, want 5s again", got)
	}
}

// The fifth consecutive failure of one ZIP stamps it searched so the rotation
// moves on. That ZIP's listings are skipped for a whole pass: say so loudly.
func TestDiscover_FifthFailurePushesTheZipBackLoudly(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0), "11111", "22222")
	h.zips.setFailures("11111", 4)
	h.zillow.pageErr = map[string]map[int]error{"11111": {1: errors.New("boom")}}
	ctx := context.Background()

	if wait := h.s.discoverStep(ctx); wait != 5*time.Second {
		t.Errorf("wait = %s, want the usual failure pause", wait)
	}
	recs := h.logs.find("pushed back")
	if len(recs) != 1 || recs[0].level != slog.LevelError || recs[0].attrs["zip"] != "11111" {
		t.Fatalf("want one Error naming the pushed-back zip, got %+v", recs)
	}

	h.discoverUntilWait(t, ctx)
	if _, ok := h.zips.markedZips()["22222"]; !ok {
		t.Error("discovery should carry on with the next ZIP")
	}
}

func TestDiscover_RateLimitInvalidatesTheQuotaGate(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0), "11111", "22222")
	h.zillow.pageErr = map[string]map[int]error{"11111": {1: &zillow.StatusError{Code: 429, Body: "Too Many Requests"}}}
	ctx := context.Background()

	h.s.discoverStep(ctx)
	if got := h.zillow.usageCallCount(); got != 1 {
		t.Fatalf("Usage called %d times during the first step, want 1", got)
	}

	// The provider now says what the 429 meant.
	h.zillow.setUsage(&zillow.Usage{Status: "exceeded"}, nil)
	wait := h.s.discoverStep(ctx)

	if got := h.zillow.usageCallCount(); got != 2 {
		t.Errorf("Usage called %d times, want 2: a 429 must invalidate the cached verdict", got)
	}
	if wait != maxWindowSleep || len(h.zillow.searchCalls()) != 1 {
		t.Errorf("wait = %s, searches = %v: the closed gate should stop discovery", wait, h.zillow.searchedLocations())
	}
}

func TestDiscover_OtherErrorsLeaveTheGateAlone(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0), "11111", "22222")
	h.zillow.pageErr = map[string]map[int]error{"11111": {1: &zillow.StatusError{Code: 503}}}
	ctx := context.Background()

	h.s.discoverStep(ctx)
	h.s.discoverStep(ctx)
	if got := h.zillow.usageCallCount(); got != 1 {
		t.Errorf("Usage called %d times, want 1: only a 429 invalidates the gate", got)
	}
}

func TestDiscover_EnqueueFilter(t *testing.T) {
	// "new" is not stored, "novideo" is stored without a ready video, "done"
	// is stored with one; the fourth listing has no zpid at all.
	found := []property.Property{listing("new"), listing("novideo"), listing("done"), listing("")}
	tests := []struct {
		name         string
		skipExisting bool
		videoEnabled bool
		noRenderer   bool
		want         []string
		wantStates   int // VideoStates calls
	}{
		{name: "SKIP_EXISTING off: everything with a zpid", skipExisting: false, videoEnabled: true,
			want: []string{"done", "new", "novideo"}, wantStates: 0},
		{name: "SKIP_EXISTING on, video wanted: new and videoless", skipExisting: true, videoEnabled: true,
			want: []string{"new", "novideo"}, wantStates: 1},
		{name: "SKIP_EXISTING on, video disabled: new only", skipExisting: true, videoEnabled: false,
			want: []string{"new"}, wantStates: 1},
		{name: "SKIP_EXISTING on, no renderer: new only", skipExisting: true, videoEnabled: true, noRenderer: true,
			want: []string{"new"}, wantStates: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := leanConfig(10, 0)
			cfg.SkipExisting = tt.skipExisting
			cfg.Video.Enabled = tt.videoEnabled
			h := newHarness(t, cfg, "33950")
			if tt.noRenderer {
				h.s.render = nil
			}
			h.zillow.pages = map[string][][]property.Property{"33950": {found}}
			h.store.existing = map[string]bool{"novideo": true, "done": true}
			h.store.needsVideo = map[string]bool{"novideo": true}

			h.discoverUntilWait(t, context.Background())

			if got := h.queue.queued(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("queued %v, want %v", got, tt.want)
			}
			if got := h.store.videoStatesCalls(); got != tt.wantStates {
				t.Errorf("VideoStates calls = %d, want %d (one batched query per ZIP)", got, tt.wantStates)
			}
			if got := h.zips.markedZips()["33950"]; got != len(found) {
				t.Errorf("listing count = %d, want %d: the count is what the search returned", got, len(found))
			}
			if recs := h.logs.find("empty zpid"); len(recs) != 1 || recs[0].level != slog.LevelError {
				t.Errorf("want one Error about the listing without a zpid, got %+v", recs)
			}

			row, ok := h.queue.row("new")
			if !ok {
				t.Fatal("listing \"new\" not queued")
			}
			if row.sourceZip != "33950" {
				t.Errorf("source zip = %q, want 33950", row.sourceZip)
			}
			p, revisit, err := DecodeListing(row.payload)
			if err != nil || revisit || !reflect.DeepEqual(*p, listing("new")) {
				t.Errorf("payload decodes to %+v revisit=%v err=%v", p, revisit, err)
			}

			recs := h.logs.find("properties discovered")
			if len(recs) != 1 {
				t.Fatalf("want one \"properties discovered\" line, got %d", len(recs))
			}
			a := recs[0].attrs
			wantSkipped := int64(3 - len(tt.want))
			if a["zip"] != "33950" || a["count"] != int64(4) || a["enqueued"] != int64(len(tt.want)) || a["skipped"] != wantSkipped {
				t.Errorf("log attrs = %v, want zip=33950 count=4 enqueued=%d skipped=%d", a, len(tt.want), wantSkipped)
			}
		})
	}
}

func TestDiscover_EnqueueFailure(t *testing.T) {
	dbDown := errors.New("db down")

	t.Run("once: the retry saves the ZIP", func(t *testing.T) {
		h := newHarness(t, leanConfig(10, 0), "11111")
		h.zillow.pages = map[string][][]property.Property{"11111": {props("a")}}
		h.queue.enqueueErrs = []error{dbDown}

		if wait := h.s.discoverStep(context.Background()); wait != 0 {
			t.Errorf("wait = %s, want 0", wait)
		}
		if got := h.queue.queued(); !reflect.DeepEqual(got, []string{"a"}) {
			t.Errorf("queued %v, want [a]", got)
		}
		if _, ok := h.zips.markedZips()["11111"]; !ok {
			t.Error("zip should be marked after the retried enqueue")
		}
	})

	// Marking the ZIP searched with its listings lost would drop them for a
	// whole pass. Failing it with the page the search STARTED at buys the
	// pages again instead.
	t.Run("twice: the ZIP fails with its ORIGINAL resume page", func(t *testing.T) {
		h := newHarness(t, leanConfig(10, 0), "11111")
		h.zips.setResume("11111", 2)
		h.zillow.pages = map[string][][]property.Property{"11111": {props("a"), props("b"), props("c")}}
		h.queue.enqueueErrs = []error{dbDown, dbDown}

		wait := h.s.discoverStep(context.Background())

		if wait != 5*time.Second {
			t.Errorf("wait = %s, want the failure pause", wait)
		}
		want := []zipTransition{{kind: "fail", zip: "11111", until: h.nextWindow(), resumePage: 2}}
		if got := h.zips.history(); !reflect.DeepEqual(got, want) {
			t.Errorf("zip transitions = %+v\nwant %+v", got, want)
		}
		if enq, _ := h.queue.calls(); enq != 2 {
			t.Errorf("Enqueue called %d times, want 2 (one retry)", enq)
		}
	})

	t.Run("a failed VideoStates lookup counts as a failed enqueue", func(t *testing.T) {
		cfg := leanConfig(10, 0)
		cfg.SkipExisting = true
		h := newHarness(t, cfg, "11111")
		h.zillow.pages = map[string][][]property.Property{"11111": {props("a")}}
		h.store.videoStatesErr = dbDown

		h.s.discoverStep(context.Background())

		if hist := h.zips.history(); len(hist) != 1 || hist[0].kind != "fail" {
			t.Errorf("zip transitions = %+v, want one fail", hist)
		}
		if got := h.store.videoStatesCalls(); got != 2 {
			t.Errorf("VideoStates called %d times, want 2 (one retry)", got)
		}
	})

	// The queue stored everything it could; the listing PostgreSQL refused
	// will be refused again, so a retry only costs the ZIP its other listings.
	t.Run("unstorable items: not a failure, no retry", func(t *testing.T) {
		h := newHarness(t, leanConfig(10, 0), "11111")
		h.zillow.pages = map[string][][]property.Property{"11111": {props("a", "b")}}
		h.queue.enqueueErrs = []error{fmt.Errorf("%w: zpid b", workqueue.ErrUnstorable)}

		if wait := h.s.discoverStep(context.Background()); wait != 0 {
			t.Errorf("wait = %s, want 0", wait)
		}
		if enq, _ := h.queue.calls(); enq != 1 {
			t.Errorf("Enqueue called %d times, want 1: a retry cannot help", enq)
		}
		if _, ok := h.zips.markedZips()["11111"]; !ok {
			t.Error("zip should still be marked searched")
		}
		if recs := h.logs.find("could not be stored"); len(recs) != 1 || recs[0].level != slog.LevelWarn {
			t.Errorf("want one Warn about the unstorable listings, got %+v", recs)
		}
	})
}

func TestDiscover_Shutdown(t *testing.T) {
	t.Run("before anything was fetched the ZIP is released, not failed", func(t *testing.T) {
		h := newHarness(t, leanConfig(10, 0), "11111")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h.zillow.beforePage = func(string, int) { cancel() }

		wait := h.s.discoverStep(ctx)

		want := []zipTransition{{kind: "release", zip: "11111"}}
		if got := h.zips.history(); !reflect.DeepEqual(got, want) {
			t.Errorf("zip transitions = %+v\nwant %+v", got, want)
		}
		if wait != 0 || h.s.discoverFailures != 0 {
			t.Errorf("wait = %s, failures = %d: a shutdown is not a failure", wait, h.s.discoverFailures)
		}
	})

	// Releasing would leave the resume page where it was and buy the fetched
	// pages a second time.
	t.Run("pages already paid for are enqueued and the ZIP resumes after them", func(t *testing.T) {
		h := newHarness(t, leanConfig(10, 0), "11111")
		h.zillow.pages = map[string][][]property.Property{"11111": {props("a"), props("b")}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h.zillow.beforePage = func(_ string, page int) {
			if page == 2 {
				cancel()
			}
		}

		h.s.discoverStep(ctx)

		if got := h.queue.queued(); !reflect.DeepEqual(got, []string{"a"}) {
			t.Errorf("queued %v, want the page fetched before the shutdown", got)
		}
		want := []zipTransition{{kind: "defer", zip: "11111", until: h.clock.now(), resumePage: 2}}
		if got := h.zips.history(); !reflect.DeepEqual(got, want) {
			t.Errorf("zip transitions = %+v\nwant %+v (claimable at once, no failure)", got, want)
		}
		if h.s.discoverFailures != 0 {
			t.Error("a shutdown is not a failure")
		}
	})

	t.Run("a search that already returned is still enqueued and marked", func(t *testing.T) {
		h := newHarness(t, leanConfig(10, 0), "11111")
		h.zillow.pages = map[string][][]property.Property{"11111": {props("a")}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h.zillow.afterSearch = func(string) { cancel() }

		h.s.discoverStep(ctx)

		if got := h.queue.queued(); !reflect.DeepEqual(got, []string{"a"}) {
			t.Errorf("queued %v, want [a]: bookkeeping must outlive the cancelled context", got)
		}
		if got := h.zips.markedZips(); got["11111"] != 1 {
			t.Errorf("marked = %v, want 11111 with 1 listing", got)
		}
	})
}

func TestDiscover_LostLeaseIsAWarningNotAnError(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0), "11111")
	// Somebody re-claimed the ZIP while the search ran (a paused VM).
	h.zillow.afterSearch = func(string) { h.zips.steal("11111") }

	if wait := h.s.discoverStep(context.Background()); wait != 0 {
		t.Errorf("wait = %s, want 0", wait)
	}
	recs := h.logs.find("lease lost")
	if len(recs) != 1 || recs[0].level != slog.LevelWarn {
		t.Errorf("want one Warn about the lost lease, got %+v", recs)
	}
	for _, r := range h.logs.find("") {
		if r.level == slog.LevelError {
			t.Errorf("unexpected Error log: %s %v", r.msg, r.attrs)
		}
	}
}
