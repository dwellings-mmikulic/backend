package linear

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

// A customized household's TV polls its playlist every two seconds. Its
// spans and anchors only change when the mobile page writes, so the service
// serves them from memory for householdTTL and forgets them on write.

func TestHousehold_ServesSpansFromCacheUntilTTL(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, nil, t0); err != nil {
		t.Fatal(err)
	}
	m.spansCalls = 0
	for i := 0; i < 5; i++ {
		if _, err := s.Household(context.Background(), hh); err != nil {
			t.Fatal(err)
		}
	}
	if m.spansCalls != 0 {
		t.Fatalf("spans read from the store %d times within the TTL", m.spansCalls)
	}
	s.now = func() time.Time { return t0.Add(householdTTL) }
	if _, err := s.Household(context.Background(), hh); err != nil {
		t.Fatal(err)
	}
	if m.spansCalls != 1 {
		t.Fatalf("spans read %d times after the TTL, want 1", m.spansCalls)
	}
}

func TestHousehold_UnknownIsCachedUntilCreated(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	hh := viewer.ID{2}
	for i := 0; i < 3; i++ {
		if _, err := s.Household(context.Background(), hh); !errors.Is(err, ErrUnknownHousehold) {
			t.Fatalf("got %v, want ErrUnknownHousehold", err)
		}
	}
	if m.spansCalls != 1 {
		t.Fatalf("unknown household read from the store %d times, want 1", m.spansCalls)
	}
	// The first scan creates the feed: the next poll must see it at once.
	if _, err := s.EnsureHousehold(context.Background(), hh, nil, t0); err != nil {
		t.Fatal(err)
	}
	spans, err := s.Household(context.Background(), hh)
	if err != nil || len(spans) != 1 {
		t.Fatalf("after EnsureHousehold: %v, %d spans", err, len(spans))
	}
}

func TestChoose_InvalidatesTheHouseholdCache(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 100, 40)
	s := testService(m, t0)
	hh := viewer.ID{3}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Household(context.Background(), hh); err != nil { // warm the cache
		t.Fatal(err)
	}
	now := t0.Add(time.Minute)
	s.now = func() time.Time { return now }
	if _, err := s.Choose(context.Background(), hh, Scope{Zip: "78701"}, now); err != nil {
		t.Fatal(err)
	}
	spans, err := s.Household(context.Background(), hh)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 || spans[1].Scope != austin.Key() {
		t.Fatalf("after Choose the cache still serves the old feed: %+v", spans)
	}
}

func TestAnchoredPlaylist_PollsWithoutStoreReads(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	hh := viewer.ID{4}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	ch := Scope{Zip: "77494"}
	start := t0.Add(time.Minute)
	s.now = func() time.Time { return start }
	if _, err := s.AnchoredPlaylist(context.Background(), hh, ch, start); err != nil {
		t.Fatal(err)
	}
	m.spansCalls, m.anchorCalls = 0, 0
	for now := start.Add(2 * time.Second); now.Before(start.Add(20 * time.Second)); now = now.Add(2 * time.Second) {
		now := now
		s.now = func() time.Time { return now }
		if _, err := s.AnchoredPlaylist(context.Background(), hh, ch, now); err != nil {
			t.Fatalf("at %s: %v", now, err)
		}
	}
	if m.spansCalls != 0 || m.anchorCalls != 0 {
		t.Fatalf("polls within the TTL read the store: spans %d, anchors %d", m.spansCalls, m.anchorCalls)
	}
}

func TestTTLCache_EvictsExpiredThenABatch(t *testing.T) {
	now := t0
	c := newTTLCache[int, string](20, time.Minute, func() time.Time { return now })
	c.put(1, "a")
	now = now.Add(10 * time.Second)
	for k := 2; k <= 20; k++ {
		c.put(k, "x")
	}
	now = now.Add(52 * time.Second) // 1 is 62 s old, expired; the rest are 52 s old
	if _, ok := c.get(1); ok {
		t.Fatal("expired entry served")
	}
	c.put(21, "y") // room came from the expired entry alone
	for k := 2; k <= 21; k++ {
		if _, ok := c.get(k); !ok {
			t.Fatalf("entry %d evicted while the cache had an expired entry to drop", k)
		}
	}
	c.put(22, "z") // full and nothing expired: a tenth goes, the new entry is in
	if n := len(c.entries); n > 20 {
		t.Fatalf("cache over capacity: %d entries", n)
	}
	if v, ok := c.get(22); !ok || v != "z" {
		t.Fatalf("newest entry missing: %q %v", v, ok)
	}
	c.forget(22)
	if _, ok := c.get(22); ok {
		t.Fatal("forgotten entry served")
	}
}
