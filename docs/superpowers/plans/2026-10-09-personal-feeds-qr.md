# Personal Feeds + Dynamic QR Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every TV a personal HLS feed URL whose content the server reroutes when the household picks a ZIP or city on a QR-launched mobile page.

**Architecture:** A household is the existing IP-kind viewer hash. Its feed is a chain of *spans* (`household_spans`), each "channel X from time S on" with sequence/item offsets so the personal playlist, spliced from the untouched scope channels' windows, keeps `MEDIA-SEQUENCE` and `DISCONTINUITY-SEQUENCE` climbing across a switch. New HTTP routes under `/feed/` and `/tv/` live in `internal/linear` next to the channel handlers; the mobile page is one embedded HTML file.

**Tech Stack:** Go 1.26 (`net/http` ServeMux patterns), pgx/PostgreSQL, `skip2/go-qrcode`, hls.js (CDN, mobile page only).

**Spec:** `docs/superpowers/specs/2026-10-09-personal-feeds-qr-design.md`

## Global Constraints

- Scope channels (`channel_lineups`, `Playlist`, `writePlaylist`) stay byte-for-byte deterministic; the personal layer only relabels their segments. Never modify `channel_lineups` rows or `video_hls` rows.
- `household_spans` rows are immutable except `ReplaceSpan` of a span that has not aired yet.
- All new service code uses `s.now()` (never `time.Now()`), so tests drive the clock.
- Household id = `Hasher.Household(ip)`; a raw IP is never stored or logged.
- Every `/feed/*` and `/tv/*` response that depends on the connecting IP is `Cache-Control: no-store`; only `/feed/live.m3u8?hh=` (household in the URL) is `public, max-age=2`.
- No new Go dependencies. The mobile page loads only `https://cdnjs.cloudflare.com/ajax/libs/hls.js/1.5.13/hls.min.js` (verified 200 on 2026-10-09).
- Run `gofmt -l`, `go vet ./...` and `go test ./...` before every commit. Commit messages end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- Work in the worktree `/Users/marko/Projects/Dwellings/backend.worktrees/feature/dynamic-qr` on branch `feature/dynamic-qr`.

## Review Focus

1. A well-formed but unknown `hh` on `/feed/live.m3u8` must be a 404, never a 500 or an empty playlist (test in Task 7).
2. POST bodies that are not JSON, exceed 1 KB, give a 4-digit ZIP, or a city without a state must all be 400 with a message naming the problem (tests in Task 7).
3. A second submit before the first segment of the new channel has aired must replace the pending span, and counters must stay monotonic through both switches (test in Task 6).
4. A switch whose start `S` falls inside the last segment of the new channel's current lineup version must roll to the next version rather than fail (test in Task 6).
5. A ZIP that exists but is too thin must fall back, keep `requested`, and the page must say so instead of claiming the ZIP (tests in Task 6 and Task 7; copy in Task 8).

---

### Task 1: Household identity in `internal/viewer`

**Files:**
- Modify: `internal/viewer/ident.go`
- Test: `internal/viewer/household_test.go` (new)

**Interfaces:**
- Produces: `func (h *Hasher) Household(ip string) ID` and `func ParseID(s string) (ID, error)`. `ID.String()` (hex, 32 chars) already exists.

- [ ] **Step 1: Write the failing tests**

```go
package viewer

import (
	"strings"
	"testing"
	"time"
)

func TestHousehold_IsTheIPHashIgnoringSID(t *testing.T) {
	h := NewHasher("salt", false)
	const ip = "203.0.113.5"
	if got, want := h.Household(ip), h.IDAt("", ip, time.Time{}); got != want {
		t.Errorf("Household = %s, want the ip-kind id %s", got, want)
	}
	if h.Household(ip) == h.IDAt("roku-1", ip, time.Time{}) {
		t.Error("Household must not depend on a sid")
	}
	if h.Household(ip) == h.Household("203.0.113.6") {
		t.Error("different IPs must be different households")
	}
}

func TestParseID_RoundTrip(t *testing.T) {
	id := NewHasher("salt", false).Household("203.0.113.5")
	got, err := ParseID(id.String())
	if err != nil || got != id {
		t.Fatalf("ParseID(%s) = %s, %v", id, got, err)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("zz", 16), strings.Repeat("ab", 17), strings.ToUpper(id.String()) + "x"} {
		if _, err := ParseID(bad); err == nil {
			t.Errorf("ParseID(%q) accepted", bad)
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/viewer/ -run 'TestHousehold|TestParseID' -v`
Expected: FAIL to compile with "h.Household undefined" / "undefined: ParseID".

- [ ] **Step 3: Implement**

Add to `internal/viewer/ident.go`, after `IDAt` (add `"fmt"` to the imports):

```go
// Household identifies the home a request comes from: the ip-kind hash of
// its public address, whatever sid it carries. A TV and a phone on the same
// home network share it, which is how the mobile page finds the TV's feed.
func (h *Hasher) Household(ip string) ID { return h.IDAt("", ip, h.now()) }

// ParseID reads the hex form ID.String produces.
func ParseID(s string) (ID, error) {
	var id ID
	if len(s) != 2*len(id) {
		return id, fmt.Errorf("viewer: id must be %d hex characters", 2*len(id))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("viewer: bad id: %w", err)
	}
	copy(id[:], b)
	return id, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/viewer/ -v`
Expected: PASS (all, including the existing ident tests).

- [ ] **Step 5: Commit**

```bash
git add internal/viewer/ident.go internal/viewer/household_test.go
git commit -m "viewer: household id (ip-kind hash) and ParseID

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: `qrcode.PNG` returns bytes

**Files:**
- Modify: `internal/qrcode/qrcode.go`
- Test: `internal/qrcode/qrcode_test.go` (new)

**Interfaces:**
- Produces: `func PNG(content string, size int) ([]byte, error)`. `WritePNG` keeps its signature and calls it.

- [ ] **Step 1: Write the failing test**

```go
package qrcode

import (
	"bytes"
	"image/png"
	"testing"
)

func TestPNG_RendersSquareImage(t *testing.T) {
	b, err := PNG("https://dwellings.tv/tv/0123456789abcdef0123456789abcdef", 300)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	if r := img.Bounds(); r.Dx() != 300 || r.Dy() != 300 {
		t.Errorf("size = %dx%d, want 300x300", r.Dx(), r.Dy())
	}
}

func TestPNG_RejectsEmptyContent(t *testing.T) {
	if _, err := PNG("", 300); err == nil {
		t.Error("empty content accepted")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/qrcode/ -v`
Expected: FAIL to compile, "undefined: PNG".

- [ ] **Step 3: Implement**

Replace the body of `internal/qrcode/qrcode.go` below the imports with:

```go
// PNG renders content as a QR code PNG, size pixels per side, medium error
// correction, with the quiet zone the scanner needs.
func PNG(content string, size int) ([]byte, error) {
	if content == "" {
		return nil, fmt.Errorf("qrcode: empty content")
	}
	code, err := qr.New(content, qr.Medium)
	if err != nil {
		return nil, fmt.Errorf("qrcode: build: %w", err)
	}
	png, err := code.PNG(size)
	if err != nil {
		return nil, fmt.Errorf("qrcode: encode png: %w", err)
	}
	return png, nil
}

// WritePNG renders content as a QR code PNG of the given pixel size to path.
func WritePNG(content, path string, size int) error {
	png, err := PNG(content, size)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, png, 0o644); err != nil {
		return fmt.Errorf("qrcode: write %s: %w", path, err)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/qrcode/ ./internal/video/`
Expected: PASS (video's QR tests still pass; the local render test skips as before).

- [ ] **Step 5: Commit**

```bash
git add internal/qrcode/
git commit -m "qrcode: PNG returns the encoded bytes

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Span types, Store extension, in-memory store

**Files:**
- Modify: `internal/linear/store.go`
- Modify: `internal/linear/store_test.go` (the `memStore` fake)
- Test: `internal/linear/memstore_spans_test.go` (new)

**Interfaces:**
- Produces: `type Span`, `type City`, constants `SourceChoice/SourceGeo/SourceDefault`, and four new `Store` methods: `Spans`, `InsertSpan`, `ReplaceSpan`, `ListCities` (exact signatures below). Task 4 implements them in SQL; Tasks 5–6 call them.

- [ ] **Step 1: Write the failing test**

```go
package linear

import (
	"context"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

func TestMemStore_Spans(t *testing.T) {
	ctx := context.Background()
	m := newMemStore()
	hh := viewer.ID{7}
	if spans, _ := m.Spans(ctx, hh); spans != nil {
		t.Fatalf("unknown household has spans: %+v", spans)
	}
	s0 := &Span{Household: hh, N: 0, Scope: "us", Source: SourceDefault, StartsAt: t0, CreatedAt: t0}
	if ok, err := m.InsertSpan(ctx, s0); err != nil || !ok {
		t.Fatalf("insert span 0: ok=%v err=%v", ok, err)
	}
	if ok, _ := m.InsertSpan(ctx, s0); ok {
		t.Error("duplicate (household, n) must not insert")
	}
	s1 := &Span{Household: hh, N: 1, Scope: "zip:77494", Requested: "zip:77494", Source: SourceChoice, StartsAt: t0.Add(time.Minute), SeqOffset: 5, ItemOffset: 2, CreatedAt: t0.Add(time.Minute)}
	if ok, _ := m.InsertSpan(ctx, s1); !ok {
		t.Fatal("insert span 1")
	}
	s1b := *s1
	s1b.Scope = "state:tx"
	if err := m.ReplaceSpan(ctx, &s1b); err != nil {
		t.Fatal(err)
	}
	if err := m.ReplaceSpan(ctx, &Span{Household: hh, N: 9}); err == nil {
		t.Error("replacing a missing span must fail")
	}
	spans, _ := m.Spans(ctx, hh)
	if len(spans) != 2 || spans[0].N != 0 || spans[1].N != 1 || spans[1].Scope != "state:tx" {
		t.Errorf("spans = %+v", spans)
	}
}

func TestMemStore_ListCities(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 5)
	addClips(m, austin, 101, 2)
	cs, err := m.ListCities(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0] != (City{City: "katy", State: "tx", Clips: 5}) {
		t.Errorf("cities = %+v", cs)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/linear/ -run TestMemStore -v`
Expected: FAIL to compile ("undefined: Span" etc.).

- [ ] **Step 3: Add the types and interface methods**

In `internal/linear/store.go`, add `"github.com/dwellingtw/backend/internal/viewer"` to the imports, then after the `Listing` type:

```go
// Span is one stretch of a household's personal feed (see
// docs/superpowers/specs/2026-10-09-personal-feeds-qr-design.md): from
// StartsAt the household airs channel Scope. Spans chain the way versions
// do: the offsets relabel the channel's segment and item counters so the
// personal playlist's MEDIA-SEQUENCE and DISCONTINUITY-SEQUENCE keep
// climbing across a switch.
type Span struct {
	Household  viewer.ID
	N          int       // 0, 1, 2… in order; the latest is the current choice
	Scope      string    // effective channel key after fallback
	Requested  string    // the key the viewer asked for; "" when none
	Source     string    // SourceChoice, SourceGeo or SourceDefault
	StartsAt   time.Time // when this span starts airing
	SeqOffset  int64     // personal seq = channel seq + SeqOffset
	ItemOffset int64     // personal item = channel item + ItemOffset
	CreatedAt  time.Time
}

// Span sources.
const (
	SourceChoice  = "choice"  // the viewer picked it on the mobile page
	SourceGeo     = "geo"     // the IP's location
	SourceDefault = "default" // national, nothing better known
)

// City is an area the mobile page can offer.
type City struct {
	City  string // lowercase, as in properties.city
	State string // lowercase 2-letter code
	Clips int    // current clips
}
```

And at the end of the `Store` interface, before its closing brace:

```go
	// Spans returns a household's spans oldest first; nil for an unknown
	// household.
	Spans(ctx context.Context, h viewer.ID) ([]Span, error)
	// InsertSpan stores sp unless (household, n) exists; ok reports whether
	// it was stored.
	InsertSpan(ctx context.Context, sp *Span) (ok bool, err error)
	// ReplaceSpan overwrites the stored (household, n) with sp. It is only
	// ever used on a span that has not started airing.
	ReplaceSpan(ctx context.Context, sp *Span) error
	// ListCities returns every city with at least min current clips, ordered
	// by state then city.
	ListCities(ctx context.Context, min int) ([]City, error)
```

- [ ] **Step 4: Extend `memStore`**

In `internal/linear/store_test.go`: add `"github.com/dwellingtw/backend/internal/viewer"` to the imports; add the field `spans map[viewer.ID][]Span` to the `memStore` struct; initialise it in `newMemStore` (`spans: map[viewer.ID][]Span{}`); then append:

```go
func (m *memStore) Spans(_ context.Context, h viewer.ID) ([]Span, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.spans[h]) == 0 {
		return nil, nil
	}
	out := append([]Span(nil), m.spans[h]...)
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out, nil
}

func (m *memStore) InsertSpan(_ context.Context, sp *Span) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.spans[sp.Household] {
		if e.N == sp.N {
			return false, nil
		}
	}
	m.spans[sp.Household] = append(m.spans[sp.Household], *sp)
	return true, nil
}

func (m *memStore) ReplaceSpan(_ context.Context, sp *Span) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, e := range m.spans[sp.Household] {
		if e.N == sp.N {
			m.spans[sp.Household][i] = *sp
			return nil
		}
	}
	return fmt.Errorf("replace span %d of %s: no such span", sp.N, sp.Household)
}

func (m *memStore) ListCities(_ context.Context, min int) ([]City, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	counts := map[[2]string]int{}
	for _, c := range m.clips {
		if c.scope.City != "" {
			counts[[2]string{c.scope.City, c.scope.State}]++
		}
	}
	var out []City
	for k, n := range counts {
		if n >= min {
			out = append(out, City{City: k[0], State: k[1], Clips: n})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State < out[j].State
		}
		return out[i].City < out[j].City
	})
	return out, nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go build ./... ; go test ./internal/linear/ -run TestMemStore -v`
Expected: `go build` fails only in `internal/linear/repository.go` ("*Repository does not implement Store") — that is Task 4. The linear tests compile (the package's own files only) and PASS. If `go build` reports anything else, fix it before moving on.

- [ ] **Step 6: Commit**

```bash
git add internal/linear/store.go internal/linear/store_test.go internal/linear/memstore_spans_test.go
git commit -m "linear: Span and City types, Store methods for household feeds

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: `household_spans` in PostgreSQL, purge, integration test

**Files:**
- Modify: `internal/db/schema.sql` (after the `viewer_clients` index)
- Create: `internal/linear/household_repo.go`
- Modify: `internal/viewer/repository.go:137-149` (`Purge`)
- Test: `internal/linear/household_repo_integration_test.go` (new)

**Interfaces:**
- Consumes: `Span`, `City`, `Store` methods from Task 3.
- Produces: `*Repository` implements the four methods; `viewer.Repository.Purge` also trims old, superseded spans.

- [ ] **Step 1: Schema**

Append to `internal/db/schema.sql` right after `idx_viewer_clients_last_seen`:

```sql
-- Personal feeds (see docs/superpowers/specs/2026-10-09-personal-feeds-qr-design.md).
-- household_spans: a household's feed as a chain of spans. household is the
-- ip-kind viewer hash; the highest n is the household's current channel.
-- Superseded spans are purged on the viewer retention.
CREATE TABLE IF NOT EXISTS household_spans (
    household   BYTEA       NOT NULL,
    n           INTEGER     NOT NULL,
    scope       TEXT        NOT NULL,
    requested   TEXT        NOT NULL,
    source      TEXT        NOT NULL,
    starts_at   TIMESTAMPTZ NOT NULL,
    seq_offset  BIGINT      NOT NULL,
    item_offset BIGINT      NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (household, n)
);
```

- [ ] **Step 2: Write the failing integration test**

```go
package linear

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/viewer"
)

// Skipped unless TEST_DATABASE_URL points at a database it may write to.
// Rows are written under a random household and removed again.
func TestHouseholdRepository_Integration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the household repository integration test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var hh viewer.ID
	if _, err := rand.Read(hh[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM household_spans WHERE household = $1`, hh[:]) })
	repo := NewRepository(pool)

	if spans, err := repo.Spans(ctx, hh); err != nil || spans != nil {
		t.Fatalf("unknown household: %+v, %v", spans, err)
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	s0 := &Span{Household: hh, N: 0, Scope: "us", Source: SourceDefault, StartsAt: now}
	if ok, err := repo.InsertSpan(ctx, s0); err != nil || !ok {
		t.Fatalf("insert span 0: %v %v", ok, err)
	}
	if ok, err := repo.InsertSpan(ctx, s0); err != nil || ok {
		t.Fatalf("duplicate insert: ok=%v err=%v", ok, err)
	}
	s1 := &Span{Household: hh, N: 1, Scope: "city:katy|tx", Requested: "zip:77494", Source: SourceChoice, StartsAt: now.Add(time.Minute), SeqOffset: 1234, ItemOffset: -7}
	if ok, err := repo.InsertSpan(ctx, s1); err != nil || !ok {
		t.Fatal(err)
	}
	s1.Scope = "state:tx"
	if err := repo.ReplaceSpan(ctx, s1); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceSpan(ctx, &Span{Household: hh, N: 5}); err == nil {
		t.Error("replacing a missing span must fail")
	}
	spans, err := repo.Spans(ctx, hh)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 || spans[1].Scope != "state:tx" || spans[1].SeqOffset != 1234 || spans[1].ItemOffset != -7 ||
		!spans[1].StartsAt.Equal(now.Add(time.Minute)) || spans[1].CreatedAt.IsZero() || spans[0].Household != hh {
		t.Errorf("spans = %+v", spans)
	}

	// Purge trims superseded spans older than the cutoff and keeps the latest.
	if _, err := pool.Exec(ctx, `UPDATE household_spans SET created_at = now() - interval '40 days' WHERE household = $1`, hh[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.NewRepository(pool).Purge(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	spans, _ = repo.Spans(ctx, hh)
	if len(spans) != 1 || spans[0].N != 1 {
		t.Errorf("after purge: %+v", spans)
	}

	cities, err := repo.ListCities(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cities {
		if c.City == "" || c.State == "" || c.Clips < 1 {
			t.Errorf("bad city row %+v", c)
		}
	}
}
```

- [ ] **Step 3: Implement the repository methods**

Create `internal/linear/household_repo.go`:

```go
package linear

import (
	"context"
	"fmt"

	"github.com/dwellingtw/backend/internal/viewer"
)

const spanColumns = `household, n, scope, requested, source, starts_at, seq_offset, item_offset, created_at`

// Spans implements Store.
func (r *Repository) Spans(ctx context.Context, h viewer.ID) ([]Span, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+spanColumns+` FROM household_spans WHERE household = $1 ORDER BY n`, h[:])
	if err != nil {
		return nil, fmt.Errorf("spans: %w", err)
	}
	defer rows.Close()
	var out []Span
	for rows.Next() {
		var sp Span
		var hh []byte
		if err := rows.Scan(&hh, &sp.N, &sp.Scope, &sp.Requested, &sp.Source, &sp.StartsAt, &sp.SeqOffset, &sp.ItemOffset, &sp.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan span: %w", err)
		}
		copy(sp.Household[:], hh)
		out = append(out, sp)
	}
	return out, rows.Err()
}

// InsertSpan implements Store. The primary key settles a race between two
// submits for one household: the loser sees ok=false and re-reads.
func (r *Repository) InsertSpan(ctx context.Context, sp *Span) (bool, error) {
	const q = `
INSERT INTO household_spans (household, n, scope, requested, source, starts_at, seq_offset, item_offset)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (household, n) DO NOTHING`
	tag, err := r.pool.Exec(ctx, q, sp.Household[:], sp.N, sp.Scope, sp.Requested, sp.Source, sp.StartsAt, sp.SeqOffset, sp.ItemOffset)
	if err != nil {
		return false, fmt.Errorf("insert span %d: %w", sp.N, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ReplaceSpan implements Store.
func (r *Repository) ReplaceSpan(ctx context.Context, sp *Span) error {
	const q = `
UPDATE household_spans
   SET scope = $3, requested = $4, source = $5, starts_at = $6, seq_offset = $7, item_offset = $8, created_at = now()
 WHERE household = $1 AND n = $2`
	tag, err := r.pool.Exec(ctx, q, sp.Household[:], sp.N, sp.Scope, sp.Requested, sp.Source, sp.StartsAt, sp.SeqOffset, sp.ItemOffset)
	if err != nil {
		return fmt.Errorf("replace span %d: %w", sp.N, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("replace span %d: no such span", sp.N)
	}
	return nil
}

// ListCities implements Store: the same current-clip predicate ListClips
// uses, grouped by city.
func (r *Repository) ListCities(ctx context.Context, min int) ([]City, error) {
	const q = `
SELECT lower(p.city), lower(p.state), count(*)
  FROM video_hls h
  JOIN properties p ON p.zpid = h.zpid
 WHERE p.video_status = 'ready' AND p.video_content_hash = h.content_hash
   AND p.city <> '' AND p.state <> ''
 GROUP BY 1, 2
HAVING count(*) >= $1
 ORDER BY 2, 1`
	rows, err := r.pool.Query(ctx, q, min)
	if err != nil {
		return nil, fmt.Errorf("list cities: %w", err)
	}
	defer rows.Close()
	var out []City
	for rows.Next() {
		var c City
		if err := rows.Scan(&c.City, &c.State, &c.Clips); err != nil {
			return nil, fmt.Errorf("scan city: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Extend the viewer purge**

In `internal/viewer/repository.go`, inside `Purge`, before the final `return tag.RowsAffected(), nil`:

```go
	// A household keeps its current span forever; superseded ones go with
	// the heartbeats that could have referenced them.
	if _, err := r.pool.Exec(ctx, `
DELETE FROM household_spans s
 WHERE created_at < $1
   AND n < (SELECT max(n) FROM household_spans WHERE household = s.household)`, before); err != nil {
		return tag.RowsAffected(), fmt.Errorf("purge household spans: %w", err)
	}
```

Update the `Purge` doc comment's first sentence to mention `household_spans` too.

- [ ] **Step 5: Build, unit tests, and (if a database is at hand) the integration test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS; the integration test skips without `TEST_DATABASE_URL`.

If Docker is available, run it for real:

```bash
docker compose up -d db 2>/dev/null || docker compose up -d postgres
TEST_DATABASE_URL='postgres://dwellings:dwellings@localhost:5432/dwellings?sslmode=disable' go test ./internal/linear/ -run 'Integration' -v
```

(Check `docker-compose.yml` for the service name and credentials; if the DB is not available, say so in the task report rather than claiming the integration test passed.)

- [ ] **Step 6: Commit**

```bash
git add internal/db/schema.sql internal/linear/household_repo.go internal/linear/household_repo_integration_test.go internal/viewer/repository.go
git commit -m "linear: household_spans table, repository and retention purge

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Service: channel window refactor, `EnsureHousehold`, `Household`, `Cities`

**Files:**
- Modify: `internal/linear/service.go` (`Playlist`, `Service` struct)
- Create: `internal/linear/household.go`
- Test: `internal/linear/household_test.go` (new)

**Interfaces:**
- Consumes: `Store.Spans/InsertSpan/ListCities`, `resolveScope`, `current`, `window`, `windowClipIDs`, `checkClips`.
- Produces:
  - `func (s *Service) channelWindow(ctx, key string, t time.Time) ([]segment, error)` (unexported, used by Task 6)
  - `var ErrUnknownHousehold`
  - `func (s *Service) EnsureHousehold(ctx, h viewer.ID, candidates []Scope, now time.Time) ([]Span, error)`
  - `func (s *Service) Household(ctx, h viewer.ID) ([]Span, error)`
  - `func (s *Service) Cities(ctx) ([]City, error)`

- [ ] **Step 1: Write the failing tests**

```go
package linear

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

func TestEnsureHousehold_CreatesSpan0FromFirstCandidateWithContent(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	m.addZip("77777") // real ZIP, no listings: must be skipped
	s := testService(m, t0)
	hh := viewer.ID{1}

	spans, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77777"}, {Zip: "77494"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 || spans[0].N != 0 || spans[0].Scope != "zip:77494" || spans[0].Requested != "zip:77494" ||
		spans[0].Source != SourceGeo || !spans[0].StartsAt.Equal(t0) || spans[0].SeqOffset != 0 || spans[0].ItemOffset != 0 {
		t.Errorf("span 0 = %+v", spans)
	}
	again, err := s.EnsureHousehold(context.Background(), hh, nil, t0.Add(time.Hour))
	if err != nil || len(again) != 1 || !again[0].StartsAt.Equal(t0) {
		t.Errorf("second call must return the existing span: %+v, %v", again, err)
	}
}

func TestEnsureHousehold_DefaultsToNational(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	spans, err := s.EnsureHousehold(context.Background(), viewer.ID{2}, []Scope{{State: "fl"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if spans[0].Scope != "us" || spans[0].Source != SourceDefault || spans[0].Requested != "" {
		t.Errorf("span 0 = %+v", spans[0])
	}
}

func TestHousehold_UnknownIsAnError(t *testing.T) {
	s := testService(newMemStore(), t0)
	if _, err := s.Household(context.Background(), viewer.ID{3}); !errors.Is(err, ErrUnknownHousehold) {
		t.Errorf("err = %v", err)
	}
}

func TestCities_UsesMinScopeClipsAndCaches(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 5)
	addClips(m, austin, 101, 2) // below MinScopeClips (3 in testService)
	s := testService(m, t0)
	cs, err := s.Cities(context.Background())
	if err != nil || len(cs) != 1 || cs[0].City != "katy" {
		t.Fatalf("cities = %+v, %v", cs, err)
	}
	addClips(m, austin, 201, 5)
	cs, _ = s.Cities(context.Background())
	if len(cs) != 1 {
		t.Errorf("cached answer expected within 10 min, got %+v", cs)
	}
	s.now = func() time.Time { return t0.Add(11 * time.Minute) }
	cs, _ = s.Cities(context.Background())
	if len(cs) != 2 {
		t.Errorf("refreshed answer expected after 10 min, got %+v", cs)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/linear/ -run 'TestEnsureHousehold|TestHousehold_|TestCities' -v`
Expected: FAIL to compile ("EnsureHousehold undefined" etc.).

- [ ] **Step 3: Refactor `Playlist` around `channelWindow`**

In `internal/linear/service.go` replace `Playlist` with:

```go
// channelWindow is the live window of channel key at t: the newest ended
// segments that satisfy liveWindow. Playlist writes it out as is; a personal
// feed splices several of them (household.go).
func (s *Service) channelWindow(ctx context.Context, key string, t time.Time) ([]segment, error) {
	cur, prev, err := s.current(ctx, key, t, s.newSource(key))
	if err != nil {
		return nil, err
	}
	ids := windowClipIDs(cur, prev, t, liveWindow)
	clips, err := s.store.ClipsByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	if err := checkClips(cur, clips); err != nil {
		return nil, err
	}
	if prev != nil {
		if err := checkClips(prev, clips); err != nil {
			return nil, err
		}
	}
	return window(cur, prev, clips, t, liveWindow)
}

// Playlist renders the live media playlist of sc at now.
func (s *Service) Playlist(ctx context.Context, sc Scope, now time.Time) ([]byte, error) {
	if err := s.checkArea(ctx, sc); err != nil {
		return nil, err
	}
	segs, err := s.channelWindow(ctx, sc.Key(), now)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, ErrNoContent
	}
	var buf bytes.Buffer
	writePlaylist(&buf, segs)
	return buf.Bytes(), nil
}
```

Add to the `Service` struct:

```go
	citiesMu sync.Mutex
	cities   []City
	citiesAt time.Time // when cities was last loaded
```

- [ ] **Step 4: Create `household.go`**

```go
package linear

import (
	"context"
	"errors"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

// ErrUnknownHousehold reports a household id no feed was ever created for.
var ErrUnknownHousehold = errors.New("unknown household")

// citiesTTL is how long Cities reuses its answer: the list changes only as
// the library grows, and the mobile page fetches it on every open.
const citiesTTL = 10 * time.Minute

// Household returns h's spans, oldest first.
func (s *Service) Household(ctx context.Context, h viewer.ID) ([]Span, error) {
	spans, err := s.store.Spans(ctx, h)
	if err != nil {
		return nil, err
	}
	if len(spans) == 0 {
		return nil, ErrUnknownHousehold
	}
	return spans, nil
}

// EnsureHousehold returns h's spans, creating span 0 at now when h is new.
// The default channel is the first candidate (the IP's ZIP, city, state —
// see geoCandidates) that resolves to something more specific than
// national, else national. Span 0 has zero offsets: a fresh feed's counters
// are its channel's own.
func (s *Service) EnsureHousehold(ctx context.Context, h viewer.ID, candidates []Scope, now time.Time) ([]Span, error) {
	spans, err := s.store.Spans(ctx, h)
	if err != nil {
		return nil, err
	}
	if len(spans) > 0 {
		return spans, nil
	}
	sp := Span{Household: h, N: 0, Scope: Scope{}.Key(), Source: SourceDefault, StartsAt: now, CreatedAt: now}
	for _, c := range candidates {
		eff, _, err := s.resolveScope(ctx, c)
		if err != nil {
			if errors.Is(err, ErrNoContent) {
				continue
			}
			return nil, err
		}
		if eff != (Scope{}) {
			sp.Scope, sp.Requested, sp.Source = eff.Key(), c.Key(), SourceGeo
			break
		}
	}
	if _, err := s.store.InsertSpan(ctx, &sp); err != nil {
		return nil, err
	}
	// Re-read: a concurrent first request may have won the insert, and its
	// row is the one every later request will see.
	return s.Household(ctx, h)
}

// Cities lists the areas with enough content for a channel of their own.
func (s *Service) Cities(ctx context.Context) ([]City, error) {
	s.citiesMu.Lock()
	defer s.citiesMu.Unlock()
	if s.cities != nil && s.now().Sub(s.citiesAt) < citiesTTL {
		return s.cities, nil
	}
	cs, err := s.store.ListCities(ctx, s.opts.MinScopeClips)
	if err != nil {
		return nil, err
	}
	if cs == nil {
		cs = []City{}
	}
	s.cities, s.citiesAt = cs, s.now()
	return cs, nil
}
```

- [ ] **Step 5: Run the whole linear package**

Run: `go test ./internal/linear/ -v 2>&1 | tail -20`
Expected: PASS, including every pre-existing playlist/EPG test (the refactor must not change bytes).

- [ ] **Step 6: Commit**

```bash
git add internal/linear/service.go internal/linear/household.go internal/linear/household_test.go
git commit -m "linear: household feeds — span 0, cities, channelWindow refactor

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Service: `Choose` and `PersonalPlaylist` (the splice)

**Files:**
- Modify: `internal/linear/household.go`
- Test: `internal/linear/household_test.go`

**Interfaces:**
- Consumes: `channelWindow`, `current`, `itemAt`, `expandItems`, `liveWindow.trim`, `writePlaylist`, `historyLead`, `Store.Spans/InsertSpan/ReplaceSpan`, `checkArea`, `resolveScope`.
- Produces:
  - `func (s *Service) Choose(ctx, h viewer.ID, requested Scope, now time.Time) ([]Span, error)` — errors: `ErrUnknownHousehold`, `ErrUnknownArea`, `ErrBadScope`, `ErrNoContent`.
  - `func (s *Service) PersonalPlaylist(ctx, h viewer.ID, now time.Time) (body []byte, airing Span, err error)` — `airing` is the span on air at now (for the heartbeat).

- [ ] **Step 1: Write the failing tests**

Append to `internal/linear/household_test.go` (add `"strconv"`, `"strings"`, `"regexp"` to its imports):

```go
// parsed is a live playlist as the tests read it.
type parsed struct {
	mediaSeq, discSeq int64
	urls              []string
	discAt            []bool // a DISCONTINUITY tag precedes urls[i]
}

func parsePlaylist(t *testing.T, body []byte) parsed {
	t.Helper()
	var p parsed
	disc := false
	for _, l := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		switch {
		case strings.HasPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"):
			p.mediaSeq, _ = strconv.ParseInt(strings.TrimPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
		case strings.HasPrefix(l, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			p.discSeq, _ = strconv.ParseInt(strings.TrimPrefix(l, "#EXT-X-DISCONTINUITY-SEQUENCE:"), 10, 64)
		case l == "#EXT-X-DISCONTINUITY":
			disc = true
		case strings.HasPrefix(l, "https://"):
			p.urls = append(p.urls, l)
			p.discAt = append(p.discAt, disc)
			disc = false
		}
	}
	if len(p.urls) == 0 {
		t.Fatalf("playlist lists no segments:\n%s", body)
	}
	return p
}

var clipIDRe = regexp.MustCompile(`/hls/v1/(\d+)/`)

func clipID(t *testing.T, url string) int64 {
	t.Helper()
	m := clipIDRe.FindStringSubmatch(url)
	if m == nil {
		t.Fatalf("no clip id in %s", url)
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id
}

// pollMonotonic polls hh's personal playlist every 3 s for dur from start and
// checks the live-playlist invariants a player relies on: MEDIA-SEQUENCE and
// DISCONTINUITY-SEQUENCE never go backwards, a sequence number never maps to
// two different segments, and once a segment of isNew appears no older
// channel's segment follows it. It returns whether a switch was observed with
// a discontinuity tag exactly at the first new segment.
func pollMonotonic(t *testing.T, s *Service, hh viewer.ID, start time.Time, dur time.Duration, isNew func(id int64) bool) (sawSwitch bool) {
	t.Helper()
	seen := map[int64]string{}
	lastSeq, lastDisc := int64(-1), int64(-1)
	for now := start; now.Before(start.Add(dur)); now = now.Add(3 * time.Second) {
		now := now
		s.now = func() time.Time { return now }
		body, _, err := s.PersonalPlaylist(context.Background(), hh, now)
		if err != nil {
			t.Fatalf("at %s: %v", now, err)
		}
		p := parsePlaylist(t, body)
		if p.mediaSeq < lastSeq || p.discSeq < lastDisc {
			t.Fatalf("at %s: counters went backwards (%d<%d or %d<%d)\n%s", now, p.mediaSeq, lastSeq, p.discSeq, lastDisc, body)
		}
		lastSeq, lastDisc = p.mediaSeq, p.discSeq
		firstNew := -1
		for i, u := range p.urls {
			seq := p.mediaSeq + int64(i)
			if prev, ok := seen[seq]; ok && prev != u {
				t.Fatalf("at %s: seq %d was %s, now %s", now, seq, prev, u)
			}
			seen[seq] = u
			if isNew(clipID(t, u)) {
				if firstNew < 0 {
					firstNew = i
				}
			} else if firstNew >= 0 {
				t.Fatalf("at %s: old channel's segment after the switch:\n%s", now, body)
			}
		}
		if firstNew > 0 {
			if !p.discAt[firstNew] {
				t.Fatalf("at %s: no DISCONTINUITY at the switch:\n%s", now, body)
			}
			sawSwitch = true
		}
	}
	return sawSwitch
}

func TestPersonalPlaylist_MatchesChannelBeforeAnyChoice(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	now := t0.Add(90 * time.Second)
	s.now = func() time.Time { return now }
	personal, airing, err := s.PersonalPlaylist(context.Background(), hh, now)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := s.Playlist(context.Background(), Scope{Zip: "77494"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if string(personal) != string(channel) {
		t.Errorf("personal feed differs from its channel before any choice:\n%s\n---\n%s", personal, channel)
	}
	if airing.N != 0 || airing.Scope != "zip:77494" {
		t.Errorf("airing = %+v", airing)
	}
}

func TestPersonalPlaylist_UnknownHousehold(t *testing.T) {
	s := testService(newMemStore(), t0)
	if _, _, err := s.PersonalPlaylist(context.Background(), viewer.ID{9}, t0); !errors.Is(err, ErrUnknownHousehold) {
		t.Errorf("err = %v", err)
	}
}

func TestChoose_SwitchesAtNextSegmentWithMonotonicCounters(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	s := testService(m, t0)
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	switchAt := t0.Add(61 * time.Second) // mid-segment (segments are 3 s from t0-1h)
	s.now = func() time.Time { return switchAt }
	spans, err := s.Choose(context.Background(), hh, Scope{Zip: "78701"}, switchAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 || spans[1].N != 1 || spans[1].Scope != "zip:78701" || spans[1].Requested != "zip:78701" || spans[1].Source != SourceChoice {
		t.Fatalf("spans = %+v", spans)
	}
	// The switch takes effect when the segment airing at submit ends: that
	// segment covers [t0+60s, t0+63s).
	if !spans[1].StartsAt.Equal(t0.Add(63 * time.Second)) {
		t.Errorf("span 1 starts at %s, want %s", spans[1].StartsAt, t0.Add(63*time.Second))
	}
	if !pollMonotonic(t, s, hh, switchAt, 2*time.Minute, func(id int64) bool { return id > 100 }) {
		t.Error("never saw the switch to austin with a discontinuity")
	}
	// Two minutes on, only austin is listed and the feed is still moving.
	end := switchAt.Add(2 * time.Minute)
	s.now = func() time.Time { return end }
	body, airing, err := s.PersonalPlaylist(context.Background(), hh, end)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range parsePlaylist(t, body).urls {
		if clipID(t, u) <= 100 {
			t.Fatalf("katy still listed two minutes after the switch:\n%s", body)
		}
	}
	if airing.N != 1 {
		t.Errorf("airing = %+v", airing)
	}
}

func TestChoose_SameScopeIsANoop(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	spans, err := s.Choose(context.Background(), hh, Scope{Zip: "77494"}, t0.Add(time.Minute))
	if err != nil || len(spans) != 1 {
		t.Errorf("spans = %+v, %v", spans, err)
	}
}

func TestChoose_SecondSubmitBeforeAiringReplacesThePendingSpan(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	s := testService(m, t0)
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	first := t0.Add(61 * time.Second)
	s.now = func() time.Time { return first }
	spans, err := s.Choose(context.Background(), hh, Scope{Zip: "78701"}, first)
	if err != nil {
		t.Fatal(err)
	}
	pendingStart := spans[1].StartsAt
	second := first.Add(time.Second) // still before any austin segment aired
	s.now = func() time.Time { return second }
	spans, err = s.Choose(context.Background(), hh, Scope{State: "tx"}, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 || spans[1].Scope != "state:tx" || !spans[1].StartsAt.Equal(pendingStart) {
		t.Fatalf("expected span 1 replaced in place: %+v", spans)
	}
	// state:tx holds both katy (1-40) and austin (101-140) clips, so "new"
	// means anything the personal timeline did not carry before: the
	// invariants are the counters, the seq→segment stability and the
	// discontinuity, which pollMonotonic checks whatever isNew says.
	pollMonotonic(t, s, hh, second, 2*time.Minute, func(int64) bool { return false })
}

func TestChoose_ThinOrUnknownAreas(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	m.addZip("77777") // exists, no listings → falls back to national
	s := testService(m, t0)
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	now := t0.Add(time.Minute)
	s.now = func() time.Time { return now }
	spans, err := s.Choose(context.Background(), hh, Scope{Zip: "77777"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if spans[1].Scope != "us" || spans[1].Requested != "zip:77777" {
		t.Errorf("thin ZIP: %+v", spans[1])
	}
	if _, err := s.Choose(context.Background(), hh, Scope{Zip: "12345"}, now); !errors.Is(err, ErrUnknownArea) {
		t.Errorf("unknown ZIP: err = %v", err)
	}
	if _, err := s.Choose(context.Background(), viewer.ID{9}, Scope{Zip: "77494"}, now); !errors.Is(err, ErrUnknownHousehold) {
		t.Errorf("unknown household: err = %v", err)
	}
}

// S can fall inside the last segment of the new channel's current lineup
// version: the first segment at or after S is then the next version's
// first, which must be found (and created if need be) rather than failing.
func TestChoose_InsideTheNewChannelsLastSegmentRollsToTheNextVersion(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40) // 3 s segments
	for i := int64(0); i < 3; i++ { // austin: 3 clips of 9 × 7 s = 63 s → 189 s versions
		segs := make([]int, 9)
		for j := range segs {
			segs[j] = 7000
		}
		m.addClip(101+i, austin, 400000, segs...)
	}
	s := testService(m, t0.Add(-10*time.Minute))
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Materialise austin's chain from t0-1h: version 19 covers
	// [t0-198s, t0-9s) and its last segment is [t0-16s, t0-9s).
	s.now = func() time.Time { return t0 }
	if _, err := s.Playlist(context.Background(), Scope{Zip: "78701"}, t0); err != nil {
		t.Fatal(err)
	}
	// katy's chain is created on this call from t0-61min, 3 s aligned to t0:
	// the segment airing at t0-13s is [t0-15s, t0-12s), so S = t0-12s, inside
	// austin's version-19 last segment. The first austin segment at or after
	// S is version 20's first, at t0-9s.
	at := t0.Add(-13 * time.Second)
	s.now = func() time.Time { return at }
	spans, err := s.Choose(context.Background(), hh, Scope{Zip: "78701"}, at)
	if err != nil {
		t.Fatal(err)
	}
	if !spans[1].StartsAt.Equal(t0.Add(-12 * time.Second)) {
		t.Fatalf("S = %s, want %s", spans[1].StartsAt, t0.Add(-12*time.Second))
	}
	if !pollMonotonic(t, s, hh, at, 2*time.Minute, func(id int64) bool { return id > 100 }) {
		t.Error("never saw the switch")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/linear/ -run 'TestPersonalPlaylist|TestChoose' -v`
Expected: FAIL to compile ("PersonalPlaylist undefined", "Choose undefined").

- [ ] **Step 3: Implement the splice**

Append to `internal/linear/household.go` (add `"bytes"` and `"fmt"` to its imports):

```go
// lookback bounds how far back a span can still contribute to a window: a
// window is at most 6 segments of ≤ 10 s plus one more, so anything that
// ended more than two minutes ago is out of it.
const lookback = 2 * time.Minute

// spanFrom is when sp's channel segments start to count: its start, or an
// hour earlier for span 0 so a fresh feed has history to serve at once,
// exactly like a fresh channel (historyLead).
func spanFrom(sp Span) time.Time {
	if sp.N == 0 {
		return sp.StartsAt.Add(-historyLead)
	}
	return sp.StartsAt
}

// shifted relabels channel segments as sp's personal ones: the offsets
// applied, and after a switch the first segment marked as an item start so
// writePlaylist emits the discontinuity there even mid-clip.
func shifted(segs []segment, sp Span) []segment {
	out := make([]segment, len(segs))
	for i, g := range segs {
		g.Seq += sp.SeqOffset
		g.Item += sp.ItemOffset
		out[i] = g
	}
	if sp.N > 0 && len(out) > 0 {
		out[0].FirstOfItem = true
	}
	return out
}

// contribution is what sp adds to its feed up to end: the segments of its
// channel that start at or after spanFrom(sp) and have ended by end,
// relabelled. end is the span's own end (the next span's start) or now.
func (s *Service) contribution(ctx context.Context, sp Span, end time.Time) ([]segment, error) {
	segs, err := s.channelWindow(ctx, sp.Scope, end)
	if err != nil {
		return nil, err
	}
	from := spanFrom(sp)
	i := 0
	for i < len(segs) && segs[i].Start.Before(from) {
		i++
	}
	return shifted(segs[i:], sp), nil
}

// PersonalPlaylist renders household h's live playlist at now: the spans'
// contributions, oldest first, trimmed to the usual window. airing is the
// span on air at now.
func (s *Service) PersonalPlaylist(ctx context.Context, h viewer.ID, now time.Time) ([]byte, Span, error) {
	spans, err := s.Household(ctx, h)
	if err != nil {
		return nil, Span{}, err
	}
	airing := spans[0]
	var segs []segment
	for k, sp := range spans {
		if sp.StartsAt.After(now) {
			break // not airing yet
		}
		airing = sp
		end := now
		if k+1 < len(spans) && spans[k+1].StartsAt.Before(end) {
			end = spans[k+1].StartsAt
		}
		if end.Before(now.Add(-lookback)) {
			continue
		}
		c, err := s.contribution(ctx, sp, end)
		if err != nil {
			return nil, Span{}, err
		}
		segs = append(segs, c...)
	}
	if len(segs) == 0 {
		return nil, Span{}, ErrNoContent
	}
	var buf bytes.Buffer
	writePlaylist(&buf, liveWindow.trim(segs))
	return buf.Bytes(), airing, nil
}

// endOfAiring is when the segment of channel key airing at now ends — the
// earliest moment a switch away from key takes effect — or now when nothing
// is airing.
func (s *Service) endOfAiring(ctx context.Context, key string, now time.Time) (time.Time, error) {
	cur, _, err := s.current(ctx, key, now, s.newSource(key))
	if err != nil {
		return time.Time{}, err
	}
	i := itemAt(cur, now)
	if i < 0 {
		return now, nil
	}
	clips, err := s.store.ClipsByID(ctx, cur.ItemIDs[i:i+1])
	if err != nil {
		return time.Time{}, err
	}
	segs, err := expandItems(cur, clips, i, i)
	if err != nil {
		return time.Time{}, err
	}
	for _, g := range segs {
		end := g.Start.Add(time.Duration(g.DurMS) * time.Millisecond)
		if !g.Start.After(now) && end.After(now) {
			return end, nil
		}
	}
	return now, nil
}

// firstAtOrAfter is the first segment of channel key starting at or after t:
// in the version covering t, or — when t falls inside that version's last
// segment — the next version, materialised here if it does not exist yet.
func (s *Service) firstAtOrAfter(ctx context.Context, key string, t time.Time) (segment, error) {
	v, _, err := s.current(ctx, key, t, s.newSource(key))
	if err != nil {
		return segment{}, err
	}
	for {
		i := itemAt(v, t)
		if i < 0 {
			i = 0 // t precedes v (v is the next version): its first segment
		}
		to := i + 1
		if to > len(v.ItemIDs)-1 {
			to = len(v.ItemIDs) - 1
		}
		clips, err := s.store.ClipsByID(ctx, v.ItemIDs[i:to+1])
		if err != nil {
			return segment{}, err
		}
		segs, err := expandItems(v, clips, i, to)
		if err != nil {
			return segment{}, err
		}
		for _, g := range segs {
			if !g.Start.Before(t) {
				return g, nil
			}
		}
		next, _, err := s.current(ctx, key, v.EndsAt, s.newSource(key))
		if err != nil {
			return segment{}, err
		}
		if next.Version == v.Version {
			return segment{}, fmt.Errorf("channel %s: no segment at or after %s", key, t)
		}
		v = next
	}
}

// splice builds span n of a feed: channel eff from S on, chained after prev
// so the personal counters continue from prev's last segment.
func (s *Service) splice(ctx context.Context, prev Span, eff, requested Scope, S time.Time, n int, now time.Time) (*Span, error) {
	tail, err := s.contribution(ctx, prev, S)
	if err != nil {
		return nil, err
	}
	first, err := s.firstAtOrAfter(ctx, eff.Key(), S)
	if err != nil {
		return nil, err
	}
	sp := &Span{Household: prev.Household, N: n, Scope: eff.Key(), Requested: requested.Key(), Source: SourceChoice, StartsAt: S, CreatedAt: now}
	if len(tail) > 0 {
		last := tail[len(tail)-1]
		sp.SeqOffset = last.Seq + 1 - first.Seq
		sp.ItemOffset = last.Item + 1 - first.Item
	}
	return sp, nil
}

// Choose points household h's feed at the channel for requested (after the
// usual thin-scope fallback) from the next segment boundary on, and returns
// the household's spans. Choosing the channel already on air is a no-op. A
// choice made before the previous one aired a single segment replaces it
// rather than chaining after it: there is nothing of it to splice after.
func (s *Service) Choose(ctx context.Context, h viewer.ID, requested Scope, now time.Time) ([]Span, error) {
	if err := s.checkArea(ctx, requested); err != nil {
		return nil, err
	}
	eff, _, err := s.resolveScope(ctx, requested)
	if err != nil {
		return nil, err
	}
	spans, err := s.Household(ctx, h)
	if err != nil {
		return nil, err
	}
	latest := spans[len(spans)-1]
	if latest.Scope == eff.Key() {
		return spans, nil
	}
	aired, err := s.contribution(ctx, latest, now)
	if err != nil {
		return nil, err
	}
	prev, n := latest, latest.N+1
	var S time.Time
	if len(aired) == 0 && latest.N > 0 {
		prev, n, S = spans[len(spans)-2], latest.N, latest.StartsAt
	} else if S, err = s.endOfAiring(ctx, latest.Scope, now); err != nil {
		return nil, err
	}
	sp, err := s.splice(ctx, prev, eff, requested, S, n, now)
	if err != nil {
		return nil, err
	}
	sp.Household = h
	if n == latest.N {
		err = s.store.ReplaceSpan(ctx, sp)
	} else {
		_, err = s.store.InsertSpan(ctx, sp) // a lost race means the other submit's span wins
	}
	if err != nil {
		return nil, err
	}
	s.log.Info("household feed switched", "span", sp.N, "scope", sp.Scope, "requested", sp.Requested, "starts_at", sp.StartsAt)
	return s.Household(ctx, h)
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/linear/ -run 'TestPersonalPlaylist|TestChoose' -v`
Expected: PASS. If `TestChoose_InsideTheNewChannelsLastSegmentRollsToTheNextVersion` fails on `S`, print katy's segment boundaries around `at` (`s.channelWindow(ctx, "zip:77494", at)`) and austin's version 19/20 bounds (`m.ListVersions(ctx, "zip:78701")`), then adjust `at` so S lands strictly inside austin's last segment of a version; do not loosen the assertion.

- [ ] **Step 5: Run the package with the race detector**

Run: `go test -race ./internal/linear/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/linear/household.go internal/linear/household_test.go
git commit -m "linear: personal playlists spliced across household spans

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: HTTP: `/feed/*` routes and `resolve` additions

**Files:**
- Create: `internal/linear/feed.go`
- Modify: `internal/linear/handler.go` (`Handler` struct, `Register`)
- Modify: `internal/linear/resolve.go` (`resolveResponse`, `writeResolve`)
- Test: `internal/linear/feed_test.go` (new)

**Interfaces:**
- Consumes: `Service.EnsureHousehold/Household/Choose/PersonalPlaylist/Cities`, `viewer.Hasher.Household`, `viewer.ParseID`, `qrcode.PNG`, `geoCandidates`, `writeMaster`, `writeError`, `ParseScope`, `ParseKey`, `Scope.Name`.
- Produces: `type FeedOptions{MobileBaseURL string; QRShowSeconds, QREverySeconds int}`, `func (h *Handler) EnableFeeds(o FeedOptions)`, routes listed in the spec, and `feed`/`qr` fields on `/channels/resolve`. Task 8 adds `/tv/` on the same handler; Task 9 wires it.

- [ ] **Step 1: Write the failing tests**

```go
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

type feedTestAudience struct{}

func (feedTestAudience) LastChannel(context.Context, viewer.ID, time.Time) (string, bool, error) {
	return "", false, nil
}
func (feedTestAudience) Stats(context.Context, string, time.Time) (viewer.Stats, error) {
	return viewer.Stats{}, nil
}

func feedMux(store Store, now time.Time) (*Service, *http.ServeMux) {
	svc := testService(store, now)
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.EnableViewers(ViewerOptions{Hasher: viewer.NewHasher("salt", false), Audience: feedTestAudience{}, PublicBaseURL: "https://api.example.test"})
	h.EnableFeeds(FeedOptions{MobileBaseURL: "https://tv.example.test", QRShowSeconds: 60, QREverySeconds: 300})
	mux := http.NewServeMux()
	h.Register(mux)
	return svc, mux
}

func do(mux *http.ServeMux, method, path, ip string, body string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("X-Real-IP", ip)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

var hhRe = regexp.MustCompile(`live\.m3u8\?hh=([0-9a-f]{32})\n`)

func TestFeed_MasterIsPerHouseholdAndUncached(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	rec := do(mux, "GET", "/feed/master.m3u8", "203.0.113.5", "")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != playlistContentType {
		t.Fatalf("status %d headers %v body %s", rec.Code, rec.Header(), rec.Body)
	}
	a := hhRe.FindStringSubmatch(rec.Body.String())
	if a == nil {
		t.Fatalf("no household media URI:\n%s", rec.Body)
	}
	b := hhRe.FindStringSubmatch(do(mux, "GET", "/feed/master.m3u8", "203.0.113.5", "").Body.String())
	c := hhRe.FindStringSubmatch(do(mux, "GET", "/feed/master.m3u8", "203.0.113.9", "").Body.String())
	if a[1] != b[1] || a[1] == c[1] {
		t.Errorf("same IP must share a feed and different IPs must not: %s %s %s", a[1], b[1], c[1])
	}
}

func TestFeed_Live(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	id := hhRe.FindStringSubmatch(do(mux, "GET", "/feed/master.m3u8", "203.0.113.5", "").Body.String())[1]
	rec := do(mux, "GET", "/feed/live.m3u8?hh="+id, "203.0.113.5", "")
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "#EXTM3U\n") || rec.Header().Get("Cache-Control") != "public, max-age=2" {
		t.Errorf("status %d cc %q body %s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body)
	}
	if rec := do(mux, "GET", "/feed/live.m3u8?hh="+strings.Repeat("0", 32), "203.0.113.5", ""); rec.Code != 404 {
		t.Errorf("unknown household: status %d body %s", rec.Code, rec.Body)
	}
	if rec := do(mux, "GET", "/feed/live.m3u8?hh=nope", "203.0.113.5", ""); rec.Code != 400 {
		t.Errorf("malformed hh: status %d", rec.Code)
	}
}

func TestFeed_QR(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	rec := do(mux, "GET", "/feed/qr.png", "203.0.113.5", "")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil || img.Bounds().Dx() != 400 {
		t.Errorf("decode: %v, width %d", err, img.Bounds().Dx())
	}
	if rec := do(mux, "GET", "/feed/qr.png?size=50", "203.0.113.5", ""); rec.Code != 400 {
		t.Errorf("size 50: status %d", rec.Code)
	}
	// The QR's household must be the same one the master playlist uses.
	id := hhRe.FindStringSubmatch(do(mux, "GET", "/feed/master.m3u8", "203.0.113.5", "").Body.String())[1]
	var f map[string]any
	_ = json.Unmarshal(do(mux, "GET", "/feed/me", "203.0.113.5", "").Body.Bytes(), &f)
	if f["id"] != id {
		t.Errorf("/feed/me id %v != master's %s", f["id"], id)
	}
}

func TestFeed_GetAndChoose(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	m.addZip("77777")
	_, mux := feedMux(m, t0)
	var me struct {
		ID, Scope, Name, Requested, Source, Live string
	}
	rec := do(mux, "GET", "/feed/me", "203.0.113.5", "")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me.Scope != "us" || me.Source != SourceDefault || me.Name == "" || !strings.HasSuffix(me.Live, "/feed/live.m3u8?hh="+me.ID) {
		t.Fatalf("me = %+v", me)
	}
	rec = do(mux, "GET", "/feed/"+me.ID, "198.51.100.1", "") // any IP may read by id
	if rec.Code != 200 {
		t.Fatalf("get by id: %d", rec.Code)
	}

	rec = do(mux, "POST", "/feed/"+me.ID, "203.0.113.5", `{"zip":"78701"}`)
	var after struct{ Scope, Requested, Source string }
	_ = json.Unmarshal(rec.Body.Bytes(), &after)
	if rec.Code != 200 || after.Scope != "zip:78701" || after.Source != SourceChoice {
		t.Fatalf("choose: %d %s", rec.Code, rec.Body)
	}
	rec = do(mux, "POST", "/feed/"+me.ID, "203.0.113.5", `{"zip":"77777"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &after)
	if rec.Code != 200 || after.Scope != "us" || after.Requested != "zip:77777" {
		t.Errorf("thin zip must fall back and keep requested: %d %s", rec.Code, rec.Body)
	}
	for body, want := range map[string]int{
		`{"zip":"12345"}`:         404, // unknown area
		`{"zip":"1234"}`:          400,
		`{"city":"Katy"}`:         400, // city without state
		`not json`:                400,
		`{"zip":"` + strings.Repeat("7", 2000) + `"}`: 400, // over 1 KB
		`{}`:                      200, // national: a valid choice
	} {
		if rec := do(mux, "POST", "/feed/"+me.ID, "203.0.113.5", body); rec.Code != want {
			t.Errorf("POST %.40s: status %d, want %d (%s)", body, rec.Code, want, rec.Body)
		}
	}
	if rec := do(mux, "POST", "/feed/"+strings.Repeat("0", 32), "203.0.113.5", `{"zip":"78701"}`); rec.Code != 404 {
		t.Errorf("unknown household: %d", rec.Code)
	}
	if rec := do(mux, "GET", "/feed/zz", "203.0.113.5", ""); rec.Code != 400 {
		t.Errorf("malformed id: %d", rec.Code)
	}
	rec = do(mux, "OPTIONS", "/feed/"+me.ID, "203.0.113.5", "")
	if rec.Code != 204 || !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") || rec.Header().Get("Access-Control-Allow-Headers") != "Content-Type" {
		t.Errorf("preflight: %d %v", rec.Code, rec.Header())
	}
}

func TestFeed_Areas(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	rec := do(mux, "GET", "/feed/areas", "203.0.113.5", "")
	var areas []struct {
		City, State, Name string
		Clips             int
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &areas); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(areas) != 1 || areas[0].Name != "Katy, TX" || areas[0].Clips != 40 || areas[0].City != "katy" {
		t.Errorf("areas = %+v", areas)
	}
	if rec.Header().Get("Cache-Control") != "public, max-age=600" {
		t.Errorf("cache control %q", rec.Header().Get("Cache-Control"))
	}
}

func TestFeed_ResolveAdvertisesFeedAndQR(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	rec := do(mux, "GET", "/channels/resolve", "203.0.113.5", "")
	var res struct {
		Feed string
		QR   struct {
			URL          string
			ShowSeconds  int `json:"show_seconds"`
			EverySeconds int `json:"every_seconds"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if res.Feed != "https://api.example.test/feed/master.m3u8" || res.QR.URL != "https://api.example.test/feed/qr.png" || res.QR.ShowSeconds != 60 || res.QR.EverySeconds != 300 {
		t.Errorf("resolve = %+v", res)
	}
}

func TestFeed_NotMountedWithoutEnableFeeds(t *testing.T) {
	rec := serve(t, newMemStore(), t0, "/feed/master.m3u8")
	if rec.Code != 404 {
		t.Errorf("status %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/linear/ -run TestFeed -v`
Expected: FAIL to compile ("EnableFeeds undefined", "FeedOptions undefined").

- [ ] **Step 3: Handler plumbing**

In `internal/linear/handler.go`:
- add the field `feeds *FeedOptions // nil until EnableFeeds` to `Handler`;
- in `Register`, inside the `if h.viewers != nil {` block, add at its end:

```go
		if h.feeds != nil {
			h.registerFeeds(mux)
		}
```

In `internal/linear/resolve.go`:
- add to `resolveResponse`:

```go
	// Personal feed (set when feeds are enabled): the master the app should
	// play, and how to show the QR that opens the mobile page.
	Feed string  `json:"feed,omitempty"`
	QR   *qrInfo `json:"qr,omitempty"`
```

- add the type:

```go
// qrInfo tells the app where the QR image is and on what cadence to show it.
type qrInfo struct {
	URL          string `json:"url"`
	ShowSeconds  int    `json:"show_seconds"`
	EverySeconds int    `json:"every_seconds"`
}
```

- in `writeResolve`, build the response in a variable before encoding and, when `h.feeds != nil`, set:

```go
	res.Feed = h.viewers.PublicBaseURL + "/feed/master.m3u8"
	res.QR = &qrInfo{URL: h.viewers.PublicBaseURL + "/feed/qr.png", ShowSeconds: h.feeds.QRShowSeconds, EverySeconds: h.feeds.QREverySeconds}
```

- [ ] **Step 4: Create `feed.go`**

```go
package linear

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/dwellingtw/backend/internal/qrcode"
	"github.com/dwellingtw/backend/internal/viewer"
)

// FeedOptions enables the personal feeds (/feed/*) and the mobile page
// (/tv/*). Requires EnableViewers: feeds are keyed by the viewer hash.
type FeedOptions struct {
	MobileBaseURL  string // origin of the mobile page, for the QR (https://dwellings.tv)
	QRShowSeconds  int    // how long the Roku app shows the QR
	QREverySeconds int    // how often
}

// EnableFeeds turns on the personal feeds. Call before Register.
func (h *Handler) EnableFeeds(o FeedOptions) { h.feeds = &o }

func (h *Handler) registerFeeds(mux *http.ServeMux) {
	mux.HandleFunc("GET /feed/master.m3u8", h.feedMaster)
	mux.HandleFunc("GET /feed/live.m3u8", h.feedLive)
	mux.HandleFunc("GET /feed/qr.png", h.feedQR)
	mux.HandleFunc("GET /feed/me", h.feedMe)
	mux.HandleFunc("GET /feed/areas", h.feedAreas)
	mux.HandleFunc("GET /feed/{id}", h.feedGet)
	mux.HandleFunc("POST /feed/{id}", h.feedChoose)
	mux.HandleFunc("OPTIONS /feed/{id}", h.feedPreflight)
	mux.HandleFunc("GET /tv/", h.tvPage)
	mux.HandleFunc("GET /tv/{id}", h.tvPage)
}

const (
	qrDefaultSize = 400
	qrMinSize     = 100
	qrMaxSize     = 1000
	maxChoiceBody = 1024
)

// household is the home the request comes from.
func (h *Handler) household(r *http.Request) viewer.ID {
	return h.viewers.Hasher.Household(viewer.RequestIP(r))
}

// ensure creates the household's feed on first contact, defaulting to the
// IP's area when the geo database is present.
func (h *Handler) ensure(r *http.Request, id viewer.ID) ([]Span, error) {
	var cands []Scope
	if h.viewers.Geo != nil {
		if loc, ok := h.viewers.Geo.Locate(net.ParseIP(viewer.RequestIP(r))); ok {
			cands = geoCandidates(loc)
		}
	}
	return h.svc.EnsureHousehold(r.Context(), id, cands, h.svc.now())
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

// failFeed maps feed errors to responses.
func (h *Handler) failFeed(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUnknownHousehold) {
		writeError(w, http.StatusNotFound, "unknown feed")
		return
	}
	h.fail(w, Scope{}, err)
}

func parseHousehold(s string) (viewer.ID, bool) {
	id, err := viewer.ParseID(s)
	return id, err == nil
}

func (h *Handler) feedMaster(w http.ResponseWriter, r *http.Request) {
	id := h.household(r)
	if _, err := h.ensure(r, id); err != nil {
		h.failFeed(w, err)
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", playlistContentType)
	writeMaster(w, "live.m3u8?hh="+id.String())
}

func (h *Handler) feedLive(w http.ResponseWriter, r *http.Request) {
	id, ok := parseHousehold(r.URL.Query().Get("hh"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid hh")
		return
	}
	now := h.svc.now()
	body, airing, err := h.svc.PersonalPlaylist(r.Context(), id, now)
	if err != nil {
		h.failFeed(w, err)
		return
	}
	if sc, err := ParseKey(airing.Scope); err == nil {
		h.track(r, sc, now)
	}
	playlistHeaders(w)
	_, _ = w.Write(body)
}

func (h *Handler) feedQR(w http.ResponseWriter, r *http.Request) {
	id := h.household(r)
	if _, err := h.ensure(r, id); err != nil {
		h.failFeed(w, err)
		return
	}
	size := qrDefaultSize
	if v := r.URL.Query().Get("size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < qrMinSize || n > qrMaxSize {
			writeError(w, http.StatusBadRequest, "invalid size (100-1000)")
			return
		}
		size = n
	}
	png, err := qrcode.PNG(h.feeds.MobileBaseURL+"/tv/"+id.String(), size)
	if err != nil {
		h.log.Error("qr render failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

// feedResponse is the body of GET/POST /feed/{id} and GET /feed/me.
type feedResponse struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`     // effective channel key
	Name      string    `json:"name"`      // its viewer-facing title
	Requested string    `json:"requested"` // what the viewer asked for ("" = nothing yet)
	Source    string    `json:"source"`    // choice | geo | default
	UpdatedAt time.Time `json:"updated_at"`
	Live      string    `json:"live"` // the personal media playlist, for an in-page player
}

func (h *Handler) writeFeed(w http.ResponseWriter, spans []Span) {
	sp := spans[len(spans)-1]
	name := ""
	if sc, err := ParseKey(sp.Scope); err == nil {
		name = sc.Name()
	}
	noStore(w)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(feedResponse{
		ID: sp.Household.String(), Scope: sp.Scope, Name: name, Requested: sp.Requested, Source: sp.Source,
		UpdatedAt: sp.CreatedAt, Live: h.viewers.PublicBaseURL + "/feed/live.m3u8?hh=" + sp.Household.String(),
	})
}

func (h *Handler) feedMe(w http.ResponseWriter, r *http.Request) {
	spans, err := h.ensure(r, h.household(r))
	if err != nil {
		h.failFeed(w, err)
		return
	}
	h.writeFeed(w, spans)
}

func (h *Handler) feedGet(w http.ResponseWriter, r *http.Request) {
	id, ok := parseHousehold(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid feed id")
		return
	}
	spans, err := h.svc.Household(r.Context(), id)
	if err != nil {
		h.failFeed(w, err)
		return
	}
	h.writeFeed(w, spans)
}

// choiceBody is what the mobile page posts: a ZIP, or a city and state.
type choiceBody struct {
	Zip   string `json:"zip"`
	City  string `json:"city"`
	State string `json:"state"`
}

func (h *Handler) feedChoose(w http.ResponseWriter, r *http.Request) {
	id, ok := parseHousehold(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid feed id")
		return
	}
	var body choiceBody
	if err := json.NewDecoder(io.LimitReader(r.Body, maxChoiceBody+1)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON with zip, or city and state")
		return
	}
	sc, err := ParseScope(url.Values{"zip": {body.Zip}, "city": {body.City}, "state": {body.State}})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	spans, err := h.svc.Choose(r.Context(), id, sc, h.svc.now())
	if err != nil {
		h.failFeed(w, err)
		return
	}
	h.writeFeed(w, spans)
}

func (h *Handler) feedPreflight(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// areaResponse is one entry of GET /feed/areas.
type areaResponse struct {
	City  string `json:"city"`
	State string `json:"state"`
	Name  string `json:"name"`
	Clips int    `json:"clips"`
}

func (h *Handler) feedAreas(w http.ResponseWriter, r *http.Request) {
	cs, err := h.svc.Cities(r.Context())
	if err != nil {
		h.fail(w, Scope{}, err)
		return
	}
	out := make([]areaResponse, 0, len(cs))
	for _, c := range cs {
		out = append(out, areaResponse{City: c.City, State: c.State, Name: Scope{City: c.City, State: c.State}.areaName(), Clips: c.Clips})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=600")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(out)
}
```

The over-1 KB body test passes because `json.Decoder` over a `LimitReader` of 1025 bytes hits EOF mid-value and errors. Add to `scope.go`, after `Name`:

```go
// areaName is the bare place name ("Katy, TX", "Texas", "77494", "the US").
func (s Scope) areaName() string {
	switch {
	case s.Zip != "":
		return s.Zip
	case s.City != "":
		return titleCase(s.City) + ", " + strings.ToUpper(s.State)
	case s.State != "":
		if n, ok := stateNames[s.State]; ok {
			return n
		}
		return strings.ToUpper(s.State)
	}
	return "the US"
}
```

`tvPage` is Task 8; for this task add a stub so the package compiles:

```go
func (h *Handler) tvPage(w http.ResponseWriter, _ *http.Request) { http.Error(w, "not yet", http.StatusNotFound) }
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/linear/ -run 'TestFeed|TestHandler|TestResolve' -v 2>&1 | tail -30`
Expected: PASS (existing handler and resolve tests included: `resolve` without feeds must not emit `feed`/`qr`).

- [ ] **Step 6: Commit**

```bash
git add internal/linear/feed.go internal/linear/feed_test.go internal/linear/handler.go internal/linear/resolve.go internal/linear/scope.go
git commit -m "linear: /feed routes — personal master, live, QR, choice, areas

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: The mobile page

**Files:**
- Create: `internal/linear/tv.html`
- Modify: `internal/linear/feed.go` (`tvPage`, embed)
- Test: `internal/linear/feed_test.go`

**Interfaces:**
- Consumes: `GET /feed/me`, `GET /feed/{id}`, `POST /feed/{id}`, `GET /feed/areas` (Task 7); `h.viewers.PublicBaseURL`.
- Produces: `GET /tv/` and `GET /tv/{id}` serve the page with `__API_BASE__` replaced.

- [ ] **Step 1: Write the failing test**

Append to `feed_test.go`:

```go
func TestFeed_TVPage(t *testing.T) {
	_, mux := feedMux(newMemStore(), t0)
	for _, p := range []string{"/tv/", "/tv/" + strings.Repeat("a", 32)} {
		rec := do(mux, "GET", p, "203.0.113.5", "")
		body := rec.Body.String()
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(body, `"https://api.example.test"`) || strings.Contains(body, "__API_BASE__") {
			t.Errorf("%s: API base not injected", p)
		}
		if !strings.Contains(body, "hls.js/1.5.13/hls.min.js") || !strings.Contains(body, "/feed/areas") {
			t.Errorf("%s: page is missing the player or the areas call", p)
		}
		if rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: cache control %q", p, rec.Header().Get("Cache-Control"))
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/linear/ -run TestFeed_TVPage -v`
Expected: FAIL (404 from the stub).

- [ ] **Step 3: Write `internal/linear/tv.html`**

```html
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Dwellings TV</title>
<style>
  :root { --bg:#0f1115; --card:#181b22; --line:#2a2f3a; --text:#f3f4f6; --muted:#9aa3b2; --accent:#2f80ed; --ok:#2ecc71; --err:#eb5757; }
  * { box-sizing:border-box; }
  body { margin:0; font:16px/1.5 -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; background:var(--bg); color:var(--text); }
  main { max-width:480px; margin:0 auto; padding:24px 16px 48px; }
  h1 { font-size:22px; margin:0 0 4px; }
  .muted { color:var(--muted); font-size:14px; }
  .card { background:var(--card); border-radius:14px; padding:16px; margin:16px 0; }
  .big { font-size:20px; font-weight:600; }
  label { display:block; font-size:14px; color:var(--muted); margin:12px 0 6px; }
  input { width:100%; font-size:18px; padding:12px; border-radius:10px; border:1px solid var(--line); background:var(--bg); color:var(--text); }
  button { width:100%; font-size:18px; padding:14px; border:0; border-radius:10px; background:var(--accent); color:#fff; margin-top:16px; }
  button:disabled { opacity:.5; }
  .list { max-height:230px; overflow:auto; margin-top:6px; }
  .list div { padding:10px 12px; border-radius:8px; }
  .list div.sel, .list div:active { background:#232836; }
  .status { font-size:15px; margin-top:12px; min-height:1.5em; }
  .ok { color:var(--ok); } .err { color:var(--err); }
  video { width:100%; border-radius:10px; background:#000; margin-top:12px; }
</style>
</head>
<body>
<main>
  <h1>Dwellings TV</h1>
  <div class="muted">Pick the area your TV shows homes from.</div>

  <div class="card">
    <div class="muted">Your TV is showing homes in</div>
    <div id="current" class="big">…</div>
  </div>

  <div class="card">
    <label for="zip">ZIP code</label>
    <input id="zip" inputmode="numeric" pattern="[0-9]*" maxlength="5" placeholder="e.g. 90025" autocomplete="postal-code">
    <label for="city">or search a city</label>
    <input id="city" placeholder="e.g. Los Angeles" autocomplete="off">
    <div id="cities" class="list"></div>
    <button id="go" disabled>Show this area on my TV</button>
    <div id="status" class="status"></div>
  </div>

  <div class="card" id="watch" hidden>
    <div class="muted">No TV nearby? Watch your feed here.</div>
    <video id="player" controls playsinline></video>
  </div>
</main>
<script src="https://cdnjs.cloudflare.com/ajax/libs/hls.js/1.5.13/hls.min.js"></script>
<script>
(function () {
  var API = "__API_BASE__";
  var id = location.pathname.replace(/\/+$/, "").split("/").pop();
  if (!/^[0-9a-f]{32}$/.test(id)) id = "";
  var $ = function (s) { return document.getElementById(s); };
  var cities = [], picked = null;

  function areaOf(f) { return f.name.replace(/^Homes for sale (in |across )?/, ""); }
  function setStatus(msg, cls) { $("status").textContent = msg; $("status").className = "status " + (cls || ""); }
  function show(f) {
    id = f.id;
    $("current").textContent = areaOf(f);
    if (history.replaceState && location.pathname.indexOf(f.id) < 0) history.replaceState(null, "", "/tv/" + f.id);
    $("watch").hidden = false;
    play(f.live);
    update();
  }
  function load() {
    fetch(API + (id ? "/feed/" + id : "/feed/me"), { cache: "no-store" })
      .then(function (r) {
        if (!r.ok) throw new Error(r.status === 404 ? "We could not find your TV. Scan the code on the screen again." : "Something went wrong. Please try again.");
        return r.json();
      })
      .then(show)
      .catch(function (e) { $("current").textContent = "—"; setStatus(e.message, "err"); });
    fetch(API + "/feed/areas").then(function (r) { return r.json(); }).then(function (cs) { cities = cs; }).catch(function () {});
  }
  function renderCities(q) {
    q = q.trim().toLowerCase();
    var box = $("cities");
    box.innerHTML = "";
    if (!q) return;
    cities.filter(function (c) { return c.name.toLowerCase().indexOf(q) >= 0; }).slice(0, 8).forEach(function (c) {
      var d = document.createElement("div");
      d.textContent = c.name + " · " + c.clips + " homes";
      if (picked && picked.city === c.city && picked.state === c.state) d.className = "sel";
      d.onclick = function () { picked = c; $("zip").value = ""; $("city").value = c.name; renderCities(c.name); update(); };
      box.appendChild(d);
    });
  }
  function choice() {
    var z = $("zip").value.trim();
    if (/^\d{5}$/.test(z)) return { zip: z };
    if (picked) return { city: picked.city, state: picked.state };
    return null;
  }
  function update() { $("go").disabled = !choice() || !id; }
  function play(url) {
    var v = $("player");
    if (v.canPlayType("application/vnd.apple.mpegurl")) { v.src = url; return; }
    if (window.Hls && Hls.isSupported()) {
      if (v._hls) v._hls.destroy();
      var h = new Hls({ liveSyncDurationCount: 3 });
      v._hls = h;
      h.loadSource(url);
      h.attachMedia(v);
    }
  }
  $("zip").addEventListener("input", function () { picked = null; $("city").value = ""; renderCities(""); update(); });
  $("city").addEventListener("input", function () { picked = null; renderCities($("city").value); update(); });
  $("go").addEventListener("click", function () {
    var c = choice();
    if (!c || !id) return;
    $("go").disabled = true;
    setStatus("Switching your TV…");
    fetch(API + "/feed/" + id, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(c) })
      .then(function (r) { return r.json().then(function (b) { return { ok: r.ok, status: r.status, body: b }; }); })
      .then(function (res) {
        if (!res.ok) {
          setStatus(res.status === 404 && res.body.error === "unknown area" ? "We don't have that area yet." : (res.body.error || "Something went wrong."), "err");
          update();
          return;
        }
        var f = res.body, want = c.zip ? "zip:" + c.zip : "city:" + c.city + "|" + c.state;
        setStatus(f.scope === want
          ? "Done. Your TV switches in a few seconds."
          : "Done. Not enough homes there yet, so your TV shows " + areaOf(f) + ".", "ok");
        show(f);
      })
      .catch(function () { setStatus("Something went wrong. Please try again.", "err"); update(); });
  });
  load();
})();
</script>
</body>
</html>
```

- [ ] **Step 4: Serve it**

In `feed.go`, add `_ "embed"` and `"strings"` to the imports, then replace the stub with:

```go
//go:embed tv.html
var tvHTML string

// tvPage serves the mobile page. The page talks to this API from whatever
// origin nginx exposes it on, so the API origin is baked in here.
func (h *Handler) tvPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, strings.ReplaceAll(tvHTML, "__API_BASE__", h.viewers.PublicBaseURL))
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/linear/ -run TestFeed -v 2>&1 | tail -15`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/linear/tv.html internal/linear/feed.go internal/linear/feed_test.go
git commit -m "linear: mobile page at /tv for picking a household's area

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: Config, wiring, env, nginx, docs

**Files:**
- Modify: `internal/config/config.go` (struct near `AdsConfig`, `Load` near `Ads:`)
- Modify: `cmd/server/main.go:216-219`
- Modify: `.env.example` (after the `GEOIP_DB_PATH` line)
- Modify: `deploy/nginx-dwellings.conf` (api host before `location /channels/`; apex before `location = /404.html`)
- Modify: `deploy/README.md` (rollout notes)
- Modify: `docs/superpowers/specs/2026-10-09-personal-feeds-qr-design.md` (`/feed/me` row)
- Test: `internal/config/config_test.go` (if it exists; else skip the config test and rely on `go build`)

**Interfaces:**
- Consumes: `linear.FeedOptions`, `Handler.EnableFeeds`.
- Produces: env `MOBILE_BASE_URL` (default `https://dwellings.tv`), `QR_SHOW_SECONDS` (60), `QR_EVERY_SECONDS` (300).

- [ ] **Step 1: Config**

Add to `config.go` after `AdsConfig`:

```go
// FeedConfig controls the personal feeds and the QR the Roku app shows (see
// docs/superpowers/specs/2026-10-09-personal-feeds-qr-design.md).
type FeedConfig struct {
	// MobileBaseURL is the origin the mobile page is reached on; the QR
	// encodes <MobileBaseURL>/tv/<household>.
	MobileBaseURL string
	// QRShowSeconds / QREverySeconds are the cadence reported to the app.
	QRShowSeconds  int
	QREverySeconds int
}
```

Add the field `Feed FeedConfig` to `Config` after `Ads`, and in `Load` after the `Ads:` literal:

```go
		Feed: FeedConfig{
			MobileBaseURL:  strings.TrimRight(getenv("MOBILE_BASE_URL", "https://dwellings.tv"), "/"),
			QRShowSeconds:  getenvInt("QR_SHOW_SECONDS", 60),
			QREverySeconds: getenvInt("QR_EVERY_SECONDS", 300),
		},
```

In the validation section, next to the ad-tag URL check:

```go
	if !isHTTPURL(c.Feed.MobileBaseURL) {
		return nil, fmt.Errorf("MOBILE_BASE_URL must be an absolute http(s) URL, got %q", c.Feed.MobileBaseURL)
	}
	if c.Feed.QRShowSeconds < 1 || c.Feed.QREverySeconds < c.Feed.QRShowSeconds {
		return nil, fmt.Errorf("QR_SHOW_SECONDS (%d) must be >= 1 and <= QR_EVERY_SECONDS (%d)", c.Feed.QRShowSeconds, c.Feed.QREverySeconds)
	}
```

If `internal/config/config_test.go` exists, add a test that `Load` with `QR_SHOW_SECONDS=400 QR_EVERY_SECONDS=300` fails and that defaults give 60/300 and `https://dwellings.tv`; follow the file's existing env-setting helper.

- [ ] **Step 2: Wire it**

In `cmd/server/main.go`, replace

```go
		if cfg.Viewer.Enabled {
			h.EnableViewers(viewerOptions(ctx, cfg, pool, log))
		}
```

with

```go
		if cfg.Viewer.Enabled {
			h.EnableViewers(viewerOptions(ctx, cfg, pool, log))
			h.EnableFeeds(linear.FeedOptions{
				MobileBaseURL:  cfg.Feed.MobileBaseURL,
				QRShowSeconds:  cfg.Feed.QRShowSeconds,
				QREverySeconds: cfg.Feed.QREverySeconds,
			})
			log.Info("personal feeds enabled", "mobile_base_url", cfg.Feed.MobileBaseURL, "qr_show_s", cfg.Feed.QRShowSeconds, "qr_every_s", cfg.Feed.QREverySeconds)
		}
```

- [ ] **Step 3: `.env.example`**

After the `# GEOIP_DB_PATH=...` line:

```
# Personal feeds + the QR the Roku app shows (needs viewer tracking: feeds
# are keyed by the household's IP hash). The QR opens <MOBILE_BASE_URL>/tv/.
# MOBILE_BASE_URL=https://dwellings.tv
# QR_SHOW_SECONDS=60
# QR_EVERY_SECONDS=300
```

- [ ] **Step 4: nginx**

In the api host block, before `location /channels/ {`:

```nginx
    # Personal feeds. The media playlist names its household in the query,
    # so it caches per feed like the channel playlists. Everything else under
    # /feed/ and /tv/ depends on the connecting IP and falls through to the
    # uncached catch-all below.
    location = /feed/live.m3u8 {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 300s;

        proxy_cache dwellings_channels;
        proxy_cache_key $scheme$host$uri$is_args$args;
        proxy_ignore_headers Cache-Control Expires;
        proxy_cache_valid 200 2s;
        proxy_cache_lock on;
        proxy_cache_use_stale updating error timeout;
        add_header X-Cache-Status $upstream_cache_status always;
    }
```

In the apex 443 block, before `location = /404.html {`:

```nginx
    # The mobile page the TV's QR opens (served by the app; the page calls
    # api.dwellings.tv itself). X-Real-IP is what identifies the household.
    location = /tv { return 301 /tv/; }
    location /tv/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host              api.dwellings.tv;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
```

Update the file's header comment: the apex also serves `/tv/`.

- [ ] **Step 5: Docs**

In `deploy/README.md`, add a short "Personal feeds (2026-10)" rollout note: reinstall nginx conf (`nginx -t && systemctl reload nginx`), optionally set `MOBILE_BASE_URL` in `/opt/dwellings/.env` (default is right for prod), schema applies on restart, the GeoLite2 file gates the IP-city default, and the Roku app changes (play `resolve.feed`, show `resolve.qr.url` for `show_seconds` every `every_seconds`).

In the spec, change the `GET /feed/me` row to: "Same JSON as `GET /feed/{id}` for the connecting household (creates span 0 if needed), so a typed-in `/tv/` URL finds its feed by IP alone. `no-store`." and add `id` and `live` to the listed JSON fields of `GET /feed/{id}`.

- [ ] **Step 6: Build and test everything**

Run: `gofmt -l . ; go vet ./... && go test ./... && nginx -t -c "$PWD/deploy/nginx-dwellings.conf" 2>/dev/null || echo "nginx not installed locally: skip the syntax check"`
Expected: no gofmt output, vet clean, all tests PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/config/ cmd/server/main.go .env.example deploy/ docs/superpowers/specs/2026-10-09-personal-feeds-qr-design.md
git commit -m "feeds: config, wiring, nginx and rollout notes for personal feeds

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 10: End-to-end smoke in Docker

**Files:** none changed unless the smoke finds a bug.

- [ ] **Step 1: Bring the stack up locally**

`docker-compose.yml` defines the local DB and app. Run:

```bash
docker compose up -d --build
docker compose logs --tail=30 app
```

Expected: the app logs `personal feeds enabled` and `linear channels enabled`. (It needs `VIEWER_SALT` and `LINEAR_ENABLED=true` in the local `.env`; add them if missing.) If the local library has no segmented clips, the feed answers 503 `no content`: in that case verify only the routes mount (step 2's 503/404/400 cases) and say so in the report.

- [ ] **Step 2: Exercise the routes**

```bash
B=http://localhost:8080
curl -si -H 'X-Real-IP: 203.0.113.5' $B/feed/master.m3u8 | sed -n '1,12p'
HH=$(curl -s -H 'X-Real-IP: 203.0.113.5' $B/feed/me | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -s $B/feed/$HH
curl -s $B/feed/areas | head -c 300; echo
curl -s -o /tmp/qr.png -w '%{http_code} %{content_type}\n' -H 'X-Real-IP: 203.0.113.5' $B/feed/qr.png
curl -s "$B/feed/live.m3u8?hh=$HH" | head -20
curl -s -X POST -H 'Content-Type: application/json' -d '{"zip":"77494"}' $B/feed/$HH
sleep 15; curl -s "$B/feed/live.m3u8?hh=$HH" | head -30
curl -s $B/channels/resolve -H 'X-Real-IP: 203.0.113.5'
```

Expected: master lists `live.m3u8?hh=<id>`; the PNG is `image/png`; after the POST the live playlist, within ~15 s, shows an `#EXT-X-DISCONTINUITY` followed by segments of the chosen area and `#EXT-X-MEDIA-SEQUENCE` never lower than before.

- [ ] **Step 3: Play it in a browser**

Open `http://localhost:8080/tv/` in Chrome, pick a city with content, submit, and confirm the in-page player keeps playing through the switch (no stall or error in the console). Scan the QR from `http://localhost:8080/feed/qr.png` with a phone to confirm it decodes to `https://dwellings.tv/tv/<id>`.

- [ ] **Step 4: Report**

Report what was verified and what could not be (no DB content, no phone). Fix any bug found, with a test, and commit it as its own change.
