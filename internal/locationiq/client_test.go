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
		_, _ = w.Write([]byte(`[{"lat":"30.2672","lon":"-97.7431","display_name":"1234 Hilltop Drive, Austin, TX",
			"address":{"house_number":"1234","road":"Hilltop Drive","city":"Austin","state":"Texas","postcode":"78746"}}]`))
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
		"key":            "test-key",
		"format":         "json",
		"country":        "us",
		"addressdetails": "1",
		"street":         "1234 Hilltop Drive",
		"city":           "Austin",
		"state":          "TX",
		"postalcode":     "78746",
	}
	for k, v := range want {
		if got := gotQuery.Get(k); got != v {
			t.Errorf("query %q = %q, want %q", k, got, v)
		}
	}
}

// LocationIQ does not report an unresolvable US address as an error or an empty
// result — it falls back to the country centroid. This fixture is the real
// response captured live for "99999 Zzqqxx Nonexistent Boulevard, Zzqqxxville,
// ZZ 00000": coordinates in Kansas, display_name "USA", and an address object
// with nothing below country level. Accepting it would pin a listing's map
// thousands of miles from the property, permanently.
func TestGeocode_CountryCentroidFallbackIsErrNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"place_id":"330080572487","lat":"39.71614","lon":"-96.999246",
			"display_name":"USA","importance":0.025,
			"address":{"country":"United States of America","country_code":"us"}}]`))
	}))
	defer srv.Close()

	c := New("k", 5*time.Second)
	c.geocodeURL = srv.URL

	lat, lon, err := c.Geocode(context.Background(), testAddress())
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("err = %v, want ErrNoMatch (got coords %v, %v)", err, lat, lon)
	}
}

// A city-level match (no house number, no road) is the same failure in a
// subtler form: the right town, the wrong house.
func TestGeocode_CityLevelFallbackIsErrNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"lat":"30.2672","lon":"-97.7431","display_name":"Austin, Texas, USA",
			"address":{"city":"Austin","state":"Texas","country_code":"us"}}]`))
	}))
	defer srv.Close()

	c := New("k", 5*time.Second)
	c.geocodeURL = srv.URL

	if _, _, err := c.Geocode(context.Background(), testAddress()); !errors.Is(err, ErrNoMatch) {
		t.Errorf("err = %v, want ErrNoMatch", err)
	}
}

// A street-level match without a house number is still the right street, which
// is good enough to pin a map on.
func TestGeocode_RoadWithoutHouseNumberIsAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"lat":"30.2672","lon":"-97.7431","display_name":"Hilltop Drive, Austin",
			"address":{"road":"Hilltop Drive","city":"Austin","state":"Texas"}}]`))
	}))
	defer srv.Close()

	c := New("k", 5*time.Second)
	c.geocodeURL = srv.URL

	lat, lon, err := c.Geocode(context.Background(), testAddress())
	if err != nil {
		t.Fatalf("Geocode: %v", err)
	}
	if lat != 30.2672 || lon != -97.7431 {
		t.Errorf("coords = %v, %v", lat, lon)
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
		"zoom":    "15",
		"size":    "1200x800",
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

// The response body is the second route an API key can reach a log. Upstream
// error pages and proxies routinely quote the request URI back at you, and the
// non-2xx / non-PNG paths put up to 200 bytes of that body straight into the
// error message. These tests make the server echo the request URI — exactly
// what a captive portal or a 4xx page from a proxy does.

// echoRequestURIServer replies with the given status and a body that quotes the
// full request URI, key query parameter and all.
func echoRequestURIServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`<html><body>Blocked request: ` + r.URL.RequestURI() + `</body></html>`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGeocode_ErrorBodyEchoingRequestDoesNotLeakAPIKey(t *testing.T) {
	srv := echoRequestURIServer(t, http.StatusBadGateway)

	c := New(fakeAPIKey, 5*time.Second)
	c.geocodeURL = srv.URL

	_, _, err := c.Geocode(context.Background(), testAddress())
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), fakeAPIKey) {
		t.Errorf("geocode error leaks API key via the response body: %v", err)
	}
	if !strings.Contains(err.Error(), redactedKey) {
		t.Errorf("expected the key to be replaced with %q, got: %v", redactedKey, err)
	}
}

func TestStaticMap_ErrorBodyEchoingRequestDoesNotLeakAPIKey(t *testing.T) {
	srv := echoRequestURIServer(t, http.StatusForbidden)

	c := New(fakeAPIKey, 5*time.Second)
	c.staticMapURL = srv.URL

	_, err := c.StaticMap(context.Background(), 30.2672, -97.7431)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), fakeAPIKey) {
		t.Errorf("static map error leaks API key via the response body: %v", err)
	}
}

// The non-PNG path is a 200, so it bypasses the status-code branch entirely
// and needs its own scrubbing.
func TestStaticMap_NonPNGBodyEchoingRequestDoesNotLeakAPIKey(t *testing.T) {
	srv := echoRequestURIServer(t, http.StatusOK)

	c := New(fakeAPIKey, 5*time.Second)
	c.staticMapURL = srv.URL

	_, err := c.StaticMap(context.Background(), 30.2672, -97.7431)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), fakeAPIKey) {
		t.Errorf("non-PNG error leaks API key via the response body: %v", err)
	}
}

// A body may carry key material that is not our literal key — a differently
// encoded copy, or another tenant's key echoed by a shared proxy. The generic
// key= backstop has to catch those too.
func TestScrub_RedactsForeignKeyMaterial(t *testing.T) {
	c := New(fakeAPIKey, time.Second)

	for _, tc := range []struct {
		name string
		in   string
	}{
		{"our key", "https://x/v1/search?key=" + fakeAPIKey + "&city=Austin"},
		{"another key", "https://x/v1/search?key=pk.someoneelseskey123&city=Austin"},
		{"uppercase param", "https://x/v1/search?KEY=pk.shouty456&city=Austin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := c.scrub(tc.in)
			if strings.Contains(got, "pk.") {
				t.Errorf("scrub left key material: %q", got)
			}
			if !strings.Contains(got, "city=Austin") {
				t.Errorf("scrub destroyed non-secret context: %q", got)
			}
		})
	}
}

// A malformed endpoint fails inside http.NewRequestWithContext, which returns
// a *url.Error carrying the whole URL before any request is sent.
func TestGeocode_MalformedEndpointDoesNotLeakAPIKey(t *testing.T) {
	c := New(fakeAPIKey, time.Second)
	c.geocodeURL = "http://bad\x7fhost/v1/search" // control byte: url.Parse rejects it

	_, _, err := c.Geocode(context.Background(), testAddress())
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), fakeAPIKey) {
		t.Errorf("request-build error leaks API key: %v", err)
	}
}
