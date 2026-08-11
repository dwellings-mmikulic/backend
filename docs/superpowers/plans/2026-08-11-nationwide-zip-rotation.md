# Nationwide ZIP Rotation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the 3-ZIP env-var search list with a DB-backed rotation over all 29,670 US residential ZIPs, throttled by a per-cycle API request budget that fits the OpenWebNinja Pro plan (10k requests/month).

**Architecture:** A `zip_codes` table (seeded at startup from a CSV embedded in the binary) provides a rotation cursor ordered by `last_searched_at NULLS FIRST, population DESC`. Each scheduler cycle spends at most `API_BUDGET_PER_CYCLE` OpenWebNinja requests (search pages + details calls), marks ZIPs searched as it goes, and stops mid-batch when the budget runs out. A quota guard skips cycles when the provider reports exhaustion; an overlap guard prevents concurrent cycles.

**Tech Stack:** Go 1.26, pgx/v5 (pgxpool + CopyFrom), robfig/cron v3, stdlib testing with hand-rolled fakes (existing pattern — no testify in this repo).

**Spec:** `docs/superpowers/specs/2026-08-10-nationwide-zip-rotation-design.md`

## Global Constraints

- Work on a feature branch: `feat/nationwide-zip-rotation` (create from `main` before Task 1).
- Env var names exactly: `API_BUDGET_PER_CYCLE` (new, default `150`); `SEARCH_LOCATION` removed; `SEARCH_MAX_RESULTS=0` means unlimited (existing semantics, unchanged).
- The CSV is `internal/zipseed/zips.csv`, columns exactly `zip,city,state,county,population`, **29,670 data rows**, ZIPs are 5-char strings with leading zeros preserved.
- `schema.sql` must stay idempotent (`CREATE ... IF NOT EXISTS`) — it runs on every startup.
- Hard pagination cap of 20 pages per search stays.
- Tests: `go test ./...` must pass after every task; also run `go vet ./...`.
- Commit messages follow repo style: `feat:`/`fix:`/`docs:` prefix, plus the `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` trailer.
- The repo has no DB-backed tests; repository SQL is verified by convention/review and at rollout (this matches every existing repository in the codebase).

---

### Task 1: Generate the embedded ZIP CSV

**Files:**
- Create: `internal/zipseed/zips.csv`

**Interfaces:**
- Consumes: `/Users/marko/Downloads/zip_code_database (1).xls` (spreadsheet, 42,736 rows). If missing, stop and ask the user — do not substitute another source.
- Produces: `internal/zipseed/zips.csv` with header `zip,city,state,county,population` and 29,670 sorted data rows. Task 2 embeds this file.

- [ ] **Step 1: Write the conversion script** (scratchpad, not committed)

Write to `/private/tmp/claude-501/-Users-marko-Projects-Dwellings-backend/a9175da3-7bec-4210-a059-29496e81d4d9/scratchpad/make_zips_csv.py`:

```python
import csv, xlrd

XLS = "/Users/marko/Downloads/zip_code_database (1).xls"
OUT = "/Users/marko/Projects/Dwellings/backend/internal/zipseed/zips.csv"
TERRITORIES = {"PR", "VI", "GU", "AS", "MP", "FM", "MH", "PW", "AA", "AE", "AP"}

wb = xlrd.open_workbook(XLS)
sh = wb.sheet_by_index(0)
hdr = sh.row_values(0)
i = {k: hdr.index(k) for k in
     ("zip", "type", "decommissioned", "primary_city", "state", "county",
      "irs_estimated_population")}

rows = []
for r in range(1, sh.nrows):
    v = sh.row_values(r)
    if v[i["type"]] != "STANDARD" or v[i["decommissioned"]] or v[i["state"]] in TERRITORIES:
        continue
    zip5 = str(int(v[i["zip"]])).zfill(5)
    pop = int(v[i["irs_estimated_population"]] or 0)
    rows.append((zip5, v[i["primary_city"]], v[i["state"]], v[i["county"]], pop))

rows.sort()
with open(OUT, "w", newline="") as f:
    w = csv.writer(f)
    w.writerow(["zip", "city", "state", "county", "population"])
    w.writerows(rows)
print(len(rows))
```

- [ ] **Step 2: Run it**

```bash
cd /private/tmp/claude-501/-Users-marko-Projects-Dwellings-backend/a9175da3-7bec-4210-a059-29496e81d4d9/scratchpad
python3 -m venv venv 2>/dev/null; ./venv/bin/pip -q install xlrd   # venv may already exist with xlrd
mkdir -p /Users/marko/Projects/Dwellings/backend/internal/zipseed
./venv/bin/python make_zips_csv.py
```

Expected output: `29670`

- [ ] **Step 3: Validate the CSV**

```bash
cd /Users/marko/Projects/Dwellings/backend
wc -l internal/zipseed/zips.csv                      # expect 29671 (header + 29670)
head -3 internal/zipseed/zips.csv                    # header, then rows like 01001,Agawam,MA,Hampden County,16769
awk -F, 'NR>1 && length($1)!=5 {print; exit 1}' internal/zipseed/zips.csv && echo ZIPS-OK
awk -F, 'NR>1 {s[$3]=1} END {print length(s)}' internal/zipseed/zips.csv   # expect 51 (50 states + DC)
```

Expected: 29671 lines, first data row starts with a leading-zero ZIP (e.g. `01001`), `ZIPS-OK`, `51`.

- [ ] **Step 4: Commit**

```bash
git add internal/zipseed/zips.csv
git commit -m "feat: add embedded US residential ZIP database (29,670 ZIPs)

Filtered from zip_code_database.xls: type=STANDARD, not decommissioned,
50 states + DC only.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: `zipseed` package — parse + seed

**Files:**
- Create: `internal/zipseed/zipseed.go`
- Test: `internal/zipseed/zipseed_test.go`

**Interfaces:**
- Consumes: `internal/zipseed/zips.csv` (Task 1).
- Produces: `zipseed.Seed(ctx context.Context, pool *pgxpool.Pool) (int, error)` — seeds `zip_codes` if empty, returns rows inserted (0 when already seeded). Used by `cmd/server` in Task 6. Internal: `parseRows() ([]row, error)` with `type row struct { Zip, City, State, County string; Population int }`.

- [ ] **Step 1: Write the failing test**

`internal/zipseed/zipseed_test.go`:

```go
package zipseed

import "testing"

func TestParseRows(t *testing.T) {
	rows, err := parseRows()
	if err != nil {
		t.Fatalf("parseRows: %v", err)
	}
	if len(rows) != 29670 {
		t.Fatalf("got %d rows, want 29670", len(rows))
	}
	first := rows[0]
	if len(first.Zip) != 5 || first.Zip[0] != '0' {
		t.Errorf("first zip %q: want 5 chars with leading zero (CSV is sorted)", first.Zip)
	}
	states := map[string]bool{}
	for _, r := range rows {
		if len(r.Zip) != 5 {
			t.Fatalf("zip %q is not 5 chars", r.Zip)
		}
		if r.Population < 0 {
			t.Fatalf("zip %s has negative population %d", r.Zip, r.Population)
		}
		states[r.State] = true
	}
	if len(states) != 51 {
		t.Errorf("got %d states, want 51 (50 + DC)", len(states))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/zipseed/`
Expected: FAIL to build — `parseRows` undefined.

- [ ] **Step 3: Implement**

`internal/zipseed/zipseed.go`:

```go
// Package zipseed seeds the zip_codes rotation table from a CSV of all US
// residential ZIP codes embedded in the binary. Seeding only runs when the
// table is empty, so redeploys never reset rotation state.
package zipseed

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/csv"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed zips.csv
var zipsCSV []byte

type row struct {
	Zip, City, State, County string
	Population               int
}

// parseRows decodes the embedded CSV (header: zip,city,state,county,population).
func parseRows() ([]row, error) {
	r := csv.NewReader(bytes.NewReader(zipsCSV))
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read zips csv: %w", err)
	}
	if len(records) < 2 {
		return nil, fmt.Errorf("zips csv has no data rows")
	}
	out := make([]row, 0, len(records)-1)
	for _, rec := range records[1:] { // skip header
		pop, err := strconv.Atoi(rec[4])
		if err != nil {
			return nil, fmt.Errorf("zip %s: bad population %q: %w", rec[0], rec[4], err)
		}
		out = append(out, row{Zip: rec[0], City: rec[1], State: rec[2], County: rec[3], Population: pop})
	}
	return out, nil
}

// Seed inserts the embedded ZIP set into zip_codes when the table is empty.
// It returns the number of rows inserted (0 when the table was already
// seeded, so rotation state survives redeploys).
func Seed(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM zip_codes`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count zip_codes: %w", err)
	}
	if n > 0 {
		return 0, nil
	}
	rows, err := parseRows()
	if err != nil {
		return 0, err
	}
	src := make([][]any, len(rows))
	for i, r := range rows {
		src[i] = []any{r.Zip, r.City, r.State, r.County, r.Population}
	}
	inserted, err := pool.CopyFrom(ctx,
		pgx.Identifier{"zip_codes"},
		[]string{"zip", "city", "state", "county", "population"},
		pgx.CopyFromRows(src),
	)
	if err != nil {
		return 0, fmt.Errorf("copy zip_codes: %w", err)
	}
	return int(inserted), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/zipseed/ && go vet ./internal/zipseed/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/zipseed/zipseed.go internal/zipseed/zipseed_test.go
git commit -m "feat: zipseed package seeds zip_codes from embedded CSV

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Schema + `zipcode` rotation repository

**Files:**
- Modify: `internal/db/schema.sql` (append at end)
- Create: `internal/zipcode/repository.go`

**Interfaces:**
- Produces (used by scheduler in Task 6):
  - `zipcode.NewRepository(pool *pgxpool.Pool) *Repository`
  - `(*Repository) NextBatch(ctx context.Context, limit int) ([]string, error)` — ZIPs in rotation order.
  - `(*Repository) MarkSearched(ctx context.Context, zip string, listingCount int) error`
- No unit test: repository SQL is untestable without a DB, matching every existing repository in this codebase (see Global Constraints). Verified at rollout (Task 8 step 5).

- [ ] **Step 1: Append to `internal/db/schema.sql`**

```sql

-- ZIP rotation table for nationwide collection (see
-- docs/superpowers/specs/2026-08-10-nationwide-zip-rotation-design.md).
-- Seeded from an embedded CSV at startup when empty; last_searched_at is the
-- rotation cursor (NULL = never searched).
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

- [ ] **Step 2: Create `internal/zipcode/repository.go`**

```go
// Package zipcode persists the ZIP rotation state that drives nationwide
// collection: which ZIP codes to search next and when each was last searched.
package zipcode

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository reads and updates ZIP rotation state in PostgreSQL.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository creates a ZIP rotation repository backed by the given pool.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// NextBatch returns up to limit ZIP codes in rotation order: never-searched
// ZIPs first (most populous first, so dense markets are indexed early), then
// stalest-searched first.
func (r *Repository) NextBatch(ctx context.Context, limit int) ([]string, error) {
	const q = `
SELECT zip FROM zip_codes
 ORDER BY last_searched_at ASC NULLS FIRST, population DESC
 LIMIT $1`
	rows, err := r.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("next zip batch: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var zip string
		if err := rows.Scan(&zip); err != nil {
			return nil, fmt.Errorf("scan zip: %w", err)
		}
		out = append(out, zip)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate zip rows: %w", err)
	}
	return out, nil
}

// MarkSearched stamps a ZIP as searched now with the number of listings its
// search returned. Only called after a successful search, so failed ZIPs
// stay at the front of the rotation and retry next cycle.
func (r *Repository) MarkSearched(ctx context.Context, zip string, listingCount int) error {
	const q = `
UPDATE zip_codes
   SET last_searched_at = now(), last_listing_count = $2
 WHERE zip = $1`
	if _, err := r.pool.Exec(ctx, q, zip, listingCount); err != nil {
		return fmt.Errorf("mark zip searched zip=%s: %w", zip, err)
	}
	return nil
}
```

- [ ] **Step 3: Build + vet + full tests**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all pass (nothing consumes the new package yet).

- [ ] **Step 4: Commit**

```bash
git add internal/db/schema.sql internal/zipcode/repository.go
git commit -m "feat: zip_codes table and rotation repository

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: Config — add `API_BUDGET_PER_CYCLE`, make `SEARCH_LOCATION` optional

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (new)

**Interfaces:**
- Produces: `Config.APIBudgetPerCycle int` (env `API_BUDGET_PER_CYCLE`, default 150); `SearchCriteria.MaxPages int` (no env — set per-call by the scheduler; 0 = client's hard cap of 20). `SEARCH_LOCATION` no longer required (field removal happens in Task 6 with the scheduler rewrite, keeping the tree green).

- [ ] **Step 1: Write the failing test**

`internal/config/config_test.go`:

```go
package config

import "testing"

// minimalEnv sets just enough for Load to succeed, with images disabled so
// Bunny vars are not required.
func minimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ZILLOW_API_KEY", "test-key")
	t.Setenv("IMAGES_ENABLED", "false")
	t.Setenv("LOCATIONIQ_API_KEY", "")
	t.Setenv("SEARCH_LOCATION", "")
	t.Setenv("API_BUDGET_PER_CYCLE", "")
}

func TestLoad_APIBudgetDefault(t *testing.T) {
	minimalEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIBudgetPerCycle != 150 {
		t.Errorf("APIBudgetPerCycle = %d, want 150", cfg.APIBudgetPerCycle)
	}
}

func TestLoad_APIBudgetFromEnv(t *testing.T) {
	minimalEnv(t)
	t.Setenv("API_BUDGET_PER_CYCLE", "300")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIBudgetPerCycle != 300 {
		t.Errorf("APIBudgetPerCycle = %d, want 300", cfg.APIBudgetPerCycle)
	}
}

func TestLoad_SearchLocationNotRequired(t *testing.T) {
	minimalEnv(t)
	if _, err := Load(); err != nil {
		t.Fatalf("Load without SEARCH_LOCATION: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/`
Expected: FAIL — `cfg.APIBudgetPerCycle` undefined; `TestLoad_SearchLocationNotRequired` fails with "missing required environment variables: SEARCH_LOCATION".

- [ ] **Step 3: Implement**

In `internal/config/config.go`:

1. Add to `Config` (after the `DetailsPerCycle` field):

```go
	// APIBudgetPerCycle caps the total OpenWebNinja requests (search pages +
	// details calls) one collection cycle may spend, so a month of cycles
	// fits the API plan's quota. Search gets APIBudgetPerCycle -
	// DetailsPerCycle; details keeps its own cap.
	APIBudgetPerCycle int
```

2. Add to `SearchCriteria` (after `MaxResults`):

```go
	MaxPages int // per-search page cap set by the scheduler; 0 = client hard cap
```

3. In `Load()`, after the `DetailsPerCycle` line:

```go
		APIBudgetPerCycle: getenvInt("API_BUDGET_PER_CYCLE", 150),
```

4. Delete this validation block (SEARCH_LOCATION is no longer required; the `SearchLocations` field itself is removed in Task 6):

```go
	if len(c.SearchLocations) == 0 {
		missing = append(missing, "SEARCH_LOCATION")
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ && go test ./...`
Expected: PASS (scheduler still compiles — `SearchLocations` still exists, just never required).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat: add API_BUDGET_PER_CYCLE config, make SEARCH_LOCATION optional

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: Zillow client — `SearchPages` with page accounting

**Files:**
- Modify: `internal/zillow/client.go`
- Test: `internal/zillow/search_test.go` (new)

**Interfaces:**
- Produces: `(*Client) SearchPages(ctx context.Context, s config.SearchCriteria) ([]property.Property, int, error)` — the int is the number of HTTP search requests made (≥1 whenever any request was attempted, including on error). Honors `s.MaxPages` (0 → hard cap 20). The old `Search` becomes a thin wrapper (kept this task so the scheduler still compiles; deleted in Task 6).

- [ ] **Step 1: Write the failing test**

`internal/zillow/search_test.go`:

```go
package zillow

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/config"
)

// searchServer serves n pages of one listing each, then an empty page.
func searchServer(t *testing.T, pagesWithData int, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		page := r.URL.Query().Get("page")
		var p int
		fmt.Sscanf(page, "%d", &p)
		if p > pagesWithData {
			fmt.Fprint(w, `{"status":"OK","data":[]}`)
			return
		}
		fmt.Fprintf(w, `{"status":"OK","data":[{"zpid":"z%d","price":100000,"streetAddress":"s","city":"c","state":"FL","zipcode":"33950"}]}`, p)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSearchPages_MaxPagesLimitsRequests(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 100, &calls) // always has data
	c := New(srv.URL, "k", time.Second)

	props, pages, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950", MaxPages: 2})
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if pages != 2 || calls.Load() != 2 {
		t.Errorf("pages=%d calls=%d, want 2 and 2", pages, calls.Load())
	}
	if len(props) != 2 {
		t.Errorf("got %d props, want 2", len(props))
	}
}

func TestSearchPages_StopsOnEmptyPageAndCountsIt(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 1, &calls) // page 1 has data, page 2 empty
	c := New(srv.URL, "k", time.Second)

	props, pages, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"})
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if pages != 2 {
		t.Errorf("pages=%d, want 2 (data page + empty page both cost a request)", pages)
	}
	if len(props) != 1 {
		t.Errorf("got %d props, want 1", len(props))
	}
}

func TestSearchPages_ErrorStillReportsPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "k", time.Second)

	_, pages, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950"})
	if err == nil {
		t.Fatal("want error")
	}
	if pages != 1 {
		t.Errorf("pages=%d, want 1 (the failed request was still made)", pages)
	}
}

func TestSearchPages_MaxResultsStillHonored(t *testing.T) {
	var calls atomic.Int32
	srv := searchServer(t, 100, &calls)
	c := New(srv.URL, "k", time.Second)

	props, _, err := c.SearchPages(context.Background(), config.SearchCriteria{Location: "33950", MaxResults: 1})
	if err != nil {
		t.Fatalf("SearchPages: %v", err)
	}
	if len(props) != 1 {
		t.Errorf("got %d props, want 1", len(props))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/zillow/`
Expected: FAIL to build — `SearchPages` undefined.

- [ ] **Step 3: Implement**

In `internal/zillow/client.go`, replace the whole `Search` method with:

```go
// SearchPages returns properties matching the configured criteria, paging
// until MaxResults is reached, the API runs out of results, or the page cap
// is hit. The returned int is the number of HTTP search requests actually
// made — the scheduler charges them against its per-cycle API budget — and
// is reported even when an error is returned (a failed request still counted
// against the provider's quota). Price and bedroom criteria are applied
// client-side. s.MaxPages, when > 0, lowers the hard 20-page safety cap.
func (c *Client) SearchPages(ctx context.Context, s config.SearchCriteria) ([]property.Property, int, error) {
	maxPages := 20 // hard safety cap on pagination
	if s.MaxPages > 0 && s.MaxPages < maxPages {
		maxPages = s.MaxPages
	}

	var out []property.Property
	pages := 0
	for page := 1; page <= maxPages; page++ {
		raw, err := c.searchPage(ctx, s, page)
		pages++
		if err != nil {
			return nil, pages, err
		}
		if len(raw) == 0 {
			break
		}
		for i := range raw {
			p := toProperty(&raw[i])
			if !matches(&p, s) {
				continue
			}
			out = append(out, p)
			if s.MaxResults > 0 && len(out) >= s.MaxResults {
				return out, pages, nil
			}
		}
	}
	return out, pages, nil
}

// Search is SearchPages without page accounting. Deprecated: the scheduler
// uses SearchPages; this remains only until callers migrate.
func (c *Client) Search(ctx context.Context, s config.SearchCriteria) ([]property.Property, error) {
	props, _, err := c.SearchPages(ctx, s)
	return props, err
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/zillow/ && go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/zillow/client.go internal/zillow/search_test.go
git commit -m "feat: zillow SearchPages reports request count, honors MaxPages

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: Scheduler rotation + budget governor + guards, wired into main

This is the core task. It rewrites the cycle loop, updates the scheduler's interfaces and fakes, removes `Config.SearchLocations`, deletes the deprecated `zillow.Search` wrapper, and rewires `cmd/server`.

**Files:**
- Modify: `internal/scheduler/scheduler.go`
- Modify: `internal/scheduler/scheduler_test.go`
- Modify: `internal/config/config.go` (remove `SearchLocations` + `parseLocations`)
- Modify: `internal/zillow/client.go` (delete the deprecated `Search` wrapper)
- Modify: `cmd/server/main.go`

**Interfaces:**
- Consumes: `zipcode.Repository` (Task 3: `NextBatch(ctx, limit) ([]string, error)`, `MarkSearched(ctx, zip, listingCount) error`), `zillow.SearchPages` (Task 5), `zillow.Usage(ctx) (*zillow.Usage, error)` (already exists on the client), `zipseed.Seed` (Task 2), `cfg.APIBudgetPerCycle` (Task 4).
- Produces: `scheduler.New(cfg *config.Config, z zillowAPI, b uploader, repo store, zips zipSource, render Renderer, log *slog.Logger) *Scheduler` — note the new `zips` parameter, fifth position. `RunCycle` semantics: skips if already running; skips if provider quota exhausted; searches ZIPs from `zips` until the search budget (`APIBudgetPerCycle - DetailsPerCycle`, floor 0) is spent.

- [ ] **Step 1: Update the fakes and write the failing tests**

In `internal/scheduler/scheduler_test.go`:

1. Replace `fakeSearch`'s `Search` method and add `Usage` support. Replace the whole `fakeSearch` struct and its `Search` method (keep its existing `PropertyDetails` method unchanged):

```go
type fakeSearch struct {
	props []property.Property
	// byLocation, when set, returns per-location props and records each queried
	// location in order. Takes precedence over props.
	byLocation map[string][]property.Property
	queried    []string
	// searchErr, when set for a location, is returned by SearchPages for it.
	searchErr map[string]error
	// pagesFor, when set for a location, is the page count SearchPages reports
	// for it (default 1).
	pagesFor map[string]int
	// detailsErr, when set for a zpid, is returned by PropertyDetails.
	detailsErr map[string]error
	// usage is returned by Usage; usageErr takes precedence. A nil usage with
	// nil usageErr returns an "ok" report with ample remaining quota.
	usage    *zillow.Usage
	usageErr error
	// blockSearch, when non-nil, is closed-waited inside SearchPages after
	// signalling searchEntered — for overlap-guard tests.
	blockSearch   chan struct{}
	searchEntered chan struct{}
	mu            sync.Mutex
}

func (f *fakeSearch) SearchPages(_ context.Context, c config.SearchCriteria) ([]property.Property, int, error) {
	if f.searchEntered != nil {
		f.searchEntered <- struct{}{}
	}
	if f.blockSearch != nil {
		<-f.blockSearch
	}
	f.mu.Lock()
	f.queried = append(f.queried, c.Location)
	f.mu.Unlock()

	pages := 1
	if p, ok := f.pagesFor[c.Location]; ok {
		pages = p
	}
	if err := f.searchErr[c.Location]; err != nil {
		return nil, pages, err
	}
	if f.byLocation != nil {
		return f.byLocation[c.Location], pages, nil
	}
	return f.props, pages, nil
}

func (f *fakeSearch) Usage(_ context.Context) (*zillow.Usage, error) {
	if f.usageErr != nil {
		return nil, f.usageErr
	}
	if f.usage != nil {
		return f.usage, nil
	}
	u := &zillow.Usage{Status: "ok"}
	u.Quotas = []zillow.QuotaMetric{{Name: "Requests", Limit: 10000, Used: 0, Remaining: 10000}}
	return u, nil
}

func (f *fakeSearch) queriedLocations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queried...)
}
```

2. Add a `fakeZips` fake after `fakeRenderer`:

```go
// fakeZips serves a fixed queue in order, skipping already-marked ZIPs —
// mirroring the real rotation query, where marking pushes a ZIP to the back.
type fakeZips struct {
	mu     sync.Mutex
	queue  []string
	marked map[string]int // zip → listing count recorded by MarkSearched
}

func (f *fakeZips) NextBatch(_ context.Context, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, z := range f.queue {
		if _, done := f.marked[z]; done {
			continue
		}
		out = append(out, z)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeZips) MarkSearched(_ context.Context, zip string, listingCount int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.marked == nil {
		f.marked = map[string]int{}
	}
	f.marked[zip] = listingCount
	return nil
}

func (f *fakeZips) markedZips() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for k, v := range f.marked {
		out[k] = v
	}
	return out
}
```

3. Every existing test that builds a config/scheduler changes in two ways: `cfg.SearchLocations = []string{...}` is replaced by a `fakeZips{queue: []string{...}}` passed to `New` (new fifth argument, before `render`), and `cfg.APIBudgetPerCycle` must be set high enough not to interfere (use `1000`, with `cfg.DetailsPerCycle` as each test already sets it). Example — in `TestRunCycle_SkipsExisting`, where the test currently builds `cfg := &config.Config{SearchLocations: []string{"33950"}, ...}` and calls `New(cfg, search, up, store, nil, testLogger())`, it becomes:

```go
	cfg.APIBudgetPerCycle = 1000
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(cfg, search, up, store, zips, nil, testLogger())
```

Apply the same mechanical change to every `New(` call site in the test file. `TestRunCycle_SearchesEachLocation` is renamed and reworked in the next item, since location order now comes from the ZIP queue.

4. Replace `TestRunCycle_SearchesEachLocation` with these new tests:

```go
func TestRunCycle_RotatesUntilBudgetExhausted(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 3, // no details reserve → 3 search pages
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{byLocation: map[string][]property.Property{}} // every zip: 0 listings, 1 page
	zips := &fakeZips{queue: []string{"11111", "22222", "33333", "44444", "55555"}}
	store := &fakeStore{}
	s := New(cfg, search, &fakeUploader{}, store, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	marked := zips.markedZips()
	if len(marked) != 3 {
		t.Fatalf("marked %d zips, want 3 (budget): %v", len(marked), marked)
	}
	for _, z := range []string{"11111", "22222", "33333"} {
		if _, ok := marked[z]; !ok {
			t.Errorf("zip %s not marked; queue order should win", z)
		}
	}
}

func TestRunCycle_DetailsReserveShrinksSearchBudget(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 3,
		DetailsPerCycle:   2, // search budget = 3 - 2 = 1
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{byLocation: map[string][]property.Property{}}
	zips := &fakeZips{queue: []string{"11111", "22222"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(zips.markedZips()); got != 1 {
		t.Fatalf("marked %d zips, want 1 (search budget 1)", got)
	}
}

func TestRunCycle_FailedSearchNotMarkedButBudgetSpent(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 2,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{
		byLocation: map[string][]property.Property{},
		searchErr:  map[string]error{"11111": errors.New("boom")},
	}
	zips := &fakeZips{queue: []string{"11111", "22222"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	marked := zips.markedZips()
	if _, ok := marked["11111"]; ok {
		t.Error("failed zip 11111 must not be marked (retries next cycle)")
	}
	if _, ok := marked["22222"]; !ok {
		t.Error("zip 22222 should be searched with the remaining budget")
	}
}

func TestRunCycle_MarksListingCount(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 10,
		SkipExisting:      true,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{byLocation: map[string][]property.Property{
		"33950": {{ZPID: "a"}, {ZPID: "b"}},
	}}
	store := &fakeStore{existing: map[string]bool{"a": true, "b": true}}
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(cfg, search, &fakeUploader{}, store, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := zips.markedZips()["33950"]; got != 2 {
		t.Errorf("last_listing_count = %d, want 2", got)
	}
}

func TestRunCycle_SkipsWhenQuotaExceeded(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 10,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{usage: &zillow.Usage{Status: "exceeded"}}
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := search.queriedLocations(); len(got) != 0 {
		t.Errorf("searched %v, want none when quota exceeded", got)
	}
}

func TestRunCycle_SkipsWhenRemainingBelowBudget(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 150,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	u := &zillow.Usage{Status: "ok"}
	u.Quotas = []zillow.QuotaMetric{{Name: "Requests", Limit: 10000, Used: 9900, Remaining: 100}}
	search := &fakeSearch{usage: u}
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := search.queriedLocations(); len(got) != 0 {
		t.Errorf("searched %v, want none when remaining < budget", got)
	}
}

func TestRunCycle_ProceedsWhenUsageCheckFails(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 10,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{
		byLocation: map[string][]property.Property{},
		usageErr:   errors.New("usage endpoint down"),
	}
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := zips.markedZips()["33950"]; !ok {
		t.Error("cycle should proceed (fail-open) when the usage check errors")
	}
}

func TestRunCycle_SkipsOverlappingCycle(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 10,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{
		byLocation:    map[string][]property.Property{},
		blockSearch:   make(chan struct{}),
		searchEntered: make(chan struct{}, 1),
	}
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	done := make(chan struct{})
	go func() {
		_ = s.RunCycle(context.Background())
		close(done)
	}()
	<-search.searchEntered // first cycle is now mid-search

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(search.queriedLocations()); got != 0 {
		// queried is appended after the block, so at this point the first
		// cycle hasn't recorded its search yet; any entry means the second
		// cycle ran a search.
		t.Errorf("second cycle performed %d searches, want 0", got)
	}

	close(search.blockSearch)
	<-done
}
```

Add `"errors"` to the test file imports if not present (it is already imported today).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/scheduler/`
Expected: FAIL to build — `New` has the old arity, `fakeSearch` no longer satisfies the old `zillowAPI` (this is the point: the interface changes next).

- [ ] **Step 3: Rewrite the scheduler**

In `internal/scheduler/scheduler.go`:

1. Update the `zillowAPI` interface and add `zipSource`:

```go
// zillowAPI discovers properties, fetches their one-time details record, and
// reports the provider's quota state.
type zillowAPI interface {
	SearchPages(ctx context.Context, s config.SearchCriteria) ([]property.Property, int, error)
	PropertyDetails(ctx context.Context, zpid string) (*property.Details, []byte, error)
	Usage(ctx context.Context) (*zillow.Usage, error)
}

// zipSource yields ZIP codes in rotation order and records searches.
type zipSource interface {
	NextBatch(ctx context.Context, limit int) ([]string, error)
	MarkSearched(ctx context.Context, zip string, listingCount int) error
}
```

2. Add fields to `Scheduler` and update `New`:

```go
type Scheduler struct {
	cfg     *config.Config
	zillow  zillowAPI
	bunny   uploader
	repo    store
	zips    zipSource
	render  Renderer
	http    *http.Client
	log     *slog.Logger
	cron    *cron.Cron
	running atomic.Bool // overlap guard: one cycle at a time
}

// New creates a Scheduler. render may be nil when video rendering is disabled.
func New(cfg *config.Config, z zillowAPI, b uploader, repo store, zips zipSource, render Renderer, log *slog.Logger) *Scheduler {
	return &Scheduler{
		cfg:    cfg,
		zillow: z,
		bunny:  b,
		repo:   repo,
		zips:   zips,
		render: render,
		http:   &http.Client{Timeout: cfg.HTTPTimeout},
		log:    log,
		cron:   cron.New(),
	}
}
```

3. Replace `RunCycle` and `runLocation` with:

```go
// zipBatchSize is how many ZIPs are pulled from the rotation per query.
// Small enough that a budget-exhausted cycle doesn't skip far ahead of the
// cursor, large enough to avoid a query per ZIP.
const zipBatchSize = 25

// RunCycle performs one full cycle: check the provider quota, then search
// ZIPs in rotation order until the per-cycle API budget is spent, processing
// every discovered listing, then enrich details. Only one cycle runs at a
// time; a cycle that fires while another is running is skipped.
func (s *Scheduler) RunCycle(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		s.log.Warn("collection cycle still running, skipping this trigger")
		return nil
	}
	defer s.running.Store(false)

	if !s.quotaAllowsCycle(ctx) {
		return nil
	}

	start := time.Now()
	searchBudget := s.cfg.APIBudgetPerCycle - s.cfg.DetailsPerCycle
	if searchBudget < 0 {
		searchBudget = 0
	}
	s.log.Info("collection cycle started",
		"search_budget", searchBudget, "details_cap", s.cfg.DetailsPerCycle)

	var saved, skipped, failed atomic.Int64
	zipsSearched := 0
	for searchBudget > 0 && ctx.Err() == nil {
		batch, err := s.zips.NextBatch(ctx, min(zipBatchSize, searchBudget))
		if err != nil {
			s.log.Error("next zip batch failed", "error", err)
			break
		}
		if len(batch) == 0 {
			break
		}
		for _, zip := range batch {
			if searchBudget <= 0 || ctx.Err() != nil {
				break
			}
			criteria := s.cfg.Search
			criteria.Location = zip
			criteria.MaxPages = searchBudget
			props, pages, err := s.zillow.SearchPages(ctx, criteria)
			if pages < 1 {
				pages = 1 // an attempt was made; charge at least one request
			}
			searchBudget -= pages
			if err != nil {
				// Not marked searched — stays at the rotation front, retries
				// next cycle.
				s.log.Error("search failed", "zip", zip, "error", err)
				continue
			}
			s.log.Info("properties discovered", "zip", zip,
				"count", len(props), "pages", pages)
			s.processListings(ctx, props, &saved, &skipped, &failed)
			if err := s.zips.MarkSearched(ctx, zip, len(props)); err != nil {
				s.log.Error("mark zip searched failed", "zip", zip, "error", err)
			}
			zipsSearched++
		}
	}

	s.enrichDetails(ctx)

	s.log.Info("collection cycle finished",
		"zips_searched", zipsSearched, "search_budget_left", searchBudget,
		"saved", saved.Load(), "skipped", skipped.Load(), "failed", failed.Load(),
		"duration", time.Since(start).String())
	return nil
}

// quotaAllowsCycle checks the provider's usage report before spending any
// searches. It skips the cycle when the quota is exhausted or has less
// headroom than one cycle's budget — continuing would only burn 429s. A
// failed usage check proceeds (fail-open) so a flaky endpoint cannot halt
// collection.
func (s *Scheduler) quotaAllowsCycle(ctx context.Context) bool {
	u, err := s.zillow.Usage(ctx)
	if err != nil {
		s.log.Warn("zillow usage check failed, proceeding", "error", err)
		return true
	}
	if u.Status == "exceeded" {
		s.log.Warn("zillow quota exhausted, skipping cycle", "status", u.Status)
		return false
	}
	for _, q := range u.Quotas {
		if q.Name == "Requests" && q.Remaining < s.cfg.APIBudgetPerCycle {
			s.log.Warn("zillow quota below one cycle's budget, skipping cycle",
				"remaining", q.Remaining, "budget", s.cfg.APIBudgetPerCycle)
			return false
		}
	}
	return true
}

// processListings handles one ZIP's discovered listings concurrently, adding
// to the shared tallies. No errgroup context: one listing's failure must not
// cancel its siblings, so each task handles its own error and we tally with
// atomics.
func (s *Scheduler) processListings(ctx context.Context, props []property.Property, saved, skipped, failed *atomic.Int64) {
	var g errgroup.Group
	g.SetLimit(s.cfg.Concurrency.Listings)
	for i := range props {
		if ctx.Err() != nil {
			break // shutting down — stop scheduling new work
		}
		p := &props[i]
		g.Go(func() error {
			wasSkipped, err := s.processListing(ctx, p)
			switch {
			case err != nil:
				s.log.Error("listing failed", "zpid", p.ZPID, "error", err)
				failed.Add(1)
			case wasSkipped:
				skipped.Add(1)
			default:
				saved.Add(1)
			}
			return nil
		})
	}
	_ = g.Wait()
}
```

4. Add the import `"github.com/dwellingtw/backend/internal/zillow"` to scheduler.go (needed for `*zillow.Usage`). It is currently imported only for `zillow.ErrDetailsNotFound` — already present; no change needed. Verify the `min` builtin is used (Go ≥1.21 — this repo is on 1.26).

5. In `internal/config/config.go`: delete the `SearchLocations` field, its comment, the `SearchLocations: parseLocations(...)` line in `Load()`, and the entire `parseLocations` function.

6. In `internal/zillow/client.go`: delete the deprecated `Search` wrapper method (keep `SearchPages`).

7. In `cmd/server/main.go`: add imports `"github.com/dwellingtw/backend/internal/zipcode"` and `"github.com/dwellingtw/backend/internal/zipseed"`; after the `db.Migrate` block and its `log.Info("database ready")`, add:

```go
	seeded, err := zipseed.Seed(ctx, pool)
	if err != nil {
		return fmt.Errorf("seed zip codes: %w", err)
	}
	if seeded > 0 {
		log.Info("zip rotation table seeded", "zips", seeded)
	}
```

(add `"fmt"` to imports), and change the scheduler construction to:

```go
	zipRepo := zipcode.NewRepository(pool)
	sched := scheduler.New(cfg, zillowClient, bunnyClient, repo, zipRepo, rendererOrNil(renderer), log)
```

- [ ] **Step 4: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS. If any pre-existing scheduler test still references `SearchLocations` or the old `New` arity, fix it per Step 1 item 3.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/ internal/config/config.go internal/zillow/client.go cmd/server/main.go
git commit -m "feat: ZIP rotation with per-cycle API budget, quota and overlap guards

Replaces the SEARCH_LOCATION env list with the zip_codes rotation table.
Each cycle spends at most API_BUDGET_PER_CYCLE OpenWebNinja requests,
skips entirely when the provider reports quota exhaustion, and never
overlaps a still-running cycle.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 7: Documentation — `.env.example` and README

**Files:**
- Modify: `.env.example`
- Modify: `README.md`

**Interfaces:** none (docs only). Copy the wording below verbatim.

- [ ] **Step 1: Update `.env.example`**

Replace the "Search criteria" block:

```
# Search criteria
# SEARCH_LOCATION: comma-separated ZIP codes; each is searched every cycle.
SEARCH_LOCATION=33950,33948,33983
SEARCH_HOME_STATUS=FOR_SALE
SEARCH_MIN_PRICE=0
SEARCH_MAX_PRICE=0
SEARCH_MIN_BEDROOMS=0
SEARCH_MAX_RESULTS=50
```

with:

```
# Search criteria. ZIP codes come from the built-in zip_codes rotation table
# (all 29,670 US residential ZIPs, seeded automatically on first startup) —
# there is no location env var. Each cycle searches ZIPs in rotation order:
# never-searched first (most populous first), then stalest.
SEARCH_HOME_STATUS=FOR_SALE
SEARCH_MIN_PRICE=0
SEARCH_MAX_PRICE=0
SEARCH_MIN_BEDROOMS=0
# 0 = no per-ZIP result cap (the API budget below is the real limiter).
SEARCH_MAX_RESULTS=0

# Max OpenWebNinja requests one cycle may spend (search pages + details
# calls). Sized so a month of cycles fits the API plan: 150/cycle at two
# cycles/day ≈ 9,000/month against the Pro plan's 10,000.
API_BUDGET_PER_CYCLE=150
```

- [ ] **Step 2: Update `README.md`**

1. In "Running locally (Docker)", change the comment on the `cp` line from `# fill in ZILLOW_API_KEY, BUNNY_* and SEARCH_LOCATION` to `# fill in ZILLOW_API_KEY and BUNNY_*`.

2. In "Configuration", change the required-vars sentence to end at `BUNNY_CDN_BASE_URL` (drop `` `SEARCH_LOCATION` ``), and append this paragraph after the `DETAILS_PER_CYCLE` paragraph:

```markdown
`API_BUDGET_PER_CYCLE` (default `150`) caps the total OpenWebNinja requests
one cycle may spend — search pages plus details calls. ZIP codes are not
configured: a built-in table of all 29,670 US residential ZIPs is seeded on
first startup, and each cycle works through it in rotation order
(never-searched first, most populous first, then stalest), stopping when the
budget is spent and resuming from the cursor next cycle. A cycle is skipped
entirely when the provider reports the monthly quota is exhausted, or when a
previous cycle is still running.
```

3. In "OpenWebNinja Zillow API", replace the bullet

```markdown
- `Search` pages until `SEARCH_MAX_RESULTS` is reached (hard cap 20 pages).
  Price and bedroom criteria are applied client-side.
```

with:

```markdown
- `SearchPages` pages until `SEARCH_MAX_RESULTS` is reached (`0` = uncapped)
  or the per-cycle API budget runs out (hard cap 20 pages per ZIP). Price and
  bedroom criteria are applied client-side.
```

- [ ] **Step 3: Verify and commit**

Run: `go test ./...` (nothing should have broken; docs only)

```bash
git add .env.example README.md
git commit -m "docs: nationwide ZIP rotation configuration

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 8: Merge + production rollout

**Files:** none (branch merge + server-side `.env` edit)

This task is gated on two things outside the repo: review/approval of the branch, and the OpenWebNinja plan upgrade. Check with Marko before executing.

- [ ] **Step 1: Verify branch is green and merge**

```bash
cd /Users/marko/Projects/Dwellings/backend
go test ./... && go vet ./...
git checkout main && git merge --no-ff feat/nationwide-zip-rotation -m "Merge feat/nationwide-zip-rotation: nationwide ZIP rotation on an API budget

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
git push   # NOTE: default SSH key lacks access — push with ~/.ssh/dwellings_tv or gh creds (see memory: git-push-identity)
```

Pushing `main` triggers the GHCR build + deploy workflow (existing CI/CD).

- [ ] **Step 2: Confirm the OpenWebNinja plan is Pro** (manual — Marko)

The dashboard upgrade to Pro ($25/mo, 10k requests) must be done before the new code can collect anything; the free quota is already exceeded. Verify from the web box:

```bash
ssh -i ~/.ssh/dwellings_tv root@87.99.154.101 'KEY=$(grep -E "^ZILLOW_API_KEY=" /opt/dwellings/.env | cut -d= -f2-); curl -s -H "X-API-Key: $KEY" "https://api.openwebninja.com/usage?api_id=realtime_zillow_data"'
```

Expected: `"plan":{"key":"pro"...}` (or at minimum `"status"` not `"exceeded"`).

- [ ] **Step 3: Update production env**

On the web box, edit `/opt/dwellings/.env`:
- Delete the `SEARCH_LOCATION=...` line.
- Set `SEARCH_MAX_RESULTS=0`.
- Add `API_BUDGET_PER_CYCLE=150`.

Then restart: `cd /opt/dwellings && docker compose -f compose.prod.yml up -d` (after CI has pushed the new image; `docker compose pull` first if needed).

- [ ] **Step 4: Watch the first cycle**

```bash
ssh -i ~/.ssh/dwellings_tv root@87.99.154.101 'docker logs --since 10m dwellings-app-1 2>&1 | grep -E "zip rotation table seeded|collection cycle|properties discovered" | head -30'
```

Expected: `zip rotation table seeded zips=29670` (first boot only), `collection cycle started search_budget=100 details_cap=50`, then `properties discovered` lines for high-population ZIPs, and eventually `collection cycle finished zips_searched=...`.

- [ ] **Step 5: Verify rotation state in the DB**

```bash
ssh -i ~/.ssh/dwellings_tv root@87.99.154.101 'DBURL=$(grep -E "^DATABASE_URL=" /opt/dwellings/.env | cut -d= -f2-); docker run --rm --network host postgres:16-alpine psql "$DBURL" -c "SELECT count(*) AS total, count(last_searched_at) AS searched FROM zip_codes; SELECT zip, city, state, population, last_listing_count FROM zip_codes WHERE last_searched_at IS NOT NULL ORDER BY last_searched_at DESC LIMIT 5;"'
```

Expected: `total = 29670`, `searched` > 0 and growing each cycle, searched rows are high-population ZIPs.

---

## Self-review notes

- Spec coverage: table+seed (Tasks 1–3), rotation cursor (Tasks 3, 6), budget governor (Tasks 4–6), quota guard (Task 6), overlap guard (Task 6), config summary (Tasks 4, 6, 7), rollout (Task 8). The spec's "repository tests for the rotation query" are downgraded to rollout verification (Task 8 step 5) because the codebase has no DB-backed test infrastructure — matching every existing repository; the spec's scheduler/zipseed/client tests are all present.
- Type consistency: `SearchPages(ctx, criteria) ([]property.Property, int, error)` defined in Task 5, consumed in Task 6; `NextBatch`/`MarkSearched` defined in Task 3, faked and consumed in Task 6; `Seed(ctx, pool) (int, error)` defined in Task 2, called in Task 6; `New` arity change is applied to all call sites in Task 6.
