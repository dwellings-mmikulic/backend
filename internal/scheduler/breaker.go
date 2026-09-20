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

// probeID identifies the one item the breaker hands out while half-open. The
// zero value means "not the probe": every other item — a straggler claimed
// while the breaker was still closed, or anything claimed at full concurrency
// — carries it. Ids are never reused, so a straggler that finishes long after
// the breaker changed state cannot be mistaken for the probe of the moment.
type probeID uint64

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
	probe       probeID   // the probe handed out and not reported back; 0 = none
	lastProbe   probeID   // the last id issued; ids are never reused
}

func newBreaker(now func() time.Time, log *slog.Logger) *breaker {
	return &breaker{now: now, log: log}
}

// allow reports how many items may be claimed now, given that slots exist, and
// — when the one item is the half-open probe — its id.
// Closed: all of them, probe 0. Open: none, and wait is what is left of the
// pause. Half-open: one, the probe, which allow reserves there and then; while
// it is out the answer is none with a zero wait, and the caller polls.
//
// Whoever is handed the probe must end it with success, failure or
// inconclusive under that id, or the breaker waits for it forever.
func (b *breaker) allow(slots int) (n int, probe probeID, wait time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.elapseLocked()
	switch b.state {
	case breakerClosed:
		return slots, 0, 0
	case breakerOpen:
		return 0, 0, b.openUntil.Sub(b.now())
	}
	if b.probe != 0 || slots < 1 {
		return 0, 0, 0
	}
	b.lastProbe++
	b.probe = b.lastProbe
	return 1, b.probe, 0
}

// success records an item that went through the whole pipeline: the box
// works, so the breaker closes whatever state it was in, and the history that
// lengthens the pauses is forgotten. A straggler proves the box works just as
// well as the probe does, so the id is not looked at.
func (b *breaker) success(probeID) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.elapseLocked()
	if b.state != breakerClosed {
		b.log.Info("media breaker closed, claiming at full concurrency", "was", b.state.String())
	}
	b.state = breakerClosed
	b.consecutive = 0
	b.openings = 0
	b.probe = 0
}

// failure records an infrastructure failure and reports whether the breaker
// was NOT closed when it arrived. That is the attempt-refund rule: a failure
// on a box already known to be suspect says nothing about the item, so the
// item gets its attempt back and only waits out the backoff.
//
// Only the probe's own failure is the probe's verdict. A straggler failing
// while half-open is refunded like any other, but it must not re-open the
// breaker or free the probe's place: doing so would put a second item out next
// to the probe that is still running.
func (b *breaker) failure(p probeID) (refund bool) {
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
		if !b.isProbeLocked(p) {
			return true
		}
		b.openLocked("probe failed")
		return true
	default:
		// Stragglers claimed before it opened. The pause is not extended:
		// they are the same outage, not a new one.
		return true
	}
}

// inconclusive ends a probe that proved nothing (nothing was claimed for it,
// or the item was released on shutdown, or it only read the database), so that
// the next allow hands out another one. The state is left alone.
//
// From anything that is not the probe of the moment it does nothing at all: a
// skip or a shutdown release elsewhere says nothing about the probe in flight.
func (b *breaker) inconclusive(p probeID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.isProbeLocked(p) {
		b.probe = 0
	}
}

// isProbeLocked reports whether p is the probe currently in flight.
func (b *breaker) isProbeLocked(p probeID) bool {
	return p != 0 && p == b.probe
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
	b.probe = 0
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
	b.probe = 0
	b.log.Warn("media breaker opened, claiming paused",
		"reason", reason, "pause", pause.String(), "openings", b.openings)
}
