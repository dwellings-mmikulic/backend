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
	"regexp"
	"strconv"
	"strings"
	"time"
)

// redactedKey replaces API key material in anything destined for an error
// message or a log line.
const redactedKey = "[redacted]"

// keyParamRE matches a key query parameter and its value in text echoed back
// to us — an upstream error page or proxy quoting the request URI, say. It is
// the backstop for key material we cannot match literally (percent-encoded
// differently, or a key belonging to some other request entirely).
var keyParamRE = regexp.MustCompile(`(?i)key=[^&\s"'<>]+`)

// maxBodySize bounds how much of a response we will ever read. Static maps
// are a few hundred KB at most; anything wildly larger is not a map (a proxy
// or captive-portal page, say) and must not be read into memory in full.
const maxBodySize = 5 << 20 // 5 MiB

// pngSignature is the fixed 8-byte header every valid PNG starts with.
var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// ErrNoMatch means LocationIQ could not geocode the address. Callers should
// record the property as permanently unmappable rather than retrying.
var ErrNoMatch = errors.New("locationiq: no match for address")

// StyleVersion identifies the current static-map look. It is part of the CDN
// object path, so bumping it gives restyled maps a fresh URL instead of
// overwriting the old object — a pull zone would otherwise keep serving the
// cached previous image and the change would appear not to have worked.
//
// Bump this whenever any rendering parameter below changes, and clear
// map_image_url/map_generated_at so stored maps regenerate:
//
//	UPDATE properties SET map_image_url = NULL, map_generated_at = NULL;
//
// v1: zoom 16, 600x400. v2: zoom 15, 1200x800 (wider framing, sharper on TV).
const StyleVersion = "v2"

// Static map rendering parameters. Constants rather than configuration —
// every listing map looks the same.
const (
	// mapZoom 15 at mapSize 1200x800 frames a named arterial road or landmark
	// alongside the pin while keeping the property's own street legible; at 16
	// the frame was all unnamed residential streets, which viewers could not
	// place. Zoom and size are chosen together: doubling the size at a fixed
	// zoom doubles the ground area covered, so changing one without the other
	// reframes the map. Changing either does NOT restyle maps already
	// generated; see StyleVersion.
	mapZoom = "15"
	// mapSize is deliberately larger than the display size: these are shown on
	// 1080p TV screens via Roku, where a 600x400 image visibly softens when
	// upscaled. LocationIQ caps size at 1280x1280.
	mapSize       = "1200x800"
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
	Lat     string `json:"lat"`
	Lon     string `json:"lon"`
	Address struct {
		HouseNumber string `json:"house_number"`
		Road        string `json:"road"`
	} `json:"address"`
}

// Geocode resolves a street address to coordinates. It returns ErrNoMatch when
// LocationIQ cannot resolve the address to a street, which is a permanent
// answer; every other error is transient and safe to retry.
//
// LocationIQ does not report an unresolvable US address as an error or an empty
// result. It falls back to a coarser match — for a wholly bogus address, the
// country centroid, which lands in Kansas with display_name "USA". Taking that
// at face value would pin a listing's map thousands of miles from the property
// and, because the caller treats a stored map as final, never correct it. So we
// request addressdetails and require the result to carry a road: a genuine
// street-level match always has one, and the centroid fallback never does.
//
// Deliberately NOT validated: that the returned postcode/city match the request.
// Correct matches routinely differ — 1600 Pennsylvania Ave NW resolves with
// postcode 20006 when queried as 20500 — so comparing them rejects good results.
func (c *Client) Geocode(ctx context.Context, a Address) (float64, float64, error) {
	q := url.Values{}
	q.Set("key", c.apiKey)
	q.Set("format", "json")
	q.Set("country", "us")
	q.Set("addressdetails", "1")
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
		return 0, 0, fmt.Errorf("geocode returned status %d: %s", status, c.scrub(truncate(body)))
	}

	var results []geocodeResult
	if err := json.Unmarshal(body, &results); err != nil {
		return 0, 0, fmt.Errorf("decode geocode response: %w", err)
	}
	if len(results) == 0 {
		return 0, 0, ErrNoMatch
	}
	// No road means LocationIQ fell back to a city/state/country centroid
	// rather than finding the street. Treat it as unresolvable.
	if results[0].Address.Road == "" {
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
		return nil, fmt.Errorf("static map returned status %d: %s", status, c.scrub(truncate(body)))
	}
	// A 200 with a non-PNG body (an HTML interstitial, a proxy or
	// captive-portal page) must not be treated as a usable map: it would get
	// uploaded and permanently stamped on the listing. This is a normal,
	// retryable error, not ErrNoMatch — the address itself may be fine.
	if !bytes.HasPrefix(body, pngSignature) {
		return nil, fmt.Errorf("static map returned non-PNG content: %s", c.scrub(truncate(body)))
	}
	return body, nil
}

// get performs a GET and returns the body and status. A non-2xx is not an
// error here — callers decide what each status means.
func (c *Client) get(ctx context.Context, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		// A malformed endpoint surfaces as a *url.Error from url.Parse, which
		// carries the whole URL — key included.
		return nil, 0, c.redactErr(err)
	}
	res, err := c.http.Do(req)
	if err != nil {
		// c.http.Do failures are ordinary events (timeout, DNS failure) and
		// come back as a *url.Error whose Error() string embeds the full
		// request URL — including our API key query parameter. Redact it
		// before it can reach a log line.
		return nil, 0, c.redactErr(err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodySize))
	if err != nil {
		return nil, 0, c.redactErr(err)
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

// scrub removes API key material from text that is about to become an error
// message. Response bodies are the second route a key can reach a log: an
// upstream error page or a proxy that quotes the request URI back at us puts
// the key in the body, which callers then log alongside the status code.
func (c *Client) scrub(s string) string {
	if c.apiKey != "" {
		s = strings.ReplaceAll(s, c.apiKey, redactedKey)
		s = strings.ReplaceAll(s, url.QueryEscape(c.apiKey), redactedKey)
	}
	return keyParamRE.ReplaceAllString(s, "key="+redactedKey)
}

// redactErr makes an arbitrary error safe to log: it strips the URL out of a
// *url.Error, then verifies no key material survived. If any did, the message
// is scrubbed and the chain dropped — an unwrappable error is a fair price for
// not printing a live credential.
func (c *Client) redactErr(err error) error {
	red := redactURLError(err)
	msg := red.Error()
	if scrubbed := c.scrub(msg); scrubbed != msg {
		return errors.New(scrubbed)
	}
	return red
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
