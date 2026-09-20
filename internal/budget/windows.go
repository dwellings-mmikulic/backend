// Package budget is the fleet-wide spending control for the paid provider API.
// Windows turns CRON_SCHEDULE into budget windows that every instance computes
// identically from its own clock, and Ledger counts the requests spent inside
// a window in PostgreSQL, so ten instances share one budget and a restart
// joins the current window instead of getting a fresh one.
package budget

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// lookbacks are how far before now the search for the current window's start
// begins. robfig only offers Next, so the start is found by walking forward
// through every activation up to now, and the walk costs one Next per
// activation: a five-year walk of "*/5 * * * *" would be half a million calls.
// Starting an hour back and widening only while nothing is found keeps the
// walk short for dense schedules and still reaches a yearly or leap-day one.
var lookbacks = []time.Duration{
	time.Hour,
	24 * time.Hour,
	31 * 24 * time.Hour,
	366 * 24 * time.Hour,
	5 * 366 * 24 * time.Hour,
}

// fallbackWindow is the window length Current degrades to when the schedule
// cannot answer; see Current.
const fallbackWindow = 24 * time.Hour

// Windows maps an instant to the budget window containing it. A window runs
// from one activation of the schedule to the next; its start is the key of the
// ledger row, so every instance has to arrive at the very same instant.
// Everything is therefore evaluated in UTC, whatever zone the caller's clock
// is in, and nothing depends on when the process started.
//
// It is safe for concurrent use.
type Windows struct {
	// Exactly one of the two is set: a cron expression, or the delay of an
	// "@every" schedule.
	sched cron.Schedule
	every time.Duration

	// Current is asked before every paid request but changes a few times a
	// day: the last answer is served until now leaves [start, next).
	mu          sync.Mutex
	start, next time.Time
}

// ParseWindows parses a standard five-field cron expression or descriptor
// ("@daily", "@every 12h"). A schedule that has no window for the current
// time, such as "0 0 30 2 *" which never activates, is an error: better to
// refuse to start than to run without a budget key. So is an "@every" delay
// that is not a whole, positive number of seconds; see checkEvery.
func ParseWindows(spec string) (*Windows, error) {
	return parseWindowsAt(spec, time.Now())
}

// parseWindowsAt is ParseWindows on an injected clock.
func parseWindowsAt(spec string, now time.Time) (*Windows, error) {
	sched, err := parseStandard(spec)
	if err != nil {
		return nil, fmt.Errorf("parse schedule %q: %w", spec, err)
	}
	if every, ok := sched.(cron.ConstantDelaySchedule); ok {
		if err := checkEvery(spec, every.Delay); err != nil {
			return nil, fmt.Errorf("schedule %q: %w", spec, err)
		}
	}
	w, err := newWindows(sched)
	if err != nil {
		return nil, fmt.Errorf("schedule %q: %w", spec, err)
	}
	now = now.UTC()
	start, next, ok := w.compute(now)
	if !ok {
		return nil, fmt.Errorf("schedule %q has no window containing %s: it never activates, or only years apart",
			spec, now.Format(time.RFC3339))
	}
	w.start, w.next = start, next
	return w, nil
}

// parseStandard is cron.ParseStandard, which slices out of range on a spec
// that is a time zone and nothing else ("TZ=UTC"). CRON_SCHEDULE is typed by
// an operator; a typo should be a config error, not a stack trace.
func parseStandard(spec string) (sched cron.Schedule, err error) {
	defer func() {
		if r := recover(); r != nil {
			sched, err = nil, fmt.Errorf("malformed spec: %v", r)
		}
	}()
	return cron.ParseStandard(spec)
}

// everyPrefix introduces robfig's constant-delay descriptor.
const everyPrefix = "@every "

// checkEvery refuses an "@every" spec whose delay robfig does not run as
// written. cron.Every silently raises anything below a second to one second
// and drops the fraction of a second, so "@every -5h", "@every 0h" and
// "@every 100ms" all parse, into one-second windows: a typo that hands the
// fleet a fresh budget every second, and costs money instead of failing to
// start. delay is what the parser made of spec.
func checkEvery(spec string, delay time.Duration) error {
	// The parser only got to a constant delay by reading the text after the
	// descriptor (an optional "TZ=... " comes before it) as a duration, so
	// reading it again cannot fail; if it somehow does, the delay is
	// unverified and refused all the same.
	_, text, found := strings.Cut(spec, everyPrefix)
	written, err := time.ParseDuration(strings.TrimSpace(text))
	if !found || err != nil {
		return fmt.Errorf("cannot read the @every delay back to verify it (parsed as %s)", delay)
	}
	if written != delay {
		return fmt.Errorf("@every %s would run as @every %s: the delay must be a whole number of seconds, and at least 1s",
			written, delay)
	}
	return nil
}

// newWindows accepts the two schedule types the standard parser produces. Any
// other implementation is refused: nothing says its Next is a pure function of
// its argument, and a window that differs between two boxes splits the budget.
func newWindows(sched cron.Schedule) (*Windows, error) {
	switch s := sched.(type) {
	case *cron.SpecSchedule:
		return &Windows{sched: s}, nil
	case cron.ConstantDelaySchedule:
		// The parser never returns less than a second (see checkEvery, which
		// catches what an operator can type). This keeps a hand-built value
		// from becoming a Windows with neither a schedule nor a delay.
		if s.Delay <= 0 {
			return nil, fmt.Errorf("@every delay %s is not positive", s.Delay)
		}
		return &Windows{every: s.Delay}, nil
	default:
		return nil, fmt.Errorf("unsupported schedule type %T", sched)
	}
}

// Current returns the window containing now: start <= now < next, both in UTC.
//
// It cannot fail, and callers sleep until next, so it always answers with a
// real window. A schedule ParseWindows accepted can only stop answering when
// its activations are more than five years apart, which takes a leap-day
// schedule and the year 2100; Current then degrades to UTC days, which are at
// least still identical on every box and bound the spend.
func (w *Windows) Current(now time.Time) (start, next time.Time) {
	now = now.UTC()

	w.mu.Lock()
	defer w.mu.Unlock()
	if !now.Before(w.start) && now.Before(w.next) {
		return w.start, w.next
	}
	start, next, ok := w.compute(now)
	if !ok {
		start = now.Truncate(fallbackWindow)
		next = start.Add(fallbackWindow)
	}
	w.start, w.next = start, next
	return start, next
}

// compute finds the window containing now, which must be in UTC. ok is false
// when the schedule has no activation in the five years before now or none in
// the five years after it.
func (w *Windows) compute(now time.Time) (start, next time.Time, ok bool) {
	if w.every > 0 {
		// robfig runs "@every" relative to process start, so ten boxes would
		// have ten different windows. Truncate counts from the zero time
		// instead: the same instants everywhere, and UTC midnight is one of
		// them for every delay that divides a day.
		start = now.Truncate(w.every)
		return start, start.Add(w.every), true
	}

	// A schedule without an explicit TZ= is evaluated in the zone of the time
	// it is given, which is why now is UTC here.
	for _, lookback := range lookbacks {
		// Next is strictly after its argument, so an activation exactly at now
		// is only found by starting before it, and it does open the window
		// now is in: t <= now counts.
		t := w.sched.Next(now.Add(-lookback))
		for !t.IsZero() && !t.After(now) {
			start, t = t, w.sched.Next(t)
		}
		if !start.IsZero() {
			break
		}
	}
	next = w.sched.Next(now)
	if start.IsZero() || next.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	return start, next, true
}
