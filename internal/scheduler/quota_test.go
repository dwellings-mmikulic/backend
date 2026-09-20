package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/budget"
	"github.com/dwellingtw/backend/internal/zillow"
)

func TestLimits_DetailsShareComesOutOfTheSearchBudget(t *testing.T) {
	tests := []struct {
		name                    string
		budget, details         int
		wantSearch, wantDetails int
	}{
		{"no details reserve", 100, 0, 100, 0},
		{"details reserve shrinks search", 100, 30, 70, 30},
		{"reserve larger than the budget leaves search nothing", 10, 25, 0, 25},
		{"negative details cap is no reserve and no details", 10, -1, 10, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, leanConfig(tt.budget, tt.details))
			if got := h.s.searchLimit(); got != tt.wantSearch {
				t.Errorf("searchLimit = %d, want %d", got, tt.wantSearch)
			}
			if got := h.s.detailsLimit(); got != tt.wantDetails {
				t.Errorf("detailsLimit = %d, want %d", got, tt.wantDetails)
			}
		})
	}
}

func TestPermit_ReservesOneRequestInTheCurrentWindow(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0))
	permit := h.s.permit(budget.KindSearch, 2)
	ctx := context.Background()

	first := h.windowStart()
	if !permit(ctx) || !permit(ctx) {
		t.Fatal("permit denied within the limit")
	}
	if permit(ctx) {
		t.Error("permit granted a third request against a limit of 2")
	}
	if got := h.ledger.get(first, budget.KindSearch); got != 2 {
		t.Errorf("ledger spent = %d, want 2", got)
	}
	if got := h.ledger.get(first, budget.KindDetails); got != 0 {
		t.Errorf("details spent = %d, want 0: the kinds have separate rows", got)
	}

	// The window is looked up per call, so a permit that outlives its window
	// spends from the new one rather than being denied by the old.
	h.clock.advance(testWindow)
	if !permit(ctx) {
		t.Error("permit denied in a fresh window")
	}
	if got := h.ledger.get(h.windowStart(), budget.KindSearch); got != 1 {
		t.Errorf("new window spent = %d, want 1", got)
	}
	if got := h.ledger.get(first, budget.KindSearch); got != 2 {
		t.Errorf("old window spent = %d, want 2 (untouched)", got)
	}
}

func TestPermit_DeniesWhenTheLedgerErrors(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0))
	h.ledger.reserveErr = errors.New("connection refused")

	if h.s.permit(budget.KindSearch, 10)(context.Background()) {
		t.Fatal("permit granted although the ledger failed: spending must fail closed")
	}
	if recs := h.logs.find("budget reservation failed"); len(recs) != 1 || recs[0].level != slog.LevelError {
		t.Errorf("want one Error log about the failed reservation, got %+v", recs)
	}
}

func TestUntilNextWindow_CappedAndNeverZero(t *testing.T) {
	tests := []struct {
		name    string
		elapsed time.Duration // into the 12h test window
		want    time.Duration
	}{
		{"far from the boundary sleeps are capped", time.Hour, maxWindowSleep},
		{"close to the boundary sleeps to it", testWindow - 3*time.Minute, 3 * time.Minute},
		{"at the boundary's edge at least a second", testWindow - 10*time.Millisecond, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, leanConfig(10, 0))
			h.clock.advance(tt.elapsed)
			if got := h.s.untilNextWindow(); got != tt.want {
				t.Errorf("untilNextWindow = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestGate_VerdictIsCachedPerWindow(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0))
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if !h.s.gateOpen(ctx) {
			t.Fatal("gate closed with ample quota")
		}
	}
	if got := h.zillow.usageCallCount(); got != 1 {
		t.Errorf("Usage called %d times in one window, want 1", got)
	}

	h.clock.advance(testWindow)
	h.s.gateOpen(ctx)
	if got := h.zillow.usageCallCount(); got != 2 {
		t.Errorf("Usage called %d times after the window rolled, want 2", got)
	}
}

func TestGate_ExceededIsCachedClosedUntilTheNextWindow(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0))
	h.zillow.setUsage(&zillow.Usage{Status: "exceeded"}, nil)
	ctx := context.Background()

	if h.s.gateOpen(ctx) || h.s.gateOpen(ctx) {
		t.Fatal("gate open although the provider reports the quota exceeded")
	}
	if got := h.zillow.usageCallCount(); got != 1 {
		t.Errorf("Usage called %d times, want 1: a closed verdict is cached too", got)
	}
	if recs := h.logs.find("zillow quota exhausted"); len(recs) != 1 || recs[0].level != slog.LevelWarn {
		t.Errorf("want one Warn about the exhausted quota, got %+v", recs)
	}

	// The provider's quota was topped up and a new window began.
	h.zillow.setUsage(nil, nil)
	h.clock.advance(testWindow)
	if !h.s.gateOpen(ctx) {
		t.Error("gate still closed in the next window")
	}
}

func TestGate_FailsOpenWithoutCachingTheVerdict(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0))
	h.zillow.setUsage(nil, errors.New("usage endpoint down"))
	ctx := context.Background()

	if !h.s.gateOpen(ctx) || !h.s.gateOpen(ctx) {
		t.Fatal("gate closed on a failed usage check: a flaky endpoint must not halt collection")
	}
	if got := h.zillow.usageCallCount(); got != 2 {
		t.Errorf("Usage called %d times, want 2: a failed check must not be cached", got)
	}
	if recs := h.logs.find("zillow usage check failed"); len(recs) != 2 {
		t.Errorf("want a Warn per failed check, got %d", len(recs))
	}

	// Once the endpoint answers, its verdict is what counts.
	h.zillow.setUsage(&zillow.Usage{Status: "exceeded"}, nil)
	if h.s.gateOpen(ctx) {
		t.Error("gate open although the recovered usage check reports exceeded")
	}
}

func TestGate_RemainingIsComparedWithTheUnspentBudget(t *testing.T) {
	tests := []struct {
		name                      string
		remaining                 int
		spentSearch, spentDetails int
		spentErr                  error
		wantOpen                  bool
	}{
		{name: "fresh window, remaining covers the budget", remaining: 150, wantOpen: true},
		{name: "fresh window, remaining below the budget", remaining: 100, wantOpen: false},
		{name: "remaining below the budget but above the unspent part",
			remaining: 100, spentSearch: 40, spentDetails: 20, wantOpen: true},
		{name: "remaining below even the unspent part",
			remaining: 80, spentSearch: 40, spentDetails: 20, wantOpen: false},
		{name: "budget fully spent: nothing left to protect",
			remaining: 0, spentSearch: 120, spentDetails: 30, wantOpen: true},
		{name: "unreadable ledger counts as nothing spent",
			remaining: 100, spentSearch: 40, spentDetails: 20, spentErr: errors.New("db down"), wantOpen: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, leanConfig(150, 30))
			h.zillow.setUsage(usageWith("ok", tt.remaining), nil)
			h.ledger.set(h.windowStart(), budget.KindSearch, tt.spentSearch)
			h.ledger.set(h.windowStart(), budget.KindDetails, tt.spentDetails)
			h.ledger.spentErr = tt.spentErr

			if got := h.s.gateOpen(context.Background()); got != tt.wantOpen {
				t.Errorf("gateOpen = %v, want %v", got, tt.wantOpen)
			}
			if !tt.wantOpen {
				if recs := h.logs.find("zillow quota below"); len(recs) != 1 {
					t.Errorf("want one Warn about the low quota, got %+v", recs)
				}
			}
		})
	}
}

// A closed verdict that rests on a ledger read that failed is a guess, not a
// judgement: counting an unreadable ledger as nothing spent makes the gate
// demand the provider's whole budget. Caching it would idle this instance's
// discovery and details loops until the window rolls — up to 12 hours — over
// one failed lookup, which contradicts the fail-open rule the failed /usage
// call already follows.
func TestGate_ClosedVerdictFromAFailedLedgerReadIsNotCached(t *testing.T) {
	h := newHarness(t, leanConfig(1000, 0))
	h.zillow.setUsage(usageWith("ok", 500), nil)
	h.ledger.set(h.windowStart(), budget.KindSearch, 900)
	h.ledger.spentErr = errors.New("db down")
	ctx := context.Background()

	if h.s.gateOpen(ctx) {
		t.Fatal("gate open although 500 remaining is below the 1000 it has to assume is unspent")
	}
	if h.s.gate.cached {
		t.Error("the guessed closed verdict was cached for the whole window")
	}

	// The ledger answers: 900 of 1000 spent, so 500 remaining is ample.
	h.ledger.spentErr = nil
	if !h.s.gateOpen(ctx) {
		t.Error("gate still closed although the ledger now says only 100 is unspent")
	}
	if got := h.zillow.usageCallCount(); got != 2 {
		t.Errorf("Usage called %d times, want 2: the guess must be re-checked", got)
	}

	// An open verdict is firm however the ledger behaved: counting a failed
	// lookup as zero can only overstate the unspent part.
	if !h.s.gate.cached || !h.s.gate.open {
		t.Error("the open verdict must be cached")
	}
}

func TestGate_NoRequestsQuotaProceedsAndIsCached(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0))
	u := &zillow.Usage{Status: "ok"}
	u.Quotas = []zillow.QuotaMetric{{Name: "Bandwidth", Remaining: 1}}
	h.zillow.setUsage(u, nil)
	ctx := context.Background()

	if !h.s.gateOpen(ctx) || !h.s.gateOpen(ctx) {
		t.Fatal("gate closed although the report has no Requests quota to judge by")
	}
	if got := h.zillow.usageCallCount(); got != 1 {
		t.Errorf("Usage called %d times, want 1 (cached)", got)
	}
	if recs := h.logs.find("no Requests quota"); len(recs) != 1 {
		t.Errorf("want one Warn about the missing quota, got %+v", recs)
	}
}

func TestGate_InvalidateForcesAFreshCheck(t *testing.T) {
	h := newHarness(t, leanConfig(10, 0))
	ctx := context.Background()

	h.s.gateOpen(ctx)
	h.zillow.setUsage(&zillow.Usage{Status: "exceeded"}, nil)
	if !h.s.gateOpen(ctx) {
		t.Fatal("cached open verdict should still hold")
	}
	h.s.invalidateGate()
	if h.s.gateOpen(ctx) {
		t.Error("gate open after invalidation although the provider now reports exceeded")
	}
	if got := h.zillow.usageCallCount(); got != 2 {
		t.Errorf("Usage called %d times, want 2", got)
	}
}

func TestIsRateLimited(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"500", &zillow.StatusError{Code: 500}, false},
		{"429", &zillow.StatusError{Code: 429}, true},
		{"wrapped 429", errors.Join(context.Canceled, &zillow.StatusError{Code: 429}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRateLimited(tt.err); got != tt.want {
				t.Errorf("isRateLimited = %v, want %v", got, tt.want)
			}
		})
	}
}
