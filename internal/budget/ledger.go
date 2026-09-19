package budget

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The kinds of paid request. Each has its own row, and so its own limit, in
// every window.
const (
	KindSearch  = "search"
	KindDetails = "details"
)

// Ledger counts the provider requests the whole fleet has spent in each
// budget window. The count lives in PostgreSQL because that is the one thing
// every instance shares: an in-process counter gives each of ten boxes the
// full budget and forgets it on restart.
//
// There are no refunds. A reservation is taken immediately before the HTTP
// attempt it pays for, so a crash leaks at most one request, retries are
// charged exactly, and nothing is carried across a window boundary.
type Ledger struct {
	pool *pgxpool.Pool
}

// NewLedger creates a Ledger.
func NewLedger(pool *pgxpool.Pool) *Ledger { return &Ledger{pool: pool} }

// TryReserve spends one request of kind in the window starting at window if
// fewer than limit are spent, and reports whether it did. Any error is a
// denial: the caller is about to spend money, so an unreachable ledger fails
// closed.
//
// limit is not stored; every call brings its own. Lowering it below what is
// already spent simply denies, and the effective fleet limit is the largest
// one any instance passes, which is why the budget variables have to match on
// every box.
func (l *Ledger) TryReserve(ctx context.Context, window time.Time, kind string, limit int) (bool, error) {
	if limit <= 0 {
		// A zero budget means "never call the provider"; it should not cost a
		// round trip per non-call, nor leave rows behind.
		return false, nil
	}
	// spent is an INTEGER, so $3 is one too, and pgx refuses to encode
	// anything wider: an "unlimited" budget spelled 9999999999 would deny
	// every request with an encoding error. The column cannot count past this
	// either, so the clamp changes nothing else.
	limit = min(limit, math.MaxInt32)

	// One statement, so there is no read-then-write gap for another instance
	// to slip through: the insert settles who opens the window, and on
	// conflict the row is locked and the WHERE re-checked against its newest
	// version before the increment. No row back means the WHERE said no.
	const reserve = `
INSERT INTO api_budget (window_start, kind, spent) VALUES ($1, $2, 1)
ON CONFLICT (window_start, kind) DO UPDATE SET spent = api_budget.spent + 1
 WHERE api_budget.spent < $3
RETURNING spent`
	window = window.UTC()
	var spent int
	err := l.pool.QueryRow(ctx, reserve, window, kind, limit).Scan(&spent)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reserve %s request in window %s: %w", kind, window.Format(time.RFC3339), err)
	}

	if spent == 1 {
		// First reservation of a new window: the one moment per window that is
		// a natural place to sweep, since nothing else ever deletes from this
		// table. Best effort: the request is already paid for in the ledger,
		// and whoever opens the next window sweeps again.
		//
		// The 90 days count back from the window or from now, whichever is
		// older, never from now alone. A window can run for longer than that
		// ("@yearly"): measured from the clock, the sweep would delete the row
		// inserted a moment ago, every reservation would be the first one
		// again and the limit would never bind; likewise the first details
		// request of such a window would wipe its spent search row, because
		// the sweep covers every kind. Measured from the window it can reach
		// neither that window nor anything newer. now() still bounds it for a
		// box whose clock, and so whose window, is ahead of the database.
		const prune = `
DELETE FROM api_budget
 WHERE window_start < LEAST(now(), $1::timestamptz) - interval '90 days'`
		_, _ = l.pool.Exec(ctx, prune, window)
	}
	return true, nil
}

// Spent returns how many requests of kind have been reserved in the window; 0
// when nobody has reserved one yet.
func (l *Ledger) Spent(ctx context.Context, window time.Time, kind string) (int, error) {
	const q = `SELECT spent FROM api_budget WHERE window_start = $1 AND kind = $2`
	window = window.UTC()
	var spent int
	err := l.pool.QueryRow(ctx, q, window, kind).Scan(&spent)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("spent %s requests in window %s: %w", kind, window.Format(time.RFC3339), err)
	}
	return spent, nil
}
