package scheduler

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/workqueue"
)

// listingSlots is how many listings this instance processes side by side.
func (s *Scheduler) listingSlots() int {
	return max(s.cfg.Concurrency.Listings, 1)
}

// mediaLoop is the dispatcher: it keeps the slots full for as long as the
// breaker lets it, and sleeps when there is nothing to claim. It is not
// budget-gated: media work costs CPU and bandwidth, not API quota.
func (s *Scheduler) mediaLoop(ctx context.Context) {
	slots := make(chan struct{}, s.listingSlots())
	for ctx.Err() == nil {
		wait := s.dispatch(ctx, slots, func(it workqueue.Item) {
			s.launch(ctx, &s.wg, slots, it, nil)
		})
		if wait > 0 {
			s.sleep(ctx, wait)
		}
	}
}

// drainQueue is the media half of RunOnce: the same dispatcher, run
// synchronously until a claim returns nothing and nothing is in flight. It
// does not wait out an open breaker; it returns with the queue as it is.
func (s *Scheduler) drainQueue(ctx context.Context) {
	slots := make(chan struct{}, s.listingSlots())
	var (
		wg       sync.WaitGroup
		launched int64        // touched by this goroutine only
		finished atomic.Int64 // items that ran to the end and gave their slot back
	)
	wake := make(chan struct{}, 1)
	itemDone := func() {
		finished.Add(1)
		select {
		case wake <- struct{}{}:
		default: // a wake-up is already pending; one is enough
		}
	}

	for ctx.Err() == nil {
		// Read before dispatch decides: if an item finishes in between, the
		// decision ("nothing to claim", "a probe is still out") may already
		// be stale, and must be taken again rather than acted on.
		seen := finished.Load()
		wait := s.dispatch(ctx, slots, func(it workqueue.Item) {
			launched++
			s.launch(ctx, &wg, slots, it, itemDone)
		})
		if wait == 0 || finished.Load() != seen {
			continue
		}
		if launched == seen {
			break // nothing claimable and nothing in flight: drained
		}
		select {
		case <-wake:
		case <-ctx.Done():
		}
	}
	wg.Wait()
}

// dispatch claims as many items as the breaker and the free slots allow,
// hands each to run, and returns how long to wait before dispatching again
// (0: go again now). run must start the item and give its slot back when the
// item is done; launch does both.
//
// The first slot is waited for BEFORE the breaker is asked. The other way
// round, an allowance granted while every slot was busy would still be acted
// on after those very items had failed and opened the breaker.
func (s *Scheduler) dispatch(ctx context.Context, slots chan struct{}, run func(workqueue.Item)) time.Duration {
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return 0
	}
	held := 1
	handBack := func(n int) {
		for ; n > 0; n-- {
			<-slots
		}
	}

	allowed, wait := s.breaker.allow(cap(slots))
	if allowed == 0 {
		handBack(held)
		if wait <= 0 {
			wait = queuePoll // a probe is out: look again soon
		}
		return wait
	}
	// The rest only if they are free right now: a busy slot is a listing
	// being rendered, and there is nothing to gain from waiting for it here.
fill:
	for held < allowed {
		select {
		case slots <- struct{}{}:
			held++
		default:
			break fill
		}
	}

	items, err := s.queue.Claim(ctx, s.owner, held, listingLease)
	if err != nil && ctx.Err() == nil {
		s.log.Error("claim listings failed", "error", err)
	}
	if len(items) > held {
		// Cannot happen with a queue that honours its limit. Work only on
		// what there are slots for; the rest frees itself with the lease.
		s.log.Error("queue returned more items than were asked for",
			"asked", held, "got", len(items))
		items = items[:held]
	}
	handBack(held - len(items))
	if len(items) == 0 {
		// A probe reserved for an item that does not exist must not block
		// the next one.
		s.breaker.inconclusive()
		return queuePoll
	}
	for _, it := range items {
		run(it)
	}
	return 0
}

// launch runs one claimed item in its own goroutine, tracked by wg, and gives
// the item's slot back when it is done. done, when set, runs after that.
func (s *Scheduler) launch(ctx context.Context, wg *sync.WaitGroup, slots chan struct{}, it workqueue.Item, done func()) {
	wg.Go(func() {
		defer func() {
			<-slots
			if done != nil {
				done()
			}
		}()
		s.processItem(ctx, it)
	})
}

// processItem works on one claimed item and records exactly one transition
// for it. ctx is the loop's context: its cancellation means shutdown. The
// work itself runs under the listing deadline, which is shorter than the
// lease, so a live worker never works on an expired claim.
func (s *Scheduler) processItem(ctx context.Context, it workqueue.Item) {
	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)

	p, revisit, err := DecodeListing(it.Payload)
	if err != nil {
		// Nothing will ever make this payload decodable. It is parked until
		// its attempts run out and then stays dead for inspection. It says
		// nothing about this box, so the breaker is not told.
		s.log.Error("undecodable queue payload", "zpid", it.ZPID, "attempt", it.Attempts, "error", err)
		s.failed.Add(1)
		s.breaker.inconclusive()
		s.failItem(ctx, it, err, poisonRetry, false)
		return
	}
	// Decided before the pipeline replaces the source URLs with CDN ones.
	noSourcePhotos := len(p.ImageURLs) == 0

	ictx, cancel := context.WithTimeoutCause(ctx, listingDeadline, errListingDeadline)
	skipped, err := s.processListingSafely(ictx, p, revisit)
	cancel()

	switch {
	case err == nil:
		s.completed.Add(1)
		if skipped || noSourcePhotos {
			// Done, but no proof that this box can download, render and
			// upload: a skip only read the database, and a listing without
			// photos only wrote to it. Closing the breaker on that would let
			// a broken box back to full concurrency.
			s.breaker.inconclusive()
		} else {
			s.breaker.success()
		}
		bctx, cancel := s.bookkeeping(ctx)
		defer cancel()
		if err := s.queue.Complete(bctx, it, s.owner); err != nil {
			s.transitionFailed("complete listing", err, "zpid", it.ZPID)
		}

	case ctx.Err() != nil:
		// Shutdown. Whatever the error says, it is not a verdict on the item
		// or on this box: the attempt is refunded and the item is claimable
		// again at once, by a box that is staying up.
		s.breaker.inconclusive()
		s.log.Info("listing released on shutdown", "zpid", it.ZPID)
		s.releaseItem(ctx, it, 0)

	case errors.Is(err, errVideoDisabledRevisit):
		// Completing it would lose the backfill request; failing it would
		// spend its attempts on boxes that can never do it.
		s.breaker.inconclusive()
		s.log.Info("revisit item released, this worker renders no video",
			"zpid", it.ZPID, "retry_in", revisitNoVideoDelay.String())
		s.releaseItem(ctx, it, revisitNoVideoDelay)

	default:
		s.failed.Add(1)
		refund := s.breaker.failure()
		backoff := listingBackoff(it.Attempts)
		s.log.Error("listing failed", "zpid", it.ZPID, "attempt", it.Attempts,
			"retry_in", backoff.String(), "attempt_refunded", refund, "error", err)
		s.failItem(ctx, it, err, backoff, refund)
	}
}

// processListingSafely turns a panic anywhere in the pipeline into an error.
// One listing that trips a bug (a nil map in overlay code, an image the
// decoder chokes on) must cost that item an attempt, not the process — and
// with it every other listing in flight.
func (s *Scheduler) processListingSafely(ctx context.Context, p *property.Property, revisit bool) (skipped bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("listing processing panicked", "zpid", p.ZPID,
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			skipped, err = false, fmt.Errorf("panic: %v", r)
		}
	}()
	return s.processListing(ctx, p, revisit)
}

// listingBackoff is how long a failed item waits: 5 min after the first
// attempt, 30 min after every later one.
func listingBackoff(attempts int) time.Duration {
	if attempts <= 1 {
		return listingRetryFirst
	}
	return listingRetryLater
}

func (s *Scheduler) failItem(ctx context.Context, it workqueue.Item, cause error, retryAfter time.Duration, refund bool) {
	bctx, cancel := s.bookkeeping(ctx)
	defer cancel()
	if err := s.queue.Fail(bctx, it, s.owner, errorText(cause), retryAfter, refund); err != nil {
		s.transitionFailed("fail listing", err, "zpid", it.ZPID)
	}
}

func (s *Scheduler) releaseItem(ctx context.Context, it workqueue.Item, delay time.Duration) {
	bctx, cancel := s.bookkeeping(ctx)
	defer cancel()
	if err := s.queue.Release(bctx, it, s.owner, delay); err != nil {
		s.transitionFailed("release listing", err, "zpid", it.ZPID)
	}
}

// errorText makes an error storable: PostgreSQL refuses a NUL and invalid
// UTF-8 in text, and a Fail that is refused would leave the item claimed
// until its lease runs out.
func errorText(err error) string {
	msg := strings.ToValidUTF8(stripNUL(err.Error()), "?")
	if len(msg) > maxErrorLen {
		msg = strings.ToValidUTF8(msg[:maxErrorLen], "") + "…"
	}
	return msg
}
