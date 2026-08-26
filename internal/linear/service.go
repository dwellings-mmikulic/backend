package linear

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
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

const (
	// cacheTTL bounds how long a cached version pair is reused (spec §4).
	cacheTTL = 30 * time.Second
	// cacheMaxEntries caps the cache. The endpoints are unauthenticated and
	// every distinct filter is a key, so the map must not be allowed to grow
	// with the number of filters anyone cares to invent.
	cacheMaxEntries = 4096
)

// Service assembles channels from the store.
type Service struct {
	store Store
	opts  Options
	now   func() time.Time
	log   *slog.Logger

	group singleflight.Group

	mu    sync.Mutex
	cache map[string]cached // channel key → versions last served

	contentMu  sync.Mutex
	hasContent bool
	contentAt  time.Time // when hasContent was last refreshed
}

// contentTTL is how long HasContent reuses its answer.
const contentTTL = time.Minute

type cached struct {
	cur, prev *Version
	at        time.Time // when the entry was stored
}

// New creates a Service.
func New(store Store, opts Options, log *slog.Logger) *Service {
	return &Service{store: store, opts: opts.withDefaults(), now: time.Now, log: log, cache: map[string]cached{}}
}

// cacheGet returns the cached versions of key when the entry is younger than
// cacheTTL and its cur still covers t.
func (s *Service) cacheGet(key string, t time.Time) (cur, prev *Version, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cache[key]
	if !ok {
		return nil, nil, false
	}
	if s.now().Sub(c.at) >= cacheTTL {
		delete(s.cache, key)
		return nil, nil, false
	}
	if !covers(c.cur, t) {
		return nil, nil, false
	}
	return c.cur, c.prev, true
}

// cachePut stores the versions of key, evicting first when the cache is full.
func (s *Service) cachePut(key string, cur, prev *Version) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.cache[key]; !exists && len(s.cache) >= cacheMaxEntries {
		s.evictLocked()
	}
	s.cache[key] = cached{cur: cur, prev: prev, at: s.now()}
}

// evictLocked makes room: expired entries first, then the oldest.
func (s *Service) evictLocked() {
	now := s.now()
	for k, c := range s.cache {
		if now.Sub(c.at) >= cacheTTL {
			delete(s.cache, k)
		}
	}
	for len(s.cache) >= cacheMaxEntries {
		var (
			oldestKey string
			oldest    time.Time
			found     bool
		)
		for k, c := range s.cache {
			if !found || c.at.Before(oldest) {
				oldestKey, oldest, found = k, c.at, true
			}
		}
		if !found {
			return
		}
		delete(s.cache, oldestKey)
	}
}

// current returns the versions serving key at t. Versions are immutable once
// stored, so a cached version that still covers t is served without a query.
// Concurrent misses on one key share a single resolution: a miss can cost a
// full scope resolution (up to four ListClips over the whole library) plus a
// version insert, and a version rollover makes every in-flight request for
// that channel miss at once.
func (s *Service) current(ctx context.Context, key string, t time.Time, src *lineupSource) (cur, prev *Version, err error) {
	if cur, prev, ok := s.cacheGet(key, t); ok {
		return cur, prev, nil
	}
	type pair struct{ cur, prev *Version }
	v, err, _ := s.group.Do(key, func() (any, error) {
		if cur, prev, ok := s.cacheGet(key, t); ok {
			return pair{cur, prev}, nil
		}
		cur, prev, err := s.versionAt(ctx, key, t, src)
		if err != nil {
			return nil, err
		}
		s.cachePut(key, cur, prev)
		return pair{cur, prev}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	p := v.(pair)
	if covers(p.cur, t) {
		return p.cur, p.prev, nil
	}
	// Two requests a few milliseconds apart can straddle a version boundary
	// and share one leader, whose answer then does not cover this t. The
	// version this one needs was stored by that leader, so this is a read,
	// not a second resolution.
	cur, prev, err = s.versionAt(ctx, key, t, src)
	if err != nil {
		return nil, nil, err
	}
	s.cachePut(key, cur, prev)
	return cur, prev, nil
}

// ErrUnknownArea reports a filter that names a place the library has never
// heard of, as opposed to a real place with no content yet.
var ErrUnknownArea = errors.New("unknown area")

// checkArea rejects a scope naming a place that does not exist, before any
// lineup version or cache entry is created for it.
func (s *Service) checkArea(ctx context.Context, sc Scope) error {
	if !sc.knownState() {
		return fmt.Errorf("%w: %s", ErrUnknownArea, sc.Key())
	}
	ok, err := s.store.AreaExists(ctx, sc)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownArea, sc.Key())
	}
	return nil
}

// Playlist renders the live media playlist of sc at now.
func (s *Service) Playlist(ctx context.Context, sc Scope, now time.Time) ([]byte, error) {
	if err := s.checkArea(ctx, sc); err != nil {
		return nil, err
	}
	key := sc.Key()
	cur, prev, err := s.current(ctx, key, now, s.newSource(key))
	if err != nil {
		return nil, err
	}
	ids := windowClipIDs(cur, prev, now, liveWindow)
	clips, err := s.store.ClipsByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	if err := checkClips(cur, clips); err != nil {
		return nil, err
	}
	if prev != nil {
		if err := checkClips(prev, clips); err != nil {
			return nil, err
		}
	}
	segs, err := window(cur, prev, clips, now, liveWindow)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, ErrNoContent
	}
	var buf bytes.Buffer
	writePlaylist(&buf, segs)
	return buf.Bytes(), nil
}

// checkClips verifies that every clip of v that was fetched agrees with the
// segment layout the version recorded. Items outside the window are not
// fetched and are not checked here; expandItems catches those.
func checkClips(v *Version, clips map[int64]ClipSegments) error {
	for i, id := range v.ItemIDs {
		c, ok := clips[id]
		if !ok {
			continue // outside the window: not fetched
		}
		if len(c.SegmentMS) != v.ItemSegs[i] {
			return &InconsistentLineupError{
				Channel: v.Key, Version: v.Version, Item: i, ClipID: id,
				Want: v.ItemSegs[i], Got: len(c.SegmentMS),
			}
		}
	}
	return nil
}

// HasContent reports whether the library has any current clip at all, i.e.
// whether the channels can play anything. The answer is cached for
// contentTTL: the Roku feed asks on every request, and the count is a scan
// of the whole video_hls join. The lock is held across the query, which is
// fine at feed request rates and keeps a cold cache from firing N counts.
// A failed count keeps the previous answer rather than pulling the channel
// out of the feed on a transient database error.
func (s *Service) HasContent(ctx context.Context) bool {
	s.contentMu.Lock()
	defer s.contentMu.Unlock()
	if !s.contentAt.IsZero() && s.now().Sub(s.contentAt) < contentTTL {
		return s.hasContent
	}
	n, err := s.store.CountCurrentClips(ctx)
	if err != nil {
		s.log.Error("count current clips failed", "error", err)
		return s.hasContent
	}
	s.hasContent, s.contentAt = n > 0, s.now()
	return s.hasContent
}
