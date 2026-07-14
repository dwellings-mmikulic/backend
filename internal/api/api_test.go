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

func strp(s string) *string   { return &s }
func intp(n int) *int         { return &n }
func f64p(f float64) *float64 { return &f }

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
