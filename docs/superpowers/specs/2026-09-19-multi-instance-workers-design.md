# Multi-instance workers — design

Date: 2026-09-19 · Branch: `feature/multiple-workers` · Revision 2 (after a
four-lens adversarial design review)

## 1. Goal

Process more than 1,000,000 listings by running about ten instances of this
binary (4–8 vCPU each) against the one PostgreSQL, the one Bunny storage zone
and the one OpenWebNinja key — with no ZIP searched twice, no listing rendered
twice, no API budget spent twice, and no instance able to corrupt another's
work. A single instance must keep working as it does today with **no new
required configuration**.

Non-goals (found during the audit, deliberately left for follow-up work):

- API-tier behaviour at 1M rows: unbounded `/roku/feed.json`, `ListClips` full
  scan in `/channels/resolve`, per-request `COUNT(*)`, missing
  `lower(city)/lower(state)` expression indexes. Independent of the worker
  fleet; next branch.
- More than one **API** instance behind a load balancer (per-box nginx cache,
  `X-Real-IP`, in-process LocationIQ budget).
- Storage reaping, listing refresh/delisting, a fleet-wide Zillow
  requests-per-second limiter, PgBouncer.
- The content hash differs between the first-ingest path (hashes CDN URLs) and
  the scheduler's revisit of a *search result* (hashes source URLs). With
  exclusive claims this can no longer produce concurrent double rows.
- `cmd/backfill-hls` stays a one-box tool: two copies, or a copy racing a live
  worker, produce byte-identical segments (`-c copy` remux) under the same
  immutable prefix and `SetVideoHLS` is `ON CONFLICT DO NOTHING`.
- A VM paused for longer than the lease margin (5 min) can resume work on an
  expired claim. Accepted.

## 2. What is wrong today

Verified by a ten-area audit with an adversarial second pass (file:line against
`main` at `1239945`):

| # | Hazard | Where |
|---|---|---|
| 1 | `NextBatch` is a plain `SELECT`; every instance gets the same ZIPs. The cursor is written only after all renders of the ZIP finish. | `internal/zipcode/repository.go:25`, `internal/scheduler/scheduler.go:215` |
| 2 | The API budget, the quota guard, the overlap guard and the `tried` map are process-local; a restart resets the budget. | `scheduler.go:148,159,174,237` |
| 3 | No listing claim. `video_status='pending'` means both "never tried" and "rendering now", so `NeedsVideo` sends other instances into in-flight listings. | `scheduler.go:311`, `internal/property/repository.go:211` |
| 4 | Unconditional last-writer-wins updates: `SetVideoFailed` clobbers `ready`; an empty `SetDetails` wipes a stored details record. | `property/repository.go:72,264` |
| 5 | Two writers to `properties/<zpid>/<idx>` with compacted indexes mix photo sets; an empty photo set is persisted forever under `SKIP_EXISTING`. | `scheduler.go:349,488` |
| 6 | `enrichDetails` selects the same oldest 50 zpids on every instance (a paid call each) and only runs after the whole search+render phase. | `property/repository.go:229`, `scheduler.go:223` |
| 7 | Every boot runs the whole DDL script (ACCESS EXCLUSIVE on `properties`, lock-upgrade deadlock between two booting instances); ZIP seeding is count-then-COPY. | `internal/db/db.go:42`, `internal/zipseed/zipseed.go:52` |
| 8 | Pool hard-coded to 10 connections: ten instances = Postgres' default `max_connections`. | `db/db.go:21` |
| 9 | No role or identity: every instance is scheduler + public API; CI deploys one host. | `cmd/server/main.go:114` |
| 10 | A broken instance fails fast, stamps ZIPs searched and would win most claims in any queue. No retry on Bunny/Zillow 429/5xx, no ffmpeg deadline. | `scheduler.go:215`, `internal/bunny/client.go:51`, `internal/zillow/client.go:176`, `internal/video/video.go:144` |

Already safe and left alone: lineup version creation (PK settles races),
`video_hls` and heartbeat inserts (`ON CONFLICT DO NOTHING`), per-process temp
directories.

## 3. Architecture

PostgreSQL is the coordinator; there is no leader and no new infrastructure.
The cron-triggered "cycle" is replaced by three independent loops per worker
instance. Every unit of work is taken with an atomic, expiring **claim**.

```
            zip_codes (claim)            listing_queue (claim)          properties (details claim)
                 │                              │                                │
 discovery loop ─┘ search ZIP ── enqueue ──────▶│                                │
                                   media loop ──┘ photos → upsert → render → HLS │
                                                       details loop ─────────────┘ fetch → SetDetails
        api_budget (ledger): one request is reserved immediately before every paid HTTP attempt
```

### 3.1 The claim rule

Every claim is `UPDATE … FROM (CTE: SELECT … FOR UPDATE SKIP LOCKED)` and
writes `claimed_by` + `claimed_until = now() + lease` using the **database
clock**. Under READ COMMITTED the lock step re-evaluates the
`claimed_until < now()` qual on the newest row version, so two transactions can
never claim the same row.

> The work done under a claim runs under a context deadline shorter than the
> lease. A live worker therefore never works on an expired claim, no
> heartbeat/renewal is needed, and a crashed worker's claim frees itself.

| Claim | Work deadline | Lease | Notes |
|---|---|---|---|
| ZIP | 10 min | 15 min | |
| Listing | 60 min | 65 min | videos run up to 771 s; the lease only delays recovery after a hard crash, graceful shutdown releases immediately |
| Details batch | 5 min | 10 min | ownerless by design: the guard is `details_fetched_at IS NULL` |

- The claim **owner** is `INSTANCE_ID + "/" + random per-boot nonce`, so
  uniqueness never depends on the operator and a restarted process never
  passes a guard meant for its predecessor. Logs carry `INSTANCE_ID` only.
- Claims return `claimed_until`; it is the per-claim **fencing token**. Every
  completion is guarded by `claimed_by = $owner AND claimed_until = $token`;
  zero rows affected is logged as a lost lease and changes nothing.
- Every post-work transition (enqueue, mark, defer, fail, complete, release)
  runs under `context.WithoutCancel(ctx)` with a 15 s timeout — the lease
  margin exists to cover it — so deadlines and SIGTERM never lose bookkeeping.

### 3.2 Discovery loop

One ZIP per step:

1. Backpressure: claimable queue depth ≥ `QUEUE_HIGH_WATER` → wait 30 s.
   (Soft limit: overshoot is at most one ZIP per discovering instance.)
2. Quota gate (§3.3) not open → wait for the next window.
3. Search budget of the current window already spent → wait for the next
   window (sleeps are capped at 10 min).
4. Claim one ZIP (returns its `resume_page`). None claimable → wait 1 min.
5. `SearchPages(startPage, permit)`: the ledger is asked for **one request
   immediately before every HTTP attempt**, retries included.
6. Outcome:
   - **complete** → enqueue, `MarkSearched` (resets `failures`, `resume_page`).
   - **budget ran out mid-ZIP** (permit denied) → enqueue what was fetched,
     `Defer`: released until the next window with `resume_page` = the first
     unfetched page. Not a failure. Nothing already paid for is bought again
     and a dense ZIP cannot livelock a small budget.
   - **error** → enqueue what was fetched, `Fail`: `failures + 1`,
     `claimed_until = GREATEST(now() + 1 h, next window start)` (preserves
     today's "retries next cycle"), `resume_page` = the failing page. At 5
     consecutive failures the ZIP is stamped searched (goes to the back of the
     rotation), logged loudly, counters reset. Discovery pauses
     `min(5 s · 2^k, 5 min)` for k consecutive failures. A 429 closes the
     quota gate. This replaces the in-memory `tried` map.
   - enqueue itself fails (after one retry) → `Fail` with the *original*
     resume page, so the paid pages are fetched again rather than lost.
7. Shutdown: a search that has already returned is still enqueued and marked;
   a claimed ZIP that has not been searched is `Release`d.

Enqueue filter — one batched `VideoStates(zpids)` query, same predicate as
today: skip when `SKIP_EXISTING && stored && !(videoWanted && needsVideo)`.
Empty zpids are dropped. The media worker re-checks, as `processListing` does
today.

### 3.3 Budget ledger and quota gate

`CRON_SCHEDULE` no longer triggers anything; it **defines the budget windows**.
`API_BUDGET_PER_CYCLE` is the **fleet-wide** budget per window: `search` may
spend `max(0, API_BUDGET_PER_CYCLE − DETAILS_PER_CYCLE)`, `details` may spend
`DETAILS_PER_CYCLE` — today's split.

Windows: the schedule is parsed with `cron.ParseStandard` at startup.
`*cron.SpecSchedule`: evaluate `Next` on `now.UTC()`, walking forward from
`now − lookback` (1 h, 1 d, 31 d, 366 d, 5 y) and taking the last activation
≤ now; none found is a startup error. `cron.ConstantDelaySchedule` (`@every`):
`window = now.UTC().Truncate(delay)` — epoch-anchored, identical on every box.
Any other schedule type is a startup error. (The runtime image has no tzdata,
so production already evaluates in UTC.)

`TryReserve(window, kind, limit)` is one atomic statement:

```sql
INSERT INTO api_budget (window_start, kind, spent) VALUES ($1, $2, 1)
ON CONFLICT (window_start, kind) DO UPDATE SET spent = api_budget.spent + 1
 WHERE api_budget.spent < $3
RETURNING spent
```

(`limit ≤ 0` is denied in Go before the statement.) No refunds exist, so a
crash leaks at most one request, retries are charged exactly, nothing crosses
a window boundary, and a lowered limit simply denies. A DB error denies
(fail closed). The effective fleet limit is the largest limit any instance
passes — budget variables must be identical on every box (§3.12).

For one instance this is the old behaviour: a fresh budget becomes available
at each cron activation and is spent down. A restarted instance joins the
current window instead of getting a fresh budget.

**Quota gate** — shared by discovery and details. Once per window per
instance it calls the provider's `/usage`: `status = exceeded`, or
`Requests.remaining < (API_BUDGET_PER_CYCLE − spent this window)` → closed
until the next window. A failed `/usage` call proceeds (fail-open, as today)
but is **not cached**. Any 429 from search or details invalidates the cached
verdict.

### 3.4 Media loop

Continuous, not budget-gated. A dispatcher with `LISTING_CONCURRENCY` slots
claims as many items as it has free slots (ordered by `available_at`), polls
every 5 s when the queue is empty, and runs `processListing` for each item
under the 60 min deadline, inside a `recover()`.

Outcomes:

- success, skip, or **no source photos at all** (terminal, non-error: upsert,
  `SetVideoFailed`, as today) → `Complete` (row deleted; `properties` is the
  record)
- parent context cancelled (shutdown) → `Release`: claim cleared, attempt
  refunded
- infrastructure error (download/Bunny/DB/ffmpeg/deadline/panic, or *source
  photos exist but zero could be stored*) → `Fail` with backoff (5 min, then
  30 min; dead after 3 attempts) and counted by the breaker

`processListing` changes: render/upload failures are returned instead of only
logged; `SetVideoFailed` is not written on shutdown; with `IMAGES_ENABLED`
and not a revisit, zero stored photos fails **before** `Upsert`, so an empty
gallery is never persisted (a partial set is accepted as today); HLS
**segment → upload → `SetVideoHLS` now runs before `SetVideoReady`** so a
shutdown in the HLS phase retries the whole item instead of stranding a
ready-but-unsegmented listing. A real segmentation error stays non-fatal; a
context error during segmentation is returned.

Payloads may carry `revisit: true` (set by the `backfill-videos` enqueuer):
`processListing` then takes the video-only revisit path regardless of
`SKIP_EXISTING`. A worker with video disabled releases such items with a
10 min delay instead of completing them.

Circuit breaker: 5 consecutive infrastructure failures open it for
`min(1 min · 2^k, 15 min)`. After the pause — **and at boot** — it is
half-open: exactly one item is claimed; success closes it (full concurrency),
failure re-opens it. A failure recorded while open/half-open refunds the
attempt (backoff only), so a broken or crash-looping box cannot dead-letter
healthy items. Only `ROLE=worker` ties `/healthz` to the breaker (503 while
open); `all`/`api` stay pure liveness.

Every worker logs one status line per minute: in-flight, completed, failed,
breaker state, claimable queue depth.

### 3.5 Details loop

Runs concurrently with the other two, behind the same quota gate. Per batch:
`ClaimMissingDetails(10)` (SKIP LOCKED, 10 min lease), then for each zpid
`PropertyDetails(zpid, permit)`:

- stored or not-found → `SetDetails` (guarded by `details_fetched_at IS NULL`)
- permit denied → release the unfetched rows, wait for the next window
- 429 / 5xx / network / context error → stop the batch, release the current
  and unfetched rows, back the loop off like §3.2; **no attempt is counted**
- row-specific failure (decode error, 4xx other than 429, store error) →
  `details_attempts + 1`, lease kept as backoff; abandoned at 5 attempts

`details_attempts` is never incremented at claim time, so a provider outage or
a deploy cannot abandon healthy rows.

### 3.6 Guarded writes

- `SetVideoFailed`: `… AND video_status IS DISTINCT FROM 'ready'` — a failed
  re-render no longer takes a working video off the feed.
- `SetDetails`: `… AND details_fetched_at IS NULL`.
- Zillow: a 200 whose envelope `status` is present, is not `OK`, **and** which
  carries no usable payload (empty data, or an error object instead of a
  record / listing array) is a transient error rather than "not found"/"no
  listings". An `OK` or absent status with empty data keeps today's meaning,
  and a real payload is kept whatever the status says.

### 3.7 Startup

`db.Migrate` runs in **one transaction on one connection**:
`pg_advisory_xact_lock`, create `schema_meta` if missing, compare the stored
SHA-256 of `schema.sql`, and only when it differs run the script and store the
hash. Ten booting instances serialise; routine restarts take no table locks;
there is no unlock path to get wrong. `zipseed.Seed` does its count + COPY the
same way. `db.Connect` sets `idle_in_transaction_session_timeout = 30s`.

### 3.8 Roles, identity, pool, defaults

- `ROLE=all|api|worker` (default `all`; anything else is a config error).
  `api`: HTTP only. `worker`: the three loops plus a `/healthz`-only server;
  it honours `LINEAR_ENABLED` for segmentation but mounts no routes.
  `VIEWER_SALT` is required only for `all|api`.
- `INSTANCE_ID` (default: hostname) — attribution in logs and `claimed_by`.
- `DB_MAX_CONNS` default 10, minimum 4. Workers ship with 6; the backfill
  tools use 4.
- `QUEUE_HIGH_WATER` default 2000 (≈ 80 min of work for a 40 vCPU fleet,
  ≈ 14 h for one 4 vCPU box); `≤ 0` disables backpressure.
- `SEARCH_MAX_RESULTS` default 50 → 0 (unlimited): a worker provisioned
  without the override would otherwise truncate dense ZIPs and still mark
  them searched. Spend stays bounded by the ledger.
- One "effective fleet config" log line at boot (schedule, window, limits,
  role, instance) so drift between boxes can be found with grep.

### 3.9 Shutdown

Context cancel stops claiming; in-flight items are cancelled (ffmpeg killed,
`WaitDelay` set on both ffmpeg invocations), released with a fresh context;
see §3.2 step 7 for the ZIP. `Scheduler.Stop` waits for all loops.
`stop_grace_period: 60s` in the compose files.

### 3.10 Hardening

- Bunny `Upload`: up to 3 attempts on network errors, 429 and 5xx (1 s / 4 s
  backoff) when the body is an `io.Seeker`. The body is wrapped in a
  no-op-`Close` reader so the transport cannot close the caller's file, and it
  is **detached when the attempt ends**: net/http reads the body on its own
  goroutine and can still be reading after `Do` returns (an early 5xx), which
  would race the `Seek(0)` of the next attempt and store a truncated object.
  `Content-Length` is set, so a short body is an error, never a silent
  truncation. Non-seekable bodies get one attempt.
- Zillow: up to 3 attempts on network errors, 429 and 5xx, honouring
  `Retry-After` (capped at 60 s); every attempt asks the permit first.
  `SearchPages` returns the pages already fetched together with an error.
  Non-200 responses are a typed `*StatusError`.

### 3.11 Maintenance commands

- `cmd/backfill-videos` becomes an **enqueuer**: it selects listings with
  photos but no ready video and enqueues them with `revisit: true`; the fleet
  renders them through the revisit path (which also segments HLS — the old
  tool did not). It prints enqueued / already-queued counts, keeps `-dry-run`
  and `-limit`, and gains `-status` (claimable / claimed / backoff / dead).
  The duplicated render pipeline is deleted. It also revives dead queue rows.
- `cmd/backfill-hls`: unchanged apart from the `db.Connect` signature.

### 3.12 Deployment

- `compose.worker.yml`: no published ports, `env_file: [.env.fleet, .env.host]`,
  `stop_grace_period: 60s`, healthcheck
  `["CMD","wget","-q","-O","/dev/null","http://127.0.0.1:8080/healthz"]`
  (interval 30 s, timeout 5 s, retries 3, start_period 60 s; visibility only —
  Docker does not restart unhealthy containers).
- `.env.fleet.example` — byte-identical on every box (`CRON_SCHEDULE`,
  budgets, `SEARCH_*`, `SKIP_EXISTING`, `VIDEO_SECONDS_PER_PHOTO`,
  `QUEUE_HIGH_WATER`, keys and URLs); `.env.host.example` — `ROLE`,
  `INSTANCE_ID`, `DB_MAX_CONNS`. `.env.prod.example` corrected (dead
  `SEARCH_LOCATION` removed, `SEARCH_MAX_RESULTS=0`, new variables).
- CI `deploy-workers` job: `if: vars.WORKER_HOSTS != '' && vars.WORKER_HOSTS != '[]'`,
  `fail-fast: false`, `matrix.host: fromJSON(vars.WORKER_HOSTS || '["unset"]')`,
  SSH through the web box as jump host, per-run GHCR login.
- `deploy/README.md` fleet section: provisioning checklist (private network,
  DB firewall + `pg_hba.conf`, GHCR credentials, CI deploy key), connection
  budget (`Σ DB_MAX_CONNS + 4 per backfill tool + headroom ≤ max_connections − 3`),
  must-match variables, rollout order (replace the existing instance first —
  an old binary ignores claims; copy the compose file), rollback warning
  (never run a pre-claims image while workers run, unless
  `API_BUDGET_PER_CYCLE=0` and `DETAILS_PER_CYCLE=0`), draining a box, and a
  runbook of SQL: queue states, who holds what, dead rows by error, budget
  rows, requeue dead items, reset abandoned details.

## 4. Schema

All idempotent, appended to `schema.sql`. The literals `3` and `5` must equal
the Go constants `workqueue.MaxAttempts` and `property.MaxDetailsAttempts`
(an integration test compares them with `pg_get_indexdef`); statements that
use them are built from the constants, never from bind parameters, so the
partial-index predicates stay provable.

```sql
CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);

ALTER TABLE zip_codes ADD COLUMN IF NOT EXISTS claimed_by    TEXT;
ALTER TABLE zip_codes ADD COLUMN IF NOT EXISTS claimed_until TIMESTAMPTZ;
ALTER TABLE zip_codes ADD COLUMN IF NOT EXISTS failures      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE zip_codes ADD COLUMN IF NOT EXISTS resume_page   INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS listing_queue (
    zpid          TEXT PRIMARY KEY,
    payload       JSONB NOT NULL,
    source_zip    TEXT NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0,
    available_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_by    TEXT,
    claimed_until TIMESTAMPTZ,
    last_error    TEXT,
    enqueued_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_listing_queue_claimable
    ON listing_queue (available_at) WHERE attempts < 3;

CREATE TABLE IF NOT EXISTS api_budget (
    window_start TIMESTAMPTZ NOT NULL,
    kind         TEXT NOT NULL,                  -- 'search' | 'details'
    spent        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (window_start, kind)
);

ALTER TABLE properties ADD COLUMN IF NOT EXISTS details_claimed_until TIMESTAMPTZ;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS details_attempts INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_properties_details_todo
    ON properties (created_at) WHERE details_fetched_at IS NULL AND details_attempts < 5;
```

Queue row states: *claimable* (`attempts < 3`, `available_at ≤ now()`, no live
claim), *backoff* (`available_at > now()`), *claimed*, *dead* (`attempts ≥ 3`,
no live claim — kept for inspection). Attempts are incremented at claim time,
so process-killing items are bounded. `Enqueue` is one statement fed by
`SELECT DISTINCT ON (zpid) … ORDER BY zpid` (dedupes; one global lock order)
with `ON CONFLICT (zpid) DO UPDATE … WHERE` the existing row is dead and
unclaimed (re-discovery revives it); live rows are untouched. It retries once
on deadlock (`40P01`).

## 5. Go API contract

```go
// internal/db
func Connect(ctx context.Context, databaseURL string, maxConns int) (*pgxpool.Pool, error) // <=0 → 10, floor 4
func Migrate(ctx context.Context, pool *pgxpool.Pool) error                                  // §3.7
func WithXactLock(ctx context.Context, pool *pgxpool.Pool, key int64, fn func(pgx.Tx) error) error
const LockMigrate, LockSeed int64

// internal/zipcode
type Claim struct { Zip string; ResumePage int; Token time.Time }
var ErrLeaseLost = errors.New(...)
const MaxFailures = 5
func (r *Repository) Claim(ctx, owner string, lease time.Duration) (*Claim, error)          // nil, nil = none claimable
func (r *Repository) MarkSearched(ctx, c Claim, owner string, listingCount int) error
func (r *Repository) Defer(ctx, c Claim, owner string, until time.Time, resumePage int) error
func (r *Repository) Fail(ctx, c Claim, owner string, until time.Time, resumePage int) (pushedBack bool, err error)
func (r *Repository) Release(ctx, c Claim, owner string) error

// internal/workqueue
const MaxAttempts = 3
type NewItem struct { ZPID string; Payload []byte; SourceZip string }
type Item struct { ZPID string; Payload []byte; Attempts int; Token time.Time }
type Stats struct { Claimable, Claimed, Backoff, Dead int }
var ErrLeaseLost = errors.New(...)
var ErrUnstorable = errors.New(...)   // Enqueue: some items were refused by PostgreSQL (e.g. a NUL in a provider
                                      // string); every storable item WAS enqueued and n is valid. Callers log and carry on.
func (r *Repository) Enqueue(ctx, items []NewItem) (enqueued int, err error)
func (r *Repository) Claim(ctx, owner string, limit int, lease time.Duration) ([]Item, error)
func (r *Repository) Complete(ctx, it Item, owner string) error
func (r *Repository) Fail(ctx, it Item, owner, errMsg string, retryAfter time.Duration, refundAttempt bool) error
func (r *Repository) Release(ctx, it Item, owner string, delay time.Duration) error        // refunds the attempt
func (r *Repository) Depth(ctx) (int, error)                                                // claimable now
func (r *Repository) Stats(ctx) (Stats, error)

// internal/budget
const KindSearch, KindDetails = "search", "details"
type Windows struct{ ... }
func ParseWindows(spec string) (*Windows, error)
func (w *Windows) Current(now time.Time) (start, next time.Time)
type Ledger struct{ ... }
func NewLedger(pool *pgxpool.Pool) *Ledger
func (l *Ledger) TryReserve(ctx, window time.Time, kind string, limit int) (bool, error)
func (l *Ledger) Spent(ctx, window time.Time, kind string) (int, error)

// internal/property
const MaxDetailsAttempts = 5
func (r *Repository) VideoStates(ctx, zpids []string) (map[string]bool, error)             // stored zpid → needsVideo
func (r *Repository) ClaimMissingDetails(ctx, limit int, lease time.Duration) ([]string, error)
func (r *Repository) ReleaseDetails(ctx, zpids []string) error                              // no attempt counted
func (r *Repository) FailDetails(ctx, zpid string) error                                    // attempts+1, lease kept
// SetDetails and SetVideoFailed keep their signatures and gain the §3.6 guards.

// internal/zillow
type Permit func(ctx context.Context) bool     // nil = always allowed
type SearchResult struct { Properties []property.Property; Requests int; NextPage int }    // NextPage>0: stopped early, resume here
type StatusError struct { Code int; Body string }
var ErrBudgetExhausted = errors.New(...)
func IsTransient(err error) bool               // network, 429, 5xx, soft envelope error
func (c *Client) SearchPages(ctx, s config.SearchCriteria, startPage int, permit Permit) (SearchResult, error)
func (c *Client) PropertyDetails(ctx, zpid string, permit Permit) (*property.Details, []byte, error)

// internal/config
type Role string; const RoleAll, RoleAPI, RoleWorker
Config.Role, Config.InstanceID, Config.DBMaxConns, Config.QueueHighWater
func (r Role) RunsWorkers() bool; func (r Role) ServesAPI() bool

// internal/scheduler
type Deps struct { Zillow; Bunny; Repo; Zips; Queue; Ledger; Windows; Render }   // consumer-side interfaces
func New(cfg *config.Config, d Deps, owner string, log *slog.Logger) *Scheduler
func EncodeListing(p *property.Property, revisit bool) ([]byte, error)             // the queue payload; used by cmd/backfill-videos
func (s *Scheduler) Start(ctx) error; func (s *Scheduler) Stop(); func (s *Scheduler) Healthy() bool
func (s *Scheduler) RunOnce(ctx) // one synchronous pass: discover until it would wait, drain the queue, details until it would wait — the tests' entry point
```

## 6. Testing

- Unit tests with in-memory fakes for every loop step: budget exhaustion
  mid-ZIP and resume, backpressure, failed-ZIP backoff and push-back,
  skip-existing filter, revisit, retry/dead-letter, shutdown release, breaker
  open/half-open/close and attempt refund, details outcomes, HLS-before-ready
  ordering, zero-photo rule in every mode.
- Integration tests against a real PostgreSQL (`TEST_DATABASE_URL`, skipped
  otherwise) for everything whose correctness is the SQL: N goroutines
  claiming concurrently get disjoint ZIPs and disjoint queue items; lease
  expiry; owner + token guards; enqueue conflict rules, duplicate zpids in one
  batch, two goroutines enqueuing the same set in opposite order; concurrent
  `TryReserve` never exceeds the limit, limit below spent denies; concurrent
  `Migrate` + `Seed`; guarded writes; index predicates match the constants.
- Bunny: `*os.File` upload against a server answering 500 then 201.
- `go vet ./...`, `go test -race ./...`.

## 7. Capacity (not solved by code)

1M listings ≈ 1.04–1.07M OpenWebNinja requests (≈1M of them are the
per-listing details call; `DETAILS_PER_CYCLE=0` brings a national pass down to
≈40–70k), ≈1,160 vCPU-days of ffmpeg (≈29 days on 40 vCPU; estimated from the
87 s average video, **not benchmarked** — time one 190-photo render at
`LISTING_CONCURRENCY=4` before fleet rollout), ≈30 TB and ≈60M PUTs on Bunny.
