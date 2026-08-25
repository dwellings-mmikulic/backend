// Package propertymap generates a property's static maps on demand: geocode
// if needed, fetch the map in every style, store them on the CDN, and
// persist the URLs.
package propertymap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/dwellingtw/backend/internal/locationiq"
	"github.com/dwellingtw/backend/internal/property"
	"golang.org/x/sync/singleflight"
)

// Defaults. Overridable on the struct by tests in this package.
const (
	// defaultDeadline is how long a detail request waits for a map before
	// giving up and letting generation finish in the background.
	defaultDeadline = 1500 * time.Millisecond
	// backgroundTimeout bounds generation once the request has moved on.
	backgroundTimeout = 20 * time.Second
	// defaultCooldown is how long a zpid is skipped after a transient failure,
	// so a LocationIQ outage cannot turn every request into a retry.
	defaultCooldown = 10 * time.Minute
	// defaultBudget caps admitted Ensure attempts per tumbling window (see
	// allow), which is an upper bound on generations, not an exact count of
	// LocationIQ calls: allow() is checked before the singleflight collapse in
	// generate, so N concurrent first-viewers of the same zpid can each
	// consume a budget slot even though they share a single underlying
	// geocode/map fetch. The detail endpoint is public and unauthenticated,
	// so this bounds worst-case quota burn from enumeration; it is
	// intentionally fail-closed (it can under-admit, never over-admit).
	defaultBudget = 500
)

// Client is the LocationIQ surface this service needs.
type Client interface {
	Geocode(ctx context.Context, a locationiq.Address) (lat, lon float64, err error)
	StaticMap(ctx context.Context, lat, lon float64, style locationiq.Style) ([]byte, error)
}

// Uploader stores content and returns its public CDN URL.
type Uploader interface {
	Upload(ctx context.Context, path string, content io.Reader, contentType string) (string, error)
}

// Store persists what generation produces.
type Store interface {
	SetMapImage(ctx context.Context, zpid string, m property.MapURLs) error
	SetCoordinates(ctx context.Context, zpid string, lat, lon float64) error
}

// Service generates and caches property maps.
type Service struct {
	client   Client
	uploader Uploader
	store    Store
	log      *slog.Logger
	group    singleflight.Group

	// Tunables, defaulted in New and overridden by tests.
	deadline time.Duration
	cooldown time.Duration
	budget   int
	now      func() time.Time

	mu          sync.Mutex
	coolUntil   map[string]time.Time
	windowStart time.Time
	windowCount int
}

// New creates the service. Pass a nil *Service to consumers to disable maps —
// Ensure is nil-receiver safe. log must be non-nil: Ensure and generate log
// through it unconditionally, and a nil *slog.Logger panics on first use.
func New(client Client, up Uploader, store Store, log *slog.Logger) *Service {
	return &Service{
		client:    client,
		uploader:  up,
		store:     store,
		log:       log,
		deadline:  defaultDeadline,
		cooldown:  defaultCooldown,
		budget:    defaultBudget,
		now:       time.Now,
		coolUntil: map[string]time.Time{},
	}
}

// Ensure returns the property's map URLs, generating any that are absent.
//
//	m.Complete()            — every map is ready
//	!m.Complete(), pending  — generation is still running in the background
//	!m.Complete(), !pending — some map is missing, and none is coming right now
//
// Maps already stored on p are returned as-is; only the missing styles are
// fetched, so a row generated before dark maps existed costs one extra
// LocationIQ call, not a full regeneration.
//
// It waits at most s.deadline. Generation that outlives the deadline keeps
// running on a background context and persists its result for the next reader.
//
// Aliasing contract: when generation outlives the deadline, a background
// goroutine keeps reading p after Ensure has returned. The caller must not
// mutate p after calling Ensure, and p must not be a pointer shared across
// requests (e.g. a cached singleton) — each call must own its own
// *property.Property. This is what makes the detached goroutine safe.
func (s *Service) Ensure(ctx context.Context, p *property.Property) (property.MapURLs, bool) {
	if s == nil || p == nil || p.ZPID == "" {
		return property.MapURLs{}, false
	}
	have := property.MapURLs{Light: p.MapImageURL, Dark: p.MapImageDarkURL}
	if have.Complete() {
		return have, false
	}
	// Stamped with no light URL: the address could not be geocoded. Never
	// retried. (Stamped with a light URL but no dark one is the pre-dark-maps
	// state and falls through to generate the missing style.)
	if p.MapGeneratedAt != nil && have.Light == "" {
		return have, false
	}
	if !s.allow(p.ZPID) {
		return have, false
	}

	// Detach from the request: a viewer navigating away must not abort a
	// half-finished map.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundTimeout)

	done := make(chan property.MapURLs, 1) // buffered: the goroutine never blocks on a timed-out caller
	go func() {
		defer cancel()
		m, err := s.generate(bg, p, have)
		if err != nil {
			s.penalize(p.ZPID)
			s.log.Warn("map generation failed", "zpid", p.ZPID, "error", err)
		}
		done <- m
	}()

	timer := time.NewTimer(s.deadline)
	defer timer.Stop()
	select {
	case m := <-done:
		return m, false
	case <-timer.C:
		return have, true
	}
}

// generate does the work, collapsed per zpid so concurrent viewers of the same
// listing produce exactly one geocode and one fetch per missing style. have
// holds the maps already stored; on failure it is returned unchanged.
func (s *Service) generate(ctx context.Context, p *property.Property, have property.MapURLs) (property.MapURLs, error) {
	v, err, _ := s.group.Do(p.ZPID, func() (any, error) {
		lat, lon := p.Latitude, p.Longitude

		if lat == nil || lon == nil {
			gotLat, gotLon, err := s.client.Geocode(ctx, locationiq.Address{
				Street:     p.Address,
				City:       p.City,
				State:      p.State,
				PostalCode: p.Zip,
			})
			switch {
			case errors.Is(err, locationiq.ErrNoMatch):
				// Permanent: record an unmappable row so it is never retried.
				if err := s.store.SetMapImage(ctx, p.ZPID, property.MapURLs{}); err != nil {
					return have, err
				}
				s.log.Info("address not geocodable, marked unmappable", "zpid", p.ZPID)
				return property.MapURLs{}, nil
			case err != nil:
				return have, err
			}
			if err := s.store.SetCoordinates(ctx, p.ZPID, gotLat, gotLon); err != nil {
				return have, err
			}
			lat, lon = &gotLat, &gotLon
		}

		m := have
		if m.Light == "" {
			url, err := s.fetchAndUpload(ctx, p.ZPID, *lat, *lon, locationiq.StyleLight)
			if err != nil {
				return have, err
			}
			m.Light = url
		}
		if m.Dark == "" {
			url, err := s.fetchAndUpload(ctx, p.ZPID, *lat, *lon, locationiq.StyleDark)
			if err != nil {
				return have, err
			}
			m.Dark = url
		}
		if err := s.store.SetMapImage(ctx, p.ZPID, m); err != nil {
			return have, err
		}
		s.log.Info("maps generated", "zpid", p.ZPID, "light", m.Light, "dark", m.Dark)
		return m, nil
	})
	if err != nil {
		return have, err
	}
	m, _ := v.(property.MapURLs)
	return m, nil
}

// fetchAndUpload renders one style and stores it, returning the CDN URL.
func (s *Service) fetchAndUpload(ctx context.Context, zpid string, lat, lon float64, style locationiq.Style) (string, error) {
	png, err := s.client.StaticMap(ctx, lat, lon, style)
	if err != nil {
		return "", fmt.Errorf("%s map: %w", style, err)
	}
	return s.uploader.Upload(ctx, ObjectPath(zpid, style), bytes.NewReader(png), "image/png")
}

// ObjectPath is the CDN object path for a property's map in the given style.
// The style version is in the path so a restyle lands on a fresh URL rather
// than overwriting an object the CDN may still be serving. The light map
// keeps the original, suffix-free name so maps generated before dark maps
// existed stay valid.
func ObjectPath(zpid string, style locationiq.Style) string {
	suffix := ""
	if style != locationiq.StyleLight {
		suffix = "-" + string(style)
	}
	return "maps/" + locationiq.StyleVersion + "/" + zpid + suffix + ".png"
}

// allow applies the per-zpid cooldown and the hourly budget, counting the
// attempt when it permits one.
//
// The budget window is tumbling, not rolling: it resets to a fresh count the
// first time it is checked at least an hour after windowStart, rather than
// sliding continuously. A burst just before a window boundary (e.g. at the
// 59th minute) plus another burst just after it resets (e.g. at the 61st
// minute) can therefore admit up to 2x the budget within a couple of minutes.
func (s *Service) allow(zpid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if until, ok := s.coolUntil[zpid]; ok {
		if now.Before(until) {
			return false
		}
		delete(s.coolUntil, zpid)
	}

	if s.windowStart.IsZero() || now.Sub(s.windowStart) >= time.Hour {
		s.windowStart = now
		s.windowCount = 0
	}
	if s.windowCount >= s.budget {
		s.log.Warn("map generation budget exhausted", "zpid", zpid, "budget", s.budget)
		return false
	}
	s.windowCount++
	return true
}

// penalize puts a zpid on cooldown after a transient failure, pruning entries
// that have already expired so the map does not grow without bound.
func (s *Service) penalize(zpid string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for k, until := range s.coolUntil {
		if now.After(until) {
			delete(s.coolUntil, k)
		}
	}
	s.coolUntil[zpid] = now.Add(s.cooldown)
}
