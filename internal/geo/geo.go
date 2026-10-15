// Package geo maps a client IP to a US location through the Free IP API
// (https://freeipapi.com). Lookups are best-effort: an answer the API cannot
// give, or cannot give in time, is a miss and the caller falls back.
package geo

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Location is what the channel resolver needs. Fields may be empty.
type Location struct {
	Zip   string
	City  string
	State string // 2-letter code, lowercase
}

// Lookup locates IPs.
type Lookup interface {
	// Locate returns the location of ip and whether anything useful was
	// found (a US ZIP, city or state).
	Locate(ip net.IP) (Location, bool)
}

// DefaultFreeIPAPIURL is the keyless tier: 10 requests per 10 s, 60 per
// minute. A paid server is "https://<region>.freeipapi.com/api" with the
// caller's address whitelisted.
const DefaultFreeIPAPIURL = "https://free.freeipapi.com/api/v1"

const (
	// requestTimeout bounds a lookup: Locate runs inside a playlist request.
	requestTimeout = 2 * time.Second
	// hitTTL is how long an answer is reused. Home addresses move rarely.
	hitTTL = 24 * time.Hour
	// failureTTL is how long an error (rate limit, outage, timeout) is
	// remembered as a miss, so a burst of polls from one new address makes
	// one request, not one per poll.
	failureTTL = time.Minute
	// maxEntries bounds the cache; when it is full the cache is dropped
	// wholesale rather than evicted, which is enough at our scale.
	maxEntries = 50_000
)

// FreeIPAPI is a Lookup over the Free IP API with a per-address cache.
type FreeIPAPI struct {
	base string // ".../api/v1" or ".../api"; "/json/{ip}" is appended
	http *http.Client
	log  *slog.Logger
	now  func() time.Time

	mu       sync.Mutex
	entries  map[string]entry
	inflight map[string]chan struct{}
}

type entry struct {
	loc     Location
	ok      bool
	expires time.Time
}

// NewFreeIPAPI returns a client for the API at base (DefaultFreeIPAPIURL or
// a paid server). A nil log discards.
func NewFreeIPAPI(base string, log *slog.Logger) *FreeIPAPI {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &FreeIPAPI{
		base:     strings.TrimRight(base, "/"),
		http:     &http.Client{Timeout: requestTimeout},
		log:      log,
		now:      time.Now,
		entries:  map[string]entry{},
		inflight: map[string]chan struct{}{},
	}
}

// Locate implements Lookup. Private, loopback and unspecified addresses are
// misses without a request: the API has nothing to say about them. Only US
// results are reported; the channels are US-only, so a foreign viewer gets
// the national default.
func (c *FreeIPAPI) Locate(ip net.IP) (Location, bool) {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return Location{}, false
	}
	key := ip.String()
	for {
		c.mu.Lock()
		if e, ok := c.entries[key]; ok && c.now().Before(e.expires) {
			c.mu.Unlock()
			return e.loc, e.ok
		}
		if wait, ok := c.inflight[key]; ok {
			c.mu.Unlock()
			<-wait
			continue
		}
		done := make(chan struct{})
		c.inflight[key] = done
		c.mu.Unlock()

		loc, ok, err := c.fetch(key)
		e := entry{loc: loc, ok: ok, expires: c.now().Add(hitTTL)}
		if err != nil {
			c.log.Warn("ip lookup failed", "error", err)
			e = entry{expires: c.now().Add(failureTTL)}
		}
		c.mu.Lock()
		if len(c.entries) >= maxEntries {
			c.entries = map[string]entry{}
		}
		c.entries[key] = e
		delete(c.inflight, key)
		c.mu.Unlock()
		close(done)
		return e.loc, e.ok
	}
}

// fetch asks the API. A non-US answer is (Location{}, false, nil); the API
// writes "-" for fields it does not know.
func (c *FreeIPAPI) fetch(ip string) (Location, bool, error) {
	res, err := c.http.Get(c.base + "/json/" + ip)
	if err != nil {
		return Location{}, false, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Location{}, false, fmt.Errorf("freeipapi: status %d", res.StatusCode)
	}
	var body struct {
		CountryCode string `json:"countryCode"`
		ZipCode     string `json:"zipCode"`
		CityName    string `json:"cityName"`
		RegionCode  string `json:"regionCode"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&body); err != nil {
		return Location{}, false, fmt.Errorf("freeipapi: decode: %w", err)
	}
	if body.CountryCode != "US" {
		return Location{}, false, nil
	}
	loc := Location{Zip: field(body.ZipCode), City: field(body.CityName), State: strings.ToLower(field(body.RegionCode))}
	return loc, loc.Zip != "" || (loc.City != "" && loc.State != "") || loc.State != "", nil
}

func field(s string) string {
	s = strings.TrimSpace(s)
	if s == "-" {
		return ""
	}
	return s
}
