package zillow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/config"
)

// TestSearch_AgainstRealFixture serves the captured OpenWebNinja response and
// verifies the full Search path: envelope decode, field mapping, photo building,
// client-side filtering, and pagination termination.
func TestSearch_AgainstRealFixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/search_los_angeles.json")
	if err != nil {
		t.Fatal(err)
	}

	var gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-API-Key")
		if r.URL.Query().Get("location") == "" {
			t.Errorf("missing location param")
		}
		// Page 1 returns the fixture; later pages return an empty data array so
		// the pager terminates.
		if r.URL.Query().Get("page") != "1" {
			w.Write([]byte(`{"status":"OK","data":[]}`))
			return
		}
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "secret-key", 10*time.Second)
	props, err := c.Search(context.Background(), config.SearchCriteria{
		Location:   "Punta Gorda, FL",
		HomeStatus: "FOR_SALE",
		MaxResults: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	if gotAPIKey != "secret-key" {
		t.Errorf("X-API-Key not sent, got %q", gotAPIKey)
	}
	if len(props) != 41 {
		t.Fatalf("got %d properties, want 41", len(props))
	}

	// Every property must have a zpid; spot-check the first known listing.
	for _, p := range props {
		if p.ZPID == "" {
			t.Errorf("property with empty zpid: %+v", p)
		}
	}
	first := props[0]
	if first.ZPID != "20759214" || first.SalePrice != 1049000 {
		t.Errorf("first listing wrong: zpid=%s price=%d", first.ZPID, first.SalePrice)
	}
	if first.City != "Los Angeles" || first.State != "CA" || first.Zip != "90065" {
		t.Errorf("first address wrong: %s, %s %s", first.City, first.State, first.Zip)
	}
	if first.Bedrooms != 2 || first.Bathrooms != 2 || first.HomeSizeSqft != 2080 {
		t.Errorf("first metrics wrong: bd=%d ba=%v sqft=%d", first.Bedrooms, first.Bathrooms, first.HomeSizeSqft)
	}
	if len(first.ImageURLs) < 2 {
		t.Errorf("expected multiple carousel photos, got %d", len(first.ImageURLs))
	}
	if first.ImageURLs[0] != "https://photos.zillowstatic.com/fp/f0d1090f1fc1448fce5fc4648e9e4453-cc_ft_1536.jpg" {
		t.Errorf("first photo URL wrong: %s", first.ImageURLs[0])
	}
}

func TestSearch_FilterByPrice(t *testing.T) {
	fixture, _ := os.ReadFile("testdata/search_los_angeles.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			w.Write([]byte(`{"status":"OK","data":[]}`))
			return
		}
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "k", 10*time.Second)
	props, err := c.Search(context.Background(), config.SearchCriteria{
		Location:   "Punta Gorda, FL",
		MinPrice:   300000,
		MaxResults: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range props {
		if p.SalePrice < 300000 {
			t.Errorf("price filter leaked through: %d", p.SalePrice)
		}
	}
}
