package linear

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlaylist_IsDeterministicAcrossInstances(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	now := t0.Add(90 * time.Second)
	a := testService(m, now)
	b := testService(m, now)

	pa, err := a.Playlist(context.Background(), Scope{Zip: "77494"}, now)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := b.Playlist(context.Background(), Scope{Zip: "77494"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pa, pb) {
		t.Errorf("instances disagree:\n%s\n---\n%s", pa, pb)
	}
	if !strings.HasPrefix(string(pa), "#EXTM3U\n") || strings.Count(string(pa), "#EXTINF:") != windowSegments {
		t.Errorf("unexpected playlist:\n%s", pa)
	}
}

func TestPlaylist_AdvancesWithTime(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	p1, err := s.Playlist(context.Background(), Scope{Zip: "77494"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.Playlist(context.Background(), Scope{Zip: "77494"}, t0.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(p1, p2) {
		t.Error("playlist did not advance after 30 s")
	}
}

func TestPlaylist_NoContent(t *testing.T) {
	s := testService(newMemStore(), t0)
	if _, err := s.Playlist(context.Background(), Scope{}, t0); !errors.Is(err, ErrNoContent) {
		t.Errorf("err = %v, want ErrNoContent", err)
	}
}

// After an EPG request has pushed a channel's version chain hours into the
// future, a second instance with a cold cache must still serve the live
// playlist for now: "the version covering t" is the newest version that
// started at or before t, not the chain's tip.
func TestPlaylist_ServesWhenChainTipIsAheadOfNow(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	now := t0.Add(90 * time.Second)

	a := testService(m, now)
	if _, err := a.EPG(context.Background(), Scope{Zip: "77494"}, now); err != nil {
		t.Fatalf("EPG on instance A: %v", err)
	}
	tip, _ := m.LatestVersions(context.Background(), "zip:77494", 1)
	if len(tip) == 0 || !tip[0].StartsAt.After(now) {
		t.Fatalf("test precondition: EPG should have pushed the chain tip past now, tip = %+v", tip)
	}

	b := testService(m, now) // second instance, cold cache
	body, err := b.Playlist(context.Background(), Scope{Zip: "77494"}, now)
	if err != nil {
		t.Fatalf("Playlist on a second instance after A's EPG: %v", err)
	}
	if !bytes.HasPrefix(body, []byte("#EXTM3U\n")) {
		t.Errorf("playlist:\n%s", body)
	}
}

// The same channel, same instance: once now passes the cached version's
// EndsAt while the chain already reaches far beyond it, the next version of
// the chain must be served rather than the tip (or an error).
func TestPlaylist_FollowsTheChainPastTheCachedVersion(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40) // 40 min per version
	s := testService(m, t0)
	if _, err := s.EPG(context.Background(), Scope{Zip: "77494"}, t0); err != nil {
		t.Fatal(err)
	}
	cur, _, err := s.current(context.Background(), "zip:77494", t0)
	if err != nil {
		t.Fatal(err)
	}

	later := cur.EndsAt.Add(time.Minute)
	s.now = func() time.Time { return later }
	next, _, err := s.current(context.Background(), "zip:77494", later)
	if err != nil {
		t.Fatalf("after the cached version ended: %v", err)
	}
	if next.StartsAt.After(later) || !later.Before(next.EndsAt) {
		t.Errorf("served v%d [%s, %s), which does not cover %s", next.Version, next.StartsAt, next.EndsAt, later)
	}
	if next.Version != cur.Version+1 {
		t.Errorf("served v%d after v%d, want the immediate successor", next.Version, cur.Version)
	}
	if _, err := s.Playlist(context.Background(), Scope{Zip: "77494"}, later); err != nil {
		t.Errorf("Playlist past the cached version: %v", err)
	}
}

// Crossing that boundary must not rewind MEDIA-SEQUENCE or
// DISCONTINUITY-SEQUENCE.
func TestPlaylist_CountersMonotonicAcrossChainAdvance(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	if _, err := s.EPG(context.Background(), Scope{Zip: "77494"}, t0); err != nil {
		t.Fatal(err)
	}
	cur, _, err := s.current(context.Background(), "zip:77494", t0)
	if err != nil {
		t.Fatal(err)
	}

	lastSeq, lastDisc := int64(-1), int64(-1)
	for now := cur.EndsAt.Add(-30 * time.Second); now.Before(cur.EndsAt.Add(90 * time.Second)); now = now.Add(15 * time.Second) {
		s.now = func() time.Time { return now }
		body, err := s.Playlist(context.Background(), Scope{Zip: "77494"}, now)
		if err != nil {
			t.Fatalf("at %s: %v", now, err)
		}
		mm := seqRe.FindStringSubmatch(string(body))
		if mm == nil {
			t.Fatalf("no counters at %s:\n%s", now, body)
		}
		seq, _ := strconv.ParseInt(mm[1], 10, 64)
		disc, _ := strconv.ParseInt(mm[2], 10, 64)
		if seq < lastSeq || disc < lastDisc {
			t.Errorf("at %s counters went backwards: seq %d→%d disc %d→%d", now, lastSeq, seq, lastDisc, disc)
		}
		lastSeq, lastDisc = seq, disc
	}
}

// The version cache must not grow without bound (one entry per distinct
// filter an unauthenticated caller can invent) and must not serve entries
// older than cacheTTL.
func TestServiceCache_ExpiresEntries(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	if _, _, err := s.current(context.Background(), "zip:77494", t0); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.cacheGet("zip:77494", t0); !ok {
		t.Fatal("entry should be cached right after a lookup")
	}
	s.now = func() time.Time { return t0.Add(cacheTTL + time.Second) }
	if _, _, ok := s.cacheGet("zip:77494", t0); ok {
		t.Error("entry older than cacheTTL must not be served")
	}
	if len(s.cache) != 0 {
		t.Errorf("expired entry left in the cache: %d entries", len(s.cache))
	}
}

func TestServiceCache_IsSizeCapped(t *testing.T) {
	s := testService(newMemStore(), t0)
	for i := 0; i < cacheMaxEntries+500; i++ {
		v := &Version{Key: strconv.Itoa(i), Version: 1, StartsAt: t0, EndsAt: t0.Add(time.Hour)}
		s.now = func() time.Time { return t0.Add(time.Duration(i) * time.Millisecond) }
		s.cachePut(v.Key, v, nil)
	}
	if len(s.cache) > cacheMaxEntries {
		t.Errorf("cache holds %d entries, want at most %d", len(s.cache), cacheMaxEntries)
	}
	// The oldest entries are the ones dropped.
	if _, _, ok := s.cacheGet("0", t0); ok {
		t.Error("the oldest entry should have been evicted first")
	}
}

// countingStore counts the expensive lookups a cache miss triggers.
type countingStore struct {
	*memStore
	listClips atomic.Int64
}

func (c *countingStore) ListClips(ctx context.Context, s Scope) ([]ClipRef, error) {
	c.listClips.Add(1)
	return c.memStore.ListClips(ctx, s)
}

// A cold cache (startup, or the instant a version rolls over) makes every
// in-flight request for a channel miss at once. Each miss would otherwise
// re-run scope resolution over the whole library and race an insert, so the
// misses must share one resolution.
func TestPlaylist_ConcurrentColdMissesResolveOnce(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 200) // one version already covers t0
	cs := &countingStore{memStore: m}
	s := testService(cs, t0)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Playlist(context.Background(), Scope{Zip: "77494"}, t0); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := cs.listClips.Load(); got != 1 {
		t.Errorf("ListClips called %d times for 16 concurrent cold misses, want 1", got)
	}
}
