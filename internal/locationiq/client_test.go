package locationiq

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func testAddress() Address {
	return Address{Street: "1234 Hilltop Drive", City: "Austin", State: "TX", PostalCode: "78746"}
}

func TestGeocode_BuildsStructuredQueryAndParsesResult(t *testing.T) {
	var gotQuery url.Values
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
	var gotQuery url.Values
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
