package linear

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Options tune channel assembly. Zero values take the defaults below.
type Options struct {
	LineupHours     int // max content per lineup version
	MinScopeClips   int // a scope with fewer current clips falls back to its parent
	EPGHorizonHours int // how far ahead the EPG materialises versions
}

func (o Options) withDefaults() Options {
	if o.LineupHours <= 0 {
		o.LineupHours = 6
	}
	if o.MinScopeClips <= 0 {
		o.MinScopeClips = 20
	}
	if o.EPGHorizonHours <= 0 {
		o.EPGHorizonHours = 24
	}
	return o
}

// windowSegments is how many segments a live playlist lists.
const windowSegments = 6

// Service assembles channels from the store.
type Service struct {
	store Store
	opts  Options
	now   func() time.Time
	log   *slog.Logger

	mu    sync.Mutex
	cache map[string]cached // channel key → versions last served
}

type cached struct{ cur, prev *Version }

// New creates a Service.
func New(store Store, opts Options, log *slog.Logger) *Service {
	return &Service{store: store, opts: opts.withDefaults(), now: time.Now, log: log, cache: map[string]cached{}}
}

// current returns the versions serving key at t. Versions are immutable once
// stored, so a cached version that still covers t is served without a query.
func (s *Service) current(ctx context.Context, key string, t time.Time) (cur, prev *Version, err error) {
	s.mu.Lock()
	c, ok := s.cache[key]
	s.mu.Unlock()
	if ok && !t.Before(c.cur.StartsAt) && t.Before(c.cur.EndsAt) {
		return c.cur, c.prev, nil
	}
	cur, prev, err = s.versionAt(ctx, key, t)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	s.cache[key] = cached{cur: cur, prev: prev}
	s.mu.Unlock()
	return cur, prev, nil
}

// Playlist renders the live media playlist of sc at now.
func (s *Service) Playlist(ctx context.Context, sc Scope, now time.Time) ([]byte, error) {
	key := sc.Key()
	cur, prev, err := s.current(ctx, key, now)
	if err != nil {
		return nil, err
	}
	ids := windowClipIDs(cur, prev, now, windowSegments)
	clips, err := s.store.ClipsByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, ok := clips[id]; !ok {
			return nil, fmt.Errorf("channel %s v%d references missing clip %d", key, cur.Version, id)
		}
	}
	segs := window(cur, prev, clips, now, windowSegments)
	if len(segs) == 0 {
		return nil, ErrNoContent
	}
	var buf bytes.Buffer
	writePlaylist(&buf, segs)
	return buf.Bytes(), nil
}
