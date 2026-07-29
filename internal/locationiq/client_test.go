package locationiq

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

// TestStaticMap_NonPNGBodyIsRetryableError covers finding 3: a 200 response
// whose body is not a PNG (an HTML interstitial, a captive portal page, …)
// must not be treated as a usable map — that would get uploaded and
// permanently stamp the listing as mapped. It must be a normal, retryable
// error, never ErrNoMatch (which would mark the listing unmappable forever).
func TestStaticMap_NonPNGBodyIsRetryableError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>not a map</html>"))
	}))
	defer srv.Close()

	c := New("k", 5*time.Second)
	c.staticMapURL = srv.URL

	_, err := c.StaticMap(context.Background(), 1, 2)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if errors.Is(err, ErrNoMatch) {
		t.Error("a non-PNG body must be retryable, not ErrNoMatch")
	}
}

// fakeAPIKey is distinctive enough that finding it anywhere in an error
// string unambiguously proves a leak.
const fakeAPIKey = "pk.SECRET_DO_NOT_LOG"

// TestGeocode_TransportErrorDoesNotLeakAPIKey covers finding 1: a c.http.Do
// failure (timeout, DNS failure, connection refused — ordinary events)
// returns a *url.Error whose Error() string embeds the full request URL,
// including the "key" query parameter. That must never reach a caller (and
// from there, a log line) unredacted.
func TestGeocode_TransportErrorDoesNotLeakAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // now refuses connections

	c := New(fakeAPIKey, 2*time.Second)
	c.geocodeURL = srv.URL

	_, _, err := c.Geocode(context.Background(), testAddress())
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), fakeAPIKey) {
		t.Errorf("Geocode error leaks API key: %v", err)
	}
}

// TestStaticMap_TransportErrorDoesNotLeakAPIKey is the StaticMap counterpart
// of TestGeocode_TransportErrorDoesNotLeakAPIKey.
func TestStaticMap_TransportErrorDoesNotLeakAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // now refuses connections

	c := New(fakeAPIKey, 2*time.Second)
	c.staticMapURL = srv.URL

	_, err := c.StaticMap(context.Background(), 1, 2)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), fakeAPIKey) {
		t.Errorf("StaticMap error leaks API key: %v", err)
	}
}
