package property

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/db"
)

// attemptsPredicate is the text the claim statement and the partial index
// idx_properties_details_todo must share: PostgreSQL only uses a partial index
// when it can prove the query's WHERE implies the index's, which it can for a
// literal and cannot for a bind parameter.
var attemptsPredicate = fmt.Sprintf("details_attempts < %d", MaxDetailsAttempts)

// TestRepository_EmptyInputsDoNotQuery needs no database: a Repository without
// a pool panics on any query, so returning normally proves none was made.
func TestRepository_EmptyInputsDoNotQuery(t *testing.T) {
	ctx := context.Background()
	r := NewRepository(nil)

	states, err := r.VideoStates(ctx, nil)
	if err != nil || states == nil || len(states) != 0 {
		t.Errorf("VideoStates(nil) = %v, %v; want an empty non-nil map, nil", states, err)
	}
	for _, limit := range []int{0, -1} {
		zpids, err := r.ClaimMissingDetails(ctx, limit, time.Minute)
		if err != nil || len(zpids) != 0 {
			t.Errorf("ClaimMissingDetails(limit %d) = %v, %v; want nothing, nil", limit, zpids, err)
		}
	}
	if err := r.ReleaseDetails(ctx, nil); err != nil {
		t.Errorf("ReleaseDetails(nil) = %v, want nil", err)
	}
}

// TestClaimMissingDetails_RejectsNonPositiveLease: a lease that has already
// expired when it is written claims nothing, so every instance would pay for
// the same rows. That is a caller bug worth failing loudly on.
func TestClaimMissingDetails_RejectsNonPositiveLease(t *testing.T) {
	r := NewRepository(nil)
	for _, lease := range []time.Duration{0, -time.Second} {
		if zpids, err := r.ClaimMissingDetails(context.Background(), 5, lease); err == nil {
			t.Errorf("ClaimMissingDetails(lease %s) = %v, nil; want an error", lease, zpids)
		}
	}
}

func TestClaimStatement_UsesTheAttemptsLiteral(t *testing.T) {
	if !strings.Contains(claimMissingDetailsSQL, attemptsPredicate) {
		t.Errorf("the claim statement must contain %q so idx_properties_details_todo stays usable:\n%s",
			attemptsPredicate, claimMissingDetailsSQL)
	}
}

// TestRepository_Integration covers the SQL whose correctness is the point of
// the multi-instance design (docs/superpowers/specs/2026-09-19-multi-instance-
// workers-design.md §3.5, §3.6): the details claim, its lease and attempt
// accounting, and the guarded writes. None of it can be faked — it is the
// row-locking and the WHERE clauses themselves that are under test.
//
// Skipped unless TEST_DATABASE_URL points at a database it may write to:
//
//	TEST_DATABASE_URL=postgres://dwellings:dwellings@localhost:5432/dwellings?sslmode=disable \
//	    go test ./internal/property/ -run TestRepository_Integration
//
// Every row it writes is namespaced by a per-run suffix and removed again, so
// it can run against a database that already holds data. The details claim is
// global (oldest created_at first), so the test rows are dated 1970 to sort
// ahead of anything real; rows of other owners that a claim picks up anyway are
// released again. For the same reason the subtests must not run in parallel,
// and two runs against one database at the same time will disturb each other.
func TestRepository_Integration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the property repository integration test")
	}
	ctx := context.Background()
	// Wider than the default pool so the concurrent claimers below really do
	// reach PostgreSQL at the same time instead of queueing for a connection.
	pool, err := db.Connect(ctx, url, 16)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// A run that was killed before its cleanup leaves 1970 rows behind, which
	// would sort ahead of this run's and be claimed instead of them.
	if _, err := pool.Exec(ctx, `DELETE FROM properties WHERE zpid LIKE $1 AND created_at < '1971-01-01'`,
		zpidPrefix+"%"); err != nil {
		t.Fatalf("remove rows of an interrupted run: %v", err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)

	t.Run("index predicate matches MaxDetailsAttempts", func(t *testing.T) {
		var def string
		if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef('idx_properties_details_todo'::regclass)`).Scan(&def); err != nil {
			t.Fatalf("read index definition: %v", err)
		}
		if !strings.Contains(def, attemptsPredicate) {
			t.Errorf("idx_properties_details_todo = %q, want it to contain %q (schema.sql and MaxDetailsAttempts disagree)",
				def, attemptsPredicate)
		}
		if !strings.Contains(def, "details_fetched_at IS NULL") {
			t.Errorf("idx_properties_details_todo = %q, want it to contain %q", def, "details_fetched_at IS NULL")
		}
	})

	t.Run("claim returns the oldest rows, oldest first", func(t *testing.T) {
		f := newFixture(t, pool, suffix)
		zpids := f.insert("o1", "o2", "o3", "o4", "o5")
		// Reverse the ages so that insertion (and id) order cannot pass for
		// created_at order.
		for i, zpid := range zpids {
			f.exec(`UPDATE properties SET created_at = to_timestamp($2) WHERE zpid = $1`, zpid, 100-i)
		}
		want := []string{zpids[4], zpids[3], zpids[2]}
		if got := f.claim(3, time.Minute); !slices.Equal(got, want) {
			t.Errorf("first claim = %v, want %v", got, want)
		}
		want = []string{zpids[1], zpids[0]}
		if got := f.claim(3, time.Minute); !slices.Equal(got, want) {
			t.Errorf("second claim = %v, want the remaining %v", got, want)
		}
	})

	t.Run("concurrent claims are pairwise disjoint", func(t *testing.T) {
		f := newFixture(t, pool, suffix)
		const rows, workers, batch = 100, 12, 5
		names := make([]string, rows)
		for i := range names {
			names[i] = fmt.Sprintf("c%03d", i)
		}
		zpids := f.insert(names...)

		var (
			mu      sync.Mutex
			batches [][]string
			errs    []error
			wg      sync.WaitGroup
			start   = make(chan struct{})
		)
		for range workers {
			wg.Go(func() {
				<-start
				// Bounded so that a claim which hands out the same rows forever
				// fails the test instead of hanging it.
				for range rows/batch + 2 {
					got, err := f.repo.ClaimMissingDetails(f.ctx, batch, time.Minute)
					mu.Lock()
					if err != nil {
						errs = append(errs, err)
					} else {
						batches = append(batches, got)
					}
					mu.Unlock()
					// The test rows sort first, so a batch without any of them
					// means they are all taken.
					if err != nil || len(f.own(got)) == 0 {
						return
					}
				}
			})
		}
		close(start)
		wg.Wait()

		for _, err := range errs {
			t.Errorf("claim: %v", err)
		}
		claimed := make(map[string]int)
		for _, b := range batches {
			if len(b) > batch {
				t.Errorf("a claim returned %d zpids, want at most %d", len(b), batch)
			}
			for _, zpid := range b {
				claimed[zpid]++
			}
		}
		// One line per kind of failure: a broken claim gets all 100 rows wrong.
		var twice, never, counted []string
		for zpid, n := range claimed {
			if n != 1 {
				twice = append(twice, zpid)
			}
		}
		for _, zpid := range zpids {
			if claimed[zpid] == 0 {
				never = append(never, zpid)
			}
			if f.attempts(zpid) != 0 {
				counted = append(counted, zpid)
			}
		}
		slices.Sort(twice)
		if len(twice) > 0 {
			t.Errorf("%d zpids were handed to more than one claimer, e.g. %s", len(twice), twice[0])
		}
		if len(never) > 0 {
			t.Errorf("%d of %d zpids were never claimed, e.g. %s", len(never), rows, never[0])
		}
		if len(counted) > 0 {
			t.Errorf("%d zpids have details_attempts > 0 after a claim, e.g. %s (attempts are counted on failure only)", len(counted), counted[0])
		}
	})

	// The subtest above cannot tell SKIP LOCKED from a plain FOR UPDATE: that
	// one queues the claimers behind each other and drops the rows on the READ
	// COMMITTED re-check, so the batches come out disjoint all the same. What
	// differs is what a claimer does at a row another session holds — pass over
	// it, or wait — and showing that takes a lock held on purpose. Waiting is
	// the bug: ten instances would claim one after the other, each behind the
	// slowest writer to any of the oldest rows.
	t.Run("claim passes over a locked row instead of waiting for it", func(t *testing.T) {
		f := newFixture(t, pool, suffix)
		zpids := f.insert("k1", "k2", "k3")

		// Another session in the middle of a write to the oldest row. A defer
		// runs before the fixture's cleanup, whose DELETE would otherwise wait
		// for this very lock.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the lock-holding transaction: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT 1 FROM properties WHERE zpid = $1 FOR UPDATE`, zpids[0]); err != nil {
			t.Fatalf("lock the oldest row: %v", err)
		}

		// The deadline is what turns "waits for the lock" into a failure. A
		// claim that skips the row answers in milliseconds.
		claimCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		got, err := f.repo.ClaimMissingDetails(claimCtx, 3, time.Minute)
		if err != nil {
			t.Fatalf("claim while another session holds the oldest row: %v (the claim must skip a locked row, not wait for it)", err)
		}
		if mine, want := f.own(got), zpids[1:]; !slices.Equal(mine, want) {
			t.Errorf("claim while %s is locked = %v, want the unlocked %v", zpids[0], mine, want)
		}

		// Skipped is not leased: once the other session is done the row is
		// there for the next claim.
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("roll back the lock-holding transaction: %v", err)
		}
		if got, want := f.claim(3, time.Minute), zpids[:1]; !slices.Equal(got, want) {
			t.Errorf("claim after the lock was dropped = %v, want the skipped %v", got, want)
		}
	})

	t.Run("a lease hides rows until it expires", func(t *testing.T) {
		f := newFixture(t, pool, suffix)
		zpids := f.insert("l1", "l2", "l3")

		if got := f.claim(3, time.Minute); !slices.Equal(got, zpids) {
			t.Fatalf("first claim = %v, want %v", got, zpids)
		}
		if got := f.claim(3, time.Minute); len(got) != 0 {
			t.Errorf("second claim = %v, want none of the leased rows", got)
		}
		f.release(zpids...)

		// The lease runs on the database clock. t0 is taken before the claim
		// that starts it and elapsed after the claim that outlives it, so
		// elapsed < lease can only mean the rows came back early.
		const lease = 80 * time.Millisecond
		t0 := time.Now()
		if got := f.claim(3, lease); !slices.Equal(got, zpids) {
			t.Fatalf("claim after release = %v, want %v", got, zpids)
		}
		var back []string
		for len(back) == 0 && time.Since(t0) < 3*time.Second {
			time.Sleep(5 * time.Millisecond)
			back = f.claim(3, time.Minute)
		}
		elapsed := time.Since(t0)
		if !slices.Equal(back, zpids) {
			t.Fatalf("claim after the lease expired = %v, want %v", back, zpids)
		}
		if elapsed < lease {
			t.Errorf("the rows were claimable again after %s, before their %s lease expired", elapsed, lease)
		}
		for _, zpid := range zpids {
			if got := f.attempts(zpid); got != 0 {
				t.Errorf("zpid %s has details_attempts = %d after three claims, want 0", zpid, got)
			}
		}
	})

	t.Run("ReleaseDetails frees rows at once and counts no attempt", func(t *testing.T) {
		f := newFixture(t, pool, suffix)
		zpids := f.insert("r1", "r2", "r3")
		if got := f.claim(3, time.Minute); !slices.Equal(got, zpids) {
			t.Fatalf("claim = %v, want %v", got, zpids)
		}

		f.release(zpids[0], zpids[2])

		want := []string{zpids[0], zpids[2]}
		if got := f.claim(3, time.Minute); !slices.Equal(got, want) {
			t.Errorf("claim after release = %v, want exactly the released %v", got, want)
		}
		for _, zpid := range zpids {
			if got := f.attempts(zpid); got != 0 {
				t.Errorf("zpid %s has details_attempts = %d, want 0", zpid, got)
			}
		}
	})

	t.Run("FailDetails counts attempts, keeps the lease and abandons the row at the limit", func(t *testing.T) {
		f := newFixture(t, pool, suffix)
		zpid := f.insert("f1")[0]
		if got := f.claim(1, time.Minute); !slices.Equal(got, []string{zpid}) {
			t.Fatalf("claim = %v, want %v", got, zpid)
		}
		lease := f.leaseOf(zpid)
		if lease == nil {
			t.Fatal("the claim wrote no details_claimed_until")
		}

		for i := 1; i < MaxDetailsAttempts; i++ {
			if err := f.repo.FailDetails(f.ctx, zpid); err != nil {
				t.Fatalf("fail details: %v", err)
			}
			if got := f.attempts(zpid); got != i {
				t.Fatalf("details_attempts = %d after %d failures", got, i)
			}
		}
		if got := f.leaseOf(zpid); got == nil || !got.Equal(*lease) {
			t.Errorf("details_claimed_until = %v after failures, want the claim's %v (the lease is the backoff)", got, lease)
		}
		if got := f.claim(1, time.Minute); len(got) != 0 {
			t.Errorf("claim = %v, want nothing while the failed row's lease runs", got)
		}

		// One short of the limit the row is still worked on once its lease is
		// over; the expiry is simulated so the test does not have to wait.
		f.exec(`UPDATE properties SET details_claimed_until = now() - interval '1 second' WHERE zpid = $1`, zpid)
		if got := f.claim(1, time.Minute); !slices.Equal(got, []string{zpid}) {
			t.Fatalf("claim with %d attempts = %v, want %v", MaxDetailsAttempts-1, got, zpid)
		}

		if err := f.repo.FailDetails(f.ctx, zpid); err != nil {
			t.Fatalf("fail details: %v", err)
		}
		if got := f.attempts(zpid); got != MaxDetailsAttempts {
			t.Fatalf("details_attempts = %d, want %d", got, MaxDetailsAttempts)
		}
		f.exec(`UPDATE properties SET details_claimed_until = NULL WHERE zpid = $1`, zpid)
		if got := f.claim(1, time.Minute); len(got) != 0 {
			t.Errorf("claim = %v, want nothing: a row with %d attempts is abandoned even without a lease", got, MaxDetailsAttempts)
		}

		if err := f.repo.FailDetails(f.ctx, f.zpid("no-such-row")); err != nil {
			t.Errorf("FailDetails of an unknown zpid = %v, want nil", err)
		}
	})

	// updated_at is public: the detail endpoint returns it. A claim, a counted
	// failure and a release are the fleet's bookkeeping, not a change to the
	// listing. Every other UPDATE in the repository stamps it, so it is an easy
	// thing to add here out of habit — and then every details batch of every
	// instance reports ten untouched listings as just updated.
	t.Run("claim bookkeeping leaves updated_at alone, storing details moves it", func(t *testing.T) {
		f := newFixture(t, pool, suffix)
		zpid := f.insert("u1")[0]
		before := f.updatedAt(zpid)

		// Each step proves that it wrote the row, so that "unchanged" cannot
		// pass on a statement that matched nothing.
		steps := []struct {
			name  string
			do    func() error
			wrote func() bool
		}{
			{
				name: "ClaimMissingDetails",
				do: func() error {
					if got := f.claim(1, time.Minute); !slices.Equal(got, []string{zpid}) {
						return fmt.Errorf("claim = %v, want %v", got, zpid)
					}
					return nil
				},
				wrote: func() bool { return f.leaseOf(zpid) != nil },
			},
			{
				name:  "FailDetails",
				do:    func() error { return f.repo.FailDetails(f.ctx, zpid) },
				wrote: func() bool { return f.attempts(zpid) == 1 },
			},
			{
				name:  "ReleaseDetails",
				do:    func() error { return f.repo.ReleaseDetails(f.ctx, []string{zpid}) },
				wrote: func() bool { return f.leaseOf(zpid) == nil },
			},
		}
		for _, s := range steps {
			if err := s.do(); err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			if !s.wrote() {
				t.Fatalf("%s did not write the row", s.name)
			}
			// Judged against the value before this step, so a failure names
			// the statement at fault and not every one after it as well.
			got := f.updatedAt(zpid)
			if !got.Equal(before) {
				t.Errorf("%s moved updated_at from %v to %v; it is bookkeeping, not a change to the listing", s.name, before, got)
			}
			before = got
		}

		// The contrast: enrichment does change what the API reports.
		desc := "Now with a description."
		if err := f.repo.SetDetails(f.ctx, zpid, &Details{Description: &desc}, nil); err != nil {
			t.Fatalf("set details: %v", err)
		}
		if got := f.updatedAt(zpid); !got.After(before) {
			t.Errorf("updated_at = %v after SetDetails, want it moved on from %v", got, before)
		}
	})

	t.Run("SetDetails stores once and never overwrites", func(t *testing.T) {
		f := newFixture(t, pool, suffix)
		zpid := f.insert("d1")[0]
		if got := f.claim(1, time.Minute); !slices.Equal(got, []string{zpid}) {
			t.Fatalf("claim = %v, want %v", got, zpid)
		}

		desc, year := "A bright corner lot.", 1987
		if err := f.repo.SetDetails(f.ctx, zpid, &Details{Description: &desc, YearBuilt: &year}, []byte(`{"ok":true}`)); err != nil {
			t.Fatalf("set details: %v", err)
		}
		stored, err := f.repo.GetByZPID(f.ctx, zpid)
		if err != nil {
			t.Fatalf("get property: %v", err)
		}
		if stored.Description == nil || *stored.Description != desc || stored.YearBuilt == nil || *stored.YearBuilt != year {
			t.Errorf("stored details = %v / %v, want %q / %d", stored.Description, stored.YearBuilt, desc, year)
		}
		if stored.DetailsFetchedAt == nil {
			t.Fatal("details_fetched_at was not stamped")
		}
		if got := f.leaseOf(zpid); got != nil {
			t.Errorf("details_claimed_until = %v after SetDetails, want NULL", got)
		}

		// What a second instance does when its own call for the same listing
		// came back not-found: it must not wipe the record stored above.
		if err := f.repo.SetDetails(f.ctx, zpid, &Details{}, nil); err != nil {
			t.Fatalf("second set details = %v, want nil (zero rows is not an error)", err)
		}
		after, err := f.repo.GetByZPID(f.ctx, zpid)
		if err != nil {
			t.Fatalf("get property: %v", err)
		}
		if after.Description == nil || *after.Description != desc || after.YearBuilt == nil || *after.YearBuilt != year {
			t.Errorf("details after an empty write = %v / %v, want the stored %q / %d", after.Description, after.YearBuilt, desc, year)
		}
		if !after.DetailsFetchedAt.Equal(*stored.DetailsFetchedAt) {
			t.Errorf("details_fetched_at moved from %v to %v", stored.DetailsFetchedAt, after.DetailsFetchedAt)
		}
		var raw string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(details_raw::text, '') FROM properties WHERE zpid = $1`, zpid).Scan(&raw); err != nil {
			t.Fatalf("read details_raw: %v", err)
		}
		if !strings.Contains(raw, "ok") {
			t.Errorf("details_raw = %q after an empty write, want the stored response", raw)
		}

		if got := f.claim(1, time.Minute); len(got) != 0 {
			t.Errorf("claim = %v, want nothing: an enriched row is done", got)
		}
		if err := f.repo.SetDetails(f.ctx, f.zpid("no-such-row"), &Details{}, nil); err != nil {
			t.Errorf("SetDetails of an unknown zpid = %v, want nil", err)
		}
	})

	t.Run("SetVideoFailed never takes a ready video off the feed", func(t *testing.T) {
		f := newFixture(t, pool, suffix)
		zpids := f.insert("v-ready", "v-pending", "v-failed")
		ready, pending, failed := zpids[0], zpids[1], zpids[2]
		f.exec(`UPDATE properties SET video_status = 'ready', video_url = 'https://cdn.example/v.mp4' WHERE zpid = $1`, ready)
		f.exec(`UPDATE properties SET video_status = 'failed' WHERE zpid = $1`, failed)

		for _, zpid := range append(zpids, f.zpid("no-such-row")) {
			if err := f.repo.SetVideoFailed(f.ctx, zpid); err != nil {
				t.Fatalf("set video failed %s: %v", zpid, err)
			}
		}
		for zpid, want := range map[string]string{ready: "ready", pending: "failed", failed: "failed"} {
			if got := f.videoStatus(zpid); got != want {
				t.Errorf("video_status of %s = %q, want %q", zpid, got, want)
			}
		}
	})

	t.Run("VideoStates reports stored zpids only, as NeedsVideo would", func(t *testing.T) {
		f := newFixture(t, pool, suffix)
		zpids := f.insert("s-ready", "s-pending", "s-failed", "s-ready-empty-url", "s-ready-null-url")
		f.exec(`UPDATE properties SET video_status = 'ready', video_url = 'https://cdn.example/v.mp4' WHERE zpid = $1`, zpids[0])
		f.exec(`UPDATE properties SET video_status = 'failed' WHERE zpid = $1`, zpids[2])
		f.exec(`UPDATE properties SET video_status = 'ready', video_url = '' WHERE zpid = $1`, zpids[3])
		f.exec(`UPDATE properties SET video_status = 'ready', video_url = NULL WHERE zpid = $1`, zpids[4])
		want := map[string]bool{
			zpids[0]: false,
			zpids[1]: true,
			zpids[2]: true,
			zpids[3]: true,
			zpids[4]: true,
		}

		// An unknown zpid, a duplicate and the empty zpid a malformed search
		// result can carry must not show up in the answer.
		ask := append([]string{f.zpid("no-such-row"), "", zpids[0]}, zpids...)
		got, err := f.repo.VideoStates(f.ctx, ask)
		if err != nil {
			t.Fatalf("video states: %v", err)
		}
		if len(got) != len(want) {
			t.Errorf("VideoStates = %v, want exactly the %d stored zpids", got, len(want))
		}
		for zpid, needs := range want {
			if state, ok := got[zpid]; !ok || state != needs {
				t.Errorf("VideoStates[%s] = %v (present %v), want %v", zpid, state, ok, needs)
			}
			single, err := f.repo.NeedsVideo(f.ctx, zpid)
			if err != nil {
				t.Fatalf("needs video %s: %v", zpid, err)
			}
			if single != got[zpid] {
				t.Errorf("VideoStates[%s] = %v but NeedsVideo = %v; the two must share one predicate", zpid, got[zpid], single)
			}
		}
	})
}

// zpidPrefix namespaces every row this file writes.
const zpidPrefix = "it-property-"

// fixture owns the rows of one subtest.
type fixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	repo   *Repository
	prefix string

	mu      sync.Mutex
	rows    []string // zpids inserted, for cleanup
	foreign []string // zpids of other owners a claim picked up, to release
}

func newFixture(t *testing.T, pool *pgxpool.Pool, suffix string) *fixture {
	t.Helper()
	f := &fixture{
		t:      t,
		ctx:    context.Background(),
		pool:   pool,
		repo:   NewRepository(pool),
		prefix: zpidPrefix + suffix + "-",
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(f.ctx, `DELETE FROM properties WHERE zpid = ANY($1)`, f.rows); err != nil {
			t.Errorf("cleanup rows: %v", err)
		}
		if err := f.repo.ReleaseDetails(f.ctx, f.foreign); err != nil {
			t.Errorf("release rows of other owners: %v", err)
		}
	})
	return f
}

func (f *fixture) zpid(name string) string { return f.prefix + name }

// insert stores one un-enriched listing per name and returns their zpids.
// created_at is 1970-01-01 plus the position, so the rows are claimed before
// anything real and in the order given. updated_at gets the same date rather
// than the default now(): a statement that stamps it is then off by decades,
// not by the microseconds between two transactions.
func (f *fixture) insert(names ...string) []string {
	f.t.Helper()
	zpids := make([]string, len(names))
	for i, name := range names {
		zpids[i] = f.zpid(name)
	}
	const q = `
INSERT INTO properties (zpid, address, created_at, updated_at)
SELECT z, '1 Test Way', to_timestamp(ord), to_timestamp(ord)
  FROM unnest($1::text[]) WITH ORDINALITY AS t(z, ord)`
	if _, err := f.pool.Exec(f.ctx, q, zpids); err != nil {
		f.t.Fatalf("insert test rows: %v", err)
	}
	f.rows = append(f.rows, zpids...)
	return zpids
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

// claim calls ClaimMissingDetails and returns this run's zpids among the
// result, in the order they came back.
func (f *fixture) claim(limit int, lease time.Duration) []string {
	f.t.Helper()
	got, err := f.repo.ClaimMissingDetails(f.ctx, limit, lease)
	if err != nil {
		f.t.Fatalf("claim missing details: %v", err)
	}
	return f.own(got)
}

// own filters zpids down to this subtest's rows and remembers the others so
// the cleanup can hand them back. Safe for concurrent use.
func (f *fixture) own(zpids []string) []string {
	var mine []string
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, zpid := range zpids {
		if strings.HasPrefix(zpid, f.prefix) {
			mine = append(mine, zpid)
		} else {
			f.foreign = append(f.foreign, zpid)
		}
	}
	return mine
}

func (f *fixture) release(zpids ...string) {
	f.t.Helper()
	if err := f.repo.ReleaseDetails(f.ctx, zpids); err != nil {
		f.t.Fatalf("release details: %v", err)
	}
}

func (f *fixture) attempts(zpid string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT details_attempts FROM properties WHERE zpid = $1`, zpid).Scan(&n); err != nil {
		f.t.Fatalf("read details_attempts of %s: %v", zpid, err)
	}
	return n
}

func (f *fixture) leaseOf(zpid string) *time.Time {
	f.t.Helper()
	var until *time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT details_claimed_until FROM properties WHERE zpid = $1`, zpid).Scan(&until); err != nil {
		f.t.Fatalf("read details_claimed_until of %s: %v", zpid, err)
	}
	return until
}

func (f *fixture) updatedAt(zpid string) time.Time {
	f.t.Helper()
	var at time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT updated_at FROM properties WHERE zpid = $1`, zpid).Scan(&at); err != nil {
		f.t.Fatalf("read updated_at of %s: %v", zpid, err)
	}
	return at
}

func (f *fixture) videoStatus(zpid string) string {
	f.t.Helper()
	var status string
	if err := f.pool.QueryRow(f.ctx, `SELECT video_status FROM properties WHERE zpid = $1`, zpid).Scan(&status); err != nil {
		f.t.Fatalf("read video_status of %s: %v", zpid, err)
	}
	return status
}
