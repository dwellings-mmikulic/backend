// Package workqueue is the PostgreSQL repository of listing_queue: the
// listings discovery has found and the media pipeline has yet to process.
// PostgreSQL is the coordinator of the fleet, so everything that must hold
// between instances (one holder per item, a bounded number of attempts, no
// stale worker overwriting a newer claim) is a property of the statements in
// this file rather than of any Go code around them.
package workqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxAttempts is how many claims an item gets before it is dead. Attempts are
// counted when the item is claimed, not when it fails, so an item that kills
// the process working on it still runs out of them.
//
// schema.sql repeats the number in the predicate of
// idx_listing_queue_claimable; an integration test compares the two.
const MaxAttempts = 3

const (
	// sqlDeadlockDetected is the SQLSTATE of the victim of a deadlock.
	sqlDeadlockDetected = "40P01"
	// sqlDataExceptionClass is the SQLSTATE class of "this value cannot be
	// stored": 22P02 and 22P05 for JSON that jsonb refuses, 22003 for a number
	// beyond numeric, 22021 for text that is not UTF-8.
	sqlDataExceptionClass = "22"
)

// ErrLeaseLost is returned by Complete, Fail and Release when the claim they
// were given is no longer the row's current claim: the lease ran out and the
// item was claimed again, or the item is gone. Nothing was changed.
var ErrLeaseLost = errors.New("workqueue: lease lost")

// ErrUnstorable is wrapped by the error Enqueue returns when it left items out
// because PostgreSQL cannot store them. Everything else in the batch was
// enqueued and the returned count is valid. Trying again will not help: the
// same items will be left out again.
var ErrUnstorable = errors.New("workqueue: item cannot be stored")

// NewItem is a listing to enqueue. Payload is a JSON document this package
// stores and hands back without looking inside.
type NewItem struct {
	ZPID      string
	Payload   []byte
	SourceZip string
}

// Item is a claimed queue row. Payload is the enqueued JSON document as jsonb
// renders it: the same value, not necessarily the same bytes.
type Item struct {
	ZPID    string
	Payload []byte
	// Attempts counts this claim too: 1 on the first one.
	Attempts int
	// Token is the claim's fencing token (its claimed_until). It must be
	// passed back exactly as it was returned.
	Token time.Time
}

// Stats is the number of queue rows in each state. Every row is in exactly one.
type Stats struct {
	Claimable int // could be claimed right now
	Claimed   int // held by a live claim
	Backoff   int // waiting for available_at
	Dead      int // out of attempts; kept for inspection until revived
}

// The statements that mention the attempts limit are rendered once, here, from
// MaxAttempts. The limit has to reach PostgreSQL as a literal: the planner
// uses the partial index idx_listing_queue_claimable (WHERE attempts < 3) only
// when it can prove the statement's predicate implies the index's, which it
// cannot do for a bind parameter in a generic plan.
var (
	// unclaimedSQL: no claim, or one whose lease has run out. Expired claims
	// are never cleared by anyone; they simply stop counting.
	unclaimedSQL = `(claimed_until IS NULL OR claimed_until <= now())`

	// claimableSQL is the one definition of "claimable" that Claim, Depth and
	// Stats share, so that they cannot disagree.
	claimableSQL = fmt.Sprintf(`attempts < %d AND available_at <= now() AND %s`, MaxAttempts, unclaimedSQL)

	// enqueueSQL. DISTINCT ON makes a zpid that appears twice in the batch one
	// row (ON CONFLICT refuses to touch a row twice in one statement), and its
	// ORDER BY makes every enqueuer take its row locks in the same global
	// order, so two overlapping batches queue behind each other instead of
	// deadlocking. The ordinality makes the first of the duplicates the winner
	// rather than an arbitrary one.
	//
	// The conflict rule: a row that is dead and not held by a live claim is
	// revived in place; any other existing row (claimable, backing off,
	// claimed, or on its last attempt right now) is left exactly as it is, so
	// re-discovery can neither reset a backoff nor pull an item out from under
	// its worker. Rows left alone are not counted in the command tag.
	enqueueSQL = fmt.Sprintf(`
INSERT INTO listing_queue (zpid, payload, source_zip)
SELECT DISTINCT ON (t.zpid) t.zpid, t.payload::jsonb, t.source_zip
  FROM unnest($1::text[], $2::text[], $3::text[]) WITH ORDINALITY AS t(zpid, payload, source_zip, ord)
 WHERE t.zpid <> ''
 ORDER BY t.zpid, t.ord
ON CONFLICT (zpid) DO UPDATE
   SET payload = EXCLUDED.payload,
       source_zip = EXCLUDED.source_zip,
       attempts = 0,
       available_at = now(),
       claimed_by = NULL,
       claimed_until = NULL,
       last_error = NULL,
       enqueued_at = now()
 WHERE listing_queue.attempts >= %d
   AND (listing_queue.claimed_until IS NULL OR listing_queue.claimed_until <= now())`, MaxAttempts)

	// claimSQL. FOR UPDATE SKIP LOCKED makes concurrent claimers pass over
	// each other's rows instead of queueing on them, and under READ COMMITTED
	// the lock step re-checks the predicate against the newest version of a
	// row that changed since the snapshot, so a row another transaction has
	// just claimed is dropped rather than claimed twice. The CTE is
	// MATERIALIZED so that the locking SELECT runs once, as written, and is
	// never folded into the UPDATE's join.
	claimSQL = `
WITH picked AS MATERIALIZED (
    SELECT zpid
      FROM listing_queue
     WHERE ` + claimableSQL + `
     ORDER BY available_at
     LIMIT $2
       FOR UPDATE SKIP LOCKED
)
UPDATE listing_queue q
   SET claimed_by = $1,
       claimed_until = now() + make_interval(secs => $3::double precision),
       attempts = q.attempts + 1
  FROM picked
 WHERE q.zpid = picked.zpid
RETURNING q.zpid, q.payload, q.attempts, q.claimed_until`

	depthSQL = `SELECT count(*) FROM listing_queue WHERE ` + claimableSQL

	// statsSQL: the four filters partition the table. A row on its last
	// attempt is Claimed while its lease is live and Dead once it is not.
	statsSQL = fmt.Sprintf(`
SELECT count(*) FILTER (WHERE %[1]s),
       count(*) FILTER (WHERE NOT %[2]s),
       count(*) FILTER (WHERE %[2]s AND attempts < %[3]d AND available_at > now()),
       count(*) FILTER (WHERE %[2]s AND attempts >= %[3]d)
  FROM listing_queue`, claimableSQL, unclaimedSQL, MaxAttempts)
)

// refusedPayloadsSQL asks PostgreSQL which payloads of a batch it will not
// take, by their position in the array. It is the only judge worth asking:
// jsonb is stricter than encoding/json (no \u0000, no lone surrogates, no
// number beyond numeric), and any check written in Go would be a second
// definition of JSON, free to drift from the one that counts.
//
// pg_input_is_valid needs PostgreSQL 16. On an older server the question
// itself fails, and Enqueue returns the statement's error as it would have
// without asking.
const refusedPayloadsSQL = `
SELECT t.ord
  FROM unnest($1::text[]) WITH ORDINALITY AS t(payload, ord)
 WHERE NOT pg_input_is_valid(t.payload, 'jsonb')`

// heldSQL is the guard of every post-work transition: the row must still
// carry the claim the caller was given. The owner alone is not enough, because
// an owner can lose an item and claim it again; the token tells the two claims
// apart, so bookkeeping left over from the first cannot land on the second.
const heldSQL = `zpid = $1 AND claimed_by = $2 AND claimed_until = $3`

const completeSQL = `DELETE FROM listing_queue WHERE ` + heldSQL

// failSQL keeps the first 500 characters of the message: enough to tell
// failures apart in the runbook queries, not a copy of ffmpeg's stderr.
const failSQL = `
UPDATE listing_queue
   SET claimed_by = NULL,
       claimed_until = NULL,
       available_at = now() + make_interval(secs => $4::double precision),
       last_error = left($5, 500),
       attempts = CASE WHEN $6 THEN GREATEST(attempts - 1, 0) ELSE attempts END
 WHERE ` + heldSQL

const releaseSQL = `
UPDATE listing_queue
   SET claimed_by = NULL,
       claimed_until = NULL,
       available_at = now() + make_interval(secs => $4::double precision),
       attempts = GREATEST(attempts - 1, 0)
 WHERE ` + heldSQL

// Repository is the PostgreSQL listing queue.
type Repository struct {
	pool *pgxpool.Pool
	// enqueueExec runs the enqueue statement: pool.Exec, except in the tests
	// that need it to report a deadlock, which PostgreSQL will not do on
	// request.
	enqueueExec func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// NewRepository creates a Repository.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool, enqueueExec: pool.Exec}
}

// Enqueue adds the items to the queue, all of them in one statement, and
// returns how many rows it inserted or revived. An item whose zpid is already queued is left
// alone unless that row is dead, in which case it starts over with the new
// payload (see enqueueSQL). Items without a zpid are dropped, and of several
// items with the same zpid the first one wins.
//
// An item PostgreSQL cannot store (a payload that is not JSON as jsonb
// understands it, a zpid that is not text) is left out and the rest are
// enqueued all the same. The error then wraps ErrUnstorable and names the
// listings left out, and the count is valid alongside it. One listing must
// not cost a ZIP all of its listings: the failure is deterministic, so the
// caller's retry would fail as well, and the ZIP would be searched, and paid
// for, again and again only to fail at the same row.
func (r *Repository) Enqueue(ctx context.Context, items []NewItem) (int, error) {
	var (
		batch    []NewItem
		leftOut  []string
		probeErr error
	)
	for _, it := range items {
		switch {
		case it.ZPID == "":
			// The statement drops these too. Doing it here as well means a
			// batch of nothing else costs no round trip.
		case !storableText(it.ZPID), !utf8.Valid(it.Payload), !json.Valid(it.Payload):
			// What Go can tell. Bytes that are not UTF-8 have to be caught
			// here: PostgreSQL refuses them as a parameter, before any
			// statement could be asked which row they belong to.
			leftOut = append(leftOut, it.ZPID)
		default:
			batch = append(batch, it)
		}
	}

	n, err := r.insert(ctx, batch)
	if isDataException(err) {
		// Something in the batch is JSON to Go and not to PostgreSQL. The
		// error does not say which row, and the statement stored nothing.
		var refused []string
		if batch, refused, probeErr = r.withoutRefusedPayloads(ctx, batch); len(refused) > 0 {
			leftOut = append(leftOut, refused...)
			n, err = r.insert(ctx, batch)
		}
	}
	switch {
	case err != nil && probeErr != nil:
		return 0, fmt.Errorf("enqueue %d listings: %w (and asking which payload was refused: %v)", len(batch), err, probeErr)
	case err != nil:
		return 0, fmt.Errorf("enqueue %d listings: %w", len(batch), err)
	case len(leftOut) > 0:
		return n, fmt.Errorf("enqueue: %w: %s", ErrUnstorable, nameListings(leftOut))
	}
	return n, nil
}

// insert runs the enqueue statement for items that are believed storable.
func (r *Repository) insert(ctx context.Context, items []NewItem) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	zpids := make([]string, len(items))
	payloads := make([]string, len(items))
	sourceZips := make([]string, len(items))
	for i, it := range items {
		// The source ZIP is attribution only: cleaned, never a reason to lose
		// the listing.
		zpids[i], payloads[i], sourceZips[i] = it.ZPID, string(it.Payload), cleanText(it.SourceZip)
	}

	tag, err := r.enqueueExec(ctx, enqueueSQL, zpids, payloads, sourceZips)
	if isDeadlock(err) {
		// The sorted insert rules out a deadlock between two enqueuers, not
		// one against a statement this package does not own (a runbook UPDATE
		// over many rows, say). The victim's transaction is rolled back whole,
		// so running the statement again is safe; once, because a second
		// deadlock is not bad luck any more.
		tag, err = r.enqueueExec(ctx, enqueueSQL, zpids, payloads, sourceZips)
	}
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// withoutRefusedPayloads splits off the items whose payload PostgreSQL will
// not store as jsonb and returns the rest, in order, with the zpids of the
// ones it took out. On an error the items come back as they were.
func (r *Repository) withoutRefusedPayloads(ctx context.Context, items []NewItem) (kept []NewItem, refused []string, err error) {
	payloads := make([]string, len(items))
	for i, it := range items {
		payloads[i] = string(it.Payload)
	}
	rows, err := r.pool.Query(ctx, refusedPayloadsSQL, payloads)
	if err != nil {
		return items, nil, err
	}
	ords, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return items, nil, err
	}
	if len(ords) == 0 {
		return items, nil, nil
	}
	isRefused := make(map[int64]bool, len(ords))
	for _, ord := range ords {
		isRefused[ord] = true
	}
	for i, it := range items {
		if isRefused[int64(i+1)] { // WITH ORDINALITY counts from 1
			refused = append(refused, it.ZPID)
		} else {
			kept = append(kept, it)
		}
	}
	return kept, refused, nil
}

func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlDeadlockDetected
}

func isDataException(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, sqlDataExceptionClass)
}

// storableText reports whether PostgreSQL takes s as a text value.
func storableText(s string) bool {
	return utf8.ValidString(s) && !strings.Contains(s, "\x00")
}

// nameListings renders zpids for an error message: quoted, because the reason
// one is named may be the bytes in it, and capped, because a caller that gets
// every payload wrong would otherwise log its whole batch.
func nameListings(zpids []string) string {
	const limit = 10
	zpids = slices.Compact(slices.Sorted(slices.Values(zpids)))
	quoted := make([]string, 0, limit)
	for _, z := range zpids[:min(len(zpids), limit)] {
		quoted = append(quoted, strconv.Quote(z))
	}
	out := "zpid " + strings.Join(quoted, ", ")
	if more := len(zpids) - limit; more > 0 {
		out += fmt.Sprintf(" and %d more", more)
	}
	return out
}

// Claim takes up to limit claimable items for owner, oldest available_at
// first, and holds them until the database clock passes now() + lease. Each
// claim costs the item one attempt there and then; Release, or Fail with
// refundAttempt, gives it back.
//
// Callers must do the work under a deadline shorter than the lease: nothing
// renews a claim, and once it has run out any other Claim may take the item.
func (r *Repository) Claim(ctx context.Context, owner string, limit int, lease time.Duration) ([]Item, error) {
	if limit <= 0 {
		return nil, nil
	}
	if lease < time.Microsecond {
		// claimed_until has microsecond resolution, so a shorter lease rounds
		// to none at all: claimed_until = now(). That would spend an attempt
		// on a claim that is over before it is returned.
		return nil, fmt.Errorf("claim listings: lease must be at least 1µs, got %s", lease)
	}
	rows, err := r.pool.Query(ctx, claimSQL, owner, limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim listings: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ZPID, &it.Payload, &it.Attempts, &it.Token); err != nil {
			return nil, fmt.Errorf("scan claimed listing: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim listings: %w", err)
	}
	return out, nil
}

// Complete removes a finished item; the properties row is the record of the
// work. ErrLeaseLost means the claim was no longer the caller's and the row,
// if there still is one, was not touched.
//
// The guard is the owner and the token, not the clock: work that ends after
// its lease ran out is still recorded as long as nobody else has claimed the
// item in between.
func (r *Repository) Complete(ctx context.Context, it Item, owner string) error {
	tag, err := r.pool.Exec(ctx, completeSQL, it.ZPID, owner, it.Token)
	if err != nil {
		return fmt.Errorf("complete zpid=%s: %w", it.ZPID, err)
	}
	return held(tag, "complete", it.ZPID)
}

// Fail gives the item up after an error: the claim is cleared, the item stays
// hidden for retryAfter and the message is kept for inspection. The attempt
// stays spent unless refundAttempt is set, which is for failures that say
// nothing about the item (this worker's breaker is open), so that a broken box
// cannot dead-letter healthy listings.
func (r *Repository) Fail(ctx context.Context, it Item, owner, errMsg string, retryAfter time.Duration, refundAttempt bool) error {
	tag, err := r.pool.Exec(ctx, failSQL, it.ZPID, owner, it.Token, seconds(retryAfter), cleanText(errMsg), refundAttempt)
	if err != nil {
		return fmt.Errorf("fail zpid=%s: %w", it.ZPID, err)
	}
	return held(tag, "fail", it.ZPID)
}

// Release hands back an item that was not worked on to the end (shutdown, or a
// worker that cannot do this kind of item): the claim is cleared, the attempt
// refunded, and the item is claimable again after delay. last_error keeps
// whatever an earlier failure left there.
func (r *Repository) Release(ctx context.Context, it Item, owner string, delay time.Duration) error {
	tag, err := r.pool.Exec(ctx, releaseSQL, it.ZPID, owner, it.Token, seconds(delay))
	if err != nil {
		return fmt.Errorf("release zpid=%s: %w", it.ZPID, err)
	}
	return held(tag, "release", it.ZPID)
}

// held turns "no row matched the guard" into ErrLeaseLost.
func held(tag pgconn.CommandTag, op, zpid string) error {
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s zpid=%s: %w", op, zpid, ErrLeaseLost)
	}
	return nil
}

// seconds renders a delay for make_interval. A negative delay would backdate
// available_at and let the item jump the queue, which orders by it.
func seconds(d time.Duration) float64 {
	return max(d, 0).Seconds()
}

// cleanText makes an error message storable. PostgreSQL rejects a text value
// holding a NUL or invalid UTF-8, and messages here quote subprocess output
// and response bodies; a rejected Fail would leave the item claimed until its
// lease ran out, with the attempt spent and the error lost.
func cleanText(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "�")
}

// Depth is the number of items that could be claimed right now: what the
// discovery loop's backpressure compares with QUEUE_HIGH_WATER.
func (r *Repository) Depth(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx, depthSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("queue depth: %w", err)
	}
	return n, nil
}

// Stats counts the rows in each state with one scan of the whole table. It is
// for operators (backfill-videos -status); loops should use Depth, which the
// partial index answers.
func (r *Repository) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	if err := r.pool.QueryRow(ctx, statsSQL).Scan(&s.Claimable, &s.Claimed, &s.Backoff, &s.Dead); err != nil {
		return Stats{}, fmt.Errorf("queue stats: %w", err)
	}
	return s, nil
}
