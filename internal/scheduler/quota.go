package scheduler

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/dwellingtw/backend/internal/budget"
	"github.com/dwellingtw/backend/internal/zillow"
)

// detailsLimit is the details share of the window's budget. A negative
// DETAILS_PER_CYCLE means "off", exactly like 0: it must not be subtracted
// from the budget, or a typo would hand search more than the whole budget.
func (s *Scheduler) detailsLimit() int {
	return max(0, s.cfg.DetailsPerCycle)
}

// searchLimit is what is left of the window's budget once the details share
// is set aside — the split a single instance has always had.
func (s *Scheduler) searchLimit() int {
	return max(0, s.cfg.APIBudgetPerCycle-s.detailsLimit())
}

// permit returns the zillow.Permit for one kind of paid request: every call
// reserves one request in the fleet ledger, in whatever budget window the
// clock is in at that moment. The window is looked up per call because a
// search can straddle a window boundary, and a request belongs to the window
// it is sent in.
//
// A ledger that cannot be reached denies. The caller is about to spend money,
// and "the database is down" must never read as "the budget is unlimited".
func (s *Scheduler) permit(kind string, limit int) zillow.Permit {
	return func(ctx context.Context) bool {
		start, _ := s.windows.Current(s.now())
		ok, err := s.ledger.TryReserve(ctx, start, kind, limit)
		if err != nil {
			if ctx.Err() != nil {
				// Shutting down or out of time: the client reports the
				// context error itself, so this is not worth an Error line.
				s.log.Debug("budget reservation abandoned", "kind", kind, "error", err)
				return false
			}
			s.log.Error("budget reservation failed, denying the request", "kind", kind, "error", err)
			return false
		}
		return ok
	}
}

// untilNextWindow is how long a loop that has nothing to spend should sleep:
// until the next window opens, but never longer than maxWindowSleep, so a
// changed clock, a quota that was topped up or a budget raised on the other
// boxes is noticed within minutes instead of at the end of a 12 h window.
// Never less than a second, so a loop sitting on the boundary cannot spin.
func (s *Scheduler) untilNextWindow() time.Duration {
	now := s.now()
	_, next := s.windows.Current(now)
	return max(min(next.Sub(now), maxWindowSleep), time.Second)
}

// quotaGate is the cached verdict of the provider's /usage report, shared by
// the discovery and details loops. The ledger bounds what the fleet spends;
// the gate is about what the provider still has. Spending a window's budget
// into an exhausted plan only buys 429s.
type quotaGate struct {
	mu     sync.Mutex
	cached bool
	window time.Time // start of the window the verdict is for
	open   bool
}

// gateOpen reports whether the provider's quota allows paid requests in the
// current window. The provider is asked once per window per instance; the
// verdict, open or closed, is kept until the window ends or a 429 proves it
// wrong (invalidateGate).
//
// A failed usage check proceeds (fail-open): a flaky usage endpoint must not
// halt collection, and the ledger still bounds the spend. That verdict is NOT
// cached, so the next step asks again. Neither is a closed verdict that rests
// on a ledger read that failed: counting an unreadable ledger as nothing spent
// makes the gate demand the provider's whole budget, and caching THAT would
// idle this instance's discovery and details loops for the rest of the window
// over one failed lookup.
//
// The mutex is held across the HTTP call on purpose: when a window opens both
// loops arrive here together, and the second should wait for the first one's
// answer rather than ask the provider the same question.
func (s *Scheduler) gateOpen(ctx context.Context) bool {
	s.gate.mu.Lock()
	defer s.gate.mu.Unlock()

	window, _ := s.windows.Current(s.now())
	if s.gate.cached && s.gate.window.Equal(window) {
		return s.gate.open
	}

	u, err := s.zillow.Usage(ctx)
	if err != nil {
		if ctx.Err() == nil { // a shutdown is not a flaky endpoint
			s.log.Warn("zillow usage check failed, proceeding", "error", err)
		}
		return true
	}
	open, firm := s.quotaAllows(ctx, u, window)
	if firm {
		s.gate.cached, s.gate.window, s.gate.open = true, window, open
	}
	return open
}

// quotaAllows judges a usage report: closed when the plan is exhausted, or
// when it has less left than this window's budget still has unspent —
// continuing would only burn 429s. Comparing with the unspent part, not the
// whole budget, matters for every check after the first of a window (a
// restart, a 429): by then the fleet has used some of the quota itself, and
// holding that against the quota again would close the gate for the requests
// the plan can in fact still serve.
// It also reports whether the verdict is firm enough to cache for the rest of
// the window. Only one verdict is not: a closed one worked out from a ledger
// read that failed. Counting the unreadable part as nothing spent can only
// overstate what is unspent, so an open verdict would stay open with the true
// figures, while a closed one may well be wrong.
func (s *Scheduler) quotaAllows(ctx context.Context, u *zillow.Usage, window time.Time) (open, firm bool) {
	if u.Status == "exceeded" {
		s.log.Warn("zillow quota exhausted, waiting for the next window", "status", u.Status)
		return false, true
	}
	for _, q := range u.Quotas {
		if q.Name != "Requests" {
			continue
		}
		spent, ledgerRead := s.spentThisWindow(ctx, window)
		unspent := s.cfg.APIBudgetPerCycle - spent
		if q.Remaining < unspent {
			s.log.Warn("zillow quota below the window's unspent budget, waiting for the next window",
				"remaining", q.Remaining, "unspent", unspent, "budget", s.cfg.APIBudgetPerCycle)
			return false, ledgerRead
		}
		return true, true
	}
	s.log.Warn("zillow usage report has no Requests quota, proceeding", "status", u.Status)
	return true, true
}

// spentThisWindow adds up both kinds and reports whether every lookup
// answered. An unreadable ledger counts as nothing spent, which is the
// cautious reading here: it makes the gate demand the whole budget of the
// provider. The caller needs to know, because a verdict resting on a guess
// must not be cached for the window.
func (s *Scheduler) spentThisWindow(ctx context.Context, window time.Time) (total int, read bool) {
	read = true
	for _, kind := range []string{budget.KindSearch, budget.KindDetails} {
		n, err := s.ledger.Spent(ctx, window, kind)
		if err != nil {
			s.log.Warn("budget spent lookup failed, counting it as zero", "kind", kind, "error", err)
			read = false
			continue
		}
		total += n
	}
	return total, read
}

// invalidateGate drops the cached verdict. A 429 means the provider disagrees
// with it, so the next step asks again instead of trusting it for the rest of
// the window.
func (s *Scheduler) invalidateGate() {
	s.gate.mu.Lock()
	defer s.gate.mu.Unlock()
	s.gate.cached = false
}

// isAccountRejected reports whether err is the provider refusing the account
// rather than the request: an unauthorized, payment-required or forbidden
// answer — a revoked key, a lapsed plan, a suspended account. Every request
// would get the same answer, so it is never one row's own failure.
func isAccountRejected(err error) bool {
	var se *zillow.StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		return true
	}
	return false
}

// isRateLimited reports whether err carries the provider's 429, however
// deeply it is wrapped (the client keeps it in the chain even when a context
// ended the retries).
func isRateLimited(err error) bool {
	var se *zillow.StatusError
	return errors.As(err, &se) && se.Code == http.StatusTooManyRequests
}
