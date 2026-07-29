// Package propertymap generates a property's static map on demand: geocode if
// needed, fetch the pinned map, store it on the CDN, and persist the URL.
package propertymap

import (
	"bytes"
	"context"
	"errors"
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
	// defaultBudget caps generations per rolling hour. The detail endpoint is
	// public and unauthenticated, so this bounds quota burn from enumeration.
	defaultBudget = 500
)

// Client is the LocationIQ surface this service needs.
type Client interface {
	Geocode(ctx context.Context, a locationiq.Address) (lat, lon float64, err error)
	StaticMap(ctx context.Context, lat, lon float64) ([]byte, error)
}

// Uploader stores content and returns its public CDN URL.
type Uploader interface {
	Upload(ctx context.Context, path string, content io.Reader, contentType string) (string, error)
}

// Store persists what generation produces.
type Store interface {
	SetMapImage(ctx context.Context, zpid, url string) error
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
// Ensure is nil-receiver safe.
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

// Ensure returns the property's map URL, generating it when absent.
//
//	url != ""              — the map is ready
//	url == "", pending      — generation is still running in the background
//	url == "", !pending     — no map, and none is coming right now
//
// It waits at most s.deadline. Generation that outlives the deadline keeps
// running on a background context and persists its result for the next reader.
func (s *Service) Ensure(ctx context.Context, p *property.Property) (string, bool) {
	if s == nil || p == nil || p.ZPID == "" {
		return "", false
	}
	if p.MapImageURL != "" {
		return p.MapImageURL, false
	}
	// Stamped with no URL: the address could not be geocoded. Never retried.
	if p.MapGeneratedAt != nil {
		return "", false
	}
	if !s.allow(p.ZPID) {
		return "", false
	}

	// Detach from the request: a viewer navigating away must not abort a
	// half-finished map.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundTimeout)

	done := make(chan string, 1) // buffered: the goroutine never blocks on a timed-out caller
	go func() {
		defer cancel()
		url, err := s.generate(bg, p)
		if err != nil {
			s.penalize(p.ZPID)
			s.log.Warn("map generation failed", "zpid", p.ZPID, "error", err)
		}
		done <- url
	}()

	timer := time.NewTimer(s.deadline)
	defer timer.Stop()
	select {
	case url := <-done:
		return url, false
	case <-timer.C:
		return "", true
	}
}

// generate does the work, collapsed per zpid so concurrent viewers of the same
// listing produce exactly one geocode and one map fetch.
func (s *Service) generate(ctx context.Context, p *property.Property) (string, error) {
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
				if err := s.store.SetMapImage(ctx, p.ZPID, ""); err != nil {
					return "", err
				}
				s.log.Info("address not geocodable, marked unmappable", "zpid", p.ZPID)
				return "", nil
			case err != nil:
				return "", err
			}
			if err := s.store.SetCoordinates(ctx, p.ZPID, gotLat, gotLon); err != nil {
				return "", err
			}
			lat, lon = &gotLat, &gotLon
		}

		png, err := s.client.StaticMap(ctx, *lat, *lon)
		if err != nil {
			return "", err
		}

		url, err := s.uploader.Upload(ctx, "maps/"+p.ZPID+".png", bytes.NewReader(png), "image/png")
		if err != nil {
			return "", err
		}
		if err := s.store.SetMapImage(ctx, p.ZPID, url); err != nil {
			return "", err
		}
		s.log.Info("map generated", "zpid", p.ZPID, "url", url)
		return url, nil
	})
	if err != nil {
		return "", err
	}
	url, _ := v.(string)
	return url, nil
}

// allow applies the per-zpid cooldown and the rolling hourly budget, counting
// the generation when it permits one.
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
