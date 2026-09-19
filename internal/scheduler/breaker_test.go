package scheduler

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is the injectable clock every test shares with its fakes, so
// leases, backoffs and breaker pauses elapse when the test says so and never
// by sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestBreaker(clock *fakeClock) *breaker {
	return newBreaker(clock.now, testLogger())
}

// fail records n failures in a row and returns what the last one reported.
func failTimes(b *breaker, n int) (refund bool) {
	for i := 0; i < n; i++ {
		refund = b.failure()
	}
	return refund
}

func TestBreaker_BootsHalfOpenAndAllowsExactlyOneProbe(t *testing.T) {
	b := newTestBreaker(newFakeClock())

	if !b.healthy() {
		t.Error("a half-open breaker must report healthy: only an open one takes the box out")
	}
	if got := b.stateName(); got != "half-open" {
		t.Errorf("state at boot = %q, want half-open", got)
	}
	n, wait := b.allow(4)
	if n != 1 || wait != 0 {
		t.Fatalf("allow(4) at boot = (%d, %s), want (1, 0): exactly one probe", n, wait)
	}
	// The probe is in flight: nothing else may be claimed until it reports.
	n, wait = b.allow(4)
	if n != 0 || wait != 0 {
		t.Errorf("allow(4) with a probe in flight = (%d, %s), want (0, 0)", n, wait)
	}
}

func TestBreaker_SuccessClosesForFullConcurrency(t *testing.T) {
	b := newTestBreaker(newFakeClock())
	b.allow(4)
	b.success()

	if got := b.stateName(); got != "closed" {
		t.Errorf("state after a successful probe = %q, want closed", got)
	}
	for i := 0; i < 3; i++ { // closed never reserves anything
		if n, wait := b.allow(4); n != 4 || wait != 0 {
			t.Fatalf("allow(4) while closed = (%d, %s), want (4, 0)", n, wait)
		}
	}
}

func TestBreaker_InconclusiveProbeFreesTheSlotForAnother(t *testing.T) {
	b := newTestBreaker(newFakeClock())
	b.allow(4)
	b.inconclusive() // the probe was released (shutdown, nothing claimed, …)

	if got := b.stateName(); got != "half-open" {
		t.Errorf("state = %q, want half-open: an inconclusive probe proves nothing", got)
	}
	if n, _ := b.allow(4); n != 1 {
		t.Errorf("allow(4) after an inconclusive probe = %d, want 1 (a new probe)", n)
	}
}

func TestBreaker_ThresholdConsecutiveFailuresOpenIt(t *testing.T) {
	clock := newFakeClock()
	b := newTestBreaker(clock)
	b.allow(4)
	b.success()

	for i := 1; i < breakerThreshold; i++ {
		if refund := b.failure(); refund {
			t.Fatalf("failure %d while closed reported refund: the attempt must stay spent", i)
		}
		if !b.healthy() {
			t.Fatalf("breaker opened after %d failures, want %d", i, breakerThreshold)
		}
	}
	if refund := b.failure(); refund {
		t.Error("the failure that opens the breaker was recorded while closed: no refund")
	}
	if b.healthy() {
		t.Fatal("breaker still healthy after the threshold")
	}
	if got := b.stateName(); got != "open" {
		t.Errorf("state = %q, want open", got)
	}
	if n, wait := b.allow(4); n != 0 || wait != time.Minute {
		t.Errorf("allow(4) while open = (%d, %s), want (0, 1m0s)", n, wait)
	}

	// The wait shrinks as the pause runs down …
	clock.advance(45 * time.Second)
	if n, wait := b.allow(4); n != 0 || wait != 15*time.Second {
		t.Errorf("allow(4) 45s into the pause = (%d, %s), want (0, 15s)", n, wait)
	}
	// … and once it is over the breaker is half-open: one probe.
	clock.advance(15 * time.Second)
	if !b.healthy() {
		t.Error("breaker must be healthy again once the pause has elapsed")
	}
	if n, wait := b.allow(4); n != 1 || wait != 0 {
		t.Errorf("allow(4) after the pause = (%d, %s), want (1, 0)", n, wait)
	}
	if got := b.stateName(); got != "half-open" {
		t.Errorf("state after the pause = %q, want half-open", got)
	}
}

func TestBreaker_SuccessResetsTheConsecutiveCount(t *testing.T) {
	b := newTestBreaker(newFakeClock())
	b.allow(4)
	b.success()

	failTimes(b, breakerThreshold-1)
	b.success()
	failTimes(b, breakerThreshold-1)
	if !b.healthy() {
		t.Error("failures separated by a success are not consecutive: breaker must stay closed")
	}
}

func TestBreaker_FailureWhileNotClosedRefundsTheAttempt(t *testing.T) {
	clock := newFakeClock()
	b := newTestBreaker(clock)

	b.allow(4)
	if refund := b.failure(); !refund {
		t.Error("a failed half-open probe must refund the attempt")
	}
	if b.healthy() {
		t.Fatal("a failed probe must re-open the breaker")
	}
	// Items that were already in flight when it opened report while open.
	if refund := b.failure(); !refund {
		t.Error("a failure recorded while open must refund the attempt")
	}
	if n, wait := b.allow(4); n != 0 || wait != time.Minute {
		t.Errorf("allow(4) = (%d, %s), want (0, 1m0s): a failure while open must not extend the pause", n, wait)
	}
}

func TestBreaker_PauseDoublesPerConsecutiveOpeningUpToTheCap(t *testing.T) {
	clock := newFakeClock()
	b := newTestBreaker(clock)

	want := []time.Duration{
		1 * time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		15 * time.Minute, 15 * time.Minute, 15 * time.Minute,
	}
	for i, pause := range want {
		if n, _ := b.allow(4); n != 1 {
			t.Fatalf("opening %d: allow = %d, want a probe", i, n)
		}
		b.failure()
		n, wait := b.allow(4)
		if n != 0 || wait != pause {
			t.Fatalf("opening %d: allow(4) = (%d, %s), want (0, %s)", i, n, wait, pause)
		}
		clock.advance(pause)
	}

	// A success wipes the history: the next opening starts over at 1m.
	b.allow(4)
	b.success()
	failTimes(b, breakerThreshold)
	if n, wait := b.allow(4); n != 0 || wait != time.Minute {
		t.Errorf("first opening after a success: allow(4) = (%d, %s), want (0, 1m0s)", n, wait)
	}
}

func TestBreaker_SuccessWhileOpenClosesIt(t *testing.T) {
	b := newTestBreaker(newFakeClock())
	b.allow(4)
	b.failure() // open

	// An item claimed before the breaker opened finishes fine: the box works.
	b.success()
	if n, _ := b.allow(4); n != 4 {
		t.Errorf("allow(4) after a success = %d, want 4", n)
	}
}

func TestBreaker_ConcurrentUseIsRaceFree(t *testing.T) {
	clock := newFakeClock()
	b := newTestBreaker(clock)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			for j := 0; j < 200; j++ {
				switch (i + j) % 5 {
				case 0:
					b.success()
				case 1:
					b.failure()
				case 2:
					b.allow(4)
				case 3:
					b.inconclusive()
				default:
					_ = b.healthy()
					_ = b.stateName()
				}
			}
		})
	}
	wg.Wait()
}
