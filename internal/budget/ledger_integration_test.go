package budget

import (
	"context"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/db"
)

// The ledger is one SQL statement, and whether ten instances can overspend is
// decided by how PostgreSQL runs it under contention, so apart from the first
// test everything here needs a real database and is skipped unless
// TEST_DATABASE_URL points at one it may write to:
//
//	TEST_DATABASE_URL=postgres://dwellings:dwellings@localhost:5432/dwellings?sslmode=disable \
//	    go test -race ./internal/budget/
//
// Every row is namespaced, so the tests are safe against a database that
// already holds data: the kind carries a per-run suffix (real rows are only
// ever 'search' and 'details'), and most windows lie centuries in the future
// at an offset of their own.
//
// The one thing that crosses the namespace is the sweep: the first reservation
// of a window deletes rows older than 90 days, of every kind, because that is
// what it is for. In one direction that is what these tests do to rows that
// are not theirs, and no more than any running instance does at its next
// window. In the other, anybody else opening a window on the same database
// sweeps these tests' old rows, so:
//
//   - whether this ledger swept is never read off the rows: the ledger runs on
//     a pool of its own, and the DELETE statements sent over it are counted;
//   - no test expects a row older than 90 days to still be there, except the
//     two about a window that is itself that old (TestLedger_TryReserve_
//     WindowOlderThan90DaysKeepsItsLimit and ..._SweepIsMeasuredFromTheWindow),
//     where that is the very thing under test. Those two are deterministic
//     only while nobody else opens a budget window on the same database: a
//     scratch database, or go test -p 1 if another package ever reserves
//     against the real thing.

const day = 24 * time.Hour

// sweepCounter counts the sweeps sent over the pool it traces. On a shared
// database the rows cannot say who swept them; the wire can.
type sweepCounter struct{ n atomic.Int64 }

func (c *sweepCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "DELETE FROM api_budget") {
		c.n.Add(1)
	}
	return ctx
}

func (*sweepCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A budget of zero means "spend nothing", and must not cost a round trip or
// leave a row behind either: a nil pool would panic on first use.
func TestLedger_TryReserve_NonPositiveLimitNeverReachesTheDatabase(t *testing.T) {
	l := NewLedger(nil)
	for _, limit := range []int{0, -1, math.MinInt} {
		ok, err := l.TryReserve(context.Background(), time.Now(), KindSearch, limit)
		if ok || err != nil {
			t.Errorf("TryReserve(limit %d) = %v, %v; want false, nil", limit, ok, err)
		}
	}
}

// ledgerFixture is a Ledger on the test database plus this run's namespace.
type ledgerFixture struct {
	pool   *pgxpool.Pool // the tests' own statements, cleanup included
	ledger *Ledger       // on a second, traced pool: only its statements count
	sweeps *sweepCounter
	suffix string
	epoch  time.Time
}

func newLedgerFixture(t *testing.T) *ledgerFixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the budget ledger integration tests")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url, 0)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Registered first so it runs last: t.Cleanup is LIFO, and the row cleanup
	// below still needs the pool.
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The same pool once more, traced. Config returns a copy.
	sweeps := &sweepCounter{}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = sweeps
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create the traced pool: %v", err)
	}
	t.Cleanup(traced.Close)

	nanos := time.Now().UnixNano()
	f := &ledgerFixture{
		pool:   pool,
		ledger: NewLedger(traced),
		sweeps: sweeps,
		suffix: strconv.FormatInt(nanos, 36),
		// timestamptz keeps microseconds; anything finer would not round-trip.
		epoch: time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC).
			Add(time.Duration(nanos % int64(365*24*time.Hour))).Truncate(time.Microsecond),
	}
	t.Cleanup(func() {
		kinds := []string{f.kind(KindSearch), f.kind(KindDetails)}
		if _, err := pool.Exec(context.Background(), `DELETE FROM api_budget WHERE kind = ANY($1)`, kinds); err != nil {
			t.Errorf("cleanup api_budget: %v", err)
		}
	})
	return f
}

// kind namespaces one of the real kinds for this run.
func (f *ledgerFixture) kind(base string) string { return base + "-it-" + f.suffix }

// window returns the n-th of this run's far-future windows.
func (f *ledgerFixture) window(n int) time.Time {
	return f.epoch.Add(time.Duration(n) * 12 * time.Hour)
}

// oldWindow returns a window that started age ago by the clock: what a long
// schedule such as "@yearly" is in for most of the year.
func (f *ledgerFixture) oldWindow(age time.Duration) time.Time {
	return time.Now().UTC().Add(-age).Truncate(time.Microsecond)
}

// mustSweeps fails the test unless the ledger has swept want times so far.
func (f *ledgerFixture) mustSweeps(t *testing.T, want int64, when string) {
	t.Helper()
	if got := f.sweeps.n.Load(); got != want {
		t.Errorf("%s: the ledger has swept %d times, want %d", when, got, want)
	}
}

// rows counts the ledger rows of one window and kind; Spent cannot tell a
// missing row from an unspent one.
func (f *ledgerFixture) rows(t *testing.T, window time.Time, kind string) int {
	t.Helper()
	var n int
	err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM api_budget WHERE window_start = $1 AND kind = $2`, window, kind).Scan(&n)
	if err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func (f *ledgerFixture) insert(t *testing.T, window time.Time, kind string, spent int) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO api_budget (window_start, kind, spent) VALUES ($1, $2, $3)`, window, kind, spent)
	if err != nil {
		t.Fatalf("insert api_budget row: %v", err)
	}
}

func (f *ledgerFixture) mustSpent(t *testing.T, window time.Time, kind string) int {
	t.Helper()
	n, err := f.ledger.Spent(context.Background(), window, kind)
	if err != nil {
		t.Fatalf("spent: %v", err)
	}
	return n
}

func (f *ledgerFixture) mustReserve(t *testing.T, window time.Time, kind string, limit int) bool {
	t.Helper()
	ok, err := f.ledger.TryReserve(context.Background(), window, kind, limit)
	if err != nil {
		t.Fatalf("try reserve: %v", err)
	}
	return ok
}

// The fleet in miniature: 20 workers race for a budget of 50 in a window that
// does not exist yet, so the very first statements also race on the INSERT.
func TestLedger_TryReserve_ConcurrentNeverExceedsTheLimit(t *testing.T) {
	const (
		workers  = 20
		attempts = 10
		limit    = 50
	)
	f := newLedgerFixture(t)
	ctx := context.Background()
	window, kind := f.window(0), f.kind(KindSearch)

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		granted atomic.Int64
		denied  atomic.Int64
	)
	for range workers {
		wg.Go(func() {
			<-start
			for range attempts {
				ok, err := f.ledger.TryReserve(ctx, window, kind, limit)
				switch {
				case err != nil:
					t.Errorf("try reserve: %v", err)
					return
				case ok:
					granted.Add(1)
				default:
					denied.Add(1)
				}
			}
		})
	}
	close(start)
	wg.Wait()

	if got := granted.Load(); got != limit {
		t.Errorf("granted %d reservations, want exactly %d", got, limit)
	}
	if got := denied.Load(); got != workers*attempts-limit {
		t.Errorf("denied %d reservations, want %d", got, workers*attempts-limit)
	}
	if got := f.mustSpent(t, window, kind); got != limit {
		t.Errorf("spent = %d, want %d", got, limit)
	}
	if n := f.rows(t, window, kind); n != 1 {
		t.Errorf("the window has %d ledger rows, want 1", n)
	}
	// Exactly one of the racing statements opened the window.
	f.mustSweeps(t, 1, "after the race")
}

// Budgets are edited while a window is running. Lowering one below what is
// already spent denies and must not touch the count; raising it resumes where
// the count stands.
func TestLedger_TryReserve_LimitMovedBelowAndAboveSpent(t *testing.T) {
	f := newLedgerFixture(t)
	window, kind := f.window(1), f.kind(KindSearch)

	for i := 1; i <= 5; i++ {
		if !f.mustReserve(t, window, kind, 5) {
			t.Fatalf("reservation %d of 5 was denied", i)
		}
	}
	for _, limit := range []int{5, 4, 1} {
		if f.mustReserve(t, window, kind, limit) {
			t.Errorf("limit %d granted with 5 already spent", limit)
		}
		if got := f.mustSpent(t, window, kind); got != 5 {
			t.Fatalf("spent = %d after a denial at limit %d, want 5 unchanged", got, limit)
		}
	}
	if !f.mustReserve(t, window, kind, 6) {
		t.Error("limit raised to 6 with 5 spent was denied")
	}
	if f.mustReserve(t, window, kind, 6) {
		t.Error("limit 6 granted a seventh reservation")
	}
	if got := f.mustSpent(t, window, kind); got != 6 {
		t.Errorf("spent = %d, want 6", got)
	}
}

func TestLedger_TryReserve_NonPositiveLimitCreatesNoRow(t *testing.T) {
	f := newLedgerFixture(t)
	window, kind := f.window(2), f.kind(KindDetails)

	for _, limit := range []int{0, -1, -50} {
		if f.mustReserve(t, window, kind, limit) {
			t.Errorf("limit %d granted", limit)
		}
	}
	if n := f.rows(t, window, kind); n != 0 {
		t.Errorf("denied reservations left %d ledger rows, want none", n)
	}
	if got := f.mustSpent(t, window, kind); got != 0 {
		t.Errorf("spent of a window without a row = %d, want 0", got)
	}

	// Nor may a zero limit disturb a window that is already being spent.
	if !f.mustReserve(t, window, kind, 3) {
		t.Fatal("first reservation denied")
	}
	if f.mustReserve(t, window, kind, 0) {
		t.Error("limit 0 granted on an existing row")
	}
	if got := f.mustSpent(t, window, kind); got != 1 {
		t.Errorf("spent = %d, want 1", got)
	}
}

func TestLedger_KindsAndWindowsAreIndependent(t *testing.T) {
	f := newLedgerFixture(t)
	search, details := f.kind(KindSearch), f.kind(KindDetails)
	a, b := f.window(3), f.window(4)

	steps := []struct {
		window time.Time
		kind   string
		limit  int
		want   bool
	}{
		{a, search, 2, true},
		{a, search, 2, true},
		{a, search, 2, false}, // a/search is spent ...
		{a, details, 1, true}, // ... which is not a/details' business,
		{a, details, 1, false},
		{b, search, 2, true}, // nor the next window's.
		{b, details, 1, true},
		{b, search, 2, true},
		{b, search, 2, false},
	}
	for i, s := range steps {
		if got := f.mustReserve(t, s.window, s.kind, s.limit); got != s.want {
			t.Errorf("step %d: TryReserve(%s, limit %d) = %v, want %v", i, s.kind, s.limit, got, s.want)
		}
	}
	for _, c := range []struct {
		window time.Time
		kind   string
		want   int
	}{
		{a, search, 2}, {a, details, 1}, {b, search, 2}, {b, details, 1}, {f.window(5), search, 0},
	} {
		if got := f.mustSpent(t, c.window, c.kind); got != c.want {
			t.Errorf("spent(%s, %s) = %d, want %d", c.window.Format(time.RFC3339), c.kind, got, c.want)
		}
	}
}

// The window is an instant: a box that presents it in another zone must hit
// the same row, not open a second budget.
func TestLedger_WindowIsAnInstantNotAWallClock(t *testing.T) {
	f := newLedgerFixture(t)
	window, kind := f.window(6), f.kind(KindSearch)
	elsewhere := window.In(time.FixedZone("UTC-8", -8*3600))

	if !f.mustReserve(t, window, kind, 2) || !f.mustReserve(t, elsewhere, kind, 2) {
		t.Fatal("the first two reservations must be granted")
	}
	if f.mustReserve(t, elsewhere, kind, 2) || f.mustReserve(t, window, kind, 2) {
		t.Error("a third reservation was granted: the zones did not share a row")
	}
	if got := f.mustSpent(t, elsewhere, kind); got != 2 {
		t.Errorf("spent = %d, want 2", got)
	}
}

// "Unlimited" is spelled as a huge number in an env file; it must not turn
// into an int4 encoding error that denies every request.
func TestLedger_TryReserve_HugeLimit(t *testing.T) {
	f := newLedgerFixture(t)
	window, kind := f.window(7), f.kind(KindSearch)
	for _, limit := range []int{math.MaxInt32, math.MaxInt} {
		if ok, err := f.ledger.TryReserve(context.Background(), window, kind, limit); !ok || err != nil {
			t.Errorf("TryReserve(limit %d) = %v, %v; want true, nil", limit, ok, err)
		}
	}
}

// A ledger that cannot be reached denies: the caller is about to spend money.
func TestLedger_TryReserve_FailsClosed(t *testing.T) {
	f := newLedgerFixture(t)
	window, kind := f.window(8), f.kind(KindSearch)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, err := f.ledger.TryReserve(ctx, window, kind, 10); ok || err == nil {
		t.Errorf("TryReserve on a dead context = %v, %v; want false and an error", ok, err)
	}
	if _, err := f.ledger.Spent(ctx, window, kind); err == nil {
		t.Error("Spent on a dead context returned no error")
	}
	if got := f.mustSpent(t, window, kind); got != 0 {
		t.Errorf("spent = %d after a failed reservation, want 0", got)
	}
}

// Nothing else ever deletes from api_budget, so the first reservation of each
// window sweeps out rows older than 90 days. Only this run's own rows are
// asserted on. That the old ones are gone holds whoever swept them, and the
// 89 days old one is out of every sweep's reach.
func TestLedger_TryReserve_FirstReservationPrunesOldWindows(t *testing.T) {
	f := newLedgerFixture(t)
	kind := f.kind(KindSearch)
	var (
		now     = time.Now().UTC().Truncate(time.Microsecond)
		ancient = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
		expired = now.Add(-91 * day)
		kept    = now.Add(-89 * day)
		window  = f.window(9)
	)
	f.insert(t, ancient, kind, 7)
	f.insert(t, expired, kind, 7)
	f.insert(t, kept, kind, 3)

	if !f.mustReserve(t, window, kind, 10) {
		t.Fatal("first reservation of a new window was denied")
	}
	f.mustSweeps(t, 1, "after the first reservation of a new window")
	for name, old := range map[string]time.Time{"ancient": ancient, "91 days old": expired} {
		if n := f.rows(t, old, kind); n != 0 {
			t.Errorf("the %s row survived the first reservation of a new window", name)
		}
	}
	if got := f.mustSpent(t, kept, kind); got != 3 {
		t.Errorf("the 89 days old row has spent = %d, want 3 untouched", got)
	}
	if got := f.mustSpent(t, window, kind); got != 1 {
		t.Errorf("the new window has spent = %d, want 1", got)
	}
}

// The sweep is a DELETE in front of a paid request. It belongs to the one
// reservation that opens a window's row, not to every request, and not to a
// denial. Counted on the wire: a row that survived would prove nothing on a
// database where anybody else may sweep between two statements of this test.
func TestLedger_TryReserve_OnlyTheFirstReservationSweeps(t *testing.T) {
	f := newLedgerFixture(t)
	search, details := f.kind(KindSearch), f.kind(KindDetails)
	a, b := f.window(10), f.window(11)

	steps := []struct {
		name   string
		window time.Time
		kind   string
		want   bool
		sweeps int64 // by this ledger, in total, once the step is done
	}{
		{"a/search is opened", a, search, true, 1},
		{"a/search, second request", a, search, true, 1},
		{"a/search, denied", a, search, false, 1},
		{"a/details is opened", a, details, true, 2},
		{"b/search is opened", b, search, true, 3},
		{"b/search, second request", b, search, true, 3},
		{"a/details, second request", a, details, true, 3},
	}
	for _, s := range steps {
		if got := f.mustReserve(t, s.window, s.kind, 2); got != s.want {
			t.Errorf("%s: TryReserve = %v, want %v", s.name, got, s.want)
		}
		f.mustSweeps(t, s.sweeps, s.name)
	}
}

// A window can outlive the 90 days: "@yearly" is in one for most of the year,
// "0 0 1 */4 *" for a month of every four. A sweep measured from the clock
// alone deletes the row that was inserted a statement earlier, every
// reservation is the first one again, and the limit never binds.
func TestLedger_TryReserve_WindowOlderThan90DaysKeepsItsLimit(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		window func(f *ledgerFixture) time.Time
	}{
		{"91 days old", func(f *ledgerFixture) time.Time { return f.oldWindow(91 * day) }},
		{"200 days old", func(f *ledgerFixture) time.Time { return f.oldWindow(200 * day) }},
		{"six years old", func(f *ledgerFixture) time.Time { return f.oldWindow(6 * 366 * day) }},
		// What Windows really answers today. Whatever the date, one of the two
		// started more than 91 days ago.
		{"this year's window since January", func(*ledgerFixture) time.Time {
			start, _ := windowsAt(t, "0 0 1 1 *", now).Current(now)
			return start
		}},
		{"this year's window since July", func(*ledgerFixture) time.Time {
			start, _ := windowsAt(t, "0 0 1 7 *", now).Current(now)
			return start
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const limit = 3
			f := newLedgerFixture(t)
			window, kind := tc.window(f), f.kind(KindSearch)

			granted := 0
			for range 3 * limit {
				if f.mustReserve(t, window, kind, limit) {
					granted++
				}
			}
			if granted != limit {
				t.Errorf("granted %d reservations with limit %d in the window since %s",
					granted, limit, window.Format(time.RFC3339))
			}
			if got := f.mustSpent(t, window, kind); got != limit {
				t.Errorf("spent = %d, want %d", got, limit)
			}
			// It did sweep; it just could not reach its own window.
			f.mustSweeps(t, 1, "after the first reservation")
		})
	}
}

// Same cause, other trigger: the sweep covers every kind, so in a long window
// the first details request would wipe the search row of the very same window
// and hand search a fresh budget. What the window is measured against instead
// is itself: 90 days before its start is swept, anything later is kept.
func TestLedger_TryReserve_SweepIsMeasuredFromTheWindow(t *testing.T) {
	f := newLedgerFixture(t)
	search, details := f.kind(KindSearch), f.kind(KindDetails)
	var (
		window = f.oldWindow(100 * day)
		before = window.Add(-91 * day)
		recent = window.Add(-89 * day) // 189 days ago, but not seen from the window
	)

	const limit = 5
	f.insert(t, window, search, limit) // spent down earlier in the window
	f.insert(t, before, search, 7)
	f.insert(t, recent, search, 7)
	if f.mustReserve(t, window, search, limit) {
		t.Fatal("setup: search must be exhausted")
	}
	f.mustSweeps(t, 0, "after a denial")

	if !f.mustReserve(t, window, details, 1) {
		t.Fatal("the first details request of the window was denied")
	}
	f.mustSweeps(t, 1, "after the first details request of the window")

	if got := f.mustSpent(t, window, search); got != limit {
		t.Errorf("search spent = %d after the first details request, want %d", got, limit)
	}
	if f.mustReserve(t, window, search, limit) {
		t.Errorf("search was granted although %d of %d were already spent", limit, limit)
	}
	if got := f.mustSpent(t, window, details); got != 1 {
		t.Errorf("details spent = %d, want 1", got)
	}
	if n := f.rows(t, before, search); n != 0 {
		t.Error("the row 91 days before the window survived its first reservation")
	}
	if got := f.mustSpent(t, recent, search); got != 7 {
		t.Errorf("the row 89 days before the window has spent = %d, want 7 untouched", got)
	}
}
