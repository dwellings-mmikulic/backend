package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/budget"
	"github.com/dwellingtw/backend/internal/zillow"
)

func detailsHarness(t *testing.T, perCycle int, missing ...string) *harness {
	t.Helper()
	cfg := baseConfig()
	cfg.DetailsPerCycle = perCycle
	h := newHarness(t, cfg)
	h.store.missingDetails = missing
	return h
}

// detailsUntilWait steps the details loop the way RunOnce does.
func (h *harness) detailsUntilWait(t *testing.T, ctx context.Context) time.Duration {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if wait := h.s.detailsStep(ctx); wait > 0 {
			return wait
		}
		if ctx.Err() != nil {
			return 0
		}
	}
	t.Fatal("details never came to rest")
	return 0
}

// The cap is the ledger's now: the third call is denied its permit, and the
// row it was for goes back unharmed.
func TestDetails_EnrichesUpToTheWindowsCap(t *testing.T) {
	h := detailsHarness(t, 2, "Z1", "Z2", "Z3")

	wait := h.detailsUntilWait(t, context.Background())

	set, released, failed := h.store.detailsState()
	if !reflect.DeepEqual(set, []string{"Z1", "Z2"}) {
		t.Fatalf("SetDetails calls = %v, want exactly [Z1 Z2] (oldest first, 2-cap)", set)
	}
	if d := h.store.detailsGot["Z1"]; d == nil || d.PropertyType == nil || *d.PropertyType != "SINGLE_FAMILY" {
		t.Errorf("details not stored: %+v", d)
	}
	if !reflect.DeepEqual(released, [][]string{{"Z3"}}) {
		t.Errorf("released = %v, want [[Z3]]: the row the budget ran out on", released)
	}
	if len(failed) != 0 {
		t.Errorf("FailDetails = %v: running out of budget is not the row's fault", failed)
	}
	if wait != maxWindowSleep {
		t.Errorf("wait = %s, want %s (next window, capped)", wait, maxWindowSleep)
	}
	if got := h.ledger.get(h.windowStart(), budget.KindDetails); got != 2 {
		t.Errorf("ledger details spent = %d, want 2", got)
	}
	if got := h.ledger.limitsSeen[budget.KindDetails]; len(got) == 0 || got[0] != 2 {
		t.Errorf("limits passed to the ledger = %v, want 2", got)
	}
	if h.s.detailsFailures != 0 {
		t.Error("running out of budget is not a failure")
	}

	// The window rolls: the released row is picked up.
	h.clock.advance(testWindow)
	h.detailsUntilWait(t, context.Background())
	if set, _, _ := h.store.detailsState(); !reflect.DeepEqual(set, []string{"Z1", "Z2", "Z3"}) {
		t.Errorf("SetDetails calls = %v, want Z3 enriched in the next window", set)
	}
}

func TestDetails_DisabledWhenTheCapIsZero(t *testing.T) {
	for _, perCycle := range []int{0, -5} {
		t.Run(fmt.Sprint(perCycle), func(t *testing.T) {
			h := detailsHarness(t, perCycle, "Z1")

			wait := h.s.detailsStep(context.Background())

			if wait != detailsDisabledWait {
				t.Errorf("wait = %s, want %s: the loop idles", wait, detailsDisabledWait)
			}
			if len(h.store.claimLimits) != 0 || len(h.zillow.detailsCalled()) != 0 || h.zillow.usageCallCount() != 0 {
				t.Error("a disabled details loop must not claim, fetch or check the quota")
			}
		})
	}
}

func TestDetails_ClaimsASmallBatch(t *testing.T) {
	var missing []string
	for i := 0; i < 25; i++ {
		missing = append(missing, fmt.Sprintf("Z%02d", i))
	}
	h := detailsHarness(t, 1000, missing...)

	if wait := h.s.detailsStep(context.Background()); wait != 0 {
		t.Errorf("wait = %s, want 0 after a finished batch", wait)
	}
	if !reflect.DeepEqual(h.store.claimLimits, []int{detailsBatch}) {
		t.Errorf("claim limits = %v, want [%d]", h.store.claimLimits, detailsBatch)
	}
	if set, _, _ := h.store.detailsState(); !reflect.DeepEqual(set, missing[:detailsBatch]) {
		t.Errorf("enriched %v, want the %d oldest", set, detailsBatch)
	}
	recs := h.logs.find("details enrichment finished")
	if len(recs) != 1 || recs[0].level != slog.LevelInfo ||
		recs[0].attrs["fetched"] != int64(detailsBatch) || recs[0].attrs["failed"] != int64(0) {
		t.Errorf("want one Info with the batch's counts, got %+v", recs)
	}
}

func TestDetails_RowOutcomes(t *testing.T) {
	h := detailsHarness(t, 10, "DEAD", "BROKEN", "GONE", "OK1")
	h.zillow.detailsErr = map[string]error{
		"DEAD":   zillow.ErrDetailsNotFound,                              // definitive: mark fetched
		"BROKEN": errors.New("decode zillow response: unexpected token"), // this row's own problem
		"GONE":   &zillow.StatusError{Code: 410},                         // a 4xx that will never succeed
	}

	if wait := h.s.detailsStep(context.Background()); wait != 0 {
		t.Errorf("wait = %s, want 0: row-specific failures do not pause the loop", wait)
	}

	set, released, failed := h.store.detailsState()
	// DEAD gets empty details recorded (no infinite retry); OK1 is enriched.
	if !reflect.DeepEqual(set, []string{"DEAD", "OK1"}) {
		t.Errorf("SetDetails calls = %v, want [DEAD OK1]", set)
	}
	if d := h.store.detailsGot["DEAD"]; d == nil || d.PropertyType != nil {
		t.Errorf("DEAD must be recorded with empty details, got %+v", d)
	}
	if !reflect.DeepEqual(failed, []string{"BROKEN", "GONE"}) {
		t.Errorf("FailDetails = %v, want [BROKEN GONE]: an attempt each, lease kept as backoff", failed)
	}
	if len(released) != 0 {
		t.Errorf("released = %v, want none", released)
	}
	if recs := h.logs.find("details not found"); len(recs) != 1 || recs[0].level != slog.LevelWarn {
		t.Errorf("want one Warn about the dead zpid, got %+v", recs)
	}
	recs := h.logs.find("details enrichment finished")
	if len(recs) != 1 || recs[0].attrs["fetched"] != int64(2) || recs[0].attrs["failed"] != int64(2) {
		t.Errorf("want fetched=2 failed=2, got %+v", recs)
	}
}

func TestDetails_StoreErrorCountsAgainstTheRow(t *testing.T) {
	h := detailsHarness(t, 10, "Z1", "Z2")
	h.store.setDetailsErr = map[string]error{"Z1": errors.New("invalid byte sequence for encoding UTF8")}

	h.s.detailsStep(context.Background())

	set, _, failed := h.store.detailsState()
	if !reflect.DeepEqual(failed, []string{"Z1"}) || !reflect.DeepEqual(set, []string{"Z2"}) {
		t.Errorf("failed = %v, set = %v; want Z1 failed and Z2 stored", failed, set)
	}
}

// A provider outage or a network problem says nothing about the row: the batch
// stops, this row and the rest go back without an attempt counted, and the
// loop backs off like discovery does.
func TestDetails_TransientErrorReleasesTheRestAndBacksOff(t *testing.T) {
	transient := map[string]error{
		"503":            &zillow.StatusError{Code: 503},
		"network":        &url.Error{Op: "Get", URL: "https://api.example", Err: errors.New("connection reset")},
		"client timeout": &url.Error{Op: "Get", URL: "https://api.example", Err: context.DeadlineExceeded},
	}
	for name, terr := range transient {
		t.Run(name, func(t *testing.T) {
			h := detailsHarness(t, 10, "Z1", "Z2", "Z3")
			h.zillow.detailsErr = map[string]error{"Z2": terr}

			wait := h.s.detailsStep(context.Background())

			set, released, failed := h.store.detailsState()
			if !reflect.DeepEqual(set, []string{"Z1"}) {
				t.Errorf("SetDetails calls = %v, want [Z1]", set)
			}
			if !reflect.DeepEqual(released, [][]string{{"Z2", "Z3"}}) {
				t.Errorf("released = %v, want [[Z2 Z3]]: the failing row and everything after it", released)
			}
			if len(failed) != 0 {
				t.Errorf("FailDetails = %v: an outage must not cost a healthy row an attempt", failed)
			}
			if got := h.zillow.detailsCalled(); !reflect.DeepEqual(got, []string{"Z1", "Z2"}) {
				t.Errorf("fetched %v, want the batch stopped at Z2", got)
			}
			if wait != 5*time.Second {
				t.Errorf("wait = %s, want 5s", wait)
			}
		})
	}
}

func TestDetails_PauseGrowsAndASuccessResetsIt(t *testing.T) {
	h := detailsHarness(t, 100, "Z1")
	h.zillow.detailsErr = map[string]error{"Z1": &zillow.StatusError{Code: 502}}
	ctx := context.Background()

	for i, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second} {
		if got := h.s.detailsStep(ctx); got != want {
			t.Fatalf("pause after outage %d = %s, want %s", i+1, got, want)
		}
	}
	h.zillow.mu.Lock()
	h.zillow.detailsErr = nil // the provider is back
	h.zillow.mu.Unlock()
	if got := h.s.detailsStep(ctx); got != 0 {
		t.Fatalf("wait after a fetched batch = %s, want 0", got)
	}
	if h.s.detailsFailures != 0 {
		t.Errorf("consecutive failures = %d, want 0 after a success", h.s.detailsFailures)
	}
}

func TestDetails_RateLimitInvalidatesTheQuotaGate(t *testing.T) {
	h := detailsHarness(t, 10, "Z1")
	h.zillow.detailsErr = map[string]error{"Z1": &zillow.StatusError{Code: 429}}
	ctx := context.Background()

	h.s.detailsStep(ctx)
	h.zillow.setUsage(&zillow.Usage{Status: "exceeded"}, nil)
	wait := h.s.detailsStep(ctx)

	if got := h.zillow.usageCallCount(); got != 2 {
		t.Errorf("Usage called %d times, want 2: a 429 must invalidate the cached verdict", got)
	}
	if wait != maxWindowSleep {
		t.Errorf("wait = %s, want %s behind the closed gate", wait, maxWindowSleep)
	}
	if len(h.store.claimLimits) != 1 {
		t.Errorf("claimed %d times, want 1: nothing is claimed behind a closed gate", len(h.store.claimLimits))
	}
}

func TestDetails_NothingToSpendNeverClaims(t *testing.T) {
	t.Run("quota gate closed", func(t *testing.T) {
		h := detailsHarness(t, 10, "Z1")
		h.zillow.setUsage(&zillow.Usage{Status: "exceeded"}, nil)
		if wait := h.s.detailsStep(context.Background()); wait != maxWindowSleep {
			t.Errorf("wait = %s, want %s", wait, maxWindowSleep)
		}
		if len(h.store.claimLimits) != 0 {
			t.Error("claimed behind a closed gate")
		}
	})
	t.Run("details budget spent", func(t *testing.T) {
		h := detailsHarness(t, 10, "Z1")
		h.ledger.set(h.windowStart(), budget.KindDetails, 10)
		if wait := h.s.detailsStep(context.Background()); wait != maxWindowSleep {
			t.Errorf("wait = %s, want %s", wait, maxWindowSleep)
		}
		if len(h.store.claimLimits) != 0 {
			t.Error("claimed with nothing left to spend")
		}
	})
}

func TestDetails_NothingToClaim(t *testing.T) {
	t.Run("no rows", func(t *testing.T) {
		h := detailsHarness(t, 10)
		if wait := h.s.detailsStep(context.Background()); wait != noDetailsWait {
			t.Errorf("wait = %s, want %s", wait, noDetailsWait)
		}
		if recs := h.logs.find("details enrichment finished"); len(recs) != 0 {
			t.Errorf("nothing was claimed, nothing to report: %+v", recs)
		}
	})
	t.Run("claim error", func(t *testing.T) {
		h := detailsHarness(t, 10, "Z1")
		h.store.claimDetailsErr = errors.New("db down")
		if wait := h.s.detailsStep(context.Background()); wait != noDetailsWait {
			t.Errorf("wait = %s, want %s", wait, noDetailsWait)
		}
		if recs := h.logs.find("claim missing details failed"); len(recs) != 1 || recs[0].level != slog.LevelError {
			t.Errorf("want one Error about the failed claim, got %+v", recs)
		}
	})
}

func TestDetails_BudgetGoneOnTheFirstRowIsQuiet(t *testing.T) {
	h := detailsHarness(t, 10, "Z1", "Z2")
	h.ledger.reserveErr = errors.New("ledger unreachable") // every permit denied

	wait := h.s.detailsStep(context.Background())

	_, released, failed := h.store.detailsState()
	if !reflect.DeepEqual(released, [][]string{{"Z1", "Z2"}}) || len(failed) != 0 {
		t.Errorf("released = %v failed = %v, want the whole batch released", released, failed)
	}
	if wait != maxWindowSleep {
		t.Errorf("wait = %s, want %s", wait, maxWindowSleep)
	}
	recs := h.logs.find("details enrichment finished")
	if len(recs) != 1 || recs[0].level != slog.LevelDebug {
		t.Errorf("want the summary at Debug when nothing was fetched or failed, got %+v", recs)
	}
}

func TestDetails_Shutdown(t *testing.T) {
	// A record that was paid for is stored even though the context died;
	// the rows not reached go back, and nobody is blamed.
	t.Run("mid-batch", func(t *testing.T) {
		h := detailsHarness(t, 10, "Z1", "Z2", "Z3")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h.zillow.afterDetails = func(zpid string) {
			if zpid == "Z1" {
				cancel()
			}
		}

		wait := h.s.detailsStep(ctx)

		set, released, failed := h.store.detailsState()
		if !reflect.DeepEqual(set, []string{"Z1"}) {
			t.Errorf("SetDetails calls = %v, want [Z1] stored on a bookkeeping context", set)
		}
		if !reflect.DeepEqual(released, [][]string{{"Z2", "Z3"}}) {
			t.Errorf("released = %v, want [[Z2 Z3]]", released)
		}
		if len(failed) != 0 || h.s.detailsFailures != 0 || wait != 0 {
			t.Errorf("failed = %v failures = %d wait = %s: a shutdown counts nothing", failed, h.s.detailsFailures, wait)
		}
	})

	// The client reports a permit denied under a dead context as the context
	// error; even a bare ErrBudgetExhausted must not be read as "wait for the
	// next window" while shutting down.
	t.Run("during a fetch", func(t *testing.T) {
		h := detailsHarness(t, 10, "Z1", "Z2")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h.zillow.beforeDetails = func(string) { cancel() }

		wait := h.s.detailsStep(ctx)

		_, released, failed := h.store.detailsState()
		if !reflect.DeepEqual(released, [][]string{{"Z1", "Z2"}}) || len(failed) != 0 {
			t.Errorf("released = %v failed = %v, want everything released", released, failed)
		}
		if wait != 0 || h.s.detailsFailures != 0 {
			t.Errorf("wait = %s failures = %d, want 0 and 0", wait, h.s.detailsFailures)
		}
	})
}
