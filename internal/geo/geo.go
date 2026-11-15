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
	// found (a US ZIP, city or state). answered is false when the question
	// could not be asked: the lookup failed, timed out or was not made
	// because the request budget is spent. A caller that stores a default
	// for an address must not store that as if nothing were known about it.
	Locate(ip net.IP) (loc Location, ok, answered bool)
}

// DefaultMaxPerMinute is the request budget for the keyless tier, kept under
// its allowance of 60 a minute and 10 per 10 s so that another box or a
// retry does not push the account over it.
const DefaultMaxPerMinute = 45

// A sixth of the minute's budget may be spent in any ten seconds, so a
// burst cannot use the whole minute at once.
const (
	budgetWindow = time.Minute
	burstWindow  = 10 * time.Second
	burstShare   = int(budgetWindow / burstWindow)
)

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

	// The request budget: at most perMinute requests in any minute and a
	// sixth of that in any 10 s. sent holds the times of the requests of the
	// last minute. perMinute 0 means no budget.
	perMinute int
	sent      []time.Time
}

type entry struct {
	loc      Location
	ok       bool
	answered bool
	expires  time.Time
}

// NewFreeIPAPI returns a client for the API at base (DefaultFreeIPAPIURL or
// a paid server) with a budget of DefaultMaxPerMinute requests. A nil log
// discards.
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

		perMinute: DefaultMaxPerMinute,
	}
}

// SetMaxPerMinute sets the request budget; 0 removes it (a paid server
// without a rate limit). Call before use.
func (c *FreeIPAPI) SetMaxPerMinute(limit int) { c.perMinute = limit }

// spendLocked takes one request from the budget, or reports that none is
// left. c.mu must be held.
func (c *FreeIPAPI) spendLocked() bool {
	if c.perMinute == 0 {
		return true
	}
	now := c.now()
	keep := c.sent[:0]
	recent := 0
	for _, sentAt := range c.sent {
		if now.Sub(sentAt) < budgetWindow {
			keep = append(keep, sentAt)
			if now.Sub(sentAt) < burstWindow {
				recent++
			}
		}
	}
	c.sent = keep
	if len(c.sent) >= c.perMinute || recent >= max(1, c.perMinute/burstShare) {
		return false
	}
	c.sent = append(c.sent, now)
	return true
}

// Locate implements Lookup. Private, loopback and unspecified addresses are
// misses without a request: the API has nothing to say about them. Only US
// results are reported; the channels are US-only, so a foreign viewer gets
// the national default.
//
// An address the API has nothing to say about is answered. A lookup that is
// not made because the budget is spent is not answered and is not
// remembered, so the next request for that address asks again once there is
// budget.
func (c *FreeIPAPI) Locate(ip net.IP) (Location, bool, bool) {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return Location{}, false, true
	}
	key := ip.String()
	for {
		c.mu.Lock()
		if cached, ok := c.entries[key]; ok && c.now().Before(cached.expires) {
			c.mu.Unlock()
			return cached.loc, cached.ok, cached.answered
		}
		if wait, ok := c.inflight[key]; ok {
			c.mu.Unlock()
			<-wait
			continue
		}
		if !c.spendLocked() {
			c.mu.Unlock()
			return Location{}, false, false
		}
		done := make(chan struct{})
		c.inflight[key] = done
		c.mu.Unlock()

		loc, ok, err := c.fetch(key)
		cached := entry{loc: loc, ok: ok, answered: true, expires: c.now().Add(hitTTL)}
		if err != nil {
			c.log.Warn("ip lookup failed", "error", err)
			cached = entry{expires: c.now().Add(failureTTL)}
		}
		c.mu.Lock()
		if len(c.entries) >= maxEntries {
			c.entries = map[string]entry{}
		}
		c.entries[key] = cached
		delete(c.inflight, key)
		c.mu.Unlock()
		close(done)
		return cached.loc, cached.ok, cached.answered
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
