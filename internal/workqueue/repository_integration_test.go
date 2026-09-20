package workqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/db"
)

// The tests below run the queue's SQL against a real PostgreSQL, which is the
// only place claims, fencing tokens, conflict rules and lock ordering can be
// checked. They are skipped unless TEST_DATABASE_URL points at a database whose
// role may create schemas in it:
//
//	TEST_DATABASE_URL=postgres://dwellings:dwellings@localhost:5432/dwellings?sslmode=disable \
//	    go test -race -count=1 ./internal/workqueue/
//
// Claim, Depth and Stats are global by nature: they see every row of
// listing_queue, so no zpid suffix can namespace them. Each harness therefore
// builds the whole schema again inside a PostgreSQL schema of its own and
// drops it afterwards. The database's real listing_queue is never read,
// claimed from or reordered, counts can be asserted exactly, and two runs
// against one database cannot see each other. A run that is killed leaves its
// it_wq_* schema behind; nothing else ever looks at it.

// harness is one test's private schema, connection pool, repository and zpid
// namespace.
type harness struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	repo   *Repository
	suffix string
}

var (
	// runID tells this process's schemas from those of another run against
	// the same database. It is random because nothing else is unique there:
	// two runs started together read the same clock and are at the same
	// count, and process ids repeat from one CI container to the next.
	runID     = strconv.FormatUint(rand.Uint64(), 36)
	suffixSeq atomic.Int64
)

// newSuffix is unique per call and per process. Lower case letters, digits
// and underscores only, so that a schema named after it needs no quoting.
func newSuffix() string {
	return runID + "_" + strconv.FormatInt(suffixSeq.Add(1), 36)
}

func newHarness(t *testing.T, maxConns int) *harness {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the workqueue integration tests")
	}
	ctx := context.Background()

	schema := "it_wq_" + newSuffix()
	if err := execOnce(ctx, dsn, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create the test's schema: %v", err)
	}
	// Registered first so it runs last: t.Cleanup is LIFO, and the pool has to
	// be closed before its tables can be dropped.
	t.Cleanup(func() {
		if err := execOnce(context.Background(), dsn, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("cleanup: drop schema %s: %v", schema, err)
		}
	})

	pool, err := db.Connect(ctx, withSearchPath(dsn, schema), maxConns)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	// Were the search_path ever dropped on the way to the server, everything
	// below would quietly run against the real tables instead.
	var current string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&current); err != nil || current != schema {
		t.Fatalf("current_schema() = %q, %v; want the test's own %q", current, err, schema)
	}
	// schema.sql names no schema, so this builds every table inside ours.
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := &harness{t: t, ctx: ctx, pool: pool, repo: NewRepository(pool)}
	h.renew()
	return h
}

// execOnce runs one statement on a connection of its own, outside any
// harness's search_path.
func execOnce(ctx context.Context, dsn, sql string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, sql)
	return err
}

// withSearchPath makes every connection opened from dsn resolve unqualified
// names in schema. pgx sends a connection parameter it does not know as a
// run-time parameter in the startup message, which is all search_path needs.
func withSearchPath(dsn, schema string) string {
	if !strings.Contains(dsn, "://") { // keyword/value form
		return dsn + " search_path=" + schema
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn // db.Connect reports it better than this could
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// renew switches to a fresh zpid namespace, for tests that need the same
// names to be new rows again.
func (h *harness) renew() { h.suffix = newSuffix() }

func (h *harness) zpid(name string) string { return "it-wq-" + name + "-" + h.suffix }

func (h *harness) owner(name string) string { return name + "/" + h.suffix }

// payload is a JSON document carrying v; jsonb does not keep the bytes, so
// tests read it back with payloadV rather than comparing bytes.
func payload(v string) []byte {
	b, err := json.Marshal(map[string]string{"v": v})
	if err != nil {
		panic(err)
	}
	return b
}

func payloadV(t *testing.T, raw []byte) string {
	t.Helper()
	var doc struct {
		V string `json:"v"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("payload %q is not the JSON that was enqueued: %v", raw, err)
	}
	return doc.V
}

// enqueue enqueues one item per name, all with the payload value v.
func (h *harness) enqueue(v string, names ...string) int {
	h.t.Helper()
	items := make([]NewItem, len(names))
	for i, name := range names {
		items[i] = NewItem{ZPID: h.zpid(name), Payload: payload(v), SourceZip: "78701"}
	}
	n, err := h.repo.Enqueue(h.ctx, items)
	if err != nil {
		h.t.Fatalf("enqueue %d items (%s, ...): %v", len(names), names[0], err)
	}
	return n
}

func (h *harness) claim(owner string, limit int, lease time.Duration) []Item {
	h.t.Helper()
	got, err := h.repo.Claim(h.ctx, owner, limit, lease)
	if err != nil {
		h.t.Fatalf("claim: %v", err)
	}
	return got
}

// claimOne claims exactly one item and checks it is the one the test expects
// to be next.
func (h *harness) claimOne(owner, name string, lease time.Duration) Item {
	h.t.Helper()
	got := h.claim(owner, 1, lease)
	if len(got) != 1 || got[0].ZPID != h.zpid(name) {
		h.t.Fatalf("claim = %v, want exactly %s", zpids(got), h.zpid(name))
	}
	return got[0]
}

func (h *harness) mustClaimNothing(owner, why string) {
	h.t.Helper()
	if got := h.claim(owner, 10, time.Minute); len(got) != 0 {
		h.t.Fatalf("claimed %v, want nothing: %s", zpids(got), why)
	}
}

// burn uses up n attempts of the item: n claims, each failed without a refund
// and with no backoff.
func (h *harness) burn(owner, name string, n int) {
	h.t.Helper()
	for i := 1; i <= n; i++ {
		it := h.claimOne(owner, name, time.Minute)
		if it.Attempts != i {
			h.t.Fatalf("claim %d of %s reports attempts = %d", i, name, it.Attempts)
		}
		if err := h.repo.Fail(h.ctx, it, owner, fmt.Sprintf("burn %d", i), 0, false); err != nil {
			h.t.Fatalf("fail %s: %v", name, err)
		}
	}
}

// backdate makes the item available since secs seconds ago. Set directly
// rather than built from short Release delays: a round trip can take longer
// than any delay that would keep a test fast.
func (h *harness) backdate(name string, secs float64) {
	h.t.Helper()
	const q = `UPDATE listing_queue SET available_at = now() - make_interval(secs => $2::double precision) WHERE zpid = $1`
	if tag, err := h.pool.Exec(h.ctx, q, h.zpid(name), secs); err != nil || tag.RowsAffected() != 1 {
		h.t.Fatalf("backdate %s: %v, %v", name, tag, err)
	}
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitExpired blocks until the database clock has passed the item's lease.
func (h *harness) waitExpired(name string) {
	h.t.Helper()
	h.eventually("the lease on "+name+" to expire", func() bool {
		var expired bool
		if err := h.pool.QueryRow(h.ctx, `SELECT claimed_until <= now() FROM listing_queue WHERE zpid = $1`, h.zpid(name)).Scan(&expired); err != nil {
			h.t.Fatalf("read lease of %s: %v", name, err)
		}
		return expired
	})
}

const null = "<null>"

// rowState is a whole queue row in comparable form, so "changes nothing" can
// be asserted with ==. Times are microseconds since the epoch, 0 for NULL.
type rowState struct {
	Payload      string
	SourceZip    string
	Attempts     int
	AvailableAt  int64
	ClaimedBy    string
	ClaimedUntil int64
	LastError    string
	EnqueuedAt   int64
}

func (h *harness) row(name string) (rowState, bool) {
	h.t.Helper()
	const q = `
SELECT payload::text, source_zip, attempts, available_at, COALESCE(claimed_by, '` + null + `'),
       claimed_until, COALESCE(last_error, '` + null + `'), enqueued_at
  FROM listing_queue WHERE zpid = $1`
	var (
		s            rowState
		availableAt  time.Time
		claimedUntil *time.Time
		enqueuedAt   time.Time
	)
	err := h.pool.QueryRow(h.ctx, q, h.zpid(name)).Scan(&s.Payload, &s.SourceZip, &s.Attempts, &availableAt,
		&s.ClaimedBy, &claimedUntil, &s.LastError, &enqueuedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return rowState{}, false
	}
	if err != nil {
		h.t.Fatalf("read row %s: %v", name, err)
	}
	s.AvailableAt, s.EnqueuedAt = availableAt.UnixMicro(), enqueuedAt.UnixMicro()
	if claimedUntil != nil {
		s.ClaimedUntil = claimedUntil.UnixMicro()
	}
	return s, true
}

func (h *harness) mustRow(name string) rowState {
	h.t.Helper()
	s, ok := h.row(name)
	if !ok {
		h.t.Fatalf("row %s does not exist", name)
	}
	return s
}

// secondsFromNow evaluates a timestamptz column against the database clock.
func (h *harness) secondsFromNow(column, name string) float64 {
	h.t.Helper()
	var secs float64
	q := `SELECT extract(epoch FROM ` + column + ` - now())::float8 FROM listing_queue WHERE zpid = $1`
	if err := h.pool.QueryRow(h.ctx, q, h.zpid(name)).Scan(&secs); err != nil {
		h.t.Fatalf("read %s of %s: %v", column, name, err)
	}
	return secs
}

func zpids(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ZPID
	}
	return out
}

// mustLoseLease runs every guarded transition with a stale or foreign claim
// and checks each is refused and leaves the row exactly as it was.
func (h *harness) mustLoseLease(name string, it Item, owner string) {
	h.t.Helper()
	before := h.mustRow(name)
	for op, call := range map[string]func() error{
		"Complete": func() error { return h.repo.Complete(h.ctx, it, owner) },
		"Fail":     func() error { return h.repo.Fail(h.ctx, it, owner, "stale", time.Hour, true) },
		"Release":  func() error { return h.repo.Release(h.ctx, it, owner, time.Hour) },
	} {
		if err := call(); !errors.Is(err, ErrLeaseLost) {
			h.t.Errorf("%s with a lost lease = %v, want ErrLeaseLost", op, err)
		}
		after, ok := h.row(name)
		if !ok {
			h.t.Fatalf("%s with a lost lease deleted the row", op)
		}
		if after != before {
			h.t.Errorf("%s with a lost lease changed the row:\n was %+v\n now %+v", op, before, after)
		}
	}
}

// --- schema ----------------------------------------------------------------

// The partial index only helps while the planner can prove the statements'
// "attempts < N" implies the index's, which needs the same literal on both
// sides.
func TestIndexPredicateMatchesMaxAttempts(t *testing.T) {
	h := newHarness(t, 0)
	var def string
	if err := h.pool.QueryRow(h.ctx, `SELECT pg_get_indexdef('idx_listing_queue_claimable'::regclass)`).Scan(&def); err != nil {
		t.Fatalf("read index definition: %v", err)
	}
	want := fmt.Sprintf("(attempts < %d)", MaxAttempts)
	if !strings.Contains(def, want) {
		t.Errorf("idx_listing_queue_claimable is %q, want its predicate to be %s: schema.sql and MaxAttempts disagree", def, want)
	}
}

// The loops run these statements all day, and pgx prepares what it runs:
// after five executions PostgreSQL may switch a prepared statement to its
// generic plan, the one built without looking at the parameter values. That is
// the plan in which "attempts < $n" cannot be proved to imply the index's
// "attempts < 3", so the limit has to be a literal. EXPLAIN with arguments
// would not notice: it folds the arguments into a custom plan. GENERIC_PLAN
// (PostgreSQL 16) plans the statement the way the prepared one will be.
func TestClaimableStatementsCanUseThePartialIndex(t *testing.T) {
	h := newHarness(t, 0)
	var version int
	if err := h.pool.QueryRow(h.ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		t.Fatalf("read server version: %v", err)
	}
	if version < 160000 {
		t.Skipf("EXPLAIN (GENERIC_PLAN) needs PostgreSQL 16, this is %d", version)
	}
	for name, sql := range map[string]string{"claim": claimSQL, "depth": depthSQL} {
		plan, err := h.genericPlanWithoutSeqScan(sql)
		if err != nil {
			t.Errorf("explain %s: %v", name, err)
			continue
		}
		if !strings.Contains(plan, "idx_listing_queue_claimable") {
			t.Errorf("the generic plan of %s does not use idx_listing_queue_claimable:\n%s", name, plan)
		}
	}

	// The test has to be able to fail: the same predicate with the limit as a
	// parameter loses the index.
	bound := strings.Replace(depthSQL, fmt.Sprintf("attempts < %d", MaxAttempts), "attempts < $1", 1)
	if bound == depthSQL {
		t.Fatalf("depthSQL no longer spells the limit as %q; update this test", fmt.Sprintf("attempts < %d", MaxAttempts))
	}
	plan, err := h.genericPlanWithoutSeqScan(bound)
	if err != nil {
		t.Fatalf("explain depth with a bound limit: %v", err)
	}
	if strings.Contains(plan, "idx_listing_queue_claimable") {
		t.Errorf("a bound attempts limit still plans on the partial index, so this test proves nothing:\n%s", plan)
	}
}

// genericPlanWithoutSeqScan plans the statement (without running it) with
// sequential scans priced out, so that the partial index is chosen whenever it
// is usable at all. The transaction is always rolled back: a connection still
// held when the test ends would make pool.Close wait forever.
func (h *harness) genericPlanWithoutSeqScan(sql string) (string, error) {
	tx, err := h.pool.Begin(h.ctx)
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(h.ctx) }()
	if _, err := tx.Exec(h.ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		return "", fmt.Errorf("disable seqscan: %w", err)
	}
	// pgx refuses a statement that has $n placeholders and no arguments, which
	// is exactly what GENERIC_PLAN is for, so this goes over the simple
	// protocol underneath it.
	results, err := tx.Conn().PgConn().Exec(h.ctx, `EXPLAIN (GENERIC_PLAN) `+sql).ReadAll()
	if err != nil {
		return "", err
	}
	var plan strings.Builder
	for _, res := range results {
		for _, row := range res.Rows {
			plan.Write(row[0])
			plan.WriteByte('\n')
		}
	}
	return plan.String(), nil
}

// --- Enqueue ---------------------------------------------------------------

func TestEnqueue_InsertsDedupesAndDropsEmptyZPIDs(t *testing.T) {
	h := newHarness(t, 0)
	items := []NewItem{
		{ZPID: h.zpid("a"), Payload: payload("a"), SourceZip: "78701"},
		{ZPID: "", Payload: payload("no zpid"), SourceZip: "78701"},
		{ZPID: h.zpid("b"), Payload: payload("b"), SourceZip: "78702"},
		{ZPID: h.zpid("a"), Payload: payload("a again"), SourceZip: "78799"},
		{ZPID: "", Payload: payload("no zpid either")},
	}
	n, err := h.repo.Enqueue(h.ctx, items)
	if err != nil {
		t.Fatalf("enqueue with a duplicate zpid in the batch: %v", err)
	}
	if n != 2 {
		t.Errorf("enqueued = %d, want 2 (one per distinct non-empty zpid)", n)
	}
	var total, empty int
	if err := h.pool.QueryRow(h.ctx, `
SELECT count(*), count(*) FILTER (WHERE zpid = '') FROM listing_queue`).Scan(&total, &empty); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if total != 2 || empty != 0 {
		t.Errorf("rows = %d, %d of them with an empty zpid; want 2 and 0", total, empty)
	}

	a := h.mustRow("a")
	if a.Attempts != 0 || a.ClaimedBy != null || a.ClaimedUntil != 0 || a.LastError != null {
		t.Errorf("new row = %+v, want attempts 0, unclaimed, no error", a)
	}
	if a.SourceZip != "78701" || h.mustRow("b").SourceZip != "78702" {
		t.Errorf("source_zip = %q / %q, want 78701 / 78702", a.SourceZip, h.mustRow("b").SourceZip)
	}
	if secs := h.secondsFromNow("available_at", "a"); secs > 0 || secs < -60 {
		t.Errorf("available_at is %.3f s from now, want just before now", secs)
	}

	// The payload comes back as the JSON that went in, and of the two items
	// for "a" it is the first one's.
	got := h.claim(h.owner("w"), 10, time.Minute)
	if len(got) != 2 {
		t.Fatalf("claimed %v, want both rows", zpids(got))
	}
	for _, it := range got {
		want := strings.TrimSuffix(strings.TrimPrefix(it.ZPID, "it-wq-"), "-"+h.suffix)
		if v := payloadV(t, it.Payload); v != want {
			t.Errorf("payload of %s carries %q, want %q", it.ZPID, v, want)
		}
	}
}

// A batch of nothing (or of nothing but empty zpids) must not cost a round trip.
func TestEnqueue_NothingToEnqueueIsANoOp(t *testing.T) {
	r := &Repository{enqueueExec: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
		t.Error("Enqueue reached the database")
		return pgconn.CommandTag{}, nil
	}}
	for name, items := range map[string][]NewItem{
		"nil":         nil,
		"empty":       {},
		"empty zpids": {{ZPID: "", Payload: payload("x")}, {ZPID: ""}},
	} {
		if n, err := r.Enqueue(context.Background(), items); n != 0 || err != nil {
			t.Errorf("Enqueue(%s) = %d, %v; want 0, nil", name, n, err)
		}
	}
}

// A payload Go can already tell is unstorable never reaches PostgreSQL, where
// it would fail the whole batch with an error that does not say which listing
// it was. The rest of the batch does.
func TestEnqueue_LeavesOutWhatGoCanTellIsUnstorable(t *testing.T) {
	for name, bad := range map[string]NewItem{
		"nil payload":            {ZPID: "2468", Payload: nil},
		"truncated payload":      {ZPID: "2468", Payload: []byte(`{"v":`)},
		"payload not UTF-8":      {ZPID: "2468", Payload: []byte("{\"v\":\"\xff\"}")},
		"zpid with a NUL":        {ZPID: "24\x0068", Payload: payload("fine")},
		"zpid that is not UTF-8": {ZPID: "24\xff68", Payload: payload("fine")},
	} {
		var calls [][]any
		r := &Repository{enqueueExec: func(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
			calls = append(calls, args)
			return pgconn.NewCommandTag("INSERT 0 2"), nil
		}}
		n, err := r.Enqueue(context.Background(), []NewItem{
			{ZPID: "1", Payload: payload("fine"), SourceZip: "78701"},
			bad,
			{ZPID: "3", Payload: payload("fine"), SourceZip: "78\x00702\xff"},
		})
		if n != 2 || !errors.Is(err, ErrUnstorable) {
			t.Errorf("%s: Enqueue = %d, %v; want the other 2 enqueued and ErrUnstorable", name, n, err)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, "24") || !strings.Contains(msg, "68") || strings.ContainsAny(msg, "\x00\xff") {
			t.Errorf("%s: error %q does not name the listing in printable form", name, msg)
		}
		// A source_zip is only attribution: cleaned, not a reason to lose the listing.
		want := fmt.Sprint([]any{[]string{"1", "3"}, []string{string(payload("fine")), string(payload("fine"))}, []string{"78701", "78702\uFFFD"}})
		if len(calls) != 1 || fmt.Sprint(calls[0]) != want {
			t.Errorf("%s: statement ran with %v, want once with %v", name, calls, want)
		}
	}

	// Nothing left to enqueue: no round trip, and still an error.
	r := &Repository{enqueueExec: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
		t.Error("Enqueue reached the database with nothing storable")
		return pgconn.CommandTag{}, nil
	}}
	if n, err := r.Enqueue(context.Background(), []NewItem{{ZPID: "2468"}, {ZPID: ""}}); n != 0 || !errors.Is(err, ErrUnstorable) {
		t.Errorf("Enqueue of one bad item = %d, %v; want 0, ErrUnstorable", n, err)
	}
}

// encoding/json and jsonb do not agree on what JSON is. Every payload here
// passes json.Valid and is refused by PostgreSQL, which fails the statement,
// and with it every listing of the ZIP, without saying which row it was. The
// first one is the realistic one: it is what json.Marshal makes of a provider
// string that holds a NUL.
func TestEnqueue_LeavesOutWhatPostgresRefusesAndNamesIt(t *testing.T) {
	h := newHarness(t, 0)
	nul, err := json.Marshal(struct {
		Address string `json:"address"`
	}{"12 Main St\x00Apt 4"})
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]byte{
		"NUL escape (22P05)":            nul,
		"lone surrogate escape (22P02)": []byte(`{"v":"\ud800"}`),
		"number beyond numeric (22003)": []byte(`{"v":1e1000000}`),
	} {
		h.renew()
		if !json.Valid(bad) {
			t.Fatalf("%s: test premise: json.Valid refuses %q", name, bad)
		}
		n, err := h.repo.Enqueue(h.ctx, []NewItem{
			{ZPID: h.zpid("good-1"), Payload: payload("fine"), SourceZip: "78701"},
			{ZPID: h.zpid("poison"), Payload: bad, SourceZip: "78701"},
			{ZPID: h.zpid("good-2"), Payload: payload("fine"), SourceZip: "78701"},
			// Of two items for one listing the first wins, unless it is the one
			// that cannot be stored.
			{ZPID: h.zpid("twice"), Payload: bad, SourceZip: "78701"},
			{ZPID: h.zpid("twice"), Payload: payload("second"), SourceZip: "78701"},
		})
		if n != 3 || !errors.Is(err, ErrUnstorable) {
			t.Errorf("%s: Enqueue = %d, %v; want 3 enqueued and ErrUnstorable", name, n, err)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, h.zpid("poison")) || strings.Contains(msg, h.zpid("good-1")) || strings.Contains(msg, h.zpid("good-2")) {
			t.Errorf("%s: error %q, want it to name %s and neither good listing", name, msg, h.zpid("poison"))
		}
		if _, ok := h.row("poison"); ok {
			t.Errorf("%s: the refused listing has a row", name)
		}
		for _, good := range []string{"good-1", "good-2"} {
			if row, ok := h.row(good); !ok || !strings.Contains(row.Payload, `"fine"`) {
				t.Errorf("%s: row %s = %+v, %v; want it enqueued", name, good, row, ok)
			}
		}
		if row, ok := h.row("twice"); !ok || !strings.Contains(row.Payload, `"second"`) {
			t.Errorf("%s: row twice = %+v, %v; want the storable one of the two", name, row, ok)
		}

		// What the caller's retry sees: nothing new, the same listing named.
		if n, err := h.repo.Enqueue(h.ctx, []NewItem{
			{ZPID: h.zpid("good-1"), Payload: payload("fine"), SourceZip: "78701"},
			{ZPID: h.zpid("poison"), Payload: bad, SourceZip: "78701"},
		}); n != 0 || !errors.Is(err, ErrUnstorable) || !strings.Contains(err.Error(), h.zpid("poison")) {
			t.Errorf("%s: second Enqueue = %d, %v; want 0 and the same listing named", name, n, err)
		}
	}
}

// Asking PostgreSQL which payload it refused is only ever a way to a better
// outcome. When it names none, or cannot be asked, the statement's own error
// is what the caller gets, and the statement is not run again.
func TestEnqueue_DataExceptionWithNoPayloadAtFaultIsReturnedAsIs(t *testing.T) {
	h := newHarness(t, 0)
	refused := &pgconn.PgError{Code: "22P02", Message: "invalid input syntax for type json"}
	for name, cancelFirst := range map[string]bool{"no payload at fault": false, "cannot ask": true} {
		ctx, cancel := context.WithCancel(h.ctx)
		calls := 0
		h.repo.enqueueExec = func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			calls++
			if cancelFirst {
				cancel()
			}
			return pgconn.CommandTag{}, refused
		}
		n, err := h.repo.Enqueue(ctx, []NewItem{{ZPID: h.zpid("fine"), Payload: payload("fine")}})
		cancel()
		if n != 0 || !errors.Is(err, refused) || errors.Is(err, ErrUnstorable) {
			t.Errorf("%s: Enqueue = %d, %v; want 0 and the statement's error", name, n, err)
		}
		if calls != 1 {
			t.Errorf("%s: statement ran %d times, want once", name, calls)
		}
	}
}

func TestEnqueue_RetriesExactlyOnceOnDeadlock(t *testing.T) {
	deadlock := &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
	boom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		results   []error
		wantCalls int
		wantN     int
		wantErr   error
	}{
		{"no deadlock", []error{nil}, 1, 2, nil},
		{"one deadlock", []error{deadlock, nil}, 2, 2, nil},
		{"wrapped deadlock", []error{fmt.Errorf("exec: %w", deadlock), nil}, 2, 2, nil},
		{"two deadlocks", []error{deadlock, deadlock, nil}, 2, 0, deadlock},
		{"another error is not retried", []error{boom, nil}, 1, 0, boom},
		{"another error on the retry", []error{deadlock, boom}, 2, 0, boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]any
			r := &Repository{enqueueExec: func(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
				calls = append(calls, args)
				if err := tc.results[len(calls)-1]; err != nil {
					return pgconn.CommandTag{}, err
				}
				return pgconn.NewCommandTag("INSERT 0 2"), nil
			}}
			n, err := r.Enqueue(context.Background(), []NewItem{
				{ZPID: "1", Payload: payload("a"), SourceZip: "78701"},
				{ZPID: "2", Payload: payload("b"), SourceZip: "78701"},
			})
			if len(calls) != tc.wantCalls {
				t.Errorf("statement ran %d times, want %d", len(calls), tc.wantCalls)
			}
			if n != tc.wantN || !errors.Is(err, tc.wantErr) {
				t.Errorf("Enqueue = %d, %v; want %d, %v", n, err, tc.wantN, tc.wantErr)
			}
			for i := 1; i < len(calls); i++ {
				if fmt.Sprint(calls[i]) != fmt.Sprint(calls[0]) {
					t.Errorf("retry ran with %v, want the first attempt's %v", calls[i], calls[0])
				}
			}
		})
	}
}

func TestEnqueue_LeavesLiveRowsAloneAndRevivesDeadOnes(t *testing.T) {
	h := newHarness(t, 0)
	w := h.owner("w")

	// One row in every state, built one at a time so that the next claim can
	// only ever return the row the step is about.
	//
	// The dead row dies the way rows do in production: its last failure
	// carries a backoff, so available_at is still in the future when it is
	// revived.
	h.enqueue("old", "dead")
	h.burn(w, "dead", MaxAttempts-1)
	if err := h.repo.Fail(h.ctx, h.claimOne(w, "dead", time.Minute), w, "third strike", time.Hour, false); err != nil {
		t.Fatalf("fail: %v", err)
	}
	dead := h.mustRow("dead")
	if dead.Attempts != MaxAttempts || h.secondsFromNow("available_at", "dead") < 3000 {
		t.Fatalf("dead row = %+v, want it out of attempts and an hour into its backoff", dead)
	}

	// A row also dies when the worker on its last attempt never reports back.
	// That claim, though expired, is still on the row, and the worker may yet
	// turn up with its token.
	h.enqueue("old", "dead-crashed")
	h.burn(w, "dead-crashed", MaxAttempts-1)
	zombie := h.claimOne(w, "dead-crashed", time.Millisecond)
	h.waitExpired("dead-crashed")

	h.enqueue("old", "dying")
	h.burn(w, "dying", MaxAttempts-1)
	dying := h.claimOne(w, "dying", time.Minute) // last attempt, still in flight

	h.enqueue("old", "backoff")
	if err := h.repo.Fail(h.ctx, h.claimOne(w, "backoff", time.Minute), w, "try later", time.Hour, false); err != nil {
		t.Fatalf("fail: %v", err)
	}

	h.enqueue("old", "claimed")
	claimed := h.claimOne(w, "claimed", time.Minute)

	h.enqueue("old", "claimable")

	live := []string{"dying", "backoff", "claimed", "claimable"}
	before := map[string]rowState{}
	for _, name := range live {
		before[name] = h.mustRow(name)
	}

	// Re-discovered from another ZIP, with a new payload.
	var again []NewItem
	for _, name := range []string{"dead", "dead-crashed", "dying", "backoff", "claimed", "claimable", "fresh"} {
		again = append(again, NewItem{ZPID: h.zpid(name), Payload: payload("new"), SourceZip: "78799"})
	}
	if n, err := h.repo.Enqueue(h.ctx, again); err != nil || n != 3 {
		t.Errorf("enqueue = %d, %v; want 3 (the fresh row and the two revived ones)", n, err)
	}

	for _, name := range live {
		if after := h.mustRow(name); after != before[name] {
			t.Errorf("enqueue over the %s row changed it:\n was %+v\n now %+v", name, before[name], after)
		}
	}
	// The claims that were in flight are still good.
	if err := h.repo.Complete(h.ctx, claimed, w); err != nil {
		t.Errorf("complete after an enqueue over the claimed row: %v", err)
	}
	if err := h.repo.Complete(h.ctx, dying, w); err != nil {
		t.Errorf("complete after an enqueue over a row on its last attempt: %v", err)
	}

	revived := h.mustRow("dead")
	if revived.Attempts != 0 || revived.LastError != null || revived.ClaimedBy != null || revived.ClaimedUntil != 0 {
		t.Errorf("revived row = %+v, want attempts 0, no error, unclaimed", revived)
	}
	if !strings.Contains(revived.Payload, `"new"`) || revived.SourceZip != "78799" {
		t.Errorf("revived row kept payload %s from zip %s, want the new payload and zip 78799", revived.Payload, revived.SourceZip)
	}
	if revived.EnqueuedAt <= dead.EnqueuedAt {
		t.Errorf("revived row kept enqueued_at, want the time it was enqueued again")
	}
	// Reviving does not inherit the backoff of the failure that killed it.
	if secs := h.secondsFromNow("available_at", "dead"); secs > 0 {
		t.Errorf("revived row is hidden for another %.0f s, want it claimable now", secs)
	}
	// The revived row is a new item: the claim its last worker vanished with
	// is wiped, not merely expired, so that worker cannot complete it away.
	if crashed := h.mustRow("dead-crashed"); crashed.Attempts != 0 || crashed.ClaimedBy != null || crashed.ClaimedUntil != 0 {
		t.Errorf("revived row = %+v, want attempts 0 and the stale claim gone", crashed)
	}
	h.mustLoseLease("dead-crashed", zombie, w)

	got := h.claim(w, 10, time.Minute)
	slices.SortFunc(got, func(a, b Item) int { return strings.Compare(a.ZPID, b.ZPID) })
	want := []string{h.zpid("claimable"), h.zpid("dead"), h.zpid("dead-crashed"), h.zpid("fresh")}
	slices.Sort(want) // where the suffix falls between "dead-" and "dead-c" is not this test's business
	if !slices.Equal(zpids(got), want) {
		t.Fatalf("claimable after the enqueue = %v, want %v", zpids(got), want)
	}
	for _, it := range got {
		if it.Attempts != 1 {
			t.Errorf("%s claimed with attempts = %d, want 1", it.ZPID, it.Attempts)
		}
	}
}

// Two discoverers whose ZIPs overlap enqueue the same listings in whatever
// order the provider returned them. The statement sorts, so they queue behind
// each other instead of deadlocking.
func TestEnqueue_OppositeOrderNeverDeadlocks(t *testing.T) {
	h := newHarness(t, 0)
	// Counted underneath Enqueue's retry, which would otherwise hide most
	// deadlocks from this test: it is the ordering that is being checked.
	var deadlocks atomic.Int64
	h.repo.enqueueExec = func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
		tag, err := h.pool.Exec(ctx, sql, args...)
		if isDeadlock(err) {
			deadlocks.Add(1)
		}
		return tag, err
	}
	defer func() {
		if n := deadlocks.Load(); n != 0 {
			t.Errorf("PostgreSQL broke %d deadlocks between the enqueuers, want none", n)
		}
	}()

	const size, rounds = 300, 6
	for round := range rounds {
		h.renew()
		forward := make([]NewItem, size)
		for i := range forward {
			forward[i] = NewItem{ZPID: h.zpid(fmt.Sprintf("%04d", i)), Payload: payload("x"), SourceZip: "78701"}
		}
		backward := slices.Clone(forward)
		slices.Reverse(backward)

		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
			ns    [2]int
			errs  [2]error
		)
		for i, items := range [][]NewItem{forward, backward} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ns[i], errs[i] = h.repo.Enqueue(h.ctx, items)
			}()
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d, enqueuer %d: %v", round, i, err)
			}
		}
		if ns[0]+ns[1] != size {
			t.Errorf("round %d: enqueued %d + %d, want %d in total (every row inserted exactly once)", round, ns[0], ns[1], size)
		}
		var rows int
		if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM listing_queue WHERE right(zpid, length($1)) = $1`, "-"+h.suffix).Scan(&rows); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		if rows != size {
			t.Errorf("round %d: %d rows, want %d", round, rows, size)
		}
	}
}

// --- Claim -----------------------------------------------------------------

func TestClaim_NonPositiveLimitDoesNotQuery(t *testing.T) {
	r := &Repository{} // no pool: a query would panic
	for _, limit := range []int{0, -1} {
		if got, err := r.Claim(context.Background(), "w", limit, time.Minute); len(got) != 0 || err != nil {
			t.Errorf("Claim(limit %d) = %v, %v; want nothing", limit, got, err)
		}
	}
}

// A lease that is already over would spend an attempt on a claim nobody
// holds. claimed_until has microsecond resolution, so anything shorter than
// that is such a lease too: now() + 400 ns is now().
func TestClaim_RejectsALeaseThatIsOverOnArrival(t *testing.T) {
	r := &Repository{} // no pool: a query would panic
	for _, lease := range []time.Duration{0, -time.Second, 400 * time.Nanosecond, time.Microsecond - 1} {
		if _, err := r.Claim(context.Background(), "w", 1, lease); err == nil {
			t.Errorf("Claim(lease %s) succeeded, want an error", lease)
		}
	}
}

func TestClaim_ConcurrentClaimsAreDisjoint(t *testing.T) {
	const items, workers, batch = 200, 16, 3
	h := newHarness(t, workers)
	names := make([]string, items)
	for i := range names {
		names[i] = fmt.Sprintf("%04d", i)
	}
	if n := h.enqueue("x", names...); n != items {
		t.Fatalf("enqueued = %d, want %d", n, items)
	}

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		claimed [workers][]Item
		errs    [workers]error
	)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := h.owner(fmt.Sprintf("w%02d", w))
			<-start
			for {
				got, err := h.repo.Claim(h.ctx, owner, batch, time.Minute)
				if err != nil {
					errs[w] = err
					return
				}
				if len(got) == 0 {
					return
				}
				if len(got) > batch {
					errs[w] = fmt.Errorf("one claim returned %d items, limit was %d", len(got), batch)
					return
				}
				claimed[w] = append(claimed[w], got...)
			}
		}()
	}
	close(start)
	wg.Wait()

	holder := map[string]int{}
	for w := range workers {
		if errs[w] != nil {
			t.Fatalf("worker %d: %v", w, errs[w])
		}
		for _, it := range claimed[w] {
			if prev, dup := holder[it.ZPID]; dup {
				t.Errorf("%s was claimed by worker %d and by worker %d", it.ZPID, prev, w)
			}
			holder[it.ZPID] = w
			if it.Attempts != 1 {
				t.Errorf("%s claimed with attempts = %d, want 1", it.ZPID, it.Attempts)
			}
		}
	}
	if len(holder) != items {
		t.Errorf("%d distinct items were claimed, want all %d", len(holder), items)
	}
	var wrong int
	if err := h.pool.QueryRow(h.ctx, `
SELECT count(*) FROM listing_queue
 WHERE attempts <> 1 OR claimed_by IS NULL OR claimed_until <= now()`).Scan(&wrong); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if wrong != 0 {
		t.Errorf("%d rows are not held by exactly one live claim", wrong)
	}
}

// An unordered claim would starve old and backed-off items. The order claimed
// here is a permutation that nothing incidental produces: not the order the
// rows were inserted or last written in, and not their zpid order, forwards
// or backwards.
func TestClaim_OldestFirstWithLeaseAndToken(t *testing.T) {
	h := newHarness(t, 0)
	w := h.owner("w")
	h.enqueue("x", "a", "b", "c", "d")
	ages := map[string]float64{"a": 30, "b": 10, "c": 40, "d": 20}
	for name, secs := range ages {
		h.backdate(name, secs)
	}

	const lease = 65 * time.Minute
	for _, name := range []string{"c", "a", "d", "b"} {
		it := h.claimOne(w, name, lease)
		row := h.mustRow(name)
		if row.ClaimedBy != w || row.Attempts != 1 {
			t.Errorf("%s: claimed_by = %q, attempts = %d; want %q, 1", name, row.ClaimedBy, row.Attempts, w)
		}
		if it.Token.UnixMicro() != row.ClaimedUntil {
			t.Errorf("%s: token %s is not the row's claimed_until", name, it.Token)
		}
		if secs := h.secondsFromNow("claimed_until", name); secs > lease.Seconds() || secs < lease.Seconds()-60 {
			t.Errorf("%s: lease ends %.0f s from now, want %.0f", name, secs, lease.Seconds())
		}
	}
	h.mustClaimNothing(h.owner("other"), "every item is held by a live claim")

	// The same holds inside one batch.
	h.renew()
	h.enqueue("x", "a", "b", "c", "d")
	for name, secs := range ages {
		h.backdate(name, secs)
	}
	got := zpids(h.claim(w, 2, lease))
	slices.Sort(got)
	if want := []string{h.zpid("a"), h.zpid("c")}; !slices.Equal(got, want) {
		t.Errorf("a claim of 2 took %v, want the two oldest %v", got, want)
	}
}

// --- leases and guards -----------------------------------------------------

func TestLease_ExpiryLetsAnotherOwnerClaim(t *testing.T) {
	h := newHarness(t, 0)
	a, b := h.owner("a"), h.owner("b")
	h.enqueue("x", "item")
	stale := h.claimOne(a, "item", 50*time.Millisecond)

	var fresh []Item
	h.eventually("the lease to expire", func() bool {
		fresh = h.claim(b, 1, time.Minute)
		return len(fresh) == 1
	})
	if fresh[0].ZPID != stale.ZPID || fresh[0].Attempts != 2 {
		t.Fatalf("second claim = %+v, want %s on attempt 2", fresh[0], stale.ZPID)
	}
	if fresh[0].Token.Equal(stale.Token) {
		t.Fatalf("both claims carry the token %s", stale.Token)
	}

	h.mustLoseLease("item", stale, a)
	// The first owner's token is no better in the second owner's hands, and
	// the second owner's token is no good to the first.
	h.mustLoseLease("item", stale, b)
	h.mustLoseLease("item", fresh[0], a)

	if err := h.repo.Complete(h.ctx, fresh[0], b); err != nil {
		t.Fatalf("complete by the current holder: %v", err)
	}
	if _, ok := h.row("item"); ok {
		t.Error("Complete left the row behind")
	}
	if err := h.repo.Complete(h.ctx, fresh[0], b); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("second Complete = %v, want ErrLeaseLost", err)
	}
}

func TestLease_SameOwnerReclaimGetsANewToken(t *testing.T) {
	h := newHarness(t, 0)
	w := h.owner("w")
	h.enqueue("x", "item")
	first := h.claimOne(w, "item", 50*time.Millisecond)

	var again []Item
	h.eventually("the lease to expire", func() bool {
		again = h.claim(w, 1, time.Minute)
		return len(again) == 1
	})
	if again[0].Token.Equal(first.Token) {
		t.Fatalf("the re-claim carries the old token %s", first.Token)
	}
	if again[0].Attempts != 2 {
		t.Errorf("re-claim attempts = %d, want 2", again[0].Attempts)
	}
	h.mustLoseLease("item", first, w)
	if err := h.repo.Release(h.ctx, again[0], w, 0); err != nil {
		t.Errorf("release with the current token: %v", err)
	}
}

// Nobody else took the item, so work that finished late is still recorded:
// the guard is the owner and the token, not the clock.
func TestLease_ExpiredButNotReclaimedStillCompletes(t *testing.T) {
	h := newHarness(t, 0)
	w := h.owner("w")
	h.enqueue("x", "item")
	it := h.claimOne(w, "item", time.Millisecond)
	h.waitExpired("item")
	if err := h.repo.Complete(h.ctx, it, w); err != nil {
		t.Fatalf("complete after the lease ran out: %v", err)
	}
	if _, ok := h.row("item"); ok {
		t.Error("Complete left the row behind")
	}
}

// --- Fail and Release --------------------------------------------------------

func TestFail_RetryAfterHidesTheItemUntilThen(t *testing.T) {
	h := newHarness(t, 0)
	w := h.owner("w")
	h.enqueue("x", "later", "soon")
	for _, it := range h.claim(w, 2, time.Minute) {
		retryAfter := time.Hour
		if it.ZPID == h.zpid("soon") {
			retryAfter = 100 * time.Millisecond
		}
		if err := h.repo.Fail(h.ctx, it, w, "bunny: 503", retryAfter, false); err != nil {
			t.Fatalf("fail %s: %v", it.ZPID, err)
		}
	}

	later := h.mustRow("later")
	if later.ClaimedBy != null || later.ClaimedUntil != 0 || later.Attempts != 1 || later.LastError != "bunny: 503" {
		t.Errorf("failed row = %+v, want unclaimed, attempts 1, the error recorded", later)
	}
	if secs := h.secondsFromNow("available_at", "later"); secs > 3600 || secs < 3540 {
		t.Errorf("available_at is %.0f s from now, want 3600", secs)
	}

	// Only the short backoff comes back, and the hour-long one never does.
	var got []Item
	h.eventually("the 100 ms backoff to pass", func() bool {
		got = h.claim(w, 10, time.Minute)
		return len(got) > 0
	})
	if len(got) != 1 || got[0].ZPID != h.zpid("soon") || got[0].Attempts != 2 {
		t.Fatalf("claimed %+v, want only %s on attempt 2", got, h.zpid("soon"))
	}
}

func TestFail_ThreeClaimsWithoutRefundMakeItDead(t *testing.T) {
	h := newHarness(t, 0)
	w := h.owner("w")
	h.enqueue("x", "item")
	h.burn(w, "item", MaxAttempts)

	h.mustClaimNothing(w, "the item has used all its attempts")
	if row := h.mustRow("item"); row.Attempts != MaxAttempts || row.LastError != fmt.Sprintf("burn %d", MaxAttempts) {
		t.Errorf("dead row = %+v, want attempts %d and the last error kept for inspection", row, MaxAttempts)
	}
	if stats, err := h.repo.Stats(h.ctx); err != nil || stats != (Stats{Dead: 1}) {
		t.Errorf("stats = %+v, %v; want one dead row and nothing else", stats, err)
	}
	if depth, err := h.repo.Depth(h.ctx); err != nil || depth != 0 {
		t.Errorf("depth = %d, %v; want 0: a dead row is not claimable", depth, err)
	}
}

func TestFailWithRefundAndReleaseGiveTheAttemptBack(t *testing.T) {
	h := newHarness(t, 0)
	w := h.owner("w")
	h.enqueue("x", "item")

	// Far more rounds than MaxAttempts: a refunded claim never brings the
	// item closer to dead.
	for round := range 2 * MaxAttempts {
		it := h.claimOne(w, "item", time.Minute)
		if it.Attempts != 1 {
			t.Fatalf("round %d: attempts = %d, want 1 (the last one was refunded)", round, it.Attempts)
		}
		var err error
		if round%2 == 0 {
			err = h.repo.Fail(h.ctx, it, w, "breaker open", 0, true)
		} else {
			err = h.repo.Release(h.ctx, it, w, 0)
		}
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		row := h.mustRow("item")
		if row.Attempts != 0 || row.ClaimedBy != null || row.ClaimedUntil != 0 {
			t.Fatalf("round %d: row = %+v, want attempts 0 and no claim", round, row)
		}
	}

	// Release keeps the error of an earlier failure and honours its delay.
	it := h.claimOne(w, "item", time.Minute)
	if err := h.repo.Release(h.ctx, it, w, 10*time.Minute); err != nil {
		t.Fatalf("release: %v", err)
	}
	if secs := h.secondsFromNow("available_at", "item"); secs > 600 || secs < 540 {
		t.Errorf("available_at is %.0f s from now, want 600", secs)
	}
	if row := h.mustRow("item"); row.LastError != "breaker open" {
		t.Errorf("last_error = %q, want the earlier failure kept", row.LastError)
	}
	h.mustClaimNothing(w, "the item was released with a delay")
}

// A negative delay would backdate available_at, and the queue is ordered by it.
func TestFailAndRelease_NegativeDelayMeansNow(t *testing.T) {
	h := newHarness(t, 0)
	w := h.owner("w")
	h.enqueue("x", "item")
	if err := h.repo.Fail(h.ctx, h.claimOne(w, "item", time.Minute), w, "x", -time.Hour, true); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if secs := h.secondsFromNow("available_at", "item"); secs < -60 {
		t.Errorf("after Fail available_at is %.0f s from now, want now", secs)
	}
	if err := h.repo.Release(h.ctx, h.claimOne(w, "item", time.Minute), w, -time.Hour); err != nil {
		t.Fatalf("release: %v", err)
	}
	if secs := h.secondsFromNow("available_at", "item"); secs < -60 {
		t.Errorf("after Release available_at is %.0f s from now, want now", secs)
	}
}

// last_error is fed ffmpeg's stderr and provider response bodies. Bytes that
// PostgreSQL refuses in a text value must not cost the bookkeeping.
func TestFail_StoresATruncatedCleanError(t *testing.T) {
	h := newHarness(t, 0)
	w := h.owner("w")
	h.enqueue("x", "long", "dirty")
	for _, it := range h.claim(w, 2, time.Minute) {
		msg := strings.Repeat("é", 600)
		if it.ZPID == h.zpid("dirty") {
			msg = "bad\x00byte\xffs"
		}
		if err := h.repo.Fail(h.ctx, it, w, msg, 0, false); err != nil {
			t.Fatalf("fail %s: %v", it.ZPID, err)
		}
	}
	if got, want := h.mustRow("long").LastError, strings.Repeat("é", 500); got != want {
		t.Errorf("last_error is %d characters, want the first 500", len([]rune(got)))
	}
	if got, want := h.mustRow("dirty").LastError, "badbyte�s"; got != want {
		t.Errorf("last_error = %q, want %q", got, want)
	}
}

// --- Depth and Stats ---------------------------------------------------------

func TestDepthAndStats_AgreeWithAMixOfStates(t *testing.T) {
	h := newHarness(t, 0)
	w := h.owner("w")
	mustBe := func(when string, wantStats Stats, wantDepth int) {
		t.Helper()
		if stats, err := h.repo.Stats(h.ctx); err != nil || stats != wantStats {
			t.Errorf("%s: stats = %+v, %v; want %+v", when, stats, err, wantStats)
		}
		if depth, err := h.repo.Depth(h.ctx); err != nil || depth != wantDepth {
			t.Errorf("%s: depth = %d, %v; want %d", when, depth, err, wantDepth)
		}
	}
	mustBe("empty queue", Stats{}, 0)

	// Built one state at a time, so that each claim can only return the rows
	// the step is about.
	h.enqueue("x", "dead")
	h.burn(w, "dead", MaxAttempts)

	h.enqueue("x", "dead-expired")
	h.burn(w, "dead-expired", MaxAttempts-1)
	h.claimOne(w, "dead-expired", time.Millisecond)
	h.waitExpired("dead-expired")

	h.enqueue("x", "backoff-1", "backoff-2")
	for _, it := range h.claim(w, 2, time.Minute) {
		if err := h.repo.Fail(h.ctx, it, w, "later", time.Hour, false); err != nil {
			t.Fatalf("fail: %v", err)
		}
	}

	h.enqueue("x", "claimed-last-attempt")
	h.burn(w, "claimed-last-attempt", MaxAttempts-1)
	h.claimOne(w, "claimed-last-attempt", time.Minute)

	h.enqueue("x", "claimed-1", "claimed-2")
	if got := h.claim(w, 2, time.Minute); len(got) != 2 {
		t.Fatalf("claimed %v, want 2", zpids(got))
	}

	h.enqueue("x", "claimable-expired")
	h.claimOne(w, "claimable-expired", time.Millisecond)
	h.waitExpired("claimable-expired")

	h.enqueue("x", "claimable-1", "claimable-2", "claimable-3")

	mustBe("mix of states", Stats{Claimable: 4, Claimed: 3, Backoff: 2, Dead: 2}, 4)

	// Depth is what Claim would hand out.
	got := h.claim(w, 100, time.Minute)
	slices.SortFunc(got, func(a, b Item) int { return strings.Compare(a.ZPID, b.ZPID) })
	want := []string{h.zpid("claimable-1"), h.zpid("claimable-2"), h.zpid("claimable-3"), h.zpid("claimable-expired")}
	slices.Sort(want)
	if !slices.Equal(zpids(got), want) {
		t.Errorf("claimed %v, want %v", zpids(got), want)
	}
	mustBe("after claiming everything claimable", Stats{Claimed: 7, Backoff: 2, Dead: 2}, 0)
}
