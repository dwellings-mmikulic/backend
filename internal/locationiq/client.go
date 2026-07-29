// Package locationiq is a client for the LocationIQ geocoding and static maps
// APIs. It knows about LocationIQ's URLs and response shapes and nothing else —
// see internal/propertymap for the property-facing generation policy.
package locationiq

import (
	"bytes"
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

// maxBodySize bounds how much of a response we will ever read. Static maps
// are a few hundred KB at most; anything wildly larger is not a map (a proxy
// or captive-portal page, say) and must not be read into memory in full.
const maxBodySize = 5 << 20 // 5 MiB

// pngSignature is the fixed 8-byte header every valid PNG starts with.
var pngSignature = []byte("\x89PNG\r\n\x1a\n")

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
	// A 200 with a non-PNG body (an HTML interstitial, a proxy or
	// captive-portal page) must not be treated as a usable map: it would get
	// uploaded and permanently stamped on the listing. This is a normal,
	// retryable error, not ErrNoMatch — the address itself may be fine.
	if !bytes.HasPrefix(body, pngSignature) {
		return nil, fmt.Errorf("static map returned non-PNG content: %s", truncate(body))
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
		// c.http.Do failures are ordinary events (timeout, DNS failure) and
		// come back as a *url.Error whose Error() string embeds the full
		// request URL — including our API key query parameter. Redact it
		// before it can reach a log line.
		return nil, 0, redactURLError(err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodySize))
	if err != nil {
		return nil, 0, err
	}
	return body, res.StatusCode, nil
}

// redactURLError strips the request URL out of a *url.Error, since its
// Error() string otherwise embeds the full URL — query string and all. It
// preserves the operation and the underlying cause but deliberately does not
// keep the *url.Error itself reachable via errors.As/errors.Unwrap: any
// wrapping that left the original error in the chain would let a caller
// recover the raw URL (and the API key in it) straight back out.
func redactURLError(err error) error {
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		return err
	}
	return fmt.Errorf("%s [redacted url]: %w", uerr.Op, uerr.Err)
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
