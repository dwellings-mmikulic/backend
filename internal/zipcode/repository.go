// Package zipcode persists the ZIP rotation state that drives nationwide
// collection: which ZIP code is searched next, when each was last searched,
// and which instance of the fleet is searching one right now.
package zipcode

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxFailures is how many consecutive failed searches a ZIP gets before it is
// stamped searched anyway. Failed ZIPs keep their place at the front of the
// rotation, so without a limit a ZIP the provider cannot serve would be
// retried, and paid for, forever.
const MaxFailures = 5

// minLease is the shortest lease Claim accepts. It is not what makes a lease
// long enough, which only the caller knows (longer than the deadline its work
// runs under); it is what keeps a lease from being none at all. claimed_until
// holds microseconds, and half a microsecond or less is stored as a zero
// interval: a claim that the next Claim, by anyone, takes again while its
// holder is still searching and paying. Anything under a millisecond is over
// before the round trip that reports it, so refusing it costs nothing real,
// and it is what a bare number passed as a time.Duration comes to: 900, meant
// as seconds, is 900 ns, and 15 minutes written in milliseconds is 0.9 ms.
const minLease = time.Millisecond

// ErrLeaseLost reports that a claim is no longer the caller's: it was already
// completed, or it expired and the ZIP was claimed again. The statement that
// returns it has changed nothing, and there is nothing for the caller to undo;
// whoever holds the ZIP now does its bookkeeping.
var ErrLeaseLost = errors.New("zip claim lost")

// Claim is one instance's exclusive, expiring hold on a ZIP.
type Claim struct {
	Zip string
	// ResumePage is where the previous search of this ZIP was cut short, so
	// pages already paid for are not bought again. 0 means from the start.
	ResumePage int
	// Token is the claimed_until this claim wrote: its fencing token. The
	// owner alone cannot tell two claims by one process apart, and a process
	// whose lease ran out may well be the one that claims the ZIP again. It
	// must be passed back exactly as returned.
	Token time.Time
}

// Repository reads and updates ZIP rotation state in PostgreSQL.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository creates a ZIP rotation repository backed by the given pool.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// held is the guard on every transition out of a claim: the row must still
// carry this owner and this token. It has no clock in it on purpose: the work
// under a claim runs under a deadline shorter than the lease, and bookkeeping
// that arrives late anyway should still land as long as nobody has taken the
// ZIP over, rather than throw away a search that was paid for.
const held = `
 WHERE zip = $1 AND claimed_by = $2 AND claimed_until = $3`

// Claim takes the next ZIP in rotation order that nobody holds: never-searched
// ZIPs first (most populous first, so dense markets are indexed early), then
// stalest-searched first. It returns nil, nil when none is claimable.
//
// Selecting and claiming are one statement. FOR UPDATE SKIP LOCKED sends
// concurrent claimers to different rows instead of queueing them on the same
// one, and under READ COMMITTED the lock step re-checks claimed_until against
// the newest row version, so a ZIP claimed since the snapshot was taken is
// passed over rather than claimed twice. The CTE is MATERIALIZED so the row is
// chosen and locked exactly once, whatever plan the UPDATE gets.
//
// The lease is measured on the database clock, the one clock the whole fleet
// shares. A crashed owner's claim frees itself when the lease runs out.
func (r *Repository) Claim(ctx context.Context, owner string, lease time.Duration) (*Claim, error) {
	if owner == "" {
		return nil, errors.New("claim zip: empty owner")
	}
	if lease < minLease {
		return nil, fmt.Errorf("claim zip: lease %s is shorter than the minimum %s", lease, minLease)
	}
	const q = `
WITH next AS MATERIALIZED (
    SELECT zip FROM zip_codes
     WHERE claimed_until IS NULL OR claimed_until <= now()
     ORDER BY last_searched_at ASC NULLS FIRST, population DESC
     LIMIT 1
       FOR UPDATE SKIP LOCKED
)
UPDATE zip_codes z
   SET claimed_by = $1, claimed_until = now() + make_interval(secs => $2)
  FROM next
 WHERE z.zip = next.zip
RETURNING z.zip, z.resume_page, z.claimed_until`
	var c Claim
	err := r.pool.QueryRow(ctx, q, owner, lease.Seconds()).Scan(&c.Zip, &c.ResumePage, &c.Token)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim zip: %w", err)
	}
	return &c, nil
}

// MarkSearched completes a claim after a search that ran to the end: the ZIP
// is stamped searched now, which sends it to the back of the rotation, with
// the number of listings the search returned. The failure count and the resume
// page start over.
func (r *Repository) MarkSearched(ctx context.Context, c Claim, owner string, listingCount int) error {
	const q = `
UPDATE zip_codes
   SET last_searched_at = now(), last_listing_count = $4,
       claimed_by = NULL, claimed_until = NULL, failures = 0, resume_page = 0` + held
	return r.finish(ctx, "mark zip searched", q, c, owner, listingCount)
}

// Defer gives up a claim whose search stopped early through no fault of the
// ZIP (the budget window ran out): it becomes claimable again at until and
// the next claim resumes at resumePage. Not a failure, so failures is left
// alone. The backoff is a kept lease without an owner: claimed_until hides
// the ZIP from Claim, and the NULL owner fails every guard, this one's too.
func (r *Repository) Defer(ctx context.Context, c Claim, owner string, until time.Time, resumePage int) error {
	const q = `
UPDATE zip_codes
   SET claimed_by = NULL, claimed_until = $4, resume_page = $5` + held
	return r.finish(ctx, "defer", q, c, owner, until, resumePage)
}

// Fail gives up a claim whose search failed. Up to MaxFailures-1 times in a
// row the ZIP keeps its place in the rotation, is hidden until until, and the
// next claim resumes at resumePage (the page that failed). The failure that
// reaches MaxFailures instead stamps the ZIP searched, which sends it to the
// back of the rotation with a clean slate, and reports pushedBack so the
// caller can say so loudly: that ZIP's listings are skipped for a whole pass.
//
// Counting and deciding are one statement, so the count cannot be read stale.
// The SET expressions see the row as it was; RETURNING sees it as it is now,
// where failures is zero exactly when it was just reset.
func (r *Repository) Fail(ctx context.Context, c Claim, owner string, until time.Time, resumePage int) (pushedBack bool, err error) {
	const q = `
UPDATE zip_codes
   SET failures         = CASE WHEN failures + 1 >= $6::int THEN 0 ELSE failures + 1 END,
       resume_page      = CASE WHEN failures + 1 >= $6::int THEN 0 ELSE $5::int END,
       last_searched_at = CASE WHEN failures + 1 >= $6::int THEN now() ELSE last_searched_at END,
       claimed_until    = CASE WHEN failures + 1 >= $6::int THEN NULL ELSE $4::timestamptz END,
       claimed_by       = NULL` + held + `
RETURNING failures = 0`
	err = r.pool.QueryRow(ctx, q, c.Zip, owner, c.Token, until, resumePage, MaxFailures).Scan(&pushedBack)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("fail zip=%s: %w", c.Zip, ErrLeaseLost)
	}
	if err != nil {
		return false, fmt.Errorf("fail zip=%s: %w", c.Zip, err)
	}
	return pushedBack, nil
}

// Release hands a claim back untouched, for a ZIP that was claimed but not
// searched (shutdown). It is claimable again at once and nothing else about it
// changes: no failure is counted and the resume page stays.
func (r *Repository) Release(ctx context.Context, c Claim, owner string) error {
	const q = `
UPDATE zip_codes
   SET claimed_by = NULL, claimed_until = NULL` + held
	return r.finish(ctx, "release", q, c, owner)
}

// finish runs a transition out of a claim. q ends in the held guard, whose
// parameters come first; args are q's own, from $4. No row matching the guard
// is ErrLeaseLost.
func (r *Repository) finish(ctx context.Context, op, q string, c Claim, owner string, args ...any) error {
	tag, err := r.pool.Exec(ctx, q, append([]any{c.Zip, owner, c.Token}, args...)...)
	if err != nil {
		return fmt.Errorf("%s zip=%s: %w", op, c.Zip, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s zip=%s: %w", op, c.Zip, ErrLeaseLost)
	}
	return nil
}
