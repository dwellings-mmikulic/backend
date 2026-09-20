package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/dwellingtw/backend/internal/budget"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/zillow"
)

// detailsStep enriches at most one small batch of listings with their
// one-time details record and returns how long the details loop should wait
// before the next step (0: go again now).
//
// Every claimed row ends in exactly one of SetDetails, FailDetails or
// ReleaseDetails. Which one is the point of the whole function: only a
// failure that is the row's own costs it an attempt. An outage, an exhausted
// budget or a deploy must never use up the attempts of healthy rows — there
// are a million of them, and an abandoned row is never enriched.
func (s *Scheduler) detailsStep(ctx context.Context) time.Duration {
	limit := s.detailsLimit()
	if limit <= 0 {
		return detailsDisabledWait
	}
	if !s.gateOpen(ctx) {
		return s.untilNextWindow()
	}
	window, _ := s.windows.Current(s.now())
	spent, err := s.ledger.Spent(ctx, window, budget.KindDetails)
	switch {
	case err != nil:
		if ctx.Err() != nil {
			return 0
		}
		s.log.Warn("details budget lookup failed, relying on the permit", "error", err)
	case spent >= limit:
		return s.untilNextWindow()
	}

	// The claim runs detached from ctx: a SIGTERM landing inside its round trip
	// would otherwise abandon a statement PostgreSQL goes on to commit,
	// leaving rows claimed by an owner that never learned it holds them —
	// hidden until the lease runs out, with no release path able to fire.
	cctx, ccancel := s.bookkeeping(ctx)
	zpids, err := s.repo.ClaimMissingDetails(cctx, detailsBatch, detailsLease)
	ccancel()
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		s.log.Error("claim missing details failed", "error", err)
		return noDetailsWait
	}
	if len(zpids) == 0 {
		return noDetailsWait
	}
	if ctx.Err() != nil {
		// Shutdown during the claim: the rows go back untouched, no attempt
		// counted against any of them.
		s.releaseDetails(ctx, zpids)
		return 0
	}

	// The batch works under a deadline shorter than its lease.
	dctx, cancel := context.WithTimeout(ctx, detailsDeadline)
	defer cancel()
	permit := s.permit(budget.KindDetails, limit)

	var (
		fetched, failed int
		wait            time.Duration
	)
batch:
	for i, zpid := range zpids {
		d, raw, err := s.zillow.PropertyDetails(dctx, zpid, permit)
		if isRateLimited(err) {
			s.invalidateGate()
		}
		switch {
		case err == nil:
			if s.storeDetails(ctx, zpid, d, raw) {
				fetched++
			} else {
				failed++
			}

		case errors.Is(err, zillow.ErrDetailsNotFound):
			// Definitive: recorded with empty details so a dead zpid is not
			// paid for again on every pass.
			if s.storeDetails(ctx, zpid, &property.Details{}, nil) {
				s.log.Warn("details not found, marked fetched", "zpid", zpid)
				fetched++
			} else {
				failed++
			}

		// The context comes before everything else: IsTransient is false for
		// a cancelled context, and under a dead one no other verdict can be
		// trusted. This row and the ones not reached go back untouched.
		case dctx.Err() != nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			s.releaseDetails(ctx, zpids[i:])
			if ctx.Err() == nil {
				// Not a shutdown: the batch deadline or the client's timeout.
				// The provider is too slow to be worth hammering.
				s.log.Warn("details fetch timed out, batch released", "zpid", zpid, "error", err)
				wait = s.detailsFailed()
			}
			break batch

		case errors.Is(err, zillow.ErrBudgetExhausted):
			s.releaseDetails(ctx, zpids[i:])
			wait = s.untilNextWindow()
			break batch

		case isAccountRejected(err):
			// The provider refused the account, not the row: a revoked key, a
			// lapsed or suspended plan. Every row would get the same answer,
			// so counting it against this one is wrong twice over — it would
			// spend the window's whole details budget in seconds and burn one
			// of five attempts on that many healthy rows, and after about
			// five windows of a bad key they are abandoned for good. An
			// abandoned row is never enriched.
			s.log.Error("details fetch rejected for the account, batch released", "zpid", zpid, "error", err)
			s.invalidateGate()
			s.releaseDetails(ctx, zpids[i:])
			wait = s.detailsFailed()
			break batch

		case zillow.IsTransient(err):
			s.log.Warn("details fetch failed on the provider's side, batch released", "zpid", zpid, "error", err)
			s.releaseDetails(ctx, zpids[i:])
			wait = s.detailsFailed()
			break batch

		default:
			// Undecodable record, a 4xx that will never succeed: this row's
			// own problem. One attempt, and what is left of the lease is its
			// backoff.
			s.log.Warn("details fetch failed, attempt counted", "zpid", zpid, "error", err)
			s.failDetails(ctx, zpid)
			failed++
		}
	}

	level := slog.LevelInfo
	if fetched == 0 && failed == 0 {
		level = slog.LevelDebug
	}
	s.log.Log(ctx, level, "details enrichment finished",
		"fetched", fetched, "failed", failed, "claimed", len(zpids), "cap", limit)
	return wait
}

// storeDetails records a fetched (or definitively absent) record and reports
// whether it was stored. It runs on a bookkeeping context: the record has
// been paid for, and a shutdown must not throw it away. A store error is the
// row's own (a value PostgreSQL refuses), so it costs the row an attempt.
func (s *Scheduler) storeDetails(ctx context.Context, zpid string, d *property.Details, raw []byte) bool {
	bctx, cancel := s.bookkeeping(ctx)
	defer cancel()
	if err := s.repo.SetDetails(bctx, zpid, d, raw); err != nil {
		s.log.Error("store details failed", "zpid", zpid, "error", err)
		s.failDetails(ctx, zpid)
		return false
	}
	s.detailsFailures = 0
	return true
}

func (s *Scheduler) failDetails(ctx context.Context, zpid string) {
	bctx, cancel := s.bookkeeping(ctx)
	defer cancel()
	if err := s.repo.FailDetails(bctx, zpid); err != nil {
		s.log.Error("count details attempt failed", "zpid", zpid, "error", err)
	}
}

func (s *Scheduler) releaseDetails(ctx context.Context, zpids []string) {
	bctx, cancel := s.bookkeeping(ctx)
	defer cancel()
	if err := s.repo.ReleaseDetails(bctx, zpids); err != nil {
		s.log.Error("release details failed", "count", len(zpids), "error", err)
	}
}

// detailsFailed counts a consecutive provider failure and returns its pause.
func (s *Scheduler) detailsFailed() time.Duration {
	s.detailsFailures++
	return failurePause(s.detailsFailures)
}
