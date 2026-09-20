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
// They come from ordinary items, not from the probe.
func failTimes(b *breaker, n int) (refund bool) {
	for i := 0; i < n; i++ {
		refund = b.failure(0)
	}
	return refund
}

// takeProbe reserves the half-open probe and returns its id.
func takeProbe(t *testing.T, b *breaker, slots int) probeID {
	t.Helper()
	n, p, _ := b.allow(slots)
	if n != 1 || p == 0 {
		t.Fatalf("allow(%d) = (%d, %d), want one probe with an id", slots, n, p)
	}
	return p
}

func TestBreaker_BootsHalfOpenAndAllowsExactlyOneProbe(t *testing.T) {
	b := newTestBreaker(newFakeClock())

	if !b.healthy() {
		t.Error("a half-open breaker must report healthy: only an open one takes the box out")
	}
	if got := b.stateName(); got != "half-open" {
		t.Errorf("state at boot = %q, want half-open", got)
	}
	n, probe, wait := b.allow(4)
	if n != 1 || probe == 0 || wait != 0 {
		t.Fatalf("allow(4) at boot = (%d, %d, %s), want one probe with an id and no wait", n, probe, wait)
	}
	// The probe is in flight: nothing else may be claimed until it reports.
	n, probe2, wait := b.allow(4)
	if n != 0 || probe2 != 0 || wait != 0 {
		t.Errorf("allow(4) with a probe in flight = (%d, %d, %s), want (0, 0, 0)", n, probe2, wait)
	}
}

func TestBreaker_SuccessClosesForFullConcurrency(t *testing.T) {
	b := newTestBreaker(newFakeClock())
	b.success(takeProbe(t, b, 4))

	if got := b.stateName(); got != "closed" {
		t.Errorf("state after a successful probe = %q, want closed", got)
	}
	for i := 0; i < 3; i++ { // closed never reserves anything
		n, probe, wait := b.allow(4)
		if n != 4 || wait != 0 {
			t.Fatalf("allow(4) while closed = (%d, %s), want (4, 0)", n, wait)
		}
		if probe != 0 {
			t.Fatalf("allow(4) while closed handed out probe %d: there is no probe while closed", probe)
		}
	}
}

func TestBreaker_InconclusiveProbeFreesTheSlotForAnother(t *testing.T) {
	b := newTestBreaker(newFakeClock())
	p := takeProbe(t, b, 4)
	b.inconclusive(p) // the probe was released (shutdown, nothing claimed, …)

	if got := b.stateName(); got != "half-open" {
		t.Errorf("state = %q, want half-open: an inconclusive probe proves nothing", got)
	}
	if n, p2, _ := b.allow(4); n != 1 || p2 == p {
		t.Errorf("allow(4) after an inconclusive probe = (%d, %d), want 1 with a fresh id", n, p2)
	}
}

func TestBreaker_ThresholdConsecutiveFailuresOpenIt(t *testing.T) {
	clock := newFakeClock()
	b := newTestBreaker(clock)
	b.success(takeProbe(t, b, 4))

	for i := 1; i < breakerThreshold; i++ {
		if refund := b.failure(0); refund {
			t.Fatalf("failure %d while closed reported refund: the attempt must stay spent", i)
		}
		if !b.healthy() {
			t.Fatalf("breaker opened after %d failures, want %d", i, breakerThreshold)
		}
	}
	if refund := b.failure(0); refund {
		t.Error("the failure that opens the breaker was recorded while closed: no refund")
	}
	if b.healthy() {
		t.Fatal("breaker still healthy after the threshold")
	}
	if got := b.stateName(); got != "open" {
		t.Errorf("state = %q, want open", got)
	}
	if n, _, wait := b.allow(4); n != 0 || wait != time.Minute {
		t.Errorf("allow(4) while open = (%d, %s), want (0, 1m0s)", n, wait)
	}

	// The wait shrinks as the pause runs down …
	clock.advance(45 * time.Second)
	if n, _, wait := b.allow(4); n != 0 || wait != 15*time.Second {
		t.Errorf("allow(4) 45s into the pause = (%d, %s), want (0, 15s)", n, wait)
	}
	// … and once it is over the breaker is half-open: one probe.
	clock.advance(15 * time.Second)
	if !b.healthy() {
		t.Error("breaker must be healthy again once the pause has elapsed")
	}
	if n, _, wait := b.allow(4); n != 1 || wait != 0 {
		t.Errorf("allow(4) after the pause = (%d, %s), want (1, 0)", n, wait)
	}
	if got := b.stateName(); got != "half-open" {
		t.Errorf("state after the pause = %q, want half-open", got)
	}
}

func TestBreaker_SuccessResetsTheConsecutiveCount(t *testing.T) {
	b := newTestBreaker(newFakeClock())
	b.success(takeProbe(t, b, 4))

	failTimes(b, breakerThreshold-1)
	b.success(0)
	failTimes(b, breakerThreshold-1)
	if !b.healthy() {
		t.Error("failures separated by a success are not consecutive: breaker must stay closed")
	}
}

func TestBreaker_FailureWhileNotClosedRefundsTheAttempt(t *testing.T) {
	clock := newFakeClock()
	b := newTestBreaker(clock)

	if refund := b.failure(takeProbe(t, b, 4)); !refund {
		t.Error("a failed half-open probe must refund the attempt")
	}
	if b.healthy() {
		t.Fatal("a failed probe must re-open the breaker")
	}
	// Items that were already in flight when it opened report while open.
	if refund := b.failure(0); !refund {
		t.Error("a failure recorded while open must refund the attempt")
	}
	if n, _, wait := b.allow(4); n != 0 || wait != time.Minute {
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
		n, p, _ := b.allow(4)
		if n != 1 || p == 0 {
			t.Fatalf("opening %d: allow = (%d, %d), want a probe", i, n, p)
		}
		b.failure(p)
		n, _, wait := b.allow(4)
		if n != 0 || wait != pause {
			t.Fatalf("opening %d: allow(4) = (%d, %s), want (0, %s)", i, n, wait, pause)
		}
		clock.advance(pause)
	}

	// A success wipes the history: the next opening starts over at 1m.
	b.success(takeProbe(t, b, 4))
	failTimes(b, breakerThreshold)
	if n, _, wait := b.allow(4); n != 0 || wait != time.Minute {
		t.Errorf("first opening after a success: allow(4) = (%d, %s), want (0, 1m0s)", n, wait)
	}
}

func TestBreaker_SuccessWhileOpenClosesIt(t *testing.T) {
	b := newTestBreaker(newFakeClock())
	b.failure(takeProbe(t, b, 4)) // open

	// An item claimed before the breaker opened finishes fine: the box works.
	b.success(0)
	if n, _, _ := b.allow(4); n != 4 {
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
					b.success(probeID(j))
				case 1:
					b.failure(probeID(j))
				case 2:
					b.allow(4)
				case 3:
					b.inconclusive(probeID(j))
				default:
					_ = b.healthy()
					_ = b.stateName()
				}
			}
		})
	}
	wg.Wait()
}
