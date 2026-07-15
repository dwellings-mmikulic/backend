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
