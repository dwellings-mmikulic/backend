# Nationwide ZIP Rotation on a Request Budget

**Date:** 2026-08-10
**Status:** Approved

## Problem

Collection has been dead since 2026-07-15: the OpenWebNinja account is on the
free plan (100 requests/month) and every search since mid-July has failed with
quota errors. Independently, coverage is limited to 3 hardcoded ZIP codes
(`SEARCH_LOCATION=33950,33948,33983`) capped at 10 results each, so growth had
already plateaued at 92 listings.

The goal is nationwide coverage: index listings across all US residential ZIP
codes, with no per-ZIP result cap, on the OpenWebNinja **Pro plan
($25/month, 10,000 requests/month)**.

The constraint that shapes everything: a full pass over the country costs
30k–60k search requests, so the collector must *rotate* through ZIPs on a
strict per-cycle request budget rather than searching everything every cycle.

## Decisions already made

- **Coverage:** all US residential ZIPs (50 states + DC). Source: the
  `zip_code_database.xls` spreadsheet (42,736 rows), filtered to
  `type = STANDARD`, `decommissioned = 0`, state not in territories/military —
  **29,670 ZIPs**.
- **API plan:** Pro, 10k requests/month. Budget target ~9k/month leaves slack.
- **Videos:** keep rendering for every new listing (unchanged). Discovery is
  throttled by the API budget, so renders arrive gradually.
- **Refresh:** `SKIP_EXISTING=true` stays. Re-discovered listings cost one DB
  existence check — no API calls, no re-upsert, no re-render (except the
  existing missing-video retry path, which is unchanged).

## Design

### 1. `zip_codes` table, seeded from an embedded CSV

A one-time local conversion of the spreadsheet produces
`internal/zipseed/zips.csv` (~1 MB, committed), columns:
`zip,city,state,county,population` (population = `irs_estimated_population`,
0 when unknown). ZIP codes keep leading zeros (text, 5 digits).

Appended to the idempotent `internal/db/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS zip_codes (
    zip                TEXT PRIMARY KEY,
    city               TEXT NOT NULL DEFAULT '',
    state              TEXT NOT NULL DEFAULT '',
    county             TEXT NOT NULL DEFAULT '',
    population         INTEGER NOT NULL DEFAULT 0,
    last_searched_at   TIMESTAMPTZ,
    last_listing_count INTEGER
);
CREATE INDEX IF NOT EXISTS idx_zip_codes_rotation
    ON zip_codes (last_searched_at ASC NULLS FIRST, population DESC);
```

A new `internal/zipseed` package embeds the CSV (`go:embed`) and exposes
`Seed(ctx, db)`: if `zip_codes` is non-empty it does nothing; otherwise it
batch-inserts all rows. Called from `cmd/server` startup right after
`db.Migrate`. No manual import step; redeploys never reset rotation state.

### 2. Rotation cursor

The scheduler no longer reads locations from config. Each cycle it pulls ZIP
batches from the repository:

```sql
SELECT zip FROM zip_codes
ORDER BY last_searched_at ASC NULLS FIRST, population DESC
LIMIT $1;
```

Never-searched ZIPs come first, most-populous first among them, so dense
markets are indexed early. After a ZIP's search **succeeds** (including zero
results), the scheduler sets `last_searched_at = now()` and
`last_listing_count`. A failed search leaves the row untouched, so it retries
next cycle. ZIPs the budget never reached are likewise untouched.

`SEARCH_LOCATION` is removed from config (no longer required, no longer read).

### 3. Per-cycle API budget governor

New env `API_BUDGET_PER_CYCLE` (default **150**): the maximum number of
OpenWebNinja requests one cycle may spend, counting every search *page* and
every details call.

- Search budget = `API_BUDGET_PER_CYCLE - DETAILS_PER_CYCLE` (floor 0).
- The cycle iterates ZIPs from the cursor; each search page decrements the
  search budget. When it hits 0 the search phase stops — mid-batch is fine,
  the cursor resumes there next cycle. A ZIP whose pagination is truncated by
  budget exhaustion mid-ZIP is still marked searched (its first pages were
  processed; rotation will revisit it).
- Details enrichment then runs exactly as today, capped by `DETAILS_PER_CYCLE`
  (50). Unused reserve is simply unused.

Client change: `SearchCriteria` gains `MaxPages int` (0 = the existing hard
cap of 20). `zillow.Client.Search` honors it and returns the number of pages
fetched alongside the results, so the scheduler can decrement its budget. The
`zillowAPI` interface in the scheduler updates accordingly.

`SEARCH_MAX_RESULTS=0` (already supported: "no cap") becomes the production
setting; the budget, not a result count, limits work.

Monthly math at defaults: 2 cycles/day × (≤100 search pages + ≤50 details)
= ~9,000 requests/month against the 10k Pro quota.

### 4. Quota guard

At cycle start the scheduler calls the existing `zillow.Client.Usage`
endpoint (a lightweight status endpoint at the host root). If the
report says `status != "ok"` or `Requests.remaining <
API_BUDGET_PER_CYCLE`, the cycle logs a warning and skips — no more silently
burning a month of 429s, which is exactly what production has done since
July 15. If the usage call itself fails, the cycle proceeds (fail-open) so a
flaky usage endpoint can't halt collection.

### 5. Overlap guard

Cycles get long (every new listing renders a video). `RunCycle` gets a
`sync/atomic` in-flight flag: if the previous cycle is still running when cron
fires, the new cycle logs and returns immediately. (The startup immediate run
and the first cron run can otherwise overlap today; this closes that too.)

### 6. Config summary

| Env | Change |
|---|---|
| `SEARCH_LOCATION` | **removed** (table-driven now) |
| `SEARCH_MAX_RESULTS` | prod set to `0` (unlimited per ZIP) |
| `API_BUDGET_PER_CYCLE` | **new**, default `150` |
| `DETAILS_PER_CYCLE` | unchanged (default 50), now carved out of the cycle budget |
| `CRON_SCHEDULE` | unchanged (`0 */12 * * *`) |

## Error handling

- Search page error: logged, ZIP left unmarked (retries next cycle), spent
  pages still count against the budget (the provider counted them too).
- Seed insert error: startup fails (same severity as migration failure).
- Usage endpoint error: log + proceed (fail-open).
- Listing processing errors: unchanged (logged, tallied, never fatal).

## Testing

- **zipseed:** seeds an empty table; second call is a no-op; leading-zero ZIPs
  survive round-trip.
- **repository:** rotation query orders NULLS-first then population DESC;
  marking updates `last_searched_at`/`last_listing_count`.
- **scheduler:** budget governor stops mid-batch and resumes from the cursor;
  details reserve respected; quota guard skips when exhausted and fails open
  on usage errors; overlap guard skips a second concurrent cycle; failed
  search leaves ZIP unmarked.
- **zillow client:** `MaxPages` honored; page count returned.

## Rollout

1. Merge + CI deploy (schema/seed apply automatically on startup).
2. Manual (Marko, OpenWebNinja dashboard): upgrade the plan to **Pro**. The
   current period shows exceeded until it resets 2026-08-11.
3. On the web box, edit `/opt/dwellings/.env`: remove `SEARCH_LOCATION`, set
   `SEARCH_MAX_RESULTS=0`, add `API_BUDGET_PER_CYCLE=150`; restart compose.
4. Watch the first cycles: expect the most-populous ZIPs first, thousands of
   discoveries per month, details enrichment trailing at ≤100/day, and a
   growing render queue absorbed across cycles.

Full national pass at defaults: ~100 ZIPs per cycle → ~5 months; afterwards
rotation naturally revisits stalest ZIPs. Raising the plan later is just
raising `API_BUDGET_PER_CYCLE`.

## Out of scope

- Refreshing price/status of already-stored listings (`SKIP_EXISTING=false`
  modes) — a future budget consumer.
- Removing sold/delisted properties.
- Per-state/metro video allowlists.
- Backfilling the 3 original ZIPs' remaining listings beyond the old top-10
  cap — rotation will reach them like any other ZIP.
