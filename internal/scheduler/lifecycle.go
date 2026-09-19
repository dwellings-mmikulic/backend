package scheduler

import (
	"context"
	"errors"
	"time"
)

// Start launches the three worker loops and the status logger and returns at
// once. Nothing is scheduled any more: CRON_SCHEDULE only defines the budget
// windows, and the loops run until ctx is cancelled or Stop is called.
//
// It can be started once. A second Start would double every loop under the
// same owner, which the claims would survive but the concurrency limit not.
func (s *Scheduler) Start(ctx context.Context) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.started {
		return errors.New("scheduler: already started")
	}
	s.started = true

	ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Go(func() { s.loop(ctx, s.discoverStep) })
	s.wg.Go(func() { s.mediaLoop(ctx) })
	s.wg.Go(func() { s.loop(ctx, s.detailsStep) })
	s.wg.Go(func() { s.statusLoop(ctx) })

	// main writes the one "effective fleet config" line; this is only what
	// the scheduler made of it.
	s.log.Debug("scheduler loops launched",
		"listing_concurrency", s.listingSlots(),
		"search_limit", s.searchLimit(), "details_limit", s.detailsLimit(),
		"queue_high_water", s.cfg.QueueHighWater, "video", s.videoWanted())
	return nil
}

// Stop ends the loops and waits for every goroutine Start launched, the
// listings in flight included: they are cancelled, release their claims on a
// bookkeeping context and only then let Stop return. It must be given the time
// for that (stop_grace_period), or the claims wait for their leases instead.
//
// main cancels Start's context first, and Stop then only waits. It cancels
// too, so a Stop without that cancellation ends the loops instead of waiting
// for them forever.
func (s *Scheduler) Stop() {
	s.lifecycle.Lock()
	cancel := s.cancel
	s.lifecycle.Unlock()

	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// RunOnce is one synchronous pass: discover until discovery would wait, drain
// the queue, enrich details until that would wait. It leaves no goroutine
// behind and never sleeps, which is what makes it the tests' entry point (and
// usable from a one-shot tool). It is not meant to run next to Start.
func (s *Scheduler) RunOnce(ctx context.Context) {
	s.stepUntilWait(ctx, s.discoverStep)
	s.drainQueue(ctx)
	s.stepUntilWait(ctx, s.detailsStep)
}

func (s *Scheduler) stepUntilWait(ctx context.Context, step func(context.Context) time.Duration) {
	for ctx.Err() == nil {
		if step(ctx) > 0 {
			return
		}
	}
}

// loop drives one step function: a step does one unit of work and says how
// long to wait before the next (0: go again now). Keeping the waiting out of
// the steps is what lets tests drive them without a clock.
func (s *Scheduler) loop(ctx context.Context, step func(context.Context) time.Duration) {
	for ctx.Err() == nil {
		if wait := step(ctx); wait > 0 {
			s.sleep(ctx, wait)
		}
	}
}

// statusLoop writes one status line per interval, so that "is this box doing
// anything" can be answered from the logs of a worker that serves no API.
func (s *Scheduler) statusLoop(ctx context.Context) {
	for {
		s.sleep(ctx, statusInterval)
		if ctx.Err() != nil {
			return
		}
		s.logStatus(ctx)
	}
}

func (s *Scheduler) logStatus(ctx context.Context) {
	attrs := []any{
		"in_flight", s.inFlight.Load(),
		"completed", s.completed.Load(),
		"failed", s.failed.Load(),
		"breaker", s.breaker.stateName(),
	}
	if depth, err := s.queue.Depth(ctx); err != nil {
		attrs = append(attrs, "queue_depth", -1, "queue_depth_error", err.Error())
	} else {
		attrs = append(attrs, "queue_depth", depth)
	}
	s.log.Info("worker status", attrs...)
}
