package scheduler

import (
	"log/slog"
	"sync"
	"time"
)

type breakerState int

const (
	// breakerHalfOpen is the zero value on purpose: a worker boots half-open.
	breakerHalfOpen breakerState = iota
	breakerClosed
	breakerOpen
)

func (s breakerState) String() string {
	switch s {
	case breakerClosed:
		return "closed"
	case breakerOpen:
		return "open"
	default:
		return "half-open"
	}
}

// breaker keeps a broken box (blocked IP, full disk, dead ffmpeg) from eating
// the queue. Such a box fails fast, so without it a broken instance would win
// most claims in the fleet and spend every listing's three attempts.
//
// It starts half-open rather than closed: a box that crash-loops never gets
// to five failures in one life, so every boot has to prove itself with a
// single item before it is given full concurrency.
//
// It is safe for concurrent use: the dispatcher asks allow while item
// goroutines report outcomes.
type breaker struct {
	mu  sync.Mutex
	now func() time.Time
	log *slog.Logger

	state       breakerState
	consecutive int       // failures in a row while closed
	openings    int       // openings in a row without a success: the pause exponent
	openUntil   time.Time // end of the pause; meaningful while open
	probing     bool      // the half-open probe has been handed out and not reported back
}

func newBreaker(now func() time.Time, log *slog.Logger) *breaker {
	return &breaker{now: now, log: log}
}

// allow reports how many items may be claimed now, given that slots exist.
// Closed: all of them. Open: none, and wait is what is left of the pause.
// Half-open: one, the probe, which allow reserves there and then; while it is
// out the answer is none with a zero wait, and the caller polls.
//
// Whoever is handed the probe must end it with success, failure or
// inconclusive, or the breaker waits for it forever.
func (b *breaker) allow(slots int) (n int, wait time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.elapseLocked()
	switch b.state {
	case breakerClosed:
		return slots, 0
	case breakerOpen:
		return 0, b.openUntil.Sub(b.now())
	}
	if b.probing || slots < 1 {
		return 0, 0
	}
	b.probing = true
	return 1, 0
}

// success records an item that went through the whole pipeline: the box
// works, so the breaker closes whatever state it was in, and the history that
// lengthens the pauses is forgotten.
func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.elapseLocked()
	if b.state != breakerClosed {
		b.log.Info("media breaker closed, claiming at full concurrency", "was", b.state.String())
	}
	b.state = breakerClosed
	b.consecutive = 0
	b.openings = 0
	b.probing = false
}

// failure records an infrastructure failure and reports whether the breaker
// was NOT closed when it arrived. That is the attempt-refund rule: a failure
// on a box already known to be suspect says nothing about the item, so the
// item gets its attempt back and only waits out the backoff.
func (b *breaker) failure() (refund bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.elapseLocked()
	switch b.state {
	case breakerClosed:
		b.consecutive++
		if b.consecutive >= breakerThreshold {
			b.openLocked("consecutive failures")
		}
		return false
	case breakerHalfOpen:
		b.openLocked("probe failed")
		return true
	default:
		// Stragglers claimed before it opened. The pause is not extended:
		// they are the same outage, not a new one.
		return true
	}
}

// inconclusive ends a probe that proved nothing (nothing was claimed for it,
// or the item was released on shutdown), so that the next allow hands out
// another one. The state is left alone.
func (b *breaker) inconclusive() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}

// healthy is false while the breaker is open: what a ROLE=worker /healthz
// reports. Half-open counts as healthy, or a booting box would start life
// unhealthy.
func (b *breaker) healthy() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.elapseLocked()
	return b.state != breakerOpen
}

// stateName is the state for the status line.
func (b *breaker) stateName() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.elapseLocked()
	return b.state.String()
}

// elapseLocked turns an open breaker whose pause has run out half-open. There
// is no timer: the state is brought up to date whenever somebody looks at it.
func (b *breaker) elapseLocked() {
	if b.state != breakerOpen || b.now().Before(b.openUntil) {
		return
	}
	b.state = breakerHalfOpen
	b.probing = false
	b.log.Info("media breaker half-open, probing with one item")
}

func (b *breaker) openLocked(reason string) {
	pause := breakerMaxPause
	// Past 4 doublings the cap has long won; not shifting further also keeps
	// a box that stays broken for weeks from overflowing the duration.
	if b.openings < 4 {
		pause = min(breakerBasePause<<b.openings, breakerMaxPause)
	}
	b.openings++
	b.state = breakerOpen
	b.openUntil = b.now().Add(pause)
	b.consecutive = 0
	b.probing = false
	b.log.Warn("media breaker opened, claiming paused",
		"reason", reason, "pause", pause.String(), "openings", b.openings)
}
