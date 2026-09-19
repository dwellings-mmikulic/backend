package zipcode

import (
	"context"
	"errors"
	"fmt"
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

// The tests in this file run the claim SQL against a real PostgreSQL, which is
// the only place it can be checked: that two claimers never get the same ZIP,
// that a lease expires on the database clock, and that a completion carrying a
// stale owner or token changes nothing are all properties of the statements,
// not of the Go around them.
//
// Skipped unless TEST_DATABASE_URL points at a database they may write to, and
// it must be one that NO WORKER OR SCHEDULER IS ATTACHED TO:
//
//	TEST_DATABASE_URL=postgres://dwellings:dwellings@localhost:5432/dwellings_test?sslmode=disable \
//	    go test -race -count=1 ./internal/zipcode/
//
// Give them a database of their own (CREATE DATABASE dwellings_test; the tests
// migrate it). The "dwellings" database of docker-compose.yml and .env.example
// will only do while the app service is stopped.
//
// The rotation is one global order, so a test cannot fence itself off with a
// WHERE clause the way other packages' tests do. Instead its ZIPs are made to
// sort ahead of anything real (never searched, population around two billion),
// every Claim is checked to have returned one of them, and they are deleted
// again afterwards. A handful of assertions only make sense when nothing else
// is claimable (Claim returning none, ZIPs that have been searched coming back
// stalest first); those run only when the table holds no other rows.
//
// That makes the tests safe for the rows a database already holds, and not for
// a claimer it already has. A live discovery loop takes the test ZIPs ahead of
// every real one and spends paid provider searches on locations that do not
// exist; the tests then fail as well, having been handed a foreign ZIP or none.
// newFixture refuses to start when it sees a worker's claim, but a worker that
// is idle between searches shows nothing, so the protection is the rule above
// and not the check.

const (
	// testZipPrefix starts every ZIP these tests insert. No real ZIP has it,
	// so it tells a claim a test left behind from a claim a worker holds.
	testZipPrefix = "zt-"
	// topPopulation is above any real ZIP's population and still fits the
	// INTEGER column with room for an index to be subtracted.
	topPopulation = 2_000_000_000
	// longLease outlives every test: a claim taken with it stays live.
	longLease = 15 * time.Minute
	// shortLease is waited out on the database clock, so it only has to be a
	// real lease, not a precisely timed one.
	shortLease = 50 * time.Millisecond
)

type fixture struct {
	pool *pgxpool.Pool
	repo *Repository
	// zips are the test's own ZIPs in rotation order: zips[0] is claimed first.
	zips []string
	mine map[string]bool
}

// testPool connects to TEST_DATABASE_URL and migrates it, or skips the test.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the zipcode repository integration tests")
	}
	ctx := context.Background()
	// Wide enough that the concurrent claimers really do overlap in the
	// database instead of queueing for a connection.
	pool, err := db.Connect(ctx, url, 20)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Registered first so it runs last: t.Cleanup is LIFO, and the row
	// cleanups registered after it still need the pool.
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// foreignLiveClaim describes a live claim that something other than these
// tests holds on a ZIP, or returns "" when there is none.
//
// Only an owned, unexpired claim on a real ZIP counts. An expired one, or the
// ownerless claimed_until that Defer and Fail keep as a backoff, is what a
// worker leaves behind (in a restored dump, say) and says nothing about one
// being attached now. A live claim on a test ZIP is an aborted run of these
// tests, which claimChecked deals with once it gets in the way.
func foreignLiveClaim(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	const q = `
SELECT zip, claimed_by, claimed_until FROM zip_codes
 WHERE claimed_by IS NOT NULL AND claimed_until > now() AND NOT starts_with(zip, $1)
 ORDER BY claimed_until DESC
 LIMIT 1`
	var (
		zip, owner string
		until      time.Time
	)
	err := pool.QueryRow(ctx, q, testZipPrefix).Scan(&zip, &owner, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("look for a live claim: %w", err)
	}
	return fmt.Sprintf("zip %s is claimed by %s until %s", zip, owner, until.UTC().Format(time.RFC3339)), nil
}

// refuseADatabaseInUse fails the test, before it has inserted anything, when a
// worker is claiming from this database: the test ZIPs go to the front of the
// rotation that worker shares, and its discovery loop would spend paid
// provider searches on them. It is a tripwire and not a proof. A worker that
// is attached but between searches holds no claim, which is why the rule in
// the comment at the top of this file stands whatever this finds.
func refuseADatabaseInUse(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	claim, err := foreignLiveClaim(context.Background(), pool)
	if err != nil {
		t.Fatalf("look for a worker claiming from this database: %v", err)
	}
	if claim != "" {
		t.Fatalf("refusing to run: %s, so a worker is claiming from this database. "+
			"These tests put ZIPs at the front of the rotation it shares, and it would spend paid searches on them. "+
			"Point TEST_DATABASE_URL at a database no worker or scheduler is attached to.", claim)
	}
}

// newFixture connects, migrates and inserts n never-searched ZIPs that sort
// first in the rotation, zips[0] ahead of zips[1] and so on.
func newFixture(t *testing.T, n int) *fixture {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	refuseADatabaseInUse(t, pool)

	f := &fixture{
		pool: pool,
		repo: NewRepository(pool),
		zips: make([]string, n),
		mine: make(map[string]bool, n),
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	for i := range n {
		f.zips[i] = fmt.Sprintf("%s%s-%02d", testZipPrefix, suffix, i)
		f.mine[f.zips[i]] = true
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM zip_codes WHERE zip = ANY($1)`, f.zips); err != nil {
			t.Errorf("cleanup zip codes: %v", err)
		}
	})

	// Inserted back to front, so insertion order is the reverse of rotation
	// order and a Claim that forgot its ORDER BY cannot pass by accident.
	var (
		insZips = make([]string, n)
		insPops = make([]int32, n)
	)
	for i := range n {
		insZips[n-1-i] = f.zips[i]
		insPops[n-1-i] = int32(topPopulation - i)
	}
	const q = `
INSERT INTO zip_codes (zip, city, state, population)
SELECT z, 'Claimtest', 'ZZ', p FROM unnest($1::text[], $2::int[]) AS t(z, p)`
	if _, err := pool.Exec(ctx, q, insZips, insPops); err != nil {
		t.Fatalf("insert test zip codes: %v", err)
	}
	return f
}

// claimChecked claims the next ZIP and refuses any that is not the test's own.
// A foreign ZIP means something sorts ahead of the test rows, so the run
// proves nothing, and the row must not be left claimed in a database somebody
// uses. It reports through an error so that goroutines can call it.
func (f *fixture) claimChecked(owner string, lease time.Duration) (*Claim, error) {
	ctx := context.Background()
	c, err := f.repo.Claim(ctx, owner, lease)
	if err != nil {
		return nil, fmt.Errorf("claim as %s: %w", owner, err)
	}
	if c == nil || f.mine[c.Zip] {
		return c, nil
	}
	released := "released it again"
	if err := f.repo.Release(ctx, *c, owner); err != nil {
		released = fmt.Sprintf("and releasing it failed (%v): clear claimed_by/claimed_until of that row by hand", err)
	}
	return nil, fmt.Errorf("claim as %s returned zip %q, which this test did not insert; %s. "+
		"The test ZIPs must sort first in the rotation and have it to themselves: is a worker or another test "+
		"claiming from this database (see the comment at the top of this file), "+
		"or did an aborted run leave rows behind (DELETE FROM zip_codes WHERE zip LIKE '%s%%')?",
		owner, c.Zip, released, testZipPrefix)
}

func (f *fixture) claim(t *testing.T, owner string, lease time.Duration) *Claim {
	t.Helper()
	c, err := f.claimChecked(owner, lease)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// mustClaim claims as owner and requires the result to be want.
func (f *fixture) mustClaim(t *testing.T, owner string, lease time.Duration, want string) Claim {
	t.Helper()
	c := f.claim(t, owner, lease)
	if c == nil {
		t.Fatalf("Claim as %s returned none, want %s", owner, want)
	}
	if c.Zip != want {
		t.Fatalf("Claim as %s returned %s, want %s", owner, c.Zip, want)
	}
	return *c
}

// holdsOnlyTestZips reports whether the table has no rows besides the test's
// own, which is when "nothing is claimable" can be observed at all.
func (f *fixture) holdsOnlyTestZips(t *testing.T) bool {
	t.Helper()
	var others int
	err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM zip_codes WHERE zip <> ALL($1)`, f.zips).Scan(&others)
	if err != nil {
		t.Fatalf("count other zip codes: %v", err)
	}
	return others == 0
}

// dbNow reads the database clock, the only clock a lease is measured on.
func (f *fixture) dbNow(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatalf("read database clock: %v", err)
	}
	return now
}

// waitForDBClock blocks until the database clock is past ts. Leases expire on
// that clock, and it need not agree with this process's.
func (f *fixture) waitForDBClock(t *testing.T, ts time.Time) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var passed bool
		if err := f.pool.QueryRow(context.Background(), `SELECT now() > $1`, ts).Scan(&passed); err != nil {
			t.Fatalf("read database clock: %v", err)
		}
		if passed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("database clock did not pass %s within 5s", ts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// zipRow is every column a claim transition may touch.
type zipRow struct {
	claimedBy        *string
	claimedUntil     *time.Time
	failures         int
	resumePage       int
	lastSearchedAt   *time.Time
	lastListingCount *int
}

func (f *fixture) row(t *testing.T, zip string) zipRow {
	t.Helper()
	const q = `
SELECT claimed_by, claimed_until, failures, resume_page, last_searched_at, last_listing_count
  FROM zip_codes WHERE zip = $1`
	var r zipRow
	err := f.pool.QueryRow(context.Background(), q, zip).
		Scan(&r.claimedBy, &r.claimedUntil, &r.failures, &r.resumePage, &r.lastSearchedAt, &r.lastListingCount)
	if err != nil {
		t.Fatalf("read zip %s: %v", zip, err)
	}
	return r
}

// String renders the row so that two states compare, and print, as text.
func (r zipRow) String() string {
	str := func(s *string) string {
		if s == nil {
			return "NULL"
		}
		return *s
	}
	ts := func(v *time.Time) string {
		if v == nil {
			return "NULL"
		}
		return v.UTC().Format(time.RFC3339Nano)
	}
	count := "NULL"
	if r.lastListingCount != nil {
		count = strconv.Itoa(*r.lastListingCount)
	}
	return fmt.Sprintf("claimed_by=%s claimed_until=%s failures=%d resume_page=%d last_searched_at=%s last_listing_count=%s",
		str(r.claimedBy), ts(r.claimedUntil), r.failures, r.resumePage, ts(r.lastSearchedAt), count)
}

// assertCounters checks the two counters a transition either carries over or
// resets.
func assertCounters(t *testing.T, r zipRow, failures, resumePage int) {
	t.Helper()
	if r.failures != failures || r.resumePage != resumePage {
		t.Errorf("failures/resume_page = %d/%d, want %d/%d (row: %s)", r.failures, r.resumePage, failures, resumePage, r)
	}
}

// assertLeaseLost runs every completion with a claim that must no longer be
// honoured and checks that each reports ErrLeaseLost, names the ZIP, and
// leaves the row exactly as it was.
func (f *fixture) assertLeaseLost(t *testing.T, c Claim, owner string) {
	t.Helper()
	ctx := context.Background()
	ops := []struct {
		name string
		do   func() error
	}{
		{"MarkSearched", func() error { return f.repo.MarkSearched(ctx, c, owner, 99) }},
		{"Defer", func() error { return f.repo.Defer(ctx, c, owner, time.Now().Add(time.Hour), 9) }},
		{"Fail", func() error {
			pushedBack, err := f.repo.Fail(ctx, c, owner, time.Now().Add(time.Hour), 9)
			if pushedBack {
				t.Errorf("Fail without the lease reported pushedBack")
			}
			return err
		}},
		{"Release", func() error { return f.repo.Release(ctx, c, owner) }},
	}
	for _, op := range ops {
		before := f.row(t, c.Zip).String()
		err := op.do()
		if !errors.Is(err, ErrLeaseLost) {
			t.Errorf("%s as %s with token %s = %v, want ErrLeaseLost", op.name, owner, c.Token.Format(time.RFC3339Nano), err)
		} else if !strings.Contains(err.Error(), c.Zip) {
			t.Errorf("%s error %q does not name the zip %s", op.name, err, c.Zip)
		}
		if after := f.row(t, c.Zip).String(); after != before {
			t.Errorf("%s without the lease changed the row:\n before: %s\n  after: %s", op.name, before, after)
		}
	}
}

// pastUntil is a backoff that is already over, so the ZIP can be claimed again
// at once. A minute is far more than any clock skew between test and database.
func pastUntil() time.Time { return time.Now().Add(-time.Minute) }

func TestClaim_ConcurrentClaimersGetDisjointZips(t *testing.T) {
	const (
		zipCount = 60
		claimers = 20
	)
	f := newFixture(t, zipCount)

	// The claimers share a budget of exactly one Claim per test ZIP rather
	// than claiming until none is returned: past the last test ZIP a Claim
	// would reach into whatever else the database holds. With correct SQL
	// every budgeted Claim must succeed, because the other 59 calls can lock
	// or take at most 59 rows between them.
	var (
		budget atomic.Int64
		mu     sync.Mutex
		owners = make(map[string][]string, zipCount) // zip → owners it was handed to
		start  = make(chan struct{})
		wg     sync.WaitGroup
	)
	budget.Store(zipCount)
	for g := range claimers {
		owner := fmt.Sprintf("claimer-%02d", g)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for budget.Add(-1) >= 0 {
				c, err := f.claimChecked(owner, longLease)
				if err != nil {
					t.Error(err)
					return
				}
				if c == nil {
					t.Errorf("Claim as %s returned none while test ZIPs were still unclaimed", owner)
					return
				}
				mu.Lock()
				owners[c.Zip] = append(owners[c.Zip], owner)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}

	for _, zip := range f.zips {
		got := owners[zip]
		if len(got) != 1 {
			t.Errorf("zip %s was claimed %d times (by %v), want exactly once", zip, len(got), got)
			continue
		}
		// The row has to name the claimer that was told it won.
		if r := f.row(t, zip); r.claimedBy == nil || *r.claimedBy != got[0] {
			t.Errorf("zip %s was handed to %s but the row says: %s", zip, got[0], r)
		}
	}
	if len(owners) != zipCount {
		t.Errorf("%d distinct zips were claimed, want %d", len(owners), zipCount)
	}

	if !f.holdsOnlyTestZips(t) {
		t.Log("zip_codes holds other rows: not checking that a further Claim returns none")
		return
	}
	if c := f.claim(t, "latecomer", longLease); c != nil {
		t.Errorf("Claim with every zip held = %+v, want nil", *c)
	}
}

func TestClaim_ReturnsZipsInRotationOrder(t *testing.T) {
	f := newFixture(t, 5)
	ctx := context.Background()
	const owner = "orderer"

	// Never searched: most populous first. One owner takes all five, so this
	// also shows that a live claim hides a ZIP from its own holder.
	claims := make([]Claim, len(f.zips))
	for i, want := range f.zips {
		claims[i] = f.mustClaim(t, owner, longLease, want)
		if claims[i].ResumePage != 0 {
			t.Errorf("fresh zip %s: ResumePage = %d, want 0", want, claims[i].ResumePage)
		}
	}

	// A searched ZIP goes behind a never-searched one, however populous: with
	// zips[0] searched and zips[1] handed back, zips[1] is next.
	if err := f.repo.MarkSearched(ctx, claims[0], owner, 3); err != nil {
		t.Fatalf("mark %s searched: %v", f.zips[0], err)
	}
	if err := f.repo.Release(ctx, claims[1], owner); err != nil {
		t.Fatalf("release %s: %v", f.zips[1], err)
	}
	f.mustClaim(t, owner, longLease, f.zips[1])

	if !f.holdsOnlyTestZips(t) {
		t.Log("zip_codes holds other rows: not checking the order of searched zips")
		return
	}
	// Searched ZIPs come back stalest first, whatever their population: the
	// order below is the order they were marked in, where population alone
	// would give 0, 2, 4.
	for _, i := range []int{4, 2} {
		if err := f.repo.MarkSearched(ctx, claims[i], owner, 3); err != nil {
			t.Fatalf("mark %s searched: %v", f.zips[i], err)
		}
	}
	for _, i := range []int{0, 4, 2} {
		f.mustClaim(t, owner, longLease, f.zips[i])
	}
	if c := f.claim(t, owner, longLease); c != nil {
		t.Errorf("Claim with every zip held = %+v, want nil", *c)
	}
}

func TestClaim_TokenIsTheStoredLeaseAndCompletesAtOnce(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	const owner = "tokener"
	zip := f.zips[0]

	before := f.dbNow(t)
	c := f.mustClaim(t, owner, longLease, zip)
	after := f.dbNow(t)

	// The lease is measured from the database clock, in seconds.
	if c.Token.Before(before.Add(longLease)) || c.Token.After(after.Add(longLease)) {
		t.Errorf("Token = %s, want database now + %s, i.e. within [%s, %s]",
			c.Token, longLease, before.Add(longLease), after.Add(longLease))
	}
	r := f.row(t, zip)
	if r.claimedBy == nil || *r.claimedBy != owner {
		t.Errorf("claimed_by: %s, want %s", r, owner)
	}
	if r.claimedUntil == nil || !r.claimedUntil.Equal(c.Token) {
		t.Errorf("claimed_until: %s, want the Token %s", r, c.Token.UTC().Format(time.RFC3339Nano))
	}
	// timestamptz holds microseconds. A Token carrying more than that could
	// never compare equal to the column again.
	if c.Token.Nanosecond()%1000 != 0 {
		t.Errorf("Token %s is finer than a microsecond", c.Token.Format(time.RFC3339Nano))
	}

	// The Token goes back through a bind parameter unchanged, so the very
	// next statement is accepted.
	beforeMark := f.dbNow(t)
	if err := f.repo.MarkSearched(ctx, c, owner, 37); err != nil {
		t.Fatalf("MarkSearched right after Claim: %v", err)
	}
	afterMark := f.dbNow(t)

	r = f.row(t, zip)
	if r.claimedBy != nil || r.claimedUntil != nil {
		t.Errorf("MarkSearched left the claim in place: %s", r)
	}
	assertCounters(t, r, 0, 0)
	if r.lastListingCount == nil || *r.lastListingCount != 37 {
		t.Errorf("last_listing_count: %s, want 37", r)
	}
	if r.lastSearchedAt == nil || r.lastSearchedAt.Before(beforeMark) || r.lastSearchedAt.After(afterMark) {
		t.Errorf("last_searched_at: %s, want within [%s, %s]", r, beforeMark, afterMark)
	}

	// Completing gives the claim up: a second completion is a lost lease.
	f.assertLeaseLost(t, c, owner)
}

func TestLease_ExpiresAndAnotherOwnerReclaims(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	zip := f.zips[0]

	stale := f.mustClaim(t, "crashed/1", shortLease, zip)
	f.waitForDBClock(t, stale.Token)
	live := f.mustClaim(t, "survivor/1", longLease, zip)
	if !live.Token.After(stale.Token) {
		t.Errorf("re-claim Token %s is not after the expired one %s", live.Token, stale.Token)
	}

	// The first owner wakes up and tries to finish: nothing may change.
	f.assertLeaseLost(t, stale, "crashed/1")
	// Owner and token are both part of the guard.
	f.assertLeaseLost(t, live, "crashed/1")   // right token, wrong owner
	f.assertLeaseLost(t, stale, "survivor/1") // right owner, wrong token

	if r := f.row(t, zip); r.claimedBy == nil || *r.claimedBy != "survivor/1" || r.claimedUntil == nil || !r.claimedUntil.Equal(live.Token) {
		t.Errorf("row after the rejected completions: %s, want survivor/1 holding it until %s", r, live.Token.UTC().Format(time.RFC3339Nano))
	}
	if err := f.repo.MarkSearched(ctx, live, "survivor/1", 1); err != nil {
		t.Errorf("the live claim's MarkSearched: %v", err)
	}
}

func TestLease_SameOwnerReclaimGetsANewToken(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	const owner = "lonely/1"
	zip := f.zips[0]

	old := f.mustClaim(t, owner, shortLease, zip)
	f.waitForDBClock(t, old.Token)
	fresh := f.mustClaim(t, owner, longLease, zip)
	if fresh.Token.Equal(old.Token) {
		t.Fatalf("re-claim returned the same Token %s", old.Token)
	}

	// A one-instance fleet re-claims its own expired ZIP. The owner matches,
	// so only the token keeps the abandoned attempt from completing the new one.
	f.assertLeaseLost(t, old, owner)
	if err := f.repo.Release(ctx, fresh, owner); err != nil {
		t.Errorf("Release with the fresh Token: %v", err)
	}
}

// The guard is owner and token, with no clock in it: bookkeeping that arrives
// after the lease ran out still lands as long as nobody else has taken the
// ZIP, so a search that was paid for is recorded rather than repeated.
func TestLease_ExpiredButNotReclaimedStillCompletes(t *testing.T) {
	f := newFixture(t, 1)
	const owner = "slowpoke/1"
	zip := f.zips[0]

	c := f.mustClaim(t, owner, shortLease, zip)
	f.waitForDBClock(t, c.Token)
	if err := f.repo.MarkSearched(context.Background(), c, owner, 5); err != nil {
		t.Fatalf("MarkSearched on an expired lease nobody took over: %v", err)
	}
	if r := f.row(t, zip); r.lastSearchedAt == nil || r.claimedBy != nil {
		t.Errorf("row: %s, want searched and unclaimed", r)
	}
}

func TestDefer_HidesTheZipUntilThenAndKeepsTheResumePage(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	const owner = "deferrer/1"
	first, second := f.zips[0], f.zips[1]

	// One failure on the books, to show that Defer is not counted as another.
	c := f.mustClaim(t, owner, longLease, first)
	if _, err := f.repo.Fail(ctx, c, owner, pastUntil(), 2); err != nil {
		t.Fatalf("fail %s: %v", first, err)
	}
	c = f.mustClaim(t, owner, longLease, first)
	if c.ResumePage != 2 {
		t.Errorf("ResumePage after Fail(…, 2) = %d, want 2", c.ResumePage)
	}

	far := f.dbNow(t).Add(time.Hour)
	if err := f.repo.Defer(ctx, c, owner, far, 7); err != nil {
		t.Fatalf("defer %s: %v", first, err)
	}
	r := f.row(t, first)
	if r.claimedBy != nil {
		t.Errorf("Defer left an owner on the row: %s", r)
	}
	if r.claimedUntil == nil || !r.claimedUntil.Equal(far) {
		t.Errorf("claimed_until: %s, want until = %s (the kept lease is the backoff)", r, far.UTC().Format(time.RFC3339Nano))
	}
	assertCounters(t, r, 1, 7)
	if r.lastSearchedAt != nil {
		t.Errorf("Defer stamped the zip searched: %s", r)
	}
	f.assertLeaseLost(t, c, owner)

	// Deferred for an hour: the most populous ZIP is passed over.
	c2 := f.mustClaim(t, owner, longLease, second)

	// Deferred for a moment: claimable again once the database clock is past
	// until, and it comes back with the page to resume from.
	soon := f.dbNow(t).Add(100 * time.Millisecond)
	if err := f.repo.Defer(ctx, c2, owner, soon, 4); err != nil {
		t.Fatalf("defer %s: %v", second, err)
	}
	f.waitForDBClock(t, soon)
	c2 = f.mustClaim(t, "other/1", longLease, second)
	if c2.ResumePage != 4 {
		t.Errorf("ResumePage after Defer(…, 4) = %d, want 4", c2.ResumePage)
	}
}

func TestFail_BacksOffThenPushesBackAtMaxFailures(t *testing.T) {
	if MaxFailures != 5 {
		t.Fatalf("MaxFailures = %d, the design says 5", MaxFailures)
	}
	f := newFixture(t, 3)
	ctx := context.Background()
	const owner = "failer/1"
	backingOff, zip, last := f.zips[0], f.zips[1], f.zips[2]

	// fail claims zip, fails it with a backoff that is already over, and
	// checks the counters the failure leaves behind.
	fail := func(resumePage, wantFailures int) {
		t.Helper()
		c := f.mustClaim(t, owner, longLease, zip)
		until := pastUntil()
		pushedBack, err := f.repo.Fail(ctx, c, owner, until, resumePage)
		if err != nil {
			t.Fatalf("fail %s: %v", zip, err)
		}
		if pushedBack {
			t.Fatalf("failure %d pushed the zip back, want that only at %d", wantFailures, MaxFailures)
		}
		r := f.row(t, zip)
		assertCounters(t, r, wantFailures, resumePage)
		if r.claimedBy != nil || r.claimedUntil == nil || !r.claimedUntil.Equal(until.Truncate(time.Microsecond)) {
			t.Errorf("claim after Fail: %s, want no owner and claimed_until = %s", r, until.UTC().Format(time.RFC3339Nano))
		}
		if r.lastSearchedAt != nil {
			t.Errorf("failure %d stamped the zip searched: %s", wantFailures, r)
		}
	}

	// The kept lease is the backoff: a ZIP failed until an hour from now is
	// passed over, and the failed claim is spent.
	c := f.mustClaim(t, owner, longLease, backingOff)
	far := f.dbNow(t).Add(time.Hour)
	if pushedBack, err := f.repo.Fail(ctx, c, owner, far, 3); err != nil || pushedBack {
		t.Fatalf("Fail(%s) = %v, %v; want false, nil", backingOff, pushedBack, err)
	}
	if r := f.row(t, backingOff); r.claimedBy != nil || r.claimedUntil == nil || !r.claimedUntil.Equal(far) {
		t.Errorf("claim after Fail: %s, want no owner and claimed_until = %s", r, far.UTC().Format(time.RFC3339Nano))
	}
	f.assertLeaseLost(t, c, owner)

	// Two failures, then a success: the count starts over.
	fail(1, 1)
	fail(2, 2)
	c = f.mustClaim(t, owner, longLease, zip)
	if c.ResumePage != 2 {
		t.Errorf("ResumePage after Fail(…, 2) = %d, want 2", c.ResumePage)
	}
	if err := f.repo.MarkSearched(ctx, c, owner, 12); err != nil {
		t.Fatalf("mark %s searched: %v", zip, err)
	}
	assertCounters(t, f.row(t, zip), 0, 0)
	// Test-only: a searched ZIP is at the back of the rotation, behind every
	// real one. Un-stamp it so the test keeps getting it.
	if _, err := f.pool.Exec(ctx, `UPDATE zip_codes SET last_searched_at = NULL WHERE zip = $1`, zip); err != nil {
		t.Fatalf("un-stamp %s: %v", zip, err)
	}

	// Had MarkSearched not reset the count, the third of these would push back.
	for n := 1; n < MaxFailures; n++ {
		fail(n+10, n)
	}

	// The fifth consecutive failure sends the ZIP to the back of the rotation
	// with a clean slate instead of backing it off again.
	c = f.mustClaim(t, owner, longLease, zip)
	before := f.dbNow(t)
	pushedBack, err := f.repo.Fail(ctx, c, owner, f.dbNow(t).Add(time.Hour), 6)
	after := f.dbNow(t)
	if err != nil {
		t.Fatalf("fifth fail of %s: %v", zip, err)
	}
	if !pushedBack {
		t.Errorf("failure %d did not report pushedBack", MaxFailures)
	}
	r := f.row(t, zip)
	assertCounters(t, r, 0, 0)
	if r.claimedBy != nil || r.claimedUntil != nil {
		t.Errorf("push-back left a claim or a backoff on the row: %s", r)
	}
	if r.lastSearchedAt == nil || r.lastSearchedAt.Before(before) || r.lastSearchedAt.After(after) {
		t.Errorf("last_searched_at: %s, want stamped within [%s, %s]", r, before, after)
	}
	if r.lastListingCount == nil || *r.lastListingCount != 12 {
		t.Errorf("last_listing_count: %s, want the 12 of the last real search", r)
	}
	f.assertLeaseLost(t, c, owner)

	// Free, not backing off, and still not next: it is behind the ZIP that
	// has never been searched.
	f.mustClaim(t, owner, longLease, last)
}

func TestRelease_ClearsTheClaimAndNothingElse(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	const owner = "releaser/1"
	zip := f.zips[0]

	// Give the columns Release must not touch a value it could not restore by
	// accident: a failure, a resume page and a listing count. last_searched_at
	// stays NULL so the ZIP remains first in the rotation; a Release that
	// stamped it would still show.
	c := f.mustClaim(t, owner, longLease, zip)
	if _, err := f.repo.Fail(ctx, c, owner, pastUntil(), 2); err != nil {
		t.Fatalf("fail %s: %v", zip, err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE zip_codes SET last_listing_count = 8 WHERE zip = $1`, zip); err != nil {
		t.Fatalf("set listing count of %s: %v", zip, err)
	}
	c = f.mustClaim(t, owner, longLease, zip)

	want := f.row(t, zip)
	want.claimedBy, want.claimedUntil = nil, nil
	if err := f.repo.Release(ctx, c, owner); err != nil {
		t.Fatalf("release %s: %v", zip, err)
	}
	got := f.row(t, zip)
	if got.String() != want.String() {
		t.Errorf("row after Release:\n  got: %s\n want: %s", got, want)
	}
	assertCounters(t, got, 1, 2)

	f.assertLeaseLost(t, c, owner)

	// Released means claimable at once, by anyone, from where it stopped.
	if c = f.mustClaim(t, "other/1", longLease, zip); c.ResumePage != 2 {
		t.Errorf("ResumePage after Release = %d, want 2", c.ResumePage)
	}
}

// A lease too short to hold is refused rather than stored: the ZIP stays
// unclaimed, so the two owners below cannot both come away believing they have
// it. The shortest lease that is accepted is a real one, ending after the
// database clock it was taken on.
func TestClaim_RefusesALeaseTooShortToHold(t *testing.T) {
	f := newFixture(t, 1)
	zip := f.zips[0]

	// 900 is what 15 minutes comes to when it is written as a bare number of
	// seconds. 500 ns is stored as a zero interval, 999 ns as one microsecond;
	// either is over before the statement has returned.
	for _, lease := range []time.Duration{500 * time.Nanosecond, 900, 999 * time.Nanosecond, minLease - 1} {
		c, err := f.claimChecked("hasty/1", lease)
		if err == nil {
			t.Errorf("Claim with a lease of %s = %+v, want an error", lease, c)
		}
		if r := f.row(t, zip); r.claimedBy != nil || r.claimedUntil != nil {
			t.Fatalf("the refused Claim with a lease of %s wrote to the row: %s", lease, r)
		}
	}

	before := f.dbNow(t)
	c := f.mustClaim(t, "patient/1", minLease, zip)
	if !c.Token.After(before) {
		t.Errorf("Token of a %s lease = %s, want after the database clock %s it was taken on", minLease, c.Token, before)
	}
}

// The tripwire in newFixture: a live claim on a real ZIP means a worker is
// attached and at work. What a worker leaves behind without being there (an
// expired claim, the ownerless backoff of Defer and Fail) and what an aborted
// run of these tests leaves behind (a live claim on a test ZIP) must not trip
// it, or the suite would refuse databases it is safe in.
func TestFixture_RefusesADatabaseAWorkerIsClaimingFrom(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	refuseADatabaseInUse(t, pool)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	var (
		// Stands in for a real ZIP: all that matters is the missing prefix.
		realZip  = "not-" + testZipPrefix + suffix
		leftover = testZipPrefix + suffix + "-left"
		both     = []string{realZip, leftover}
		worker   = "box-7/4f1c"
	)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM zip_codes WHERE zip = ANY($1)`, both); err != nil {
			t.Errorf("cleanup zip codes: %v", err)
		}
	})
	// Already searched and unpopulated: the back of the rotation, out of the
	// way of anything that does claim from this database.
	const ins = `
INSERT INTO zip_codes (zip, city, state, population, last_searched_at)
SELECT z, 'Claimtest', 'ZZ', 0, now() FROM unnest($1::text[]) AS t(z)`
	if _, err := pool.Exec(ctx, ins, both); err != nil {
		t.Fatalf("insert zip codes: %v", err)
	}

	for _, tc := range []struct {
		name  string
		zip   string
		owner *string
		// secs places claimed_until relative to the database clock. The live
		// ones are short, so that an aborted run blocks the next for seconds.
		secs float64
		want bool
	}{
		{"a live claim on a real zip", realZip, &worker, 10, true},
		{"an expired claim on a real zip", realZip, &worker, -1, false},
		{"the ownerless backoff that Defer and Fail leave", realZip, nil, 10, false},
		{"a live claim on a test zip, left by an aborted run", leftover, &worker, 10, false},
	} {
		if _, err := pool.Exec(ctx, `UPDATE zip_codes SET claimed_by = NULL, claimed_until = NULL WHERE zip = ANY($1)`, both); err != nil {
			t.Fatalf("%s: clear the rows: %v", tc.name, err)
		}
		const set = `UPDATE zip_codes SET claimed_by = $2, claimed_until = now() + make_interval(secs => $3) WHERE zip = $1`
		if _, err := pool.Exec(ctx, set, tc.zip, tc.owner, tc.secs); err != nil {
			t.Fatalf("%s: set the claim: %v", tc.name, err)
		}
		got, err := foreignLiveClaim(ctx, pool)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		switch {
		case tc.want && (!strings.Contains(got, tc.zip) || !strings.Contains(got, worker)):
			t.Errorf("%s: foreignLiveClaim = %q, want it to name the zip %s and its holder %s", tc.name, got, tc.zip, worker)
		case !tc.want && got != "":
			t.Errorf("%s: foreignLiveClaim = %q, want none", tc.name, got)
		}
	}
}

// Runs without a database: all are rejected before the pool is touched. An
// empty owner would make every instance pass every other's guard, and a lease
// below minLease is a claim anyone may take again immediately.
func TestClaim_RejectsAnEmptyOwnerAndALeaseTooShortToHold(t *testing.T) {
	r := NewRepository(nil)
	for _, tc := range []struct {
		name  string
		owner string
		lease time.Duration
	}{
		{"empty owner", "", time.Minute},
		{"zero lease", "box/1", 0},
		{"negative lease", "box/1", -time.Second},
		{"lease stored as a zero interval", "box/1", 500 * time.Nanosecond},
		{"sub-microsecond lease", "box/1", 999 * time.Nanosecond},
		{"15 minutes as a bare number of seconds", "box/1", 900},
		{"15 minutes as a bare number of milliseconds", "box/1", 900_000},
		{"just under the minimum", "box/1", minLease - 1},
	} {
		if c, err := r.Claim(context.Background(), tc.owner, tc.lease); err == nil || c != nil {
			t.Errorf("%s: Claim = %v, %v; want nil and an error", tc.name, c, err)
		}
	}
}
