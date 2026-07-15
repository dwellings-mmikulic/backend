# Public Listings API + Details Enrichment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Two public JSON endpoints — `GET /api/v1/properties` (filterable, cursor-paginated browse) and `GET /api/v1/properties/{zpid}` (full detail) — served from PostgreSQL, plus a once-per-property OpenWebNinja details-enrichment step in the scheduler.

**Architecture:** New `internal/api` package with handlers/DTOs/cursor codec mounted on the existing `internal/server` mux. `internal/property` gains a `Details` struct (embedded in `Property`), a keyset-pagination query builder, and `List`/`GetByZPID`/`SetDetails`/`ListZPIDsMissingDetails` repository methods. `internal/zillow` gains a `PropertyDetails` client method; `internal/scheduler` runs enrichment after ingest, capped per cycle.

**Tech Stack:** Go stdlib `net/http` (Go 1.22 method+pattern mux, already in use), pgx/v5, no new dependencies.

**Spec:** `docs/superpowers/specs/2026-07-14-public-listings-api-design.md`

## Global Constraints

- No new Go module dependencies. Stdlib mux only (`GET /path/{param}` patterns + `r.PathValue`, as in `internal/server/server.go`).
- JSON response fields are snake_case; missing/unknown values are `null` (Go pointer fields), never omitted keys.
- Errors wrap with `fmt.Errorf("context: %w", err)`; logging via the injected `*slog.Logger` (never `log` or `fmt.Print`).
- Tests use stdlib `testing` only (no testify) — match existing test style.
- SQL is always parameterized; ORDER BY comes only from the fixed `orderBy` map (never from user input); `LIMIT` is rendered from a validated int.
- Schema changes go in `internal/db/schema.sql` as idempotent `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS` (file runs on every startup).
- Public endpoints: no auth, `Access-Control-Allow-Origin: *`, `Cache-Control: public, max-age=300`, `Content-Type: application/json` on every response including errors.
- Commit messages: conventional prefix (`feat:`/`fix:`/`docs:`/`test:`) and end with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- After every implementation step: `go build ./...` must pass.

---

### Task 1: Schema + domain model enrichment fields

**Files:**
- Modify: `internal/db/schema.sql`
- Modify: `internal/property/property.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `property.Details` struct (pointer fields, embedded in `property.Property`), `Property.DetailsFetchedAt *time.Time`. Later tasks reference promoted fields like `p.PropertyType`, `p.AgentName`.

- [ ] **Step 1: Add enrichment columns + indexes to `internal/db/schema.sql`**

Append at the end of the file:

```sql
-- Enrichment columns: fetched once per property from the Zillow
-- property-details API, then served from the DB forever (see
-- docs/superpowers/specs/2026-07-14-public-listings-api-design.md).
ALTER TABLE properties ADD COLUMN IF NOT EXISTS property_type      TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS description        TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS year_built         INTEGER;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS heating            TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS cooling            TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS garage             TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS hoa_fee_monthly    INTEGER;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS mls_number         TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS listing_status     TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS agent_name         TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS agent_phone        TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS agent_brokerage    TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS latitude           DOUBLE PRECISION;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS longitude          DOUBLE PRECISION;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS details_raw        JSONB;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS details_fetched_at TIMESTAMPTZ;

-- Public listing API filter/sort indexes.
CREATE INDEX IF NOT EXISTS idx_properties_zip           ON properties (zip);
CREATE INDEX IF NOT EXISTS idx_properties_property_type ON properties (property_type);
CREATE INDEX IF NOT EXISTS idx_properties_created_at    ON properties (created_at DESC, id DESC);
```

- [ ] **Step 2: Add `Details` to `internal/property/property.go`**

Add below the `VideoStatus` block, and embed in `Property`:

```go
// Details holds the enrichment fields fetched once per property from the
// Zillow property-details API. All fields are pointers: nil means unknown
// (not yet enriched, or absent from the API response).
type Details struct {
	PropertyType   *string
	Description    *string
	YearBuilt      *int
	Heating        *string
	Cooling        *string
	Garage         *string
	HOAFeeMonthly  *int
	MLSNumber      *string
	ListingStatus  *string
	AgentName      *string
	AgentPhone     *string
	AgentBrokerage *string
	Latitude       *float64
	Longitude      *float64
}
```

In the `Property` struct, after the `ImageURLs` field add:

```go
	Details
	DetailsFetchedAt *time.Time // nil = enrichment not yet attempted/succeeded
```

- [ ] **Step 3: Verify it builds and existing tests pass**

Run: `go build ./... && go test ./...`
Expected: PASS (no behavior change yet).

- [ ] **Step 4: Commit**

```bash
git add internal/db/schema.sql internal/property/property.go
git commit -m "feat: add property enrichment columns and Details domain model

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: Filter, keyset query builder, and repository methods

**Files:**
- Create: `internal/property/query.go`
- Create: `internal/property/query_test.go`
- Modify: `internal/property/repository.go`

**Interfaces:**
- Consumes: `property.Details`, `Property.DetailsFetchedAt` from Task 1.
- Produces (used by Tasks 4 and 6):
  - `type Sort string` with `SortNewest`, `SortPriceAsc`, `SortPriceDesc`
  - `type PageKey struct { CreatedAt time.Time; Price int64; ID int64 }`
  - `type Filter struct { Zip, City, State string; MinPrice, MaxPrice int64; PropertyType string; MinBeds int; MinBaths float64; MinSqft, MaxSqft int; Sort Sort; Limit int; After *PageKey }`
  - `var ErrNotFound = errors.New("property not found")`
  - `func (r *Repository) List(ctx context.Context, f Filter) ([]Property, int, bool, error)` — returns (page rows, total matching filter, hasMore, err)
  - `func (r *Repository) GetByZPID(ctx context.Context, zpid string) (*Property, error)`
  - `func (r *Repository) ListZPIDsMissingDetails(ctx context.Context, limit int) ([]string, error)`
  - `func (r *Repository) SetDetails(ctx context.Context, zpid string, d *Details, raw []byte) error`

- [ ] **Step 1: Write the failing query-builder tests**

Create `internal/property/query_test.go`:

```go
package property

import (
	"strings"
	"testing"
	"time"
)

func TestBuildListQuery_NoFilters(t *testing.T) {
	q, args := buildListQuery(Filter{Sort: SortNewest, Limit: 24})
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
	if !strings.Contains(q, "ORDER BY created_at DESC, id DESC") {
		t.Errorf("missing newest ordering: %s", q)
	}
	if !strings.Contains(q, "LIMIT 25") { // limit+1 to detect next page
		t.Errorf("want LIMIT 25 (limit+1): %s", q)
	}
	if strings.Contains(q, "WHERE") {
		t.Errorf("unexpected WHERE with no filters: %s", q)
	}
}

func TestBuildListQuery_AllFilters(t *testing.T) {
	f := Filter{
		Zip: "78746", City: "Austin", State: "TX",
		MinPrice: 100000, MaxPrice: 2000000, PropertyType: "SINGLE_FAMILY",
		MinBeds: 4, MinBaths: 2.5, MinSqft: 1000, MaxSqft: 5000,
		Sort: SortNewest, Limit: 24,
	}
	q, args := buildListQuery(f)
	want := []string{
		"zip = $1", "lower(city) = lower($2)", "lower(state) = lower($3)",
		"sale_price >= $4", "sale_price <= $5", "property_type = $6",
		"bedrooms >= $7", "bathrooms >= $8",
		"home_size_sqft >= $9", "home_size_sqft <= $10",
	}
	for _, w := range want {
		if !strings.Contains(q, w) {
			t.Errorf("query missing %q: %s", w, q)
		}
	}
	if len(args) != 10 {
		t.Errorf("args len = %d, want 10: %v", len(args), args)
	}
}

func TestBuildListQuery_KeysetNewest(t *testing.T) {
	ts := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	f := Filter{Sort: SortNewest, Limit: 24, After: &PageKey{CreatedAt: ts, ID: 42}}
	q, args := buildListQuery(f)
	if !strings.Contains(q, "(created_at, id) < ($1, $2)") {
		t.Errorf("missing newest keyset predicate: %s", q)
	}
	if len(args) != 2 || args[0] != ts || args[1] != int64(42) {
		t.Errorf("keyset args wrong: %v", args)
	}
}

func TestBuildListQuery_KeysetPriceAsc(t *testing.T) {
	f := Filter{Sort: SortPriceAsc, Limit: 24, After: &PageKey{Price: 500000, ID: 7}}
	q, args := buildListQuery(f)
	if !strings.Contains(q, "(sale_price, id) > ($1, $2)") {
		t.Errorf("missing price_asc keyset predicate: %s", q)
	}
	if !strings.Contains(q, "ORDER BY sale_price ASC, id ASC") {
		t.Errorf("missing price_asc ordering: %s", q)
	}
	if len(args) != 2 || args[0] != int64(500000) || args[1] != int64(7) {
		t.Errorf("keyset args wrong: %v", args)
	}
}

func TestBuildListQuery_KeysetPriceDesc(t *testing.T) {
	f := Filter{Sort: SortPriceDesc, Limit: 24, After: &PageKey{Price: 500000, ID: 7}}
	q, _ := buildListQuery(f)
	if !strings.Contains(q, "(sale_price, id) < ($1, $2)") {
		t.Errorf("missing price_desc keyset predicate: %s", q)
	}
	if !strings.Contains(q, "ORDER BY sale_price DESC, id DESC") {
		t.Errorf("missing price_desc ordering: %s", q)
	}
}

func TestBuildListQuery_FiltersAndKeysetCombined(t *testing.T) {
	f := Filter{Zip: "78746", Sort: SortPriceAsc, Limit: 10, After: &PageKey{Price: 1, ID: 2}}
	q, args := buildListQuery(f)
	if !strings.Contains(q, "zip = $1") || !strings.Contains(q, "(sale_price, id) > ($2, $3)") {
		t.Errorf("filter+keyset placeholders wrong: %s", q)
	}
	if len(args) != 3 {
		t.Errorf("args len = %d, want 3", len(args))
	}
}

func TestBuildListQuery_UnknownSortFallsBackToNewest(t *testing.T) {
	q, _ := buildListQuery(Filter{Sort: Sort("evil; DROP TABLE"), Limit: 5})
	if !strings.Contains(q, "ORDER BY created_at DESC, id DESC") {
		t.Errorf("unknown sort must fall back to newest ordering: %s", q)
	}
	if strings.Contains(q, "DROP TABLE") {
		t.Fatalf("sort input leaked into SQL: %s", q)
	}
}

func TestBuildCountQuery_IgnoresKeysetAndLimit(t *testing.T) {
	f := Filter{City: "Austin", Sort: SortNewest, Limit: 24, After: &PageKey{ID: 9, CreatedAt: time.Now()}}
	q, args := buildCountQuery(f)
	if !strings.HasPrefix(q, "SELECT COUNT(*) FROM properties") {
		t.Errorf("not a count query: %s", q)
	}
	if strings.Contains(q, "created_at, id") || strings.Contains(q, "LIMIT") || strings.Contains(q, "ORDER BY") {
		t.Errorf("count query must not contain keyset/order/limit: %s", q)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want just city", args)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/property/ -run TestBuild -v`
Expected: FAIL — `undefined: buildListQuery` (compile error).

- [ ] **Step 3: Implement the query builder**

Create `internal/property/query.go`:

```go
package property

import (
	"fmt"
	"strings"
	"time"
)

// Sort orders for public listing queries.
type Sort string

const (
	SortNewest    Sort = "newest"
	SortPriceAsc  Sort = "price_asc"
	SortPriceDesc Sort = "price_desc"
)

// orderBy is the only source of ORDER BY clauses — user input never reaches
// the SQL directly. The secondary id column makes every ordering total, which
// keyset pagination requires.
var orderBy = map[Sort]string{
	SortNewest:    "created_at DESC, id DESC",
	SortPriceAsc:  "sale_price ASC, id ASC",
	SortPriceDesc: "sale_price DESC, id DESC",
}

// PageKey is the keyset position of the last row of the previous page.
type PageKey struct {
	CreatedAt time.Time // used by SortNewest
	Price     int64     // used by the price sorts
	ID        int64     // tiebreaker, always set
}

// Filter selects and orders properties for the public listing API. Zero
// values mean "no constraint".
type Filter struct {
	Zip          string
	City         string
	State        string
	MinPrice     int64
	MaxPrice     int64
	PropertyType string
	MinBeds      int
	MinBaths     float64
	MinSqft      int
	MaxSqft      int
	Sort         Sort
	Limit        int
	After        *PageKey
}

// listColumns are the columns the browse endpoint needs; id and created_at
// feed the next-page cursor. COALESCE guards pre-enrichment NULLs on columns
// that scan into non-pointer Go fields.
const listColumns = `id, zpid, COALESCE(sale_price,0), address, COALESCE(city,''),
       COALESCE(state,''), COALESCE(zip,''), COALESCE(bedrooms,0),
       COALESCE(bathrooms,0), COALESCE(home_size_sqft,0), property_type,
       image_urls, created_at`

// buildWhere renders the filter (and optionally the keyset predicate) as a
// WHERE clause with 1-based positional args.
func buildWhere(f Filter, includeKeyset bool) (string, []any) {
	var conds []string
	var args []any
	add := func(format string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(format, len(args)))
	}

	if f.Zip != "" {
		add("zip = $%d", f.Zip)
	}
	if f.City != "" {
		add("lower(city) = lower($%d)", f.City)
	}
	if f.State != "" {
		add("lower(state) = lower($%d)", f.State)
	}
	if f.MinPrice > 0 {
		add("sale_price >= $%d", f.MinPrice)
	}
	if f.MaxPrice > 0 {
		add("sale_price <= $%d", f.MaxPrice)
	}
	if f.PropertyType != "" {
		add("property_type = $%d", f.PropertyType)
	}
	if f.MinBeds > 0 {
		add("bedrooms >= $%d", f.MinBeds)
	}
	if f.MinBaths > 0 {
		add("bathrooms >= $%d", f.MinBaths)
	}
	if f.MinSqft > 0 {
		add("home_size_sqft >= $%d", f.MinSqft)
	}
	if f.MaxSqft > 0 {
		add("home_size_sqft <= $%d", f.MaxSqft)
	}

	if includeKeyset && f.After != nil {
		k := f.After
		switch f.Sort {
		case SortPriceAsc:
			args = append(args, k.Price, k.ID)
			conds = append(conds, fmt.Sprintf("(sale_price, id) > ($%d, $%d)", len(args)-1, len(args)))
		case SortPriceDesc:
			args = append(args, k.Price, k.ID)
			conds = append(conds, fmt.Sprintf("(sale_price, id) < ($%d, $%d)", len(args)-1, len(args)))
		default: // SortNewest
			args = append(args, k.CreatedAt, k.ID)
			conds = append(conds, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
		}
	}

	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// buildListQuery renders the page SELECT. It asks for Limit+1 rows so the
// caller can detect whether a next page exists without a second query.
func buildListQuery(f Filter) (string, []any) {
	sort := f.Sort
	if _, ok := orderBy[sort]; !ok {
		sort = SortNewest
	}
	where, args := buildWhere(f, true)
	q := "SELECT " + listColumns + " FROM properties" + where +
		" ORDER BY " + orderBy[sort] +
		fmt.Sprintf(" LIMIT %d", f.Limit+1)
	return q, args
}

// buildCountQuery renders the total count for the same filter, without the
// keyset predicate (the total is page-independent).
func buildCountQuery(f Filter) (string, []any) {
	where, args := buildWhere(f, false)
	return "SELECT COUNT(*) FROM properties" + where, args
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/property/ -v`
Expected: PASS (all `TestBuild*`).

- [ ] **Step 5: Add repository methods**

Append to `internal/property/repository.go` (add `"errors"` and `"github.com/jackc/pgx/v5"` to imports):

```go
// ErrNotFound is returned when a requested property does not exist.
var ErrNotFound = errors.New("property not found")

// List returns one page of properties matching f, the total number of rows
// matching the filter (ignoring pagination), and whether another page exists.
func (r *Repository) List(ctx context.Context, f Filter) ([]Property, int, bool, error) {
	countQ, countArgs := buildCountQuery(f)
	var total int
	if err := r.pool.QueryRow(ctx, countQ, countArgs...).Scan(&total); err != nil {
		return nil, 0, false, fmt.Errorf("count properties: %w", err)
	}

	q, args := buildListQuery(f)
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, false, fmt.Errorf("list properties: %w", err)
	}
	defer rows.Close()

	var out []Property
	for rows.Next() {
		var p Property
		if err := rows.Scan(
			&p.ID, &p.ZPID, &p.SalePrice, &p.Address, &p.City, &p.State, &p.Zip,
			&p.Bedrooms, &p.Bathrooms, &p.HomeSizeSqft, &p.PropertyType,
			&p.ImageURLs, &p.CreatedAt,
		); err != nil {
			return nil, 0, false, fmt.Errorf("scan property row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("iterate property rows: %w", err)
	}

	hasMore := false
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
		hasMore = true
	}
	return out, total, hasMore, nil
}

// GetByZPID returns the full property record, or ErrNotFound.
func (r *Repository) GetByZPID(ctx context.Context, zpid string) (*Property, error) {
	const q = `
SELECT id, zpid, COALESCE(sale_price,0), address, COALESCE(city,''),
       COALESCE(state,''), COALESCE(zip,''), COALESCE(bedrooms,0),
       COALESCE(bathrooms,0), COALESCE(home_size_sqft,0),
       COALESCE(lot_size_sqft,0), COALESCE(detail_url,''), image_urls,
       COALESCE(video_url,''),
       property_type, description, year_built, heating, cooling, garage,
       hoa_fee_monthly, mls_number, listing_status,
       agent_name, agent_phone, agent_brokerage, latitude, longitude,
       details_fetched_at, created_at, updated_at
  FROM properties WHERE zpid = $1`

	var p Property
	err := r.pool.QueryRow(ctx, q, zpid).Scan(
		&p.ID, &p.ZPID, &p.SalePrice, &p.Address, &p.City, &p.State, &p.Zip,
		&p.Bedrooms, &p.Bathrooms, &p.HomeSizeSqft,
		&p.LotSizeSqft, &p.DetailURL, &p.ImageURLs,
		&p.VideoURL,
		&p.PropertyType, &p.Description, &p.YearBuilt, &p.Heating, &p.Cooling, &p.Garage,
		&p.HOAFeeMonthly, &p.MLSNumber, &p.ListingStatus,
		&p.AgentName, &p.AgentPhone, &p.AgentBrokerage, &p.Latitude, &p.Longitude,
		&p.DetailsFetchedAt, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get property zpid=%s: %w", zpid, err)
	}
	return &p, nil
}

// ListZPIDsMissingDetails returns up to limit zpids that have never been
// enriched, oldest first (so backfill drains deterministically).
func (r *Repository) ListZPIDsMissingDetails(ctx context.Context, limit int) ([]string, error) {
	const q = `
SELECT zpid FROM properties
 WHERE details_fetched_at IS NULL
 ORDER BY created_at ASC
 LIMIT $1`
	rows, err := r.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list zpids missing details: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var zpid string
		if err := rows.Scan(&zpid); err != nil {
			return nil, fmt.Errorf("scan zpid: %w", err)
		}
		out = append(out, zpid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate zpid rows: %w", err)
	}
	return out, nil
}

// SetDetails stores the enrichment fields and raw API response, and stamps
// details_fetched_at so the row is never enriched again. raw may be nil
// (e.g. a definitive not-found still marks the row as fetched).
func (r *Repository) SetDetails(ctx context.Context, zpid string, d *Details, raw []byte) error {
	const q = `
UPDATE properties SET
    property_type = $2, description = $3, year_built = $4, heating = $5,
    cooling = $6, garage = $7, hoa_fee_monthly = $8, mls_number = $9,
    listing_status = $10, agent_name = $11, agent_phone = $12,
    agent_brokerage = $13, latitude = $14, longitude = $15,
    details_raw = $16, details_fetched_at = now(), updated_at = now()
 WHERE zpid = $1`
	_, err := r.pool.Exec(ctx, q, zpid,
		d.PropertyType, d.Description, d.YearBuilt, d.Heating,
		d.Cooling, d.Garage, d.HOAFeeMonthly, d.MLSNumber,
		d.ListingStatus, d.AgentName, d.AgentPhone,
		d.AgentBrokerage, d.Latitude, d.Longitude, raw,
	)
	if err != nil {
		return fmt.Errorf("set details zpid=%s: %w", zpid, err)
	}
	return nil
}
```

- [ ] **Step 6: Build and run all tests**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/property/query.go internal/property/query_test.go internal/property/repository.go
git commit -m "feat: add property Filter, keyset query builder, and listing repository methods

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Cursor codec

**Files:**
- Create: `internal/api/cursor.go`
- Create: `internal/api/cursor_test.go`

**Interfaces:**
- Consumes: nothing outside stdlib.
- Produces (used by Task 4):
  - `type cursor struct { Sort string; ID int64; Price int64; CreatedAt *time.Time }` (JSON tags `s`, `id`, `p`, `t`)
  - `func encodeCursor(c cursor) string`
  - `func decodeCursor(token, wantSort string) (*cursor, error)` — returns `errInvalidCursor` on any problem
  - `var errInvalidCursor = errors.New("invalid cursor")`

- [ ] **Step 1: Write the failing tests**

Create `internal/api/cursor_test.go`:

```go
package api

import (
	"testing"
	"time"
)

func TestCursorRoundTrip_Newest(t *testing.T) {
	ts := time.Date(2026, 7, 1, 12, 30, 0, 0, time.UTC)
	tok := encodeCursor(cursor{Sort: "newest", ID: 42, CreatedAt: &ts})

	got, err := decodeCursor(tok, "newest")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 42 || got.CreatedAt == nil || !got.CreatedAt.Equal(ts) {
		t.Errorf("round trip lost data: %+v", got)
	}
}

func TestCursorRoundTrip_Price(t *testing.T) {
	tok := encodeCursor(cursor{Sort: "price_asc", ID: 7, Price: 500000})

	got, err := decodeCursor(tok, "price_asc")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 7 || got.Price != 500000 {
		t.Errorf("round trip lost data: %+v", got)
	}
}

func TestDecodeCursor_RejectsGarbage(t *testing.T) {
	for _, tok := range []string{"not base64 !!!", "aGVsbG8", ""} {
		if _, err := decodeCursor(tok, "newest"); err == nil {
			t.Errorf("token %q: want error, got nil", tok)
		}
	}
}

func TestDecodeCursor_RejectsSortMismatch(t *testing.T) {
	tok := encodeCursor(cursor{Sort: "price_asc", ID: 7, Price: 1})
	if _, err := decodeCursor(tok, "newest"); err == nil {
		t.Error("cursor from a different sort must be rejected")
	}
}

func TestDecodeCursor_RejectsNewestWithoutTimestamp(t *testing.T) {
	tok := encodeCursor(cursor{Sort: "newest", ID: 7}) // no CreatedAt
	if _, err := decodeCursor(tok, "newest"); err == nil {
		t.Error("newest cursor without timestamp must be rejected")
	}
}

func TestDecodeCursor_RejectsZeroID(t *testing.T) {
	ts := time.Now()
	tok := encodeCursor(cursor{Sort: "newest", CreatedAt: &ts}) // ID 0
	if _, err := decodeCursor(tok, "newest"); err == nil {
		t.Error("cursor without id must be rejected")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -v`
Expected: FAIL — `undefined: encodeCursor` (compile error; the package doesn't exist yet, which is fine).

- [ ] **Step 3: Implement the codec**

Create `internal/api/cursor.go`:

```go
// Package api serves the public read-only listings API.
package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// errInvalidCursor is returned for any undecodable, mismatched, or
// incomplete pagination token. Its text is the public 400 message.
var errInvalidCursor = errors.New("invalid cursor")

// cursor is the decoded keyset pagination token: the sort it belongs to and
// the sort-key values of the last row of the previous page.
type cursor struct {
	Sort      string     `json:"s"`
	ID        int64      `json:"id"`
	Price     int64      `json:"p,omitempty"`
	CreatedAt *time.Time `json:"t,omitempty"`
}

// encodeCursor renders the cursor as an opaque URL-safe token.
func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c) // struct of scalars — cannot fail
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses a token and validates it against the request's sort.
func decodeCursor(token, wantSort string) (*cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, errInvalidCursor
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errInvalidCursor
	}
	if c.Sort != wantSort || c.ID == 0 {
		return nil, errInvalidCursor
	}
	if c.Sort == "newest" && c.CreatedAt == nil {
		return nil, errInvalidCursor
	}
	return &c, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/cursor.go internal/api/cursor_test.go
git commit -m "feat: add opaque keyset cursor codec for the public API

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: API handlers, DTOs, and param parsing

**Files:**
- Create: `internal/api/dto.go`
- Create: `internal/api/params.go`
- Create: `internal/api/api.go`
- Create: `internal/api/api_test.go`

**Interfaces:**
- Consumes: Task 2's `property.Filter`/`Sort*`/`PageKey`/`ErrNotFound` and repository signatures; Task 3's cursor codec.
- Produces (used by Task 7):
  - `type Repo interface { List(ctx context.Context, f property.Filter) ([]property.Property, int, bool, error); GetByZPID(ctx context.Context, zpid string) (*property.Property, error) }`
  - `func New(repo Repo, log *slog.Logger) *API`
  - `func (a *API) Register(mux *http.ServeMux)` — registers `GET /api/v1/properties` and `GET /api/v1/properties/{zpid}`

- [ ] **Step 1: Write the failing handler tests**

Create `internal/api/api_test.go`:

```go
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/property"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRepo returns canned data and records the filter it was called with.
type fakeRepo struct {
	props     []property.Property
	total     int
	hasMore   bool
	listErr   error
	detail    *property.Property
	detailErr error
	gotFilter property.Filter
	gotZPID   string
}

func (f *fakeRepo) List(_ context.Context, flt property.Filter) ([]property.Property, int, bool, error) {
	f.gotFilter = flt
	return f.props, f.total, f.hasMore, f.listErr
}

func (f *fakeRepo) GetByZPID(_ context.Context, zpid string) (*property.Property, error) {
	f.gotZPID = zpid
	if f.detailErr != nil {
		return nil, f.detailErr
	}
	return f.detail, nil
}

func strp(s string) *string    { return &s }
func intp(n int) *int          { return &n }
func f64p(f float64) *float64  { return &f }

func sampleProp(zpid string, id int64) property.Property {
	return property.Property{
		ID: id, ZPID: zpid, SalePrice: 1850000,
		Address: "1234 Hilltop Drive", City: "Austin", State: "TX", Zip: "78746",
		Bedrooms: 4, Bathrooms: 3.5, HomeSizeSqft: 3200, LotSizeSqft: 12197,
		ImageURLs: []string{"https://cdn.example/0.jpg", "https://cdn.example/1.jpg"},
		DetailURL: "https://www.zillow.com/homedetails/x",
		CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC),
	}
}

func serve(t *testing.T, repo Repo, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	New(repo, testLogger()).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestList_DefaultsAndMapping(t *testing.T) {
	repo := &fakeRepo{props: []property.Property{sampleProp("Z1", 1)}, total: 312}
	rec := serve(t, repo, "/api/v1/properties")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if repo.gotFilter.Limit != 24 || repo.gotFilter.Sort != property.SortNewest {
		t.Errorf("defaults wrong: %+v", repo.gotFilter)
	}

	var resp struct {
		Total      int              `json:"total"`
		NextCursor *string          `json:"next_cursor"`
		Results    []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 312 || len(resp.Results) != 1 {
		t.Fatalf("total/results wrong: %+v", resp)
	}
	if resp.NextCursor != nil {
		t.Error("next_cursor must be null when hasMore=false")
	}
	r := resp.Results[0]
	if r["zpid"] != "Z1" || r["price"] != float64(1850000) || r["image_url"] != "https://cdn.example/0.jpg" {
		t.Errorf("item mapping wrong: %v", r)
	}
	if _, ok := r["property_type"]; !ok || r["property_type"] != nil {
		t.Errorf("property_type must be present and null pre-enrichment: %v", r)
	}
}

func TestList_FiltersParsed(t *testing.T) {
	repo := &fakeRepo{}
	rec := serve(t, repo, "/api/v1/properties?zip=78746&city=Austin&state=TX"+
		"&min_price=100000&max_price=2000000&property_type=SINGLE_FAMILY"+
		"&min_beds=4&min_baths=2.5&min_sqft=1000&max_sqft=5000&sort=price_asc&limit=50")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	f := repo.gotFilter
	if f.Zip != "78746" || f.City != "Austin" || f.State != "TX" ||
		f.MinPrice != 100000 || f.MaxPrice != 2000000 || f.PropertyType != "SINGLE_FAMILY" ||
		f.MinBeds != 4 || f.MinBaths != 2.5 || f.MinSqft != 1000 || f.MaxSqft != 5000 ||
		f.Sort != property.SortPriceAsc || f.Limit != 50 {
		t.Errorf("filter parsed wrong: %+v", f)
	}
}

func TestList_NextCursorRoundTrips(t *testing.T) {
	repo := &fakeRepo{props: []property.Property{sampleProp("Z1", 1), sampleProp("Z2", 2)}, total: 99, hasMore: true}
	rec := serve(t, repo, "/api/v1/properties?sort=price_desc")

	var resp struct {
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.NextCursor == nil {
		t.Fatal("next_cursor missing with hasMore=true")
	}

	// Feed the cursor back: the repo must receive the last row's keyset.
	repo2 := &fakeRepo{}
	rec2 := serve(t, repo2, "/api/v1/properties?sort=price_desc&cursor="+*resp.NextCursor)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec2.Code, rec2.Body)
	}
	if repo2.gotFilter.After == nil || repo2.gotFilter.After.ID != 2 || repo2.gotFilter.After.Price != 1850000 {
		t.Errorf("cursor keyset wrong: %+v", repo2.gotFilter.After)
	}
}

func TestList_BadParams(t *testing.T) {
	cases := map[string]string{
		"bad min_price":   "/api/v1/properties?min_price=abc",
		"negative price":  "/api/v1/properties?min_price=-5",
		"bad sort":        "/api/v1/properties?sort=bogus",
		"limit too big":   "/api/v1/properties?limit=101",
		"limit zero":      "/api/v1/properties?limit=0",
		"garbled cursor":  "/api/v1/properties?cursor=%21%21%21",
		"cursor sort mix": "/api/v1/properties?sort=price_asc&cursor=" + encodeCursor(func() cursor { ts := time.Now(); return cursor{Sort: "newest", ID: 1, CreatedAt: &ts} }()),
	}
	for name, target := range cases {
		rec := serve(t, &fakeRepo{}, target)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %s)", name, rec.Code, rec.Body)
		}
		var e map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e["error"] == "" {
			t.Errorf("%s: error body wrong: %s", name, rec.Body)
		}
	}
}

func TestList_RepoError(t *testing.T) {
	rec := serve(t, &fakeRepo{listErr: errors.New("boom")}, "/api/v1/properties")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestDetail_FullMapping(t *testing.T) {
	p := sampleProp("Z9", 9)
	p.PropertyType = strp("SINGLE_FAMILY")
	p.Description = strp("Stunning modern home")
	p.YearBuilt = intp(2021)
	p.Heating = strp("Central")
	p.Cooling = strp("Central Air")
	p.Garage = strp("2 Car Garage")
	p.HOAFeeMonthly = intp(125)
	p.MLSNumber = strp("1234567")
	p.ListingStatus = strp("FOR_SALE")
	p.AgentName = strp("Hill Country Dream Realty")
	p.AgentPhone = strp("512-555-0123")
	p.Latitude = f64p(30.2672)
	p.Longitude = f64p(-97.7431)
	p.VideoURL = "https://cdn.example/videos/Z9.mp4"

	rec := serve(t, &fakeRepo{detail: &p}, "/api/v1/properties/Z9")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d["zpid"] != "Z9" || d["year_built"] != float64(2021) || d["hoa_fee_monthly"] != float64(125) {
		t.Errorf("detail mapping wrong: %v", d)
	}
	if d["lot_size_acres"] != 0.28 { // 12197 sqft / 43560, rounded to 2dp
		t.Errorf("lot_size_acres = %v, want 0.28", d["lot_size_acres"])
	}
	agent, ok := d["agent"].(map[string]any)
	if !ok || agent["name"] != "Hill Country Dream Realty" || agent["brokerage"] != nil {
		t.Errorf("agent mapping wrong: %v", d["agent"])
	}
	if d["video_url"] != "https://cdn.example/videos/Z9.mp4" {
		t.Errorf("video_url wrong: %v", d["video_url"])
	}
	imgs, ok := d["image_urls"].([]any)
	if !ok || len(imgs) != 2 {
		t.Errorf("image_urls wrong: %v", d["image_urls"])
	}
}

func TestDetail_NullsPreEnrichment(t *testing.T) {
	p := sampleProp("Z1", 1)
	rec := serve(t, &fakeRepo{detail: &p}, "/api/v1/properties/Z1")

	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"description", "year_built", "agent", "video_url", "property_type"} {
		v, present := d[key]
		if !present {
			t.Errorf("%s must be present (as null), not omitted", key)
		} else if v != nil {
			t.Errorf("%s = %v, want null", key, v)
		}
	}
}

func TestDetail_NotFound(t *testing.T) {
	rec := serve(t, &fakeRepo{detailErr: property.ErrNotFound}, "/api/v1/properties/NOPE")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var e map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e["error"] != "not found" {
		t.Errorf("body = %s, want {\"error\":\"not found\"}", rec.Body)
	}
}

func TestPublicHeaders(t *testing.T) {
	repo := &fakeRepo{detail: func() *property.Property { p := sampleProp("Z1", 1); return &p }()}
	for _, target := range []string{"/api/v1/properties", "/api/v1/properties/Z1"} {
		rec := serve(t, repo, target)
		h := rec.Header()
		if h.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: missing CORS header", target)
		}
		if h.Get("Cache-Control") != "public, max-age=300" {
			t.Errorf("%s: Cache-Control = %q", target, h.Get("Cache-Control"))
		}
		if h.Get("Content-Type") != "application/json" {
			t.Errorf("%s: Content-Type = %q", target, h.Get("Content-Type"))
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -v`
Expected: FAIL — `undefined: New`, `undefined: Repo` (compile errors).

- [ ] **Step 3: Implement DTOs**

Create `internal/api/dto.go`:

```go
package api

import (
	"math"
	"time"

	"github.com/dwellingtw/backend/internal/property"
)

// listItem is one browse-screen card.
type listItem struct {
	ZPID         string  `json:"zpid"`
	Price        int64   `json:"price"`
	Address      string  `json:"address"`
	City         string  `json:"city"`
	State        string  `json:"state"`
	Zip          string  `json:"zip"`
	Bedrooms     int     `json:"bedrooms"`
	Bathrooms    float64 `json:"bathrooms"`
	HomeSizeSqft int     `json:"home_size_sqft"`
	PropertyType *string `json:"property_type"`
	ImageURL     *string `json:"image_url"`
}

type listResponse struct {
	Total      int        `json:"total"`
	Results    []listItem `json:"results"`
	NextCursor *string    `json:"next_cursor"`
}

type agentDTO struct {
	Name      *string `json:"name"`
	Phone     *string `json:"phone"`
	Brokerage *string `json:"brokerage"`
}

// detailResponse is the full detail-screen payload.
type detailResponse struct {
	ZPID          string    `json:"zpid"`
	Price         int64     `json:"price"`
	Address       string    `json:"address"`
	City          string    `json:"city"`
	State         string    `json:"state"`
	Zip           string    `json:"zip"`
	Bedrooms      int       `json:"bedrooms"`
	Bathrooms     float64   `json:"bathrooms"`
	HomeSizeSqft  int       `json:"home_size_sqft"`
	LotSizeSqft   int       `json:"lot_size_sqft"`
	LotSizeAcres  float64   `json:"lot_size_acres"`
	PropertyType  *string   `json:"property_type"`
	Description   *string   `json:"description"`
	YearBuilt     *int      `json:"year_built"`
	Heating       *string   `json:"heating"`
	Cooling       *string   `json:"cooling"`
	Garage        *string   `json:"garage"`
	HOAFeeMonthly *int      `json:"hoa_fee_monthly"`
	MLSNumber     *string   `json:"mls_number"`
	ListingStatus *string   `json:"listing_status"`
	Agent         *agentDTO `json:"agent"`
	Latitude      *float64  `json:"latitude"`
	Longitude     *float64  `json:"longitude"`
	ImageURLs     []string  `json:"image_urls"`
	VideoURL      *string   `json:"video_url"`
	DetailURL     string    `json:"detail_url"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func toListItem(p *property.Property) listItem {
	it := listItem{
		ZPID: p.ZPID, Price: p.SalePrice,
		Address: p.Address, City: p.City, State: p.State, Zip: p.Zip,
		Bedrooms: p.Bedrooms, Bathrooms: p.Bathrooms, HomeSizeSqft: p.HomeSizeSqft,
		PropertyType: p.PropertyType,
	}
	if len(p.ImageURLs) > 0 {
		it.ImageURL = &p.ImageURLs[0]
	}
	return it
}

func toDetailResponse(p *property.Property) detailResponse {
	d := detailResponse{
		ZPID: p.ZPID, Price: p.SalePrice,
		Address: p.Address, City: p.City, State: p.State, Zip: p.Zip,
		Bedrooms: p.Bedrooms, Bathrooms: p.Bathrooms, HomeSizeSqft: p.HomeSizeSqft,
		LotSizeSqft:  p.LotSizeSqft,
		LotSizeAcres: math.Round(float64(p.LotSizeSqft)/43560*100) / 100,
		PropertyType: p.PropertyType, Description: p.Description, YearBuilt: p.YearBuilt,
		Heating: p.Heating, Cooling: p.Cooling, Garage: p.Garage,
		HOAFeeMonthly: p.HOAFeeMonthly, MLSNumber: p.MLSNumber, ListingStatus: p.ListingStatus,
		Latitude: p.Latitude, Longitude: p.Longitude,
		ImageURLs: p.ImageURLs, DetailURL: p.DetailURL,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if d.ImageURLs == nil {
		d.ImageURLs = []string{}
	}
	if p.VideoURL != "" {
		d.VideoURL = &p.VideoURL
	}
	if p.AgentName != nil || p.AgentPhone != nil || p.AgentBrokerage != nil {
		d.Agent = &agentDTO{Name: p.AgentName, Phone: p.AgentPhone, Brokerage: p.AgentBrokerage}
	}
	return d
}
```

- [ ] **Step 4: Implement param parsing**

Create `internal/api/params.go`:

```go
package api

import (
	"fmt"
	"net/url"
	"strconv"

	"github.com/dwellingtw/backend/internal/property"
)

const (
	defaultLimit = 24
	maxLimit     = 100
)

// parseListParams validates browse query parameters into a property.Filter.
// The returned error text is the public 400 message and names the offending
// parameter.
func parseListParams(q url.Values) (property.Filter, error) {
	f := property.Filter{
		Sort:  property.SortNewest,
		Limit: defaultLimit,
		Zip:   q.Get("zip"), City: q.Get("city"), State: q.Get("state"),
		PropertyType: q.Get("property_type"),
	}

	var err error
	if f.MinPrice, err = int64Param(q, "min_price"); err != nil {
		return f, err
	}
	if f.MaxPrice, err = int64Param(q, "max_price"); err != nil {
		return f, err
	}
	if f.MinBeds, err = intParam(q, "min_beds"); err != nil {
		return f, err
	}
	if f.MinBaths, err = floatParam(q, "min_baths"); err != nil {
		return f, err
	}
	if f.MinSqft, err = intParam(q, "min_sqft"); err != nil {
		return f, err
	}
	if f.MaxSqft, err = intParam(q, "max_sqft"); err != nil {
		return f, err
	}

	switch s := q.Get("sort"); s {
	case "", string(property.SortNewest):
		// default already set
	case string(property.SortPriceAsc):
		f.Sort = property.SortPriceAsc
	case string(property.SortPriceDesc):
		f.Sort = property.SortPriceDesc
	default:
		return f, fmt.Errorf("invalid sort %q (newest, price_asc, price_desc)", s)
	}

	if s := q.Get("limit"); s != "" {
		n, convErr := strconv.Atoi(s)
		if convErr != nil || n < 1 || n > maxLimit {
			return f, fmt.Errorf("invalid limit (must be 1-%d)", maxLimit)
		}
		f.Limit = n
	}

	if tok := q.Get("cursor"); tok != "" {
		c, curErr := decodeCursor(tok, string(f.Sort))
		if curErr != nil {
			return f, curErr
		}
		key := &property.PageKey{ID: c.ID, Price: c.Price}
		if c.CreatedAt != nil {
			key.CreatedAt = *c.CreatedAt
		}
		f.After = key
	}
	return f, nil
}

func int64Param(q url.Values, name string) (int64, error) {
	s := q.Get(name)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return n, nil
}

func intParam(q url.Values, name string) (int, error) {
	n, err := int64Param(q, name)
	return int(n), err
}

func floatParam(q url.Values, name string) (float64, error) {
	s := q.Get(name)
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return f, nil
}
```

- [ ] **Step 5: Implement handlers**

Create `internal/api/api.go`:

```go
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/dwellingtw/backend/internal/property"
)

// Repo is the data access the public API needs.
type Repo interface {
	List(ctx context.Context, f property.Filter) ([]property.Property, int, bool, error)
	GetByZPID(ctx context.Context, zpid string) (*property.Property, error)
}

// API serves the public read-only listings endpoints.
type API struct {
	repo Repo
	log  *slog.Logger
}

// New creates the public API.
func New(repo Repo, log *slog.Logger) *API {
	return &API{repo: repo, log: log}
}

// Register mounts the public routes on mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/properties", a.public(a.handleList))
	mux.Handle("GET /api/v1/properties/{zpid}", a.public(a.handleDetail))
}

// public applies the headers every public API response carries.
func (a *API) public(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	})
}

func (a *API) handleList(w http.ResponseWriter, r *http.Request) {
	f, err := parseListParams(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	props, total, hasMore, err := a.repo.List(r.Context(), f)
	if err != nil {
		a.log.Error("list properties failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	resp := listResponse{Total: total, Results: make([]listItem, 0, len(props))}
	for i := range props {
		resp.Results = append(resp.Results, toListItem(&props[i]))
	}
	if hasMore && len(props) > 0 {
		tok := encodeCursor(cursorForLast(f.Sort, &props[len(props)-1]))
		resp.NextCursor = &tok
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) handleDetail(w http.ResponseWriter, r *http.Request) {
	p, err := a.repo.GetByZPID(r.Context(), r.PathValue("zpid"))
	if errors.Is(err, property.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		a.log.Error("get property failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, toDetailResponse(p))
}

// cursorForLast builds the next-page cursor from the last row of this page.
func cursorForLast(sort property.Sort, last *property.Property) cursor {
	c := cursor{Sort: string(sort), ID: last.ID}
	switch sort {
	case property.SortPriceAsc, property.SortPriceDesc:
		c.Price = last.SalePrice
	default: // newest
		t := last.CreatedAt
		c.CreatedAt = &t
	}
	return c
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/api/ -v`
Expected: PASS (all cursor + handler tests).

- [ ] **Step 7: Commit**

```bash
git add internal/api/
git commit -m "feat: add public listings API handlers (browse + detail)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: Zillow property-details client method

**Files:**
- Create: `internal/zillow/details.go`
- Create: `internal/zillow/details_test.go`
- Create: `internal/zillow/testdata/property_details.json`

**Interfaces:**
- Consumes: Task 1's `property.Details`; existing `Client` internals (`baseURL`, `apiKey`, `http`).
- Produces (used by Task 6):
  - `func (c *Client) PropertyDetails(ctx context.Context, zpid string) (*property.Details, []byte, error)` — the `[]byte` is the raw response body for `details_raw`
  - `var ErrDetailsNotFound = errors.New("property details not found")`

**IMPORTANT — unverified API shape:** the `/property-details` path and response field names below are the best-known shape of the OpenWebNinja Zillow details endpoint but have NOT been verified against a live call. Step 7 verifies live; if names differ, update `detailsRecord` struct tags AND the fixture to the real shape (the mapping logic stays).

- [ ] **Step 1: Create the fixture**

Create `internal/zillow/testdata/property_details.json`:

```json
{
  "status": "OK",
  "request_id": "test-fixture",
  "parameters": { "zpid": "43590635" },
  "data": {
    "zpid": "43590635",
    "homeType": "SINGLE_FAMILY",
    "homeStatus": "FOR_SALE",
    "description": "Stunning modern home with breathtaking Hill Country views.",
    "yearBuilt": 2021,
    "monthlyHoaFee": 125,
    "latitude": 30.2672,
    "longitude": -97.7431,
    "resoFacts": {
      "heating": ["Central"],
      "cooling": ["Central Air", "Ceiling Fan(s)"],
      "garageParkingCapacity": 2
    },
    "attributionInfo": {
      "agentName": "Hill Country Dream Realty",
      "agentPhoneNumber": "512-555-0123",
      "brokerName": "Dream Brokerage LLC",
      "mlsId": "1234567"
    }
  }
}
```

- [ ] **Step 2: Write the failing tests**

Create `internal/zillow/details_test.go`:

```go
package zillow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// TestPropertyDetails_MapsFixture drives the client against a fake server
// returning the pinned fixture and checks the full Details mapping.
func TestPropertyDetails_MapsFixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/property_details.json")
	if err != nil {
		t.Fatal(err)
	}

	var gotPath, gotZPID, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotZPID = r.URL.Query().Get("zpid")
		gotKey = r.Header.Get("X-API-Key")
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key", 5*time.Second)
	d, raw, err := c.PropertyDetails(context.Background(), "43590635")
	if err != nil {
		t.Fatal(err)
	}

	if gotPath != "/property-details" || gotZPID != "43590635" || gotKey != "test-key" {
		t.Errorf("request wrong: path=%q zpid=%q key=%q", gotPath, gotZPID, gotKey)
	}
	if string(raw) != string(fixture) {
		t.Error("raw body must be returned verbatim for details_raw storage")
	}

	check := func(name string, got *string, want string) {
		t.Helper()
		if got == nil || *got != want {
			t.Errorf("%s = %v, want %q", name, got, want)
		}
	}
	check("PropertyType", d.PropertyType, "SINGLE_FAMILY")
	check("ListingStatus", d.ListingStatus, "FOR_SALE")
	check("Description", d.Description, "Stunning modern home with breathtaking Hill Country views.")
	check("Heating", d.Heating, "Central")
	check("Cooling", d.Cooling, "Central Air, Ceiling Fan(s)")
	check("Garage", d.Garage, "2 Car Garage")
	check("MLSNumber", d.MLSNumber, "1234567")
	check("AgentName", d.AgentName, "Hill Country Dream Realty")
	check("AgentPhone", d.AgentPhone, "512-555-0123")
	check("AgentBrokerage", d.AgentBrokerage, "Dream Brokerage LLC")

	if d.YearBuilt == nil || *d.YearBuilt != 2021 {
		t.Errorf("YearBuilt = %v, want 2021", d.YearBuilt)
	}
	if d.HOAFeeMonthly == nil || *d.HOAFeeMonthly != 125 {
		t.Errorf("HOAFeeMonthly = %v, want 125", d.HOAFeeMonthly)
	}
	if d.Latitude == nil || *d.Latitude != 30.2672 || d.Longitude == nil || *d.Longitude != -97.7431 {
		t.Errorf("lat/long = %v/%v", d.Latitude, d.Longitude)
	}
}

// TestPropertyDetails_EmptyFieldsAreNil verifies absent fields map to nil,
// not pointers to zero values.
func TestPropertyDetails_EmptyFieldsAreNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"OK","data":{"zpid":"1"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k", 5*time.Second)
	d, _, err := c.PropertyDetails(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if d.PropertyType != nil || d.Description != nil || d.YearBuilt != nil ||
		d.Heating != nil || d.Garage != nil || d.HOAFeeMonthly != nil ||
		d.AgentName != nil || d.Latitude != nil {
		t.Errorf("absent fields must be nil: %+v", d)
	}
}

func TestPropertyDetails_NotFound(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"http 404": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
		"null data": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"OK","data":null}`))
		},
	}
	for name, h := range cases {
		srv := httptest.NewServer(h)
		c := New(srv.URL, "k", 5*time.Second)
		_, _, err := c.PropertyDetails(context.Background(), "gone")
		srv.Close()
		if !errors.Is(err, ErrDetailsNotFound) {
			t.Errorf("%s: err = %v, want ErrDetailsNotFound", name, err)
		}
	}
}

func TestPropertyDetails_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(srv.URL, "k", 5*time.Second)
	_, _, err := c.PropertyDetails(context.Background(), "1")
	if err == nil || errors.Is(err, ErrDetailsNotFound) {
		t.Errorf("want a non-not-found error, got %v", err)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/zillow/ -run TestPropertyDetails -v`
Expected: FAIL — `undefined: ErrDetailsNotFound` (compile error).

- [ ] **Step 4: Implement**

Create `internal/zillow/details.go`:

```go
package zillow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/dwellingtw/backend/internal/property"
)

// ErrDetailsNotFound means the API has no details for this zpid. Callers
// should mark the row as fetched so it is not retried forever.
var ErrDetailsNotFound = errors.New("property details not found")

// detailsPath is the provider's property-details endpoint, relative to the
// API base URL. Kept as a const because the exact path is provider-defined
// (verified live during implementation — see the plan's Task 5 Step 7).
const detailsPath = "/property-details"

// detailsRecord maps the fields we persist from the details response "data"
// object. Unknown/absent fields simply stay zero and map to nil pointers.
type detailsRecord struct {
	ZPID          json.Number `json:"zpid"`
	HomeType      string      `json:"homeType"`
	HomeStatus    string      `json:"homeStatus"`
	Description   string      `json:"description"`
	YearBuilt     json.Number `json:"yearBuilt"`
	MonthlyHOAFee json.Number `json:"monthlyHoaFee"`
	Latitude      json.Number `json:"latitude"`
	Longitude     json.Number `json:"longitude"`
	ResoFacts     struct {
		Heating               []string    `json:"heating"`
		Cooling               []string    `json:"cooling"`
		GarageParkingCapacity json.Number `json:"garageParkingCapacity"`
	} `json:"resoFacts"`
	AttributionInfo struct {
		AgentName        string `json:"agentName"`
		AgentPhoneNumber string `json:"agentPhoneNumber"`
		BrokerName       string `json:"brokerName"`
		MLSID            string `json:"mlsId"`
	} `json:"attributionInfo"`
}

// PropertyDetails fetches the one-time enrichment record for a zpid. It
// returns the mapped details plus the raw response body (stored as
// details_raw so future fields never require re-fetching).
func (c *Client) PropertyDetails(ctx context.Context, zpid string) (*property.Details, []byte, error) {
	endpoint := fmt.Sprintf("%s%s?zpid=%s", c.baseURL, detailsPath, url.QueryEscape(zpid))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build details request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("details request zpid=%s: %w", zpid, err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotFound {
		return nil, nil, fmt.Errorf("zpid=%s: %w", zpid, ErrDetailsNotFound)
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, nil, fmt.Errorf("details API returned status %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20)) // details payloads are large; 4 MiB is ample
	if err != nil {
		return nil, nil, fmt.Errorf("read details body: %w", err)
	}

	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, nil, fmt.Errorf("decode details envelope: %w", err)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, nil, fmt.Errorf("zpid=%s: %w", zpid, ErrDetailsNotFound)
	}

	var rec detailsRecord
	if err := json.Unmarshal(env.Data, &rec); err != nil {
		return nil, nil, fmt.Errorf("decode details data: %w", err)
	}
	return toDetails(&rec), body, nil
}

// toDetails maps the raw record onto the domain model, turning empty values
// into nil pointers.
func toDetails(rec *detailsRecord) *property.Details {
	return &property.Details{
		PropertyType:   strPtr(rec.HomeType),
		Description:    strPtr(rec.Description),
		YearBuilt:      intPtr(rec.YearBuilt),
		Heating:        strPtr(strings.Join(rec.ResoFacts.Heating, ", ")),
		Cooling:        strPtr(strings.Join(rec.ResoFacts.Cooling, ", ")),
		Garage:         garagePtr(rec.ResoFacts.GarageParkingCapacity),
		HOAFeeMonthly:  intPtr(rec.MonthlyHOAFee),
		MLSNumber:      strPtr(rec.AttributionInfo.MLSID),
		ListingStatus:  strPtr(rec.HomeStatus),
		AgentName:      strPtr(rec.AttributionInfo.AgentName),
		AgentPhone:     strPtr(rec.AttributionInfo.AgentPhoneNumber),
		AgentBrokerage: strPtr(rec.AttributionInfo.BrokerName),
		Latitude:       floatPtr(rec.Latitude),
		Longitude:      floatPtr(rec.Longitude),
	}
}

func strPtr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func intPtr(n json.Number) *int {
	f := toFloat(n)
	if f == 0 {
		return nil
	}
	v := int(f)
	return &v
}

func floatPtr(n json.Number) *float64 {
	f := toFloat(n)
	if f == 0 {
		return nil
	}
	return &f
}

func garagePtr(n json.Number) *string {
	cap := toFloat(n)
	if cap <= 0 {
		return nil
	}
	s := fmt.Sprintf("%d Car Garage", int(cap))
	return &s
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/zillow/ -v`
Expected: PASS (new and pre-existing tests).

- [ ] **Step 6: Commit**

```bash
git add internal/zillow/details.go internal/zillow/details_test.go internal/zillow/testdata/property_details.json
git commit -m "feat: add Zillow property-details client method with fixture-pinned mapping

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

- [ ] **Step 7: Verify the live endpoint shape (best effort)**

If a real API key is available (check for a local `.env` with `ZILLOW_API_KEY`, or ask the user), make ONE live call with a zpid taken from the production DB or a fresh search response:

```bash
curl -s "https://api.openwebninja.com/realtime-zillow-data/property-details?zpid=<ZPID>" \
  -H "X-API-Key: $ZILLOW_API_KEY" | head -c 4000
```

- If the path 404s, try `/property` and `/propertyDetails`; update the `detailsPath` const to whichever works.
- Compare the real field names against `detailsRecord` (homeType, homeStatus, description, yearBuilt, monthlyHoaFee, latitude, longitude, resoFacts.heating/cooling/garageParkingCapacity, attributionInfo.agentName/agentPhoneNumber/brokerName/mlsId). Update struct tags AND `testdata/property_details.json` to the verified shape, re-run `go test ./internal/zillow/`, and commit with message `fix: pin zillow details mapping to live response shape`.
- If no key is available, DO NOT block: leave the code as-is and clearly flag in the task report that live verification is pending (fields that don't match will just be NULL in production until fixed).

---

### Task 6: Scheduler enrichment step + config cap

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/scheduler/scheduler.go`
- Modify: `internal/scheduler/scheduler_test.go`

**Interfaces:**
- Consumes: Task 2's `ListZPIDsMissingDetails`/`SetDetails` signatures, Task 5's `PropertyDetails` + `ErrDetailsNotFound`, Task 1's `property.Details`.
- Produces: `Config.DetailsPerCycle int` (env `DETAILS_PER_CYCLE`, default 50, `<=0` disables); scheduler `zillowAPI` interface `{ Search(...); PropertyDetails(...) }` — `cmd/server/main.go` keeps passing the concrete `*zillow.Client`, which satisfies it.

- [ ] **Step 1: Add config field**

In `internal/config/config.go`, add to the `Config` struct after `SkipExisting`:

```go
	// DetailsPerCycle caps how many properties get a one-time details-API
	// enrichment call per cycle (protects API quota). <= 0 disables enrichment.
	DetailsPerCycle int
```

And in `Load()`, after the `SkipExisting` line:

```go
		DetailsPerCycle:  getenvInt("DETAILS_PER_CYCLE", 50),
```

- [ ] **Step 2: Write the failing scheduler tests**

In `internal/scheduler/scheduler_test.go`:

Add to imports: `"errors"`, `"github.com/dwellingtw/backend/internal/zillow"`.

Extend `fakeSearch` with details behavior — add fields and method:

```go
type fakeSearch struct {
	props []property.Property
	// byLocation, when set, returns per-location props and records each queried
	// location in order. Takes precedence over props.
	byLocation map[string][]property.Property
	queried    []string
	// detailsErr, when set for a zpid, is returned by PropertyDetails.
	detailsErr map[string]error
	mu         sync.Mutex
}

func (f *fakeSearch) PropertyDetails(_ context.Context, zpid string) (*property.Details, []byte, error) {
	if err := f.detailsErr[zpid]; err != nil {
		return nil, nil, err
	}
	pt := "SINGLE_FAMILY"
	return &property.Details{PropertyType: &pt}, []byte(`{"status":"OK"}`), nil
}
```

Extend `fakeStore` — add fields and methods:

```go
type fakeStore struct {
	upserts, ready, failed atomic.Int64
	existing               map[string]bool
	// missingDetails is what ListZPIDsMissingDetails returns (up to limit).
	missingDetails []string
	mu             sync.Mutex
	detailsSet     []string            // zpids passed to SetDetails, in order
	detailsGot     map[string]*property.Details
}

func (s *fakeStore) ListZPIDsMissingDetails(_ context.Context, limit int) ([]string, error) {
	if len(s.missingDetails) > limit {
		return s.missingDetails[:limit], nil
	}
	return s.missingDetails, nil
}

func (s *fakeStore) SetDetails(_ context.Context, zpid string, d *property.Details, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detailsSet = append(s.detailsSet, zpid)
	if s.detailsGot == nil {
		s.detailsGot = map[string]*property.Details{}
	}
	s.detailsGot[zpid] = d
	return nil
}
```

Add the tests:

```go
func TestRunCycle_EnrichesDetailsUpToCap(t *testing.T) {
	cfg := baseConfig()
	cfg.SearchLocations = nil // no search work — isolate enrichment
	cfg.DetailsPerCycle = 2

	store := &fakeStore{missingDetails: []string{"Z1", "Z2", "Z3"}}
	s := New(cfg, &fakeSearch{}, &fakeUploader{}, store, &fakeRenderer{}, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.detailsSet) != 2 {
		t.Fatalf("SetDetails calls = %v, want exactly the 2-cap", store.detailsSet)
	}
	if store.detailsSet[0] != "Z1" || store.detailsSet[1] != "Z2" {
		t.Errorf("enriched %v, want [Z1 Z2] (oldest first)", store.detailsSet)
	}
	if d := store.detailsGot["Z1"]; d == nil || d.PropertyType == nil || *d.PropertyType != "SINGLE_FAMILY" {
		t.Errorf("details not stored: %+v", store.detailsGot["Z1"])
	}
}

func TestRunCycle_EnrichmentDisabledWhenCapZero(t *testing.T) {
	cfg := baseConfig()
	cfg.SearchLocations = nil
	cfg.DetailsPerCycle = 0

	store := &fakeStore{missingDetails: []string{"Z1"}}
	s := New(cfg, &fakeSearch{}, &fakeUploader{}, store, &fakeRenderer{}, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.detailsSet) != 0 {
		t.Errorf("enrichment ran with cap 0: %v", store.detailsSet)
	}
}

func TestRunCycle_EnrichmentErrorHandling(t *testing.T) {
	cfg := baseConfig()
	cfg.SearchLocations = nil
	cfg.DetailsPerCycle = 10

	search := &fakeSearch{detailsErr: map[string]error{
		"DEAD": zillow.ErrDetailsNotFound,       // definitive: mark fetched
		"FLAKY": errors.New("500 whatever"),     // transient: leave for retry
	}}
	store := &fakeStore{missingDetails: []string{"DEAD", "FLAKY", "OK1"}}
	s := New(cfg, search, &fakeUploader{}, store, &fakeRenderer{}, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	// DEAD gets empty details recorded (no infinite retry); FLAKY is skipped;
	// OK1 is enriched normally.
	got := map[string]bool{}
	for _, z := range store.detailsSet {
		got[z] = true
	}
	if !got["DEAD"] || !got["OK1"] || got["FLAKY"] {
		t.Errorf("SetDetails calls = %v, want DEAD and OK1 only", store.detailsSet)
	}
	if d := store.detailsGot["DEAD"]; d == nil || d.PropertyType != nil {
		t.Errorf("DEAD must be recorded with empty details, got %+v", d)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/scheduler/ -v`
Expected: FAIL — fakes don't yet satisfy the (still-unchanged) interfaces / `enrichDetails` missing. If it compiles, the new tests fail because enrichment never runs.

- [ ] **Step 4: Implement the enrichment step**

In `internal/scheduler/scheduler.go`:

Add to imports: `"errors"`, `"github.com/dwellingtw/backend/internal/zillow"`.

Replace the `zillowSearcher` interface with:

```go
// zillowAPI discovers properties and fetches their one-time details record.
type zillowAPI interface {
	Search(ctx context.Context, s config.SearchCriteria) ([]property.Property, error)
	PropertyDetails(ctx context.Context, zpid string) (*property.Details, []byte, error)
}
```

Update the `Scheduler` struct field and `New` parameter from `zillowSearcher` to `zillowAPI` (field stays named `zillow`).

Extend the `store` interface:

```go
type store interface {
	Exists(ctx context.Context, zpid string) (bool, error)
	Upsert(ctx context.Context, p *property.Property) error
	SetVideoReady(ctx context.Context, zpid, videoURL, contentHash string, durationSecs int) error
	SetVideoFailed(ctx context.Context, zpid string) error
	ListZPIDsMissingDetails(ctx context.Context, limit int) ([]string, error)
	SetDetails(ctx context.Context, zpid string, d *property.Details, raw []byte) error
}
```

In `RunCycle`, after the locations loop and before the final log line, add:

```go
	s.enrichDetails(ctx)
```

Add the method:

```go
// enrichDetails fetches the one-time details record for rows that have never
// been enriched, capped per cycle to protect API quota. A transient failure
// is logged and left NULL so the row retries next cycle; a definitive
// not-found is recorded with empty details so dead zpids don't retry forever.
func (s *Scheduler) enrichDetails(ctx context.Context) {
	if s.cfg.DetailsPerCycle <= 0 {
		return
	}
	zpids, err := s.repo.ListZPIDsMissingDetails(ctx, s.cfg.DetailsPerCycle)
	if err != nil {
		s.log.Error("list zpids missing details failed", "error", err)
		return
	}
	if len(zpids) == 0 {
		return
	}

	var fetched, failed int
	for _, zpid := range zpids {
		if ctx.Err() != nil {
			break // shutting down
		}
		d, raw, err := s.zillow.PropertyDetails(ctx, zpid)
		switch {
		case errors.Is(err, zillow.ErrDetailsNotFound):
			if err := s.repo.SetDetails(ctx, zpid, &property.Details{}, nil); err != nil {
				s.log.Error("record empty details failed", "zpid", zpid, "error", err)
				failed++
				continue
			}
			s.log.Warn("details not found, marked fetched", "zpid", zpid)
			fetched++
		case err != nil:
			s.log.Warn("details fetch failed, will retry next cycle", "zpid", zpid, "error", err)
			failed++
		default:
			if err := s.repo.SetDetails(ctx, zpid, d, raw); err != nil {
				s.log.Error("store details failed", "zpid", zpid, "error", err)
				failed++
				continue
			}
			fetched++
		}
	}
	s.log.Info("details enrichment finished",
		"fetched", fetched, "failed", failed, "cap", s.cfg.DetailsPerCycle)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/scheduler/ -v && go build ./...`
Expected: PASS, including the pre-existing scheduler tests.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/scheduler/scheduler.go internal/scheduler/scheduler_test.go
git commit -m "feat: enrich properties with details API data during collection cycle

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 7: Wiring, docs, and end-to-end verification

**Files:**
- Modify: `internal/server/server.go`
- Modify: `cmd/server/main.go`
- Modify: `README.md` (endpoint docs — match the file's existing tone/format)

**Interfaces:**
- Consumes: Task 4's `api.New`/`Register`; everything else already wired.
- Produces: the running service exposes `GET /api/v1/properties` and `GET /api/v1/properties/{zpid}`.

- [ ] **Step 1: Mount the API in `internal/server/server.go`**

Add import `"github.com/dwellingtw/backend/internal/api"`. Change `New` to accept and register the public API:

```go
// New creates a Server bound to addr (e.g. ":8080"). publicAPI may be nil
// (e.g. in tests that only exercise the feed).
func New(addr, providerName string, repo feedSource, publicAPI *api.API, log *slog.Logger) *Server {
	s := &Server{
		repo:         repo,
		providerName: providerName,
		log:          log,
		now:          time.Now,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /roku/feed.json", s.handleFeed)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	if publicAPI != nil {
		publicAPI.Register(mux)
	}
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}
```

Also update the package doc comment to mention the public API:

```go
// Package server exposes the Roku Direct Publisher feed, the public listings
// API, and a health endpoint.
```

- [ ] **Step 2: Wire it in `cmd/server/main.go`**

Add import `"github.com/dwellingtw/backend/internal/api"`. Replace the `httpSrv := server.New(...)` line with:

```go
	publicAPI := api.New(repo, log)
	httpSrv := server.New(net.JoinHostPort("", cfg.HTTPPort), "DwellingTV", repo, publicAPI, log)
```

- [ ] **Step 3: Build and run the full test suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS everywhere.

- [ ] **Step 4: End-to-end smoke test**

Bring the stack up locally (needs the repo's `.env` / compose config; check `docker-compose.yml` for how env is injected):

```bash
docker compose up --build -d
sleep 5
curl -s http://localhost:8080/healthz
curl -s "http://localhost:8080/api/v1/properties?limit=2" | head -c 2000
```

Expected: `{"status":"ok"}`; a JSON list response with `total`, `results`, `next_cursor` keys. Then take a `zpid` from the results and:

```bash
curl -s "http://localhost:8080/api/v1/properties/<ZPID>" | head -c 2000
curl -s "http://localhost:8080/api/v1/properties/does-not-exist" # → {"error":"not found"}
curl -s "http://localhost:8080/api/v1/properties?sort=bogus"     # → {"error":"invalid sort ..."}
curl -si "http://localhost:8080/api/v1/properties?limit=1" | grep -iE "access-control|cache-control"
```

Also exercise pagination end-to-end: request `limit=1`, take `next_cursor`, request page 2 with it, and confirm the two pages return different zpids. If the DB has enriched rows (after a cycle with `DETAILS_PER_CYCLE > 0`), confirm the detail response shows non-null enrichment fields. Tear down with `docker compose down` when finished.

- [ ] **Step 5: Document the endpoints in `README.md`**

Add an "API" section (place it near any existing endpoint docs, e.g. where the Roku feed is described) listing both endpoints, their query params (`zip`, `city`, `state`, `min_price`, `max_price`, `property_type`, `min_beds`, `min_baths`, `min_sqft`, `max_sqft`, `sort`, `limit`, `cursor`), cursor-pagination usage (`next_cursor` → `cursor`), the `DETAILS_PER_CYCLE` env var (default 50, 0 disables), and a note that both endpoints are public.

- [ ] **Step 6: Commit**

```bash
git add internal/server/server.go cmd/server/main.go README.md
git commit -m "feat: mount public listings API on the HTTP server

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```
