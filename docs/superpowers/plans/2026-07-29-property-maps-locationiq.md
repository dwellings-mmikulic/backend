# Property Maps via LocationIQ — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give each property a pinned static map image, hosted on Bunny CDN and returned as `map_image_url` from the detail endpoint, generated on demand the first time someone views that listing.

**Architecture:** A thin LocationIQ HTTP client (`internal/locationiq`) provides forward geocoding and static-map fetching. A generation service (`internal/propertymap`) owns the "return the map, creating it if needed" logic — single-flighted per zpid, with a 1.5s deadline after which generation finishes in the background. The detail handler calls the service and shortens its `Cache-Control` when a map is still pending. There is no scheduled map pass.

**Tech Stack:** Go 1.26.4, `net/http` standard library, `golang.org/x/sync/singleflight` (already a direct dependency), pgx v5, swaggo/swag for the OpenAPI spec.

**Spec:** `docs/superpowers/specs/2026-07-29-property-maps-locationiq-design.md`

## Global Constraints

- Module path is `github.com/dwellingtw/backend`. All internal imports use that prefix.
- No new third-party dependencies. `golang.org/x/sync` is already in `go.mod` as a direct require; everything else is standard library.
- `LOCATIONIQ_API_KEY` is **optional**. When unset, maps are disabled, `map_image_url` is always `null`, and every existing test and deployment must continue to pass unchanged.
- Never commit a real API key. `.env.example` gets an empty placeholder only.
- Static map parameters are package constants, not configuration: `zoom=16`, `size=600x400`, `format=png`, `maptype=streets`, marker icon `large-red-cutout`.
- Bunny object path for maps is exactly `maps/<zpid>.png`, content type `image/png`.
- Schema changes are additive and idempotent — `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`, matching the existing style in `internal/db/schema.sql`.
- Tests use the standard library only (`testing`, `net/http/httptest`). The codebase has no assertion library — follow the existing table-driven, `t.Errorf`-style tests in `internal/api/api_test.go`.
- Run `make vet` and `make test` before every commit.

## Getting Started

This work should land on its own branch:

```bash
git checkout -b feat/property-maps
```

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/db/schema.sql` (modify) | Two additive columns: `map_image_url`, `map_generated_at` |
| `internal/property/property.go` (modify) | `MapImageURL`, `MapGeneratedAt` fields on `Property` |
| `internal/property/repository.go` (modify) | Select the new columns in `GetByZPID`; add `SetMapImage`, `SetCoordinates` |
| `internal/locationiq/client.go` (create) | LocationIQ HTTP client: `Geocode`, `StaticMap`, `ErrNoMatch` |
| `internal/locationiq/client_test.go` (create) | Client tests against `httptest` servers |
| `internal/propertymap/service.go` (create) | Generation service: `Ensure`, single-flight, deadline, guards |
| `internal/propertymap/service_test.go` (create) | Service tests with fake client/uploader/store |
| `internal/api/dto.go` (modify) | `map_image_url` on `detailResponse` |
| `internal/api/api.go` (modify) | `MapEnsurer` dependency, detail-handler wiring, cache-control split |
| `internal/api/api_test.go` (modify) | Update `serve` helper; add deadline/fast-path tests |
| `internal/config/config.go` (modify) | `LocationIQAPIKey` |
| `cmd/server/main.go` (modify) | Construct the client + service, pass into `api.New` |
| `.env.example`, `README.md`, `docs/` (modify) | Documentation and regenerated Swagger spec |

The two new packages are deliberately separate: `locationiq` knows about HTTP and URL construction and nothing about properties; `propertymap` knows about properties and policy and nothing about URL construction. Each is testable in isolation with fakes.

---

### Task 1: Schema, domain model, and repository methods

Persistence for the two new columns. No behavior yet — this task exists so later tasks have somewhere to write.

**Files:**
- Modify: `internal/db/schema.sql:49` (after the `details_fetched_at` line)
- Modify: `internal/property/property.go:53-58` (the video-state block)
- Modify: `internal/property/repository.go:172-203` (`GetByZPID`), and append two methods after `SetDetails`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `property.Property.MapImageURL string` — empty means no map.
  - `property.Property.MapGeneratedAt *time.Time` — non-nil with an empty `MapImageURL` means permanently unmappable.
  - `(*property.Repository).SetMapImage(ctx context.Context, zpid, url string) error`
  - `(*property.Repository).SetCoordinates(ctx context.Context, zpid string, lat, lon float64) error`

**A note on testing this task:** `internal/property/repository.go` has no unit tests today — the repository needs a live Postgres, and this codebase has no test-DB harness. Introducing one is out of scope. This task is therefore verified by compilation, `go vet`, the existing test suite still passing, and an explicit manual check against the local Docker Postgres in Step 6. Tasks 2–4 carry the real test coverage.

- [ ] **Step 1: Add the schema columns**

In `internal/db/schema.sql`, directly after the `details_fetched_at` line (line 49) and before the `-- Public listing API filter/sort indexes.` comment:

```sql

-- Static property map (LocationIQ), generated on demand by the detail endpoint
-- (see docs/superpowers/specs/2026-07-29-property-maps-locationiq-design.md).
-- map_generated_at set with a NULL map_image_url means the address could not be
-- geocoded; clearing it makes the row eligible again.
ALTER TABLE properties ADD COLUMN IF NOT EXISTS map_image_url      TEXT;
ALTER TABLE properties ADD COLUMN IF NOT EXISTS map_generated_at   TIMESTAMPTZ;
```

- [ ] **Step 2: Add the domain fields**

In `internal/property/property.go`, inside the `Property` struct, after the `// Video / Roku feed state.` block ending with `VideoDurationSecs int`:

```go
	// Static map state. MapGeneratedAt non-nil with an empty MapImageURL means
	// the address could not be geocoded and no map will be attempted again.
	MapImageURL    string
	MapGeneratedAt *time.Time
```

- [ ] **Step 3: Select the new columns in GetByZPID**

In `internal/property/repository.go`, `GetByZPID`. Change the final line of the SELECT list from:

```go
       details_fetched_at, created_at, updated_at
```

to:

```go
       details_fetched_at, COALESCE(map_image_url,''), map_generated_at,
       created_at, updated_at
```

and change the matching tail of the `Scan` call from:

```go
		&p.DetailsFetchedAt, &p.CreatedAt, &p.UpdatedAt,
```

to:

```go
		&p.DetailsFetchedAt, &p.MapImageURL, &p.MapGeneratedAt,
		&p.CreatedAt, &p.UpdatedAt,
```

The `COALESCE(map_image_url,'')` matches how the other string columns in this query are handled, so `MapImageURL` scans into a plain `string`.

- [ ] **Step 4: Add the two write methods**

Append to `internal/property/repository.go`, after `SetDetails`:

```go
// SetMapImage records the property's static map URL and stamps
// map_generated_at. An empty url records a permanently unmappable row (the
// address could not be geocoded) so it is never retried.
func (r *Repository) SetMapImage(ctx context.Context, zpid, url string) error {
	const q = `
UPDATE properties SET
    map_image_url = NULLIF($2, ''), map_generated_at = now(), updated_at = now()
 WHERE zpid = $1`
	if _, err := r.pool.Exec(ctx, q, zpid, url); err != nil {
		return fmt.Errorf("set map image zpid=%s: %w", zpid, err)
	}
	return nil
}

// SetCoordinates writes geocoded coordinates back to the row. It deliberately
// touches only latitude/longitude — SetDetails writes the whole enrichment
// block and would clobber the other fields with nils.
func (r *Repository) SetCoordinates(ctx context.Context, zpid string, lat, lon float64) error {
	const q = `
UPDATE properties SET
    latitude = $2, longitude = $3, updated_at = now()
 WHERE zpid = $1`
	if _, err := r.pool.Exec(ctx, q, zpid, lat, lon); err != nil {
		return fmt.Errorf("set coordinates zpid=%s: %w", zpid, err)
	}
	return nil
}
```

- [ ] **Step 5: Build and run the existing suite**

```bash
make vet && make test
```

Expected: PASS. Nothing consumes the new fields yet, so this only proves the schema string, struct, and SQL column/scan-target counts line up. A mismatched count in `GetByZPID` would compile fine and fail at runtime — Step 6 is what actually catches that.

- [ ] **Step 6: Verify the migration and the detail query against a real database**

```bash
make up
docker compose logs app | grep -i "database ready"
docker compose exec -T db psql -U dwellings -d dwellings -c "\d properties" | grep map_
```

Expected: both `map_image_url | text` and `map_generated_at | timestamp with time zone` are listed.

Then exercise `GetByZPID` end to end through the running API, which is the only thing that proves the scan targets match:

```bash
ZPID=$(docker compose exec -T db psql -U dwellings -d dwellings -tAc "SELECT zpid FROM properties LIMIT 1")
curl -fsS "http://localhost:8080/api/v1/properties/$ZPID" | head -c 200
```

Expected: a 200 with a JSON body. A column/scan mismatch surfaces here as a 500 plus a `scan` error in `docker compose logs app`. If the properties table is empty, run `make logs` and wait for the first collection cycle to save a listing, then retry.

```bash
make down
```

- [ ] **Step 7: Commit**

```bash
git add internal/db/schema.sql internal/property/property.go internal/property/repository.go
git commit -m "feat: map_image_url + map_generated_at columns and repository writers"
```

---

### Task 2: LocationIQ client

A standalone HTTP client. Knows about LocationIQ URLs and response shapes; knows nothing about properties or Bunny.

**Files:**
- Create: `internal/locationiq/client.go`
- Test: `internal/locationiq/client_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `locationiq.Address` — struct with `Street, City, State, PostalCode string`.
  - `locationiq.New(apiKey string, timeout time.Duration) *locationiq.Client`
  - `(*Client).Geocode(ctx context.Context, a Address) (lat, lon float64, err error)`
  - `(*Client).StaticMap(ctx context.Context, lat, lon float64) ([]byte, error)`
  - `locationiq.ErrNoMatch` — sentinel `error`, returned by `Geocode` when the result set is empty.

The base URLs are struct fields (unexported, defaulted in `New`) so tests can point them at an `httptest` server.

- [ ] **Step 1: Write the failing tests**

Create `internal/locationiq/client_test.go`:

```go
package locationiq

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testAddress() Address {
	return Address{Street: "1234 Hilltop Drive", City: "Austin", State: "TX", PostalCode: "78746"}
}

func TestGeocode_BuildsStructuredQueryAndParsesResult(t *testing.T) {
	var gotQuery map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"lat":"30.2672","lon":"-97.7431","display_name":"Austin, TX"}]`))
	}))
	defer srv.Close()

	c := New("test-key", 5*time.Second)
	c.geocodeURL = srv.URL

	lat, lon, err := c.Geocode(context.Background(), testAddress())
	if err != nil {
		t.Fatalf("Geocode: %v", err)
	}
	if lat != 30.2672 || lon != -97.7431 {
		t.Errorf("coords = %v, %v; want 30.2672, -97.7431", lat, lon)
	}

	want := map[string]string{
		"key":        "test-key",
		"format":     "json",
		"country":    "us",
		"street":     "1234 Hilltop Drive",
		"city":       "Austin",
		"state":      "TX",
		"postalcode": "78746",
	}
	for k, v := range want {
		if got := gotQuery.Get(k); got != v {
			t.Errorf("query %q = %q, want %q", k, got, v)
		}
	}
}

func TestGeocode_EmptyResultIsErrNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := New("k", 5*time.Second)
	c.geocodeURL = srv.URL

	if _, _, err := c.Geocode(context.Background(), testAddress()); !errors.Is(err, ErrNoMatch) {
		t.Errorf("err = %v, want ErrNoMatch", err)
	}
}

// LocationIQ answers an unmatched address with 404 and an {"error": ...} body,
// which means the same thing as an empty array.
func TestGeocode_NotFoundIsErrNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Unable to geocode"}`))
	}))
	defer srv.Close()

	c := New("k", 5*time.Second)
	c.geocodeURL = srv.URL

	if _, _, err := c.Geocode(context.Background(), testAddress()); !errors.Is(err, ErrNoMatch) {
		t.Errorf("err = %v, want ErrNoMatch", err)
	}
}

func TestGeocode_ServerErrorIsNotErrNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New("k", 5*time.Second)
	c.geocodeURL = srv.URL

	_, _, err := c.Geocode(context.Background(), testAddress())
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if errors.Is(err, ErrNoMatch) {
		t.Error("a 500 must be retryable, not ErrNoMatch")
	}
}

func TestStaticMap_BuildsPinnedMapURLAndReturnsBytes(t *testing.T) {
	var gotQuery map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nfake"))
	}))
	defer srv.Close()

	c := New("test-key", 5*time.Second)
	c.staticMapURL = srv.URL

	png, err := c.StaticMap(context.Background(), 30.2672, -97.7431)
	if err != nil {
		t.Fatalf("StaticMap: %v", err)
	}
	if string(png) != "\x89PNG\r\n\x1a\nfake" {
		t.Errorf("body = %q", png)
	}

	want := map[string]string{
		"key":     "test-key",
		"center":  "30.2672,-97.7431",
		"zoom":    "16",
		"size":    "600x400",
		"format":  "png",
		"maptype": "streets",
		"markers": "icon:large-red-cutout|30.2672,-97.7431",
	}
	for k, v := range want {
		if got := gotQuery.Get(k); got != v {
			t.Errorf("query %q = %q, want %q", k, got, v)
		}
	}
}

func TestStaticMap_NonOKIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"Rate Limited"}`))
	}))
	defer srv.Close()

	c := New("k", 5*time.Second)
	c.staticMapURL = srv.URL

	if _, err := c.StaticMap(context.Background(), 1, 2); err == nil {
		t.Fatal("want error, got nil")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/locationiq/... -v
```

Expected: FAIL — the package does not exist yet (`no Go files in .../internal/locationiq`).

- [ ] **Step 3: Write the client**

Create `internal/locationiq/client.go`:

```go
// Package locationiq is a client for the LocationIQ geocoding and static maps
// APIs. It knows about LocationIQ's URLs and response shapes and nothing else —
// see internal/propertymap for the property-facing generation policy.
package locationiq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrNoMatch means LocationIQ could not geocode the address. Callers should
// record the property as permanently unmappable rather than retrying.
var ErrNoMatch = errors.New("locationiq: no match for address")

// Static map rendering parameters. Constants rather than configuration —
// every listing map looks the same.
const (
	mapZoom       = "16"
	mapSize       = "600x400"
	mapFormat     = "png"
	mapType       = "streets"
	mapMarkerIcon = "large-red-cutout"
)

const (
	defaultGeocodeURL   = "https://us1.locationiq.com/v1/search/structured"
	defaultStaticMapURL = "https://maps.locationiq.com/v3/staticmap"
)

// Address is a US street address to geocode.
type Address struct {
	Street     string
	City       string
	State      string
	PostalCode string
}

// Client calls the LocationIQ APIs.
type Client struct {
	apiKey string
	http   *http.Client

	// Endpoint overrides, set by tests.
	geocodeURL   string
	staticMapURL string
}

// New creates a LocationIQ client.
func New(apiKey string, timeout time.Duration) *Client {
	return &Client{
		apiKey:       apiKey,
		http:         &http.Client{Timeout: timeout},
		geocodeURL:   defaultGeocodeURL,
		staticMapURL: defaultStaticMapURL,
	}
}

// geocodeResult is one entry of the forward-geocoding response array.
// LocationIQ returns the coordinates as strings.
type geocodeResult struct {
	Lat string `json:"lat"`
	Lon string `json:"lon"`
}

// Geocode resolves a street address to coordinates. It returns ErrNoMatch when
// LocationIQ has no result for the address, which is a permanent answer;
// every other error is transient and safe to retry.
func (c *Client) Geocode(ctx context.Context, a Address) (float64, float64, error) {
	q := url.Values{}
	q.Set("key", c.apiKey)
	q.Set("format", "json")
	q.Set("country", "us")
	q.Set("street", a.Street)
	q.Set("city", a.City)
	q.Set("state", a.State)
	q.Set("postalcode", a.PostalCode)

	body, status, err := c.get(ctx, c.geocodeURL+"?"+q.Encode())
	if err != nil {
		return 0, 0, fmt.Errorf("geocode request: %w", err)
	}
	// LocationIQ answers an unmatched address with 404.
	if status == http.StatusNotFound {
		return 0, 0, ErrNoMatch
	}
	if status != http.StatusOK {
		return 0, 0, fmt.Errorf("geocode returned status %d: %s", status, truncate(body))
	}

	var results []geocodeResult
	if err := json.Unmarshal(body, &results); err != nil {
		return 0, 0, fmt.Errorf("decode geocode response: %w", err)
	}
	if len(results) == 0 {
		return 0, 0, ErrNoMatch
	}

	lat, err := strconv.ParseFloat(results[0].Lat, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse lat %q: %w", results[0].Lat, err)
	}
	lon, err := strconv.ParseFloat(results[0].Lon, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse lon %q: %w", results[0].Lon, err)
	}
	return lat, lon, nil
}

// StaticMap returns the PNG bytes of a map centred on the coordinates with a
// pin dropped on them.
func (c *Client) StaticMap(ctx context.Context, lat, lon float64) ([]byte, error) {
	center := formatCoord(lat) + "," + formatCoord(lon)

	q := url.Values{}
	q.Set("key", c.apiKey)
	q.Set("center", center)
	q.Set("zoom", mapZoom)
	q.Set("size", mapSize)
	q.Set("format", mapFormat)
	q.Set("maptype", mapType)
	q.Set("markers", "icon:"+mapMarkerIcon+"|"+center)

	body, status, err := c.get(ctx, c.staticMapURL+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("static map request: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("static map returned status %d: %s", status, truncate(body))
	}
	return body, nil
}

// get performs a GET and returns the body and status. A non-2xx is not an
// error here — callers decide what each status means.
func (c *Client) get(ctx context.Context, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, res.StatusCode, err
	}
	return body, res.StatusCode, nil
}

// formatCoord renders a coordinate without a trailing exponent or padding,
// which is what the LocationIQ query parameters expect.
func formatCoord(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func truncate(body []byte) string {
	const max = 200
	if len(body) > max {
		return string(body[:max])
	}
	return string(body)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/locationiq/... -v
```

Expected: PASS, all six tests.

- [ ] **Step 5: Vet and commit**

```bash
make vet
git add internal/locationiq
git commit -m "feat: LocationIQ client for geocoding and static maps"
```

---

### Task 3: Map generation service

The policy layer: cached lookups, generation, single-flight, the deadline, and the abuse/outage guards.

**Files:**
- Create: `internal/propertymap/service.go`
- Test: `internal/propertymap/service_test.go`

**Interfaces:**
- Consumes: `locationiq.Address`, `locationiq.ErrNoMatch` (Task 2); `property.Property.MapImageURL`, `.MapGeneratedAt` (Task 1).
- Produces:
  - `propertymap.New(client Client, up Uploader, store Store, log *slog.Logger) *Service`
  - `(*Service).Ensure(ctx context.Context, p *property.Property) (url string, pending bool)` — **nil-receiver safe**; a nil `*Service` returns `("", false)`.
  - Exported dependency interfaces `Client`, `Uploader`, `Store`.

`Ensure`'s contract, which Task 4 depends on:

| Return | Meaning |
| --- | --- |
| `url != ""` | Map is ready. |
| `url == "", pending == true` | Generation is still running; a later request will have it. |
| `url == "", pending == false` | No map and none coming right now (unmappable, disabled, or guard-denied). |

The deadline lives inside the service, not the handler, so the handler stays thin and the timing policy has one home.

- [ ] **Step 1: Write the failing tests**

Create `internal/propertymap/service_test.go`:

```go
package propertymap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/locationiq"
	"github.com/dwellingtw/backend/internal/property"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeClient counts calls and returns canned results, optionally blocking
// StaticMap so a test can observe the deadline path.
type fakeClient struct {
	lat, lon      float64
	geocodeErr    error
	staticMapErr  error
	geocodeCalls  atomic.Int32
	staticCalls   atomic.Int32
	blockStaticOn chan struct{} // when non-nil, StaticMap waits on it
}

func (f *fakeClient) Geocode(_ context.Context, _ locationiq.Address) (float64, float64, error) {
	f.geocodeCalls.Add(1)
	if f.geocodeErr != nil {
		return 0, 0, f.geocodeErr
	}
	return f.lat, f.lon, nil
}

func (f *fakeClient) StaticMap(_ context.Context, _, _ float64) ([]byte, error) {
	f.staticCalls.Add(1)
	if f.blockStaticOn != nil {
		<-f.blockStaticOn
	}
	if f.staticMapErr != nil {
		return nil, f.staticMapErr
	}
	return []byte("PNG"), nil
}

type fakeUploader struct {
	mu       sync.Mutex
	gotPath  string
	uploaded int
	err      error
}

func (f *fakeUploader) Upload(_ context.Context, path string, _ io.Reader, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploaded++
	f.gotPath = path
	if f.err != nil {
		return "", f.err
	}
	return "https://cdn.example/" + path, nil
}

type fakeStore struct {
	mu        sync.Mutex
	mapURLs   map[string]string
	coords    map[string][2]float64
	mapCalls  int
	coordCall int
}

func newFakeStore() *fakeStore {
	return &fakeStore{mapURLs: map[string]string{}, coords: map[string][2]float64{}}
}

func (f *fakeStore) SetMapImage(_ context.Context, zpid, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mapCalls++
	f.mapURLs[zpid] = url
	return nil
}

func (f *fakeStore) SetCoordinates(_ context.Context, zpid string, lat, lon float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.coordCall++
	f.coords[zpid] = [2]float64{lat, lon}
	return nil
}

func f64p(v float64) *float64 { return &v }

func sampleProp() *property.Property {
	return &property.Property{
		ZPID: "Z1", Address: "1234 Hilltop Drive",
		City: "Austin", State: "TX", Zip: "78746",
	}
}

func newTestService(c Client, u Uploader, s Store) *Service {
	svc := New(c, u, s, testLogger())
	svc.deadline = 2 * time.Second // generous: these fakes are fast
	return svc
}

func TestEnsure_ReturnsExistingURLWithoutAPICalls(t *testing.T) {
	c, u, s := &fakeClient{}, &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	p := sampleProp()
	p.MapImageURL = "https://cdn.example/maps/Z1.png"

	url, pending := svc.Ensure(context.Background(), p)
	if url != "https://cdn.example/maps/Z1.png" || pending {
		t.Errorf("Ensure = (%q, %v)", url, pending)
	}
	if c.geocodeCalls.Load() != 0 || c.staticCalls.Load() != 0 {
		t.Error("cached map must not call LocationIQ")
	}
}

func TestEnsure_PermanentlyUnmappableMakesNoAPICalls(t *testing.T) {
	c, u, s := &fakeClient{}, &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	now := time.Now()
	p := sampleProp()
	p.MapGeneratedAt = &now // stamped, but MapImageURL empty

	url, pending := svc.Ensure(context.Background(), p)
	if url != "" || pending {
		t.Errorf("Ensure = (%q, %v), want empty and not pending", url, pending)
	}
	if c.geocodeCalls.Load() != 0 || c.staticCalls.Load() != 0 {
		t.Error("unmappable row must not call LocationIQ")
	}
}

func TestEnsure_GeocodesThenGeneratesAndPersists(t *testing.T) {
	c := &fakeClient{lat: 30.2672, lon: -97.7431}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	url, pending := svc.Ensure(context.Background(), sampleProp())
	if pending {
		t.Fatal("fast fakes should finish inside the deadline")
	}
	if url != "https://cdn.example/maps/Z1.png" {
		t.Errorf("url = %q", url)
	}
	if u.gotPath != "maps/Z1.png" {
		t.Errorf("upload path = %q, want maps/Z1.png", u.gotPath)
	}
	if got := s.coords["Z1"]; got != [2]float64{30.2672, -97.7431} {
		t.Errorf("coords written = %v", got)
	}
	if s.mapURLs["Z1"] != url {
		t.Errorf("stored url = %q, want %q", s.mapURLs["Z1"], url)
	}
}

func TestEnsure_SkipsGeocodeWhenCoordsPresent(t *testing.T) {
	c := &fakeClient{}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	p := sampleProp()
	p.Latitude, p.Longitude = f64p(30.2672), f64p(-97.7431)

	if _, pending := svc.Ensure(context.Background(), p); pending {
		t.Fatal("unexpected pending")
	}
	if c.geocodeCalls.Load() != 0 {
		t.Error("must not geocode when coordinates are already known")
	}
	if c.staticCalls.Load() != 1 {
		t.Errorf("static map calls = %d, want 1", c.staticCalls.Load())
	}
	if s.coordCall != 0 {
		t.Error("must not rewrite coordinates it did not fetch")
	}
}

func TestEnsure_NoMatchRecordsUnmappableRow(t *testing.T) {
	c := &fakeClient{geocodeErr: locationiq.ErrNoMatch}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	url, pending := svc.Ensure(context.Background(), sampleProp())
	if url != "" || pending {
		t.Errorf("Ensure = (%q, %v)", url, pending)
	}
	if s.mapCalls != 1 || s.mapURLs["Z1"] != "" {
		t.Errorf("want one SetMapImage with an empty url, got %d calls %q", s.mapCalls, s.mapURLs["Z1"])
	}
	if c.staticCalls.Load() != 0 {
		t.Error("must not fetch a map without coordinates")
	}
}

func TestEnsure_TransientFailureLeavesRowAloneAndCoolsDown(t *testing.T) {
	c := &fakeClient{lat: 1, lon: 2, staticMapErr: errors.New("locationiq 503")}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	if url, _ := svc.Ensure(context.Background(), sampleProp()); url != "" {
		t.Errorf("url = %q, want empty", url)
	}
	if s.mapCalls != 0 {
		t.Error("a transient failure must not stamp map_generated_at")
	}

	// Second attempt is refused by the cooldown, so no new API calls.
	before := c.staticCalls.Load()
	if url, pending := svc.Ensure(context.Background(), sampleProp()); url != "" || pending {
		t.Errorf("Ensure during cooldown = (%q, %v)", url, pending)
	}
	if c.staticCalls.Load() != before {
		t.Error("cooldown must suppress the retry")
	}
}

func TestEnsure_ReturnsPendingWhenGenerationOutlastsDeadline(t *testing.T) {
	release := make(chan struct{})
	c := &fakeClient{lat: 1, lon: 2, blockStaticOn: release}
	u, s := &fakeUploader{}, newFakeStore()

	svc := New(c, u, s, testLogger())
	svc.deadline = 20 * time.Millisecond

	url, pending := svc.Ensure(context.Background(), sampleProp())
	if url != "" || !pending {
		t.Fatalf("Ensure = (%q, %v), want empty and pending", url, pending)
	}

	// Generation continues in the background and still persists.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		done := s.mapURLs["Z1"] != ""
		s.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background generation never persisted the map")
}

func TestEnsure_CancelledRequestDoesNotKillGeneration(t *testing.T) {
	release := make(chan struct{})
	c := &fakeClient{lat: 1, lon: 2, blockStaticOn: release}
	u, s := &fakeUploader{}, newFakeStore()

	svc := New(c, u, s, testLogger())
	svc.deadline = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	if _, pending := svc.Ensure(ctx, sampleProp()); !pending {
		t.Fatal("want pending")
	}
	cancel() // viewer navigates away
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		done := s.mapURLs["Z1"] != ""
		s.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("generation must survive request cancellation")
}

func TestEnsure_SingleFlightCollapsesConcurrentViews(t *testing.T) {
	release := make(chan struct{})
	c := &fakeClient{lat: 1, lon: 2, blockStaticOn: release}
	u, s := &fakeUploader{}, newFakeStore()

	svc := New(c, u, s, testLogger())
	svc.deadline = 2 * time.Second

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.Ensure(context.Background(), sampleProp())
		}()
	}
	// Let all ten reach the single-flight before releasing the blocked call.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := c.geocodeCalls.Load(); got != 1 {
		t.Errorf("geocode calls = %d, want 1", got)
	}
	if got := c.staticCalls.Load(); got != 1 {
		t.Errorf("static map calls = %d, want 1", got)
	}
	if u.uploaded != 1 {
		t.Errorf("uploads = %d, want 1", u.uploaded)
	}
}

func TestEnsure_HourlyBudgetDeniesGeneration(t *testing.T) {
	c := &fakeClient{lat: 1, lon: 2}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)
	svc.budget = 2

	for i, zpid := range []string{"A", "B", "C"} {
		p := sampleProp()
		p.ZPID = zpid
		url, _ := svc.Ensure(context.Background(), p)
		if i < 2 && url == "" {
			t.Errorf("call %d denied, want allowed", i)
		}
		if i == 2 && url != "" {
			t.Error("third call should exceed the budget")
		}
	}
	if got := c.staticCalls.Load(); got != 2 {
		t.Errorf("static map calls = %d, want 2", got)
	}
}

func TestEnsure_NilServiceIsDisabled(t *testing.T) {
	var svc *Service
	if url, pending := svc.Ensure(context.Background(), sampleProp()); url != "" || pending {
		t.Errorf("nil service Ensure = (%q, %v)", url, pending)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/propertymap/... -v
```

Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Write the service**

Create `internal/propertymap/service.go`:

```go
// Package propertymap generates a property's static map on demand: geocode if
// needed, fetch the pinned map, store it on the CDN, and persist the URL.
package propertymap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/dwellingtw/backend/internal/locationiq"
	"github.com/dwellingtw/backend/internal/property"
	"golang.org/x/sync/singleflight"
)

// Defaults. Overridable on the struct by tests in this package.
const (
	// defaultDeadline is how long a detail request waits for a map before
	// giving up and letting generation finish in the background.
	defaultDeadline = 1500 * time.Millisecond
	// backgroundTimeout bounds generation once the request has moved on.
	backgroundTimeout = 20 * time.Second
	// defaultCooldown is how long a zpid is skipped after a transient failure,
	// so a LocationIQ outage cannot turn every request into a retry.
	defaultCooldown = 10 * time.Minute
	// defaultBudget caps generations per rolling hour. The detail endpoint is
	// public and unauthenticated, so this bounds quota burn from enumeration.
	defaultBudget = 500
)

// Client is the LocationIQ surface this service needs.
type Client interface {
	Geocode(ctx context.Context, a locationiq.Address) (lat, lon float64, err error)
	StaticMap(ctx context.Context, lat, lon float64) ([]byte, error)
}

// Uploader stores content and returns its public CDN URL.
type Uploader interface {
	Upload(ctx context.Context, path string, content io.Reader, contentType string) (string, error)
}

// Store persists what generation produces.
type Store interface {
	SetMapImage(ctx context.Context, zpid, url string) error
	SetCoordinates(ctx context.Context, zpid string, lat, lon float64) error
}

// Service generates and caches property maps.
type Service struct {
	client   Client
	uploader Uploader
	store    Store
	log      *slog.Logger
	group    singleflight.Group

	// Tunables, defaulted in New and overridden by tests.
	deadline time.Duration
	cooldown time.Duration
	budget   int
	now      func() time.Time

	mu          sync.Mutex
	coolUntil   map[string]time.Time
	windowStart time.Time
	windowCount int
}

// New creates the service. Pass a nil *Service to consumers to disable maps —
// Ensure is nil-receiver safe.
func New(client Client, up Uploader, store Store, log *slog.Logger) *Service {
	return &Service{
		client:    client,
		uploader:  up,
		store:     store,
		log:       log,
		deadline:  defaultDeadline,
		cooldown:  defaultCooldown,
		budget:    defaultBudget,
		now:       time.Now,
		coolUntil: map[string]time.Time{},
	}
}

// Ensure returns the property's map URL, generating it when absent.
//
//	url != ""              — the map is ready
//	url == "", pending      — generation is still running in the background
//	url == "", !pending     — no map, and none is coming right now
//
// It waits at most s.deadline. Generation that outlives the deadline keeps
// running on a background context and persists its result for the next reader.
func (s *Service) Ensure(ctx context.Context, p *property.Property) (string, bool) {
	if s == nil || p == nil || p.ZPID == "" {
		return "", false
	}
	if p.MapImageURL != "" {
		return p.MapImageURL, false
	}
	// Stamped with no URL: the address could not be geocoded. Never retried.
	if p.MapGeneratedAt != nil {
		return "", false
	}
	if !s.allow(p.ZPID) {
		return "", false
	}

	// Detach from the request: a viewer navigating away must not abort a
	// half-finished map.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundTimeout)

	done := make(chan string, 1) // buffered: the goroutine never blocks on a timed-out caller
	go func() {
		defer cancel()
		url, err := s.generate(bg, p)
		if err != nil {
			s.penalize(p.ZPID)
			s.log.Warn("map generation failed", "zpid", p.ZPID, "error", err)
		}
		done <- url
	}()

	timer := time.NewTimer(s.deadline)
	defer timer.Stop()
	select {
	case url := <-done:
		return url, false
	case <-timer.C:
		return "", true
	}
}

// generate does the work, collapsed per zpid so concurrent viewers of the same
// listing produce exactly one geocode and one map fetch.
func (s *Service) generate(ctx context.Context, p *property.Property) (string, error) {
	v, err, _ := s.group.Do(p.ZPID, func() (any, error) {
		lat, lon := p.Latitude, p.Longitude

		if lat == nil || lon == nil {
			gotLat, gotLon, err := s.client.Geocode(ctx, locationiq.Address{
				Street:     p.Address,
				City:       p.City,
				State:      p.State,
				PostalCode: p.Zip,
			})
			switch {
			case errors.Is(err, locationiq.ErrNoMatch):
				// Permanent: record an unmappable row so it is never retried.
				if err := s.store.SetMapImage(ctx, p.ZPID, ""); err != nil {
					return "", err
				}
				s.log.Info("address not geocodable, marked unmappable", "zpid", p.ZPID)
				return "", nil
			case err != nil:
				return "", err
			}
			if err := s.store.SetCoordinates(ctx, p.ZPID, gotLat, gotLon); err != nil {
				return "", err
			}
			lat, lon = &gotLat, &gotLon
		}

		png, err := s.client.StaticMap(ctx, *lat, *lon)
		if err != nil {
			return "", err
		}

		url, err := s.uploader.Upload(ctx, "maps/"+p.ZPID+".png", bytes.NewReader(png), "image/png")
		if err != nil {
			return "", err
		}
		if err := s.store.SetMapImage(ctx, p.ZPID, url); err != nil {
			return "", err
		}
		s.log.Info("map generated", "zpid", p.ZPID, "url", url)
		return url, nil
	})
	if err != nil {
		return "", err
	}
	url, _ := v.(string)
	return url, nil
}

// allow applies the per-zpid cooldown and the rolling hourly budget, counting
// the generation when it permits one.
func (s *Service) allow(zpid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if until, ok := s.coolUntil[zpid]; ok {
		if now.Before(until) {
			return false
		}
		delete(s.coolUntil, zpid)
	}

	if s.windowStart.IsZero() || now.Sub(s.windowStart) >= time.Hour {
		s.windowStart = now
		s.windowCount = 0
	}
	if s.windowCount >= s.budget {
		s.log.Warn("map generation budget exhausted", "zpid", zpid, "budget", s.budget)
		return false
	}
	s.windowCount++
	return true
}

// penalize puts a zpid on cooldown after a transient failure, pruning entries
// that have already expired so the map does not grow without bound.
func (s *Service) penalize(zpid string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for k, until := range s.coolUntil {
		if now.After(until) {
			delete(s.coolUntil, k)
		}
	}
	s.coolUntil[zpid] = now.Add(s.cooldown)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/propertymap/... -v
```

Expected: PASS, all eleven tests.

- [ ] **Step 5: Run the race detector**

This package is concurrent by design — single-flight, a background goroutine, and shared guard state. The race detector is the point.

```bash
go test -race ./internal/propertymap/...
```

Expected: PASS with no `DATA RACE` reports.

- [ ] **Step 6: Vet and commit**

```bash
make vet
git add internal/propertymap
git commit -m "feat: on-demand property map generation service"
```

---

### Task 4: API wiring

Expose `map_image_url` on the detail response, call the service, and shorten the cache when a map is still pending.

**Files:**
- Modify: `internal/api/dto.go:38-67` (`detailResponse`) and `:82-106` (`toDetailResponse`)
- Modify: `internal/api/api.go:19-34` (`API`, `New`), `:104-116` (`handleDetail`), `:131-139` (`writeJSON`)
- Modify: `internal/api/api_test.go:62-69` (the `serve` helper)

**Interfaces:**
- Consumes: `(*propertymap.Service).Ensure` (Task 3), via a locally declared interface.
- Produces:
  - `api.MapEnsurer` interface — `Ensure(ctx context.Context, p *property.Property) (string, bool)`.
  - `api.New(repo Repo, maps MapEnsurer, log *slog.Logger) *API` — **signature change**, `maps` may be nil.

**Note:** `api.New` gains a parameter, which breaks `cmd/server/main.go:78`. Task 5 fixes that. Between these two tasks `go build ./...` fails at that one call site; `go test ./internal/api/...` still passes. Do not "fix" main here — Task 5 wires it properly with the config.

- [ ] **Step 1: Write the failing tests**

In `internal/api/api_test.go`, first update the `serve` helper (line 62-69) to take a `MapEnsurer`:

```go
func serve(t *testing.T, repo Repo, target string) *httptest.ResponseRecorder {
	t.Helper()
	return serveWithMaps(t, repo, nil, target)
}

func serveWithMaps(t *testing.T, repo Repo, maps MapEnsurer, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	New(repo, maps, testLogger()).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}
```

Every existing call site keeps using `serve` unchanged, which now exercises the maps-disabled path.

Then append these tests to the same file:

```go
// fakeMaps returns a canned Ensure result and records that it was called.
type fakeMaps struct {
	url     string
	pending bool
	calls   int
}

func (f *fakeMaps) Ensure(_ context.Context, _ *property.Property) (string, bool) {
	f.calls++
	return f.url, f.pending
}

func TestDetail_MapDisabledYieldsNullMapAndNormalCaching(t *testing.T) {
	p := sampleProp("Z1", 1)
	rec := serve(t, &fakeRepo{detail: &p}, "/api/v1/properties/Z1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	v, ok := body["map_image_url"]
	if !ok {
		t.Fatal("map_image_url missing from detail response")
	}
	if v != nil {
		t.Errorf("map_image_url = %v, want null", v)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Errorf("Cache-Control = %q, want the default", got)
	}
}

func TestDetail_StoredMapIsReturned(t *testing.T) {
	p := sampleProp("Z1", 1)
	p.MapImageURL = "https://cdn.example/maps/Z1.png"
	maps := &fakeMaps{url: p.MapImageURL}

	rec := serveWithMaps(t, &fakeRepo{detail: &p}, maps, "/api/v1/properties/Z1")

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["map_image_url"] != "https://cdn.example/maps/Z1.png" {
		t.Errorf("map_image_url = %v", body["map_image_url"])
	}
}

func TestDetail_FreshlyGeneratedMapIsReturned(t *testing.T) {
	p := sampleProp("Z1", 1)
	maps := &fakeMaps{url: "https://cdn.example/maps/Z1.png"}

	rec := serveWithMaps(t, &fakeRepo{detail: &p}, maps, "/api/v1/properties/Z1")

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["map_image_url"] != "https://cdn.example/maps/Z1.png" {
		t.Errorf("map_image_url = %v", body["map_image_url"])
	}
	if maps.calls != 1 {
		t.Errorf("Ensure calls = %d, want 1", maps.calls)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Errorf("Cache-Control = %q, want the default", got)
	}
}

func TestDetail_PendingMapReturnsNullWithShortCache(t *testing.T) {
	p := sampleProp("Z1", 1)
	maps := &fakeMaps{pending: true}

	rec := serveWithMaps(t, &fakeRepo{detail: &p}, maps, "/api/v1/properties/Z1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["map_image_url"] != nil {
		t.Errorf("map_image_url = %v, want null while pending", body["map_image_url"])
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=30" {
		t.Errorf("Cache-Control = %q, want the short pending value", got)
	}
}

func TestDetail_NotFoundSkipsMapGeneration(t *testing.T) {
	maps := &fakeMaps{}
	rec := serveWithMaps(t, &fakeRepo{detailErr: property.ErrNotFound}, maps, "/api/v1/properties/nope")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if maps.calls != 0 {
		t.Error("a 404 must not trigger map generation")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/api/... -v -run TestDetail
```

Expected: FAIL to compile — `undefined: MapEnsurer` and `New` called with 3 arguments.

- [ ] **Step 3: Add the DTO field**

In `internal/api/dto.go`, in `detailResponse`, immediately after the `Longitude` line:

```go
	MapImageURL   *string   `json:"map_image_url"`
```

And in `toDetailResponse`, after the `if p.VideoURL != "" { ... }` block:

```go
	if p.MapImageURL != "" {
		d.MapImageURL = &p.MapImageURL
	}
```

- [ ] **Step 4: Wire the handler**

In `internal/api/api.go`, add the interface below the `Repo` declaration:

```go
// MapEnsurer returns a property's static map URL, generating it on demand.
// url is empty when there is no map; pending means one is still being
// generated in the background. May be nil when maps are disabled.
type MapEnsurer interface {
	Ensure(ctx context.Context, p *property.Property) (url string, pending bool)
}
```

Change the struct and constructor:

```go
// API serves the public read-only listings endpoints.
type API struct {
	repo Repo
	maps MapEnsurer
	log  *slog.Logger
}

// New creates the public API. maps may be nil, which disables property maps.
func New(repo Repo, maps MapEnsurer, log *slog.Logger) *API {
	return &API{repo: repo, maps: maps, log: log}
}
```

Replace the final line of `handleDetail` (`writeJSON(w, http.StatusOK, toDetailResponse(p))`) with:

```go
	resp := toDetailResponse(p)

	// Generate the map on demand. Ensure waits a short while; if it is still
	// working we return a null map and shorten the cache so the next viewer
	// picks up the finished one quickly.
	pending := false
	if a.maps != nil {
		if url, stillWorking := a.maps.Ensure(r.Context(), p); url != "" {
			resp.MapImageURL = &url
		} else {
			pending = stillWorking
		}
	}

	cacheControl := defaultCacheControl
	if pending {
		cacheControl = pendingCacheControl
	}
	writeJSONCache(w, http.StatusOK, resp, cacheControl)
```

Also add the `@Header` note to the handler's Swagger block, directly under the `@Description` line, so the behavior is documented in the published spec:

```go
//	@Description	map_image_url is generated on first view; it may be null on the very first request for a listing and populated shortly after.
```

- [ ] **Step 5: Split the cache-control helper**

In `internal/api/api.go`, replace `writeJSON` with:

```go
const (
	defaultCacheControl = "public, max-age=300"
	// pendingCacheControl is used when a map is still generating, so the null
	// map is not cached for the full window.
	pendingCacheControl = "public, max-age=30"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	writeJSONCache(w, status, v, defaultCacheControl)
}

func writeJSONCache(w http.ResponseWriter, status int, v any, cacheControl string) {
	if status < 400 {
		w.Header().Set("Cache-Control", cacheControl)
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 6: Run the API tests**

```bash
go test ./internal/api/... -v
```

Expected: PASS — the five new detail tests plus every pre-existing test.

- [ ] **Step 7: Commit**

```bash
git add internal/api
git commit -m "feat: serve map_image_url from the detail endpoint"
```

---

### Task 5: Configuration, startup wiring, and documentation

Turn the feature on end to end and document it. This is the task that makes `go build ./...` whole again.

**Files:**
- Modify: `internal/config/config.go:20-22` (API key block) and `:104-106` (Load)
- Modify: `cmd/server/main.go:57-60` (clients) and `:78` (api.New)
- Modify: `.env.example`, `README.md`
- Regenerate: `docs/` (swagger)

**Interfaces:**
- Consumes: `locationiq.New` (Task 2), `propertymap.New` (Task 3), `api.New`'s new signature (Task 4).
- Produces: `config.Config.LocationIQAPIKey string` — empty means maps disabled.

- [ ] **Step 1: Add the config field**

In `internal/config/config.go`, in the `Config` struct after the Zillow block:

```go
	// LocationIQ — static property maps generated on demand by the detail
	// endpoint. Empty disables maps entirely.
	LocationIQAPIKey string
```

And in `Load`, after the `ZillowAPIKey` line:

```go
		LocationIQAPIKey: getenv("LOCATIONIQ_API_KEY", ""),
```

Do **not** add it to the `missing` required-variable checks — it is optional by design.

- [ ] **Step 2: Wire startup**

In `cmd/server/main.go`, after the `repo := property.NewRepository(pool)` line:

```go
	// Property maps are optional: without a LocationIQ key the service stays
	// nil and the detail endpoint simply returns a null map_image_url.
	var mapSvc *propertymap.Service
	if cfg.LocationIQAPIKey != "" {
		mapSvc = propertymap.New(
			locationiq.New(cfg.LocationIQAPIKey, cfg.HTTPTimeout),
			bunnyClient, repo, log,
		)
		log.Info("property maps enabled")
	}
```

Change the `api.New` call:

```go
	publicAPI := api.New(repo, mapEnsurerOrNil(mapSvc), log)
```

Add the helper next to `rendererOrNil` at the bottom of the file:

```go
// mapEnsurerOrNil returns an interface-nil when maps are disabled, so the API's
// nil check works correctly.
func mapEnsurerOrNil(s *propertymap.Service) api.MapEnsurer {
	if s == nil {
		return nil
	}
	return s
}
```

Add the two imports:

```go
	"github.com/dwellingtw/backend/internal/locationiq"
	"github.com/dwellingtw/backend/internal/propertymap"
```

- [ ] **Step 3: Build and run the full suite**

```bash
make vet && make test && make build
```

Expected: PASS, and `go build ./...` succeeds — Task 4's broken call site is now fixed.

- [ ] **Step 4: Document the environment variable**

In `.env.example`, after the Zillow API key block:

```
# LocationIQ — static property maps, generated on demand by the detail
# endpoint and hosted on Bunny CDN. Leave empty to disable maps entirely.
LOCATIONIQ_API_KEY=
```

- [ ] **Step 5: Document the behavior in the README**

In `README.md`, in the `GET /api/v1/properties/{zpid}` bullet under `## API`, extend the enrichment-field sentence to mention the map. Replace `latitude`, `longitude`, `lot_size_acres`) that are `null` ...` with:

```markdown
  `latitude`, `longitude`, `lot_size_acres`) that are `null` until the
  scheduler's details-enrichment step fills them in, plus `map_image_url` — a
  static LocationIQ map with a pin on the home, hosted on Bunny CDN.
```

And add a subsection after the `### Listing videos` section:

```markdown
### Property maps

The detail endpoint returns `map_image_url`: a 600×400 static map from
[LocationIQ](https://locationiq.com) with a pin on the home, stored at
`maps/<zpid>.png` on Bunny CDN.

Maps are generated **on demand** — the first request for a listing that has no
map triggers generation, so listings nobody views cost nothing. If the property
has no coordinates yet, its address is geocoded first and the coordinates are
saved back to the row.

The request waits up to 1.5s. If generation takes longer it finishes in the
background, the response carries `map_image_url: null` with a shortened
`max-age=30`, and the next request serves the finished map. Addresses that
cannot be geocoded are recorded once and never retried.

Set `LOCATIONIQ_API_KEY` to enable maps; without it `map_image_url` is always
`null`.
```

Also add `internal/locationiq` and `internal/propertymap` to the architecture tree in the README, after the `internal/api` line:

```
internal/locationiq     LocationIQ geocoding + static map client
internal/propertymap    on-demand property map generation
```

- [ ] **Step 6: Regenerate the Swagger spec**

```bash
make swagger
git status --short docs/
```

Expected: `docs/docs.go`, `docs/swagger.json`, and `docs/swagger.yaml` are modified. Confirm `map_image_url` made it in:

```bash
grep -c map_image_url docs/swagger.json
```

Expected: at least 1.

- [ ] **Step 7: Verify end to end against a real LocationIQ key**

This is the only step that proves the real LocationIQ URLs, parameters, and response shapes are right — every test so far used an `httptest` fake. It needs the operator's key.

Put the real key in `.env` (not `.env.example`, which stays empty), then:

```bash
make up
make logs   # wait for "property maps enabled" and a saved listing, then Ctrl-C
```

```bash
ZPID=$(docker compose exec -T db psql -U dwellings -d dwellings -tAc "SELECT zpid FROM properties LIMIT 1")
curl -fsS "http://localhost:8080/api/v1/properties/$ZPID" | grep -o '"map_image_url":[^,]*'
```

Expected: on the first call, either a populated Bunny URL or `null` (generation exceeded the 1.5s deadline). Re-run the same curl a few seconds later — it must now be a populated URL.

Then confirm the image is real and pinned:

```bash
curl -fsS -o /tmp/map.png "$(docker compose exec -T db psql -U dwellings -d dwellings -tAc "SELECT map_image_url FROM properties WHERE zpid='$ZPID'")"
file /tmp/map.png
```

Expected: `PNG image data, 600 x 400`. Open it and confirm a red pin sits on the property.

Check the logs for the failure paths having stayed quiet:

```bash
docker compose logs app | grep -i "map generation failed\|not geocodable"
make down
```

If `map generation failed` appears, the logged error names which call broke — fix it before committing rather than papering over it.

- [ ] **Step 8: Commit**

```bash
git add internal/config cmd/server/main.go .env.example README.md docs/
git commit -m "feat: enable LocationIQ property maps via LOCATIONIQ_API_KEY"
```

- [ ] **Step 9: Final verification**

```bash
make vet && make test && make build
git status --short
```

Expected: all pass, working tree clean.

---

## Self-Review Notes

Checked against `docs/superpowers/specs/2026-07-29-property-maps-locationiq-design.md`:

- Every spec section maps to a task: client → Task 2; service and single-flight → Task 3; deadline, `pending`, cache-control → Tasks 3 and 4; failure semantics table → Task 3 Steps 1/3; guards → Task 3; schema and repository methods → Task 1; API surface → Task 4; configuration and rollout → Task 5; testing → distributed across Tasks 2–4 plus the live check in Task 5 Step 7.
- `Ensure(ctx, *property.Property) (string, bool)` is used with identical arity and meaning in Task 3 (definition), Task 4 (`MapEnsurer`, `fakeMaps`), and Task 5 (`mapEnsurerOrNil`).
- `SetMapImage(ctx, zpid, url)` and `SetCoordinates(ctx, zpid, lat, lon)` match between Task 1's implementation and Task 3's `Store` interface and fakes.
- The `api.New` signature change is called out explicitly in Task 4 as breaking `main.go` until Task 5, so an implementer working one task at a time is not surprised by a failing `go build ./...`.
- Deviation from the spec, deliberate: the spec put the 1.5s `select` in the handler; the plan puts it inside `Ensure`. Same observable contract, but the timing policy lives with the rest of the generation policy and the handler stays thin.
