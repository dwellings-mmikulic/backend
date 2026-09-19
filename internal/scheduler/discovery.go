package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dwellingtw/backend/internal/budget"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/workqueue"
	"github.com/dwellingtw/backend/internal/zillow"
	"github.com/dwellingtw/backend/internal/zipcode"
)

// discoverStep searches at most one ZIP and returns how long the discovery
// loop should wait before the next step (0: go again now).
//
// The order of the checks is the order of what they cost. Backpressure and
// the budget are looked up before a ZIP is claimed, so an instance with
// nothing to spend never takes a ZIP away from one that has.
func (s *Scheduler) discoverStep(ctx context.Context) time.Duration {
	// 1. Backpressure. Paid search results must not pile up faster than the
	// fleet renders them. It is a soft limit: every discovering instance can
	// overshoot it by one ZIP.
	if highWater := s.cfg.QueueHighWater; highWater > 0 {
		depth, err := s.queue.Depth(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return 0
			}
			s.log.Error("queue depth check failed, discovering anyway", "error", err)
		case depth >= highWater:
			s.log.Debug("listing queue at high water, discovery paused",
				"depth", depth, "high_water", highWater)
			return backpressureWait
		}
	}

	// 2. The provider's own quota.
	if !s.gateOpen(ctx) {
		return s.untilNextWindow()
	}

	// 3. This window's search budget. The permit is what really enforces it;
	// this check only keeps an instance with nothing to spend from claiming
	// a ZIP just to hand it back. A ledger that cannot be read backs off too:
	// the permit fails closed, so a claim now would only be handed back.
	limit := s.searchLimit()
	if limit <= 0 {
		return s.untilNextWindow()
	}
	window, _ := s.windows.Current(s.now())
	spent, err := s.ledger.Spent(ctx, window, budget.KindSearch)
	switch {
	case err != nil:
		if ctx.Err() != nil {
			return 0
		}
		s.log.Error("search budget lookup failed, discovery backs off", "error", err)
		return noZipWait
	case spent >= limit:
		return s.untilNextWindow()
	}

	// 4. One ZIP, exclusively.
	claim, err := s.zips.Claim(ctx, s.owner, zipLease)
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		s.log.Error("claim zip failed", "error", err)
		return noZipWait
	}
	if claim == nil {
		return noZipWait
	}

	// 5. Search it, under a deadline shorter than the lease. MaxPages stays 0:
	// what ends a search early is the permit, asked before every request,
	// not a page cap worked out from a budget only this process knows.
	criteria := s.cfg.Search
	criteria.Location = claim.Zip
	criteria.MaxPages = 0
	sctx, cancel := context.WithTimeout(ctx, zipDeadline)
	res, searchErr := s.zillow.SearchPages(sctx, criteria, claim.ResumePage, s.permit(budget.KindSearch, limit))
	cancel()

	return s.settleZip(ctx, *claim, res, searchErr)
}

// settleZip records what a search came to: the listings first, then exactly
// one transition of the claim. Everything in here runs on bookkeeping
// contexts, so neither the search deadline nor a shutdown loses pages that
// were already paid for.
func (s *Scheduler) settleZip(ctx context.Context, claim zipcode.Claim, res zillow.SearchResult, searchErr error) time.Duration {
	zip := claim.Zip

	// 6. Whatever came back is enqueued, also next to an error.
	enqueued, skipped, err := s.enqueueDiscovered(ctx, res.Properties, zip)
	if err != nil {
		// The listings are lost to this search. Failing the ZIP with the page
		// the search STARTED at has them fetched again; marking it searched
		// would drop them for a whole pass of the rotation.
		s.log.Error("enqueue discovered listings failed, zip will be searched again",
			"zip", zip, "count", len(res.Properties), "error", err)
		s.failZip(ctx, claim, claim.ResumePage)
		return s.discoveryFailed()
	}
	if searchErr == nil || len(res.Properties) > 0 {
		s.log.Info("properties discovered", "zip", zip,
			"count", len(res.Properties), "pages", res.Requests,
			"enqueued", enqueued, "skipped", skipped)
	}

	// 7. The claim.
	switch {
	case searchErr != nil && ctx.Err() != nil:
		// Shutdown in the middle of the search: not the ZIP's fault.
		s.handBackZip(ctx, claim, res.NextPage)
		return 0

	case searchErr != nil:
		s.log.Error("search failed", "zip", zip, "page", res.NextPage,
			"pages", res.Requests, "error", searchErr)
		if isRateLimited(searchErr) {
			s.invalidateGate()
		}
		page := res.NextPage
		if page <= 0 {
			page = claim.ResumePage
		}
		s.failZip(ctx, claim, page)
		return s.discoveryFailed()

	case res.NextPage > 0 && res.Requests == 0 && len(res.Properties) == 0:
		// Denied before the first request: another instance took the last of
		// the budget after our check, or the ledger could not be reached.
		// Nothing was fetched, so the ZIP goes back exactly as it was claimed
		// — released, not deferred, so a ledger that was only briefly away
		// does not park it until the next window. While the budget really is
		// spent, the budget check keeps every instance off it anyway.
		bctx, cancel := s.bookkeeping(ctx)
		defer cancel()
		if err := s.zips.Release(bctx, claim, s.owner); err != nil {
			s.transitionFailed("release zip", err, "zip", zip)
		}
		return s.untilNextWindow()

	case res.NextPage > 0:
		// The permit said no: the window's budget ran out mid-ZIP. Not a
		// failure: the ZIP resumes at the first unfetched page next window.
		resume := res.NextPage
		_, next := s.windows.Current(s.now())
		bctx, cancel := s.bookkeeping(ctx)
		defer cancel()
		if err := s.zips.Defer(bctx, claim, s.owner, next, resume); err != nil {
			s.transitionFailed("defer zip", err, "zip", zip)
		} else {
			s.log.Info("search budget spent mid-zip, deferred to the next window",
				"zip", zip, "resume_page", resume)
		}
		return s.untilNextWindow()

	default:
		bctx, cancel := s.bookkeeping(ctx)
		defer cancel()
		if err := s.zips.MarkSearched(bctx, claim, s.owner, len(res.Properties)); err != nil {
			s.transitionFailed("mark zip searched", err, "zip", zip)
		}
		s.discoverFailures = 0
		return 0
	}
}

// handBackZip gives up a claim on shutdown without counting a failure. When
// the search had already fetched pages they are in the queue by now, so the
// ZIP is deferred to "now" with the next page as its resume page: claimable
// at once, and the paid pages are not bought again. Release leaves the resume
// page where it was, which is right only when nothing new was fetched.
func (s *Scheduler) handBackZip(ctx context.Context, claim zipcode.Claim, nextPage int) {
	bctx, cancel := s.bookkeeping(ctx)
	defer cancel()
	if nextPage > max(claim.ResumePage, 1) {
		if err := s.zips.Defer(bctx, claim, s.owner, s.now(), nextPage); err != nil {
			s.transitionFailed("defer zip", err, "zip", claim.Zip)
		}
		return
	}
	if err := s.zips.Release(bctx, claim, s.owner); err != nil {
		s.transitionFailed("release zip", err, "zip", claim.Zip)
	}
}

// failZip counts a failure against the ZIP and hides it until the later of
// "in an hour" and the next window, so the rest of the budget goes to the ZIPs
// behind it. (This is what the in-memory "tried" map used to do for one
// process; the hidden claim does it for the fleet and survives a restart.)
func (s *Scheduler) failZip(ctx context.Context, claim zipcode.Claim, resumePage int) {
	now := s.now()
	until := now.Add(zipFailBackoff)
	if _, next := s.windows.Current(now); next.After(until) {
		until = next
	}
	bctx, cancel := s.bookkeeping(ctx)
	defer cancel()
	pushedBack, err := s.zips.Fail(bctx, claim, s.owner, until, resumePage)
	if err != nil {
		s.transitionFailed("fail zip", err, "zip", claim.Zip)
		return
	}
	if pushedBack {
		s.log.Error("zip failed too many times in a row and was pushed back to the end of the rotation: its listings are skipped for this pass",
			"zip", claim.Zip, "failures", zipcode.MaxFailures)
	}
}

// discoveryFailed counts a consecutive failure and returns the pause it earns.
func (s *Scheduler) discoveryFailed() time.Duration {
	s.discoverFailures++
	return failurePause(s.discoverFailures)
}

// failurePause is the pause after k consecutive failures: 5 s, 10 s, 20 s, …
// capped at 5 min. A provider outage must not be hammered, and it must not
// walk through the rotation failing ZIP after ZIP at full speed either.
func failurePause(k int) time.Duration {
	// 5 s << 6 is already past the cap; not shifting further also keeps a
	// long outage from overflowing the duration.
	if k > 6 {
		return failurePauseMax
	}
	return min(failurePauseBase<<max(k-1, 0), failurePauseMax)
}

// enqueueDiscovered puts a search result on the listing queue and reports how
// many rows that created and how many listings the skip-existing filter left
// out. A failure is retried once: the ZIP behind it has been paid for.
func (s *Scheduler) enqueueDiscovered(ctx context.Context, props []property.Property, zip string) (enqueued, skipped int, err error) {
	candidates := make([]*property.Property, 0, len(props))
	for i := range props {
		p := &props[i]
		if stripNUL(p.ZPID) == "" {
			s.log.Error("listing has an empty zpid, dropped", "zip", zip, "address", p.Address)
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return 0, 0, nil
	}

	enqueued, skipped, err = s.enqueueOnce(ctx, candidates, zip)
	if err != nil {
		s.log.Warn("enqueue discovered listings failed, retrying once", "zip", zip, "error", err)
		enqueued, skipped, err = s.enqueueOnce(ctx, candidates, zip)
	}
	return enqueued, skipped, err
}

// enqueueOnce filters the candidates with one batched VideoStates query and
// enqueues the rest in one statement. The filter is the predicate the old
// cycle applied per listing; the media worker checks again when it gets to
// the item, so a stale answer here only costs a no-op queue row.
func (s *Scheduler) enqueueOnce(ctx context.Context, candidates []*property.Property, zip string) (enqueued, skipped int, err error) {
	bctx, cancel := s.bookkeeping(ctx)
	defer cancel()

	// stored maps every zpid that is already in properties to "has no ready
	// video". It is only needed, and only fetched, under SKIP_EXISTING.
	var stored map[string]bool
	if s.cfg.SkipExisting {
		zpids := make([]string, len(candidates))
		for i, p := range candidates {
			zpids[i] = stripNUL(p.ZPID)
		}
		if stored, err = s.repo.VideoStates(bctx, zpids); err != nil {
			return 0, 0, fmt.Errorf("video states: %w", err)
		}
	}

	videoWanted := s.videoWanted()
	items := make([]workqueue.NewItem, 0, len(candidates))
	for _, p := range candidates {
		zpid := stripNUL(p.ZPID)
		if needsVideo, isStored := stored[zpid]; isStored && !(videoWanted && needsVideo) {
			skipped++
			continue
		}
		payload, err := EncodeListing(p, false)
		if err != nil {
			// Deterministic and this listing's own problem: the ZIP's other
			// listings must not pay for it.
			s.log.Error("listing cannot be encoded, dropped", "zip", zip, "zpid", zpid, "error", err)
			continue
		}
		items = append(items, workqueue.NewItem{ZPID: zpid, Payload: payload, SourceZip: zip})
	}
	if len(items) == 0 {
		return 0, skipped, nil
	}

	n, err := s.queue.Enqueue(bctx, items)
	if errors.Is(err, workqueue.ErrUnstorable) {
		// Everything storable is in; the rest would be refused again, so a
		// retry (let alone failing the ZIP) only costs money.
		s.log.Warn("some listings could not be stored in the queue, left out",
			"zip", zip, "enqueued", n, "error", err)
		return n, skipped, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("enqueue: %w", err)
	}
	return n, skipped, nil
}
