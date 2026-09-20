package budget

import (
	"sync"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

// parsedAt is where these tests parse their Windows unless parse time is the
// subject: far from every instant the tables ask about, so Current always has
// to leave the window that was cached at parse time.
var parsedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// ts parses a UTC instant; a fractional second is accepted.
func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC)
	if err != nil {
		t.Fatalf("bad test time %q: %v", s, err)
	}
	return v
}

func windowsAt(t *testing.T, spec string, now time.Time) *Windows {
	t.Helper()
	w, err := parseWindowsAt(spec, now)
	if err != nil {
		t.Fatalf("parseWindowsAt(%q, %s): %v", spec, now, err)
	}
	return w
}

func TestWindows_Current(t *testing.T) {
	tests := []struct {
		name, spec, now, start, next string
	}{
		// The production default.
		{"12h exactly on the activation", "0 */12 * * *", "2026-09-19 00:00:00", "2026-09-19 00:00:00", "2026-09-19 12:00:00"},
		{"12h one second in", "0 */12 * * *", "2026-09-19 00:00:01", "2026-09-19 00:00:00", "2026-09-19 12:00:00"},
		{"12h half a second in", "0 */12 * * *", "2026-09-19 00:00:00.5", "2026-09-19 00:00:00", "2026-09-19 12:00:00"},
		{"12h last second", "0 */12 * * *", "2026-09-19 11:59:59", "2026-09-19 00:00:00", "2026-09-19 12:00:00"},
		{"12h last nanosecond", "0 */12 * * *", "2026-09-19 11:59:59.999999999", "2026-09-19 00:00:00", "2026-09-19 12:00:00"},
		{"12h second activation", "0 */12 * * *", "2026-09-19 12:00:00", "2026-09-19 12:00:00", "2026-09-20 00:00:00"},
		{"12h end of day", "0 */12 * * *", "2026-09-19 23:59:59", "2026-09-19 12:00:00", "2026-09-20 00:00:00"},
		{"12h across the day boundary", "0 */12 * * *", "2026-09-20 00:00:00", "2026-09-20 00:00:00", "2026-09-20 12:00:00"},
		{"12h end of month", "0 */12 * * *", "2026-09-30 23:59:59", "2026-09-30 12:00:00", "2026-10-01 00:00:00"},
		{"12h across the month boundary", "0 */12 * * *", "2026-10-01 00:00:00", "2026-10-01 00:00:00", "2026-10-01 12:00:00"},
		{"12h end of year", "0 */12 * * *", "2026-12-31 23:59:59.999", "2026-12-31 12:00:00", "2027-01-01 00:00:00"},
		{"12h across the year boundary", "0 */12 * * *", "2027-01-01 00:00:00", "2027-01-01 00:00:00", "2027-01-01 12:00:00"},

		{"5m mid window", "*/5 * * * *", "2026-09-19 10:07:30", "2026-09-19 10:05:00", "2026-09-19 10:10:00"},
		{"5m exactly on the activation", "*/5 * * * *", "2026-09-19 10:05:00", "2026-09-19 10:05:00", "2026-09-19 10:10:00"},
		{"5m last second of the hour", "*/5 * * * *", "2026-09-19 10:59:59", "2026-09-19 10:55:00", "2026-09-19 11:00:00"},
		{"5m across midnight", "*/5 * * * *", "2026-09-20 00:00:00", "2026-09-20 00:00:00", "2026-09-20 00:05:00"},

		{"daily", "@daily", "2026-09-19 15:00:00", "2026-09-19 00:00:00", "2026-09-20 00:00:00"},
		{"daily exactly at midnight", "@daily", "2026-09-19 00:00:00", "2026-09-19 00:00:00", "2026-09-20 00:00:00"},
		{"daily on a leap day", "@daily", "2028-02-29 12:00:00", "2028-02-29 00:00:00", "2028-03-01 00:00:00"},

		// No activation in the last hour or day: the longer lookbacks.
		{"weekly on Mondays, asked on a Saturday", "0 0 * * 1", "2026-09-19 10:00:00", "2026-09-14 00:00:00", "2026-09-21 00:00:00"},
		{"monthly on the 31st", "@monthly", "2026-03-31 23:59:59", "2026-03-01 00:00:00", "2026-04-01 00:00:00"},
		{"yearly", "0 0 1 1 *", "2026-09-19 10:00:00", "2026-01-01 00:00:00", "2027-01-01 00:00:00"},
		{"yearly exactly on the activation", "0 0 1 1 *", "2026-01-01 00:00:00", "2026-01-01 00:00:00", "2027-01-01 00:00:00"},
		{"yearly last second", "0 0 1 1 *", "2025-12-31 23:59:59", "2025-01-01 00:00:00", "2026-01-01 00:00:00"},
		{"yearly last second of a leap year", "0 0 1 1 *", "2028-12-31 23:59:59", "2028-01-01 00:00:00", "2029-01-01 00:00:00"},
		{"leap days only", "0 0 29 2 *", "2027-06-01 00:00:00", "2024-02-29 00:00:00", "2028-02-29 00:00:00"},

		// @every is anchored to the epoch, not to when the process started.
		{"every 12h at midnight", "@every 12h", "2026-09-19 00:00:00", "2026-09-19 00:00:00", "2026-09-19 12:00:00"},
		{"every 12h last instant", "@every 12h", "2026-09-19 11:59:59.999999999", "2026-09-19 00:00:00", "2026-09-19 12:00:00"},
		{"every 12h at noon", "@every 12h", "2026-09-19 12:00:00", "2026-09-19 12:00:00", "2026-09-20 00:00:00"},
		{"every 12h across the month boundary", "@every 12h", "2026-10-01 00:00:00", "2026-10-01 00:00:00", "2026-10-01 12:00:00"},
		{"every 90m last second", "@every 90m", "2026-09-19 01:29:59", "2026-09-19 00:00:00", "2026-09-19 01:30:00"},
		{"every 90m on the boundary", "@every 90m", "2026-09-19 01:30:00", "2026-09-19 01:30:00", "2026-09-19 03:00:00"},
		{"every 90m late in the day", "@every 90m", "2026-09-19 23:00:00", "2026-09-19 22:30:00", "2026-09-20 00:00:00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := windowsAt(t, tc.spec, parsedAt)
			now, wantStart, wantNext := ts(t, tc.now), ts(t, tc.start), ts(t, tc.next)

			start, next := w.Current(now)
			if !start.Equal(wantStart) || !next.Equal(wantNext) {
				t.Fatalf("Current(%s) = [%s, %s), want [%s, %s)", tc.now, start, next, wantStart, wantNext)
			}
			if start.Location() != time.UTC || next.Location() != time.UTC {
				t.Errorf("window is in %s / %s, want UTC (it is a ledger key)", start.Location(), next.Location())
			}
			// Asking again is answered from the cache and must not change.
			if s2, n2 := w.Current(now); !s2.Equal(start) || !n2.Equal(next) {
				t.Errorf("second Current(%s) = [%s, %s), want the same [%s, %s)", tc.now, s2, n2, start, next)
			}
			// A process that boots at this very instant sees the same window.
			if s3, n3 := windowsAt(t, tc.spec, now).Current(now); !s3.Equal(start) || !n3.Equal(next) {
				t.Errorf("parsed at %s: Current = [%s, %s), want [%s, %s)", tc.now, s3, n3, start, next)
			}
		})
	}
}

// TestWindows_Current_MatchesTheSchedule hops around a couple of years in both
// directions with one long-lived Windows and checks every answer against the
// schedule itself: start is an activation, next is the activation right after
// it, and now lies between them.
func TestWindows_Current_MatchesTheSchedule(t *testing.T) {
	specs := []string{
		"0 */12 * * *", "*/5 * * * *", "@hourly", "@daily", "@weekly", "@monthly", "@yearly",
		"0 0 1 1 *", "30 6 * * 1-5", "15 3 1,15 * *", "0 9-17 * * *", "59 23 31 12 *",
	}
	base := ts(t, "2026-01-01 00:00:00")
	for _, spec := range specs {
		t.Run(spec, func(t *testing.T) {
			sched, err := cron.ParseStandard(spec)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			w := windowsAt(t, spec, parsedAt)
			seed := uint64(42)
			for i := range 300 {
				// A fixed LCG: deterministic, but neither ascending nor aligned
				// to anything, so the cache is left in both directions.
				seed = seed*6364136223846793005 + 1442695040888963407
				second := (seed >> 33) % (2 * 366 * 86400)
				now := base.Add(time.Duration(second) * time.Second).Add(time.Duration(i) * 7 * time.Millisecond)

				start, next := w.Current(now)
				if start.After(now) || !next.After(now) {
					t.Fatalf("Current(%s) = [%s, %s): want start <= now < next", now, start, next)
				}
				if got := sched.Next(start.Add(-time.Second)); !got.Equal(start) {
					t.Fatalf("Current(%s): start %s is not an activation (the one after start-1s is %s)", now, start, got)
				}
				if got := sched.Next(start); !got.Equal(next) {
					t.Fatalf("Current(%s) = [%s, %s): the activation after start is %s", now, start, next, got)
				}
			}
		})
	}
}

// Every instance of the fleet must key the ledger by the same window, whenever
// it booted and whenever inside the window it asks. robfig's own @every is
// relative to process start, which is exactly what must not leak through.
func TestWindows_Current_IdenticalOnEveryBox(t *testing.T) {
	tests := []struct {
		spec, bootA, askA, bootB, askB, start, next string
	}{
		{"@every 12h", "2026-09-18 07:13:00", "2026-09-19 12:00:00", "2026-09-19 03:41:17", "2026-09-19 23:59:59.999", "2026-09-19 12:00:00", "2026-09-20 00:00:00"},
		{"@every 90m", "2026-09-19 04:44:00", "2026-09-19 04:30:00", "2026-09-10 00:00:01", "2026-09-19 05:59:59", "2026-09-19 04:30:00", "2026-09-19 06:00:00"},
		{"@every 7h", "2026-09-19 04:44:00", "2026-09-19 09:00:00", "2026-09-10 00:00:01", "2026-09-19 09:00:01", "", ""},
		{"0 */12 * * *", "2026-09-18 07:13:00", "2026-09-19 12:00:00", "2026-09-19 03:41:17", "2026-09-19 23:59:59.999", "2026-09-19 12:00:00", "2026-09-20 00:00:00"},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			boxA := windowsAt(t, tc.spec, ts(t, tc.bootA))
			boxB := windowsAt(t, tc.spec, ts(t, tc.bootB))
			startA, nextA := boxA.Current(ts(t, tc.askA))
			startB, nextB := boxB.Current(ts(t, tc.askB))
			if !startA.Equal(startB) || !nextA.Equal(nextB) {
				t.Fatalf("box A sees [%s, %s), box B sees [%s, %s)", startA, nextA, startB, nextB)
			}
			if tc.start == "" {
				return
			}
			if !startA.Equal(ts(t, tc.start)) || !nextA.Equal(ts(t, tc.next)) {
				t.Errorf("window = [%s, %s), want [%s, %s)", startA, nextA, tc.start, tc.next)
			}
		})
	}
}

// A box whose clock is presented in another zone must still land in the UTC
// window. 22:00 at UTC-8 is already tomorrow in UTC, so a schedule evaluated
// in the zone of now would answer a different day.
func TestWindows_Current_NonUTCNow(t *testing.T) {
	zones := []*time.Location{
		time.FixedZone("UTC-8", -8*3600),
		time.FixedZone("UTC+5:30", 5*3600+30*60),
		time.FixedZone("UTC+14", 14*3600),
	}
	instant := ts(t, "2026-09-20 06:00:00") // 2026-09-19 22:00 at UTC-8
	for _, spec := range []string{"@daily", "0 */12 * * *", "0 0 1 1 *", "@every 12h", "@every 90m"} {
		wantStart, wantNext := windowsAt(t, spec, parsedAt).Current(instant)
		for _, zone := range zones {
			start, next := windowsAt(t, spec, parsedAt).Current(instant.In(zone))
			if !start.Equal(wantStart) || !next.Equal(wantNext) {
				t.Errorf("%s in %s: [%s, %s), want the UTC answer [%s, %s)", spec, zone, start, next, wantStart, wantNext)
			}
			if start.Location() != time.UTC || next.Location() != time.UTC {
				t.Errorf("%s in %s: window is in %s / %s, want UTC", spec, zone, start.Location(), next.Location())
			}
			// Parsing at a non-UTC instant must not matter either.
			start, next = windowsAt(t, spec, instant.In(zone)).Current(instant.In(zone))
			if !start.Equal(wantStart) || !next.Equal(wantNext) {
				t.Errorf("%s parsed in %s: [%s, %s), want [%s, %s)", spec, zone, start, next, wantStart, wantNext)
			}
		}
	}
	if start, _ := windowsAt(t, "@daily", parsedAt).Current(instant.In(zones[0])); !start.Equal(ts(t, "2026-09-20 00:00:00")) {
		t.Errorf("@daily at 22:00 UTC-8 starts %s, want 2026-09-20 00:00:00 UTC", start)
	}
}

func TestParseWindows_Rejects(t *testing.T) {
	tests := []struct{ name, spec string }{
		{"empty", ""},
		{"blank", "   "},
		{"words", "every twelve hours"},
		{"four fields", "* * * *"},
		{"six fields", "0 0 */12 * * *"},
		{"minute out of range", "61 * * * *"},
		{"unknown descriptor", "@sometimes"},
		{"every without a duration", "@every"},
		{"every with a bad duration", "@every banana"},
		// robfig accepts all of these and silently runs them as "@every 1s": a
		// fresh fleet budget every second.
		{"every with a negative duration", "@every -5h"},
		{"every with a zero duration", "@every 0h"},
		{"every with no seconds at all", "@every 0s"},
		{"every below a second", "@every 100ms"},
		{"every just below a second", "@every 999ms"},
		{"every with a time zone and a zero duration", "TZ=UTC @every 0h"},
		// ... and these with the fraction of a second dropped.
		{"every with a fractional second", "@every 1500ms"},
		{"every with a fractional second late in a long delay", "@every 12h0m0.5s"},
		// robfig slices out of range on this one instead of returning an error.
		{"time zone and nothing else", "TZ=UTC"},
		// Valid cron that never activates: no window would ever contain now.
		{"30th of February", "0 0 30 2 *"},
		{"31st of April", "0 0 31 4 *"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if w, err := ParseWindows(tc.spec); err == nil {
				t.Fatalf("ParseWindows(%q) = %+v, want an error", tc.spec, w)
			}
			if w, err := parseWindowsAt(tc.spec, parsedAt); err == nil {
				t.Fatalf("parseWindowsAt(%q) = %+v, want an error", tc.spec, w)
			}
		})
	}
}

// hourlySchedule is a cron.Schedule the parser never produces.
type hourlySchedule struct{}

func (hourlySchedule) Next(t time.Time) time.Time { return t.Truncate(time.Hour).Add(time.Hour) }

func TestNewWindows_RejectsUnknownScheduleTypes(t *testing.T) {
	if w, err := newWindows(hourlySchedule{}); err == nil {
		t.Errorf("newWindows(custom schedule) = %+v, want an error", w)
	}
	// Not something the parser can produce (see TestParseWindows_Rejects for
	// what an operator can type); the guard keeps a hand-built value from
	// becoming a Windows with neither a schedule nor a delay.
	if w, err := newWindows(cron.ConstantDelaySchedule{}); err == nil {
		t.Errorf("newWindows(zero delay) = %+v, want an error: the window would be empty", w)
	}
}

// The check that refuses "@every 100ms" must not refuse a delay robfig runs
// exactly as written, however it is spelled.
func TestParseWindows_EveryRunsAsWritten(t *testing.T) {
	tests := []struct {
		spec string
		want time.Duration
	}{
		{"@every 12h", 12 * time.Hour},
		{"@every 1.5h", 90 * time.Minute},
		{"@every 1h30m", 90 * time.Minute},
		{"@every 86400s", 24 * time.Hour},
		{"@every 90000ms", 90 * time.Second},
		{"@every 1s", time.Second},
		{"TZ=UTC @every 12h", 12 * time.Hour},
		{"CRON_TZ=UTC   @every 12h  ", 12 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			now := ts(t, "2026-09-19 10:00:00")
			start, next := windowsAt(t, tc.spec, now).Current(now)
			if got := next.Sub(start); got != tc.want {
				t.Errorf("window [%s, %s) is %s long, want %s", start, next, got, tc.want)
			}
		})
	}
}

func TestCheckEvery(t *testing.T) {
	tests := []struct {
		name, spec string
		delay      time.Duration
		wantErr    bool
	}{
		{"as written", "@every 90m", 90 * time.Minute, false},
		{"raised to a second", "@every 100ms", time.Second, true},
		{"negative raised to a second", "@every -5h", time.Second, true},
		{"fraction dropped", "@every 1500ms", time.Second, true},
		// Neither can come out of the parser together with a constant delay;
		// a delay that cannot be verified is refused, not waved through.
		{"no every descriptor", "0 */12 * * *", 12 * time.Hour, true},
		{"unreadable duration", "@every soon", time.Second, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkEvery(tc.spec, tc.delay); (err != nil) != tc.wantErr {
				t.Errorf("checkEvery(%q, %s) = %v, want an error: %v", tc.spec, tc.delay, err, tc.wantErr)
			}
		})
	}
}

func TestParseWindows_UsesTheRealClock(t *testing.T) {
	for _, spec := range []string{"0 */12 * * *", "@every 12h"} {
		w, err := ParseWindows(spec)
		if err != nil {
			t.Fatalf("ParseWindows(%q): %v", spec, err)
		}
		now := time.Now()
		start, next := w.Current(now)
		if start.After(now) || !next.After(now) {
			t.Errorf("%s: Current(%s) = [%s, %s): want start <= now < next", spec, now, start, next)
		}
		if got := next.Sub(start); got != 12*time.Hour {
			t.Errorf("%s: window is %s long, want 12h", spec, got)
		}
	}
}

// countingSchedule counts how often the schedule is consulted.
type countingSchedule struct {
	cron.Schedule
	calls *int
}

func (c countingSchedule) Next(t time.Time) time.Time {
	*c.calls++
	return c.Schedule.Next(t)
}

// Current sits in front of every paid request, so inside a window it must be
// a comparison, not a walk over the schedule.
func TestWindows_Current_IsCachedInsideTheWindow(t *testing.T) {
	sched, err := cron.ParseStandard("0 */12 * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	calls := 0
	w := &Windows{sched: countingSchedule{sched, &calls}}

	first := ts(t, "2026-09-19 00:00:00")
	start, next := w.Current(first)
	if calls == 0 {
		t.Fatal("the first Current did not consult the schedule")
	}

	calls = 0
	for i := range 1000 {
		now := first.Add(time.Duration(i) * 43 * time.Second) // stays before 12:00
		if s, n := w.Current(now); !s.Equal(start) || !n.Equal(next) {
			t.Fatalf("Current(%s) = [%s, %s), want [%s, %s)", now, s, n, start, next)
		}
	}
	if calls != 0 {
		t.Errorf("1000 calls inside the window consulted the schedule %d times, want 0", calls)
	}

	// now == next is the next window: the cache must not be served.
	if s, _ := w.Current(next); !s.Equal(next) || calls == 0 {
		t.Errorf("Current(next) starts %s after %d schedule calls, want %s recomputed", s, calls, next)
	}
	// A clock that steps back before the cached start recomputes as well.
	calls = 0
	if s, n := w.Current(first); !s.Equal(start) || !n.Equal(next) || calls == 0 {
		t.Errorf("Current(earlier) = [%s, %s) after %d schedule calls, want [%s, %s) recomputed", s, n, calls, start, next)
	}
}

// Current cannot fail, and callers sleep until next: whatever the schedule
// does, the answer has to be a real window. A leap-day schedule has no
// activation between 2096 and 2104 (2100 is not a leap year), further than
// robfig looks ahead.
func TestWindows_Current_FallsBackToUTCDays(t *testing.T) {
	w := windowsAt(t, "0 0 29 2 *", ts(t, "2024-03-01 00:00:00"))
	now := ts(t, "2097-01-01 10:00:00")
	start, next := w.Current(now.In(time.FixedZone("UTC-8", -8*3600)))
	if !start.Equal(ts(t, "2097-01-01 00:00:00")) || !next.Equal(ts(t, "2097-01-02 00:00:00")) {
		t.Errorf("Current(%s) = [%s, %s), want the UTC day", now, start, next)
	}
	if _, err := parseWindowsAt("0 0 29 2 *", now); err == nil {
		t.Error("parsing at an instant the schedule has no window for must fail")
	}
}

func TestWindows_Current_Concurrent(t *testing.T) {
	base := ts(t, "2026-09-19 00:00:00")
	for _, spec := range []string{"*/5 * * * *", "0 */12 * * *", "@every 90m"} {
		t.Run(spec, func(t *testing.T) {
			shared := windowsAt(t, spec, parsedAt)
			var wg sync.WaitGroup
			for g := range 16 {
				wg.Go(func() {
					own, err := parseWindowsAt(spec, parsedAt)
					if err != nil {
						t.Errorf("parse: %v", err)
						return
					}
					for i := range 400 {
						// Goroutines run at different offsets and half of them
						// backwards, so the shared cache is rewritten in both
						// directions while others read it.
						step := time.Duration(g*53+i) * 97 * time.Second
						if g%2 == 1 {
							step = 48*time.Hour - step
						}
						now := base.Add(step)
						start, next := shared.Current(now)
						wantStart, wantNext := own.Current(now)
						if !start.Equal(wantStart) || !next.Equal(wantNext) {
							t.Errorf("Current(%s) = [%s, %s), want [%s, %s)", now, start, next, wantStart, wantNext)
							return
						}
					}
				})
			}
			wg.Wait()
		})
	}
}
