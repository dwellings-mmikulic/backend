package linear

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

func testService(store Store, now time.Time) *Service {
	s := New(store, Options{LineupHours: 6, MinScopeClips: 3, EPGHorizonHours: 24}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return now }
	return s
}

var katy = Scope{Zip: "77494", City: "katy", State: "tx"}
var austin = Scope{Zip: "78701", City: "austin", State: "tx"}

// addClips adds n one-minute clips (20 segments of 3 s) starting at id.
func addClips(m *memStore, sc Scope, firstID int64, n int) {
	for i := int64(0); i < int64(n); i++ {
		segs := make([]int, 20)
		for j := range segs {
			segs[j] = 3000
		}
		m.addClip(firstID+i, sc, 300000+i*1000, segs...)
	}
}

func TestBuildItems_DeterministicAndCapped(t *testing.T) {
	var clips []ClipRef
	for id := int64(1); id <= 10; id++ {
		clips = append(clips, ClipRef{ID: id, TotalMS: 60000, Segments: 20})
	}
	ids1, ms, segs := buildItems(clips, seedFor("us", 1), 5*60000)
	ids2, _, _ := buildItems(clips, seedFor("us", 1), 5*60000)
	if !reflect.DeepEqual(ids1, ids2) {
		t.Errorf("same seed gave different orders: %v vs %v", ids1, ids2)
	}
	if len(ids1) != 5 {
		t.Errorf("cap: got %d items, want 5", len(ids1))
	}
	for i := range ids1 {
		if ms[i] != 60000 || segs[i] != 20 {
			t.Errorf("item %d: ms=%d segs=%d", i, ms[i], segs[i])
		}
	}
	// A clip longer than the cap still airs (a lineup is never empty).
	ids, _, _ := buildItems([]ClipRef{{ID: 9, TotalMS: 99999999, Segments: 1}}, 1, 1000)
	if len(ids) != 1 {
		t.Errorf("oversized single clip: got %d items, want 1", len(ids))
	}
	// Input order does not matter; ids are canonicalised before shuffling.
	rev := make([]ClipRef, len(clips))
	for i, c := range clips {
		rev[len(clips)-1-i] = c
	}
	ids3, _, _ := buildItems(rev, seedFor("us", 1), 5*60000)
	if !reflect.DeepEqual(ids1, ids3) {
		t.Errorf("reversed input gave different order: %v vs %v", ids1, ids3)
	}
}

func TestResolveScope_FallsBackUpTheHierarchy(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 2)     // ZIP 77494 / Katy: 2 clips (< MinScopeClips 3)
	addClips(m, austin, 100, 5) // Austin: 5 clips
	s := testService(m, t0)

	// ZIP → city has only 2 → state has 7 → resolves to state:tx.
	eff, clips, err := s.resolveScope(context.Background(), Scope{Zip: "77494"})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Key() != "state:tx" || len(clips) != 7 {
		t.Errorf("zip 77494 → %s with %d clips, want state:tx with 7", eff.Key(), len(clips))
	}
	// Austin has enough on its own.
	eff, clips, _ = s.resolveScope(context.Background(), Scope{City: "austin", State: "tx"})
	if eff.Key() != "city:austin|tx" || len(clips) != 5 {
		t.Errorf("austin → %s with %d clips", eff.Key(), len(clips))
	}
	// Unknown ZIP → national.
	eff, _, _ = s.resolveScope(context.Background(), Scope{Zip: "00000"})
	if eff.Key() != "us" {
		t.Errorf("unknown zip → %s, want us", eff.Key())
	}
	// Another state with nothing → national.
	eff, _, _ = s.resolveScope(context.Background(), Scope{State: "ca"})
	if eff.Key() != "us" {
		t.Errorf("empty state → %s, want us", eff.Key())
	}
	// Empty library → ErrNoContent.
	if _, _, err := testService(newMemStore(), t0).resolveScope(context.Background(), Scope{}); !errors.Is(err, ErrNoContent) {
		t.Errorf("empty library: err = %v, want ErrNoContent", err)
	}
}

func TestVersionAt_FirstVersionStartsAnHourBack(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 200) // 200 min of content > 1 h, so v1 covers now
	s := testService(m, t0.Add(37*time.Second))

	cur, prev, err := s.versionAt(context.Background(), "zip:77494", t0.Add(37*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if prev != nil {
		t.Errorf("prev = %+v, want nil for version 1", prev)
	}
	if cur.Version != 1 || cur.StartSeq != 0 || cur.StartItem != 0 {
		t.Errorf("v1 counters: %+v", cur)
	}
	if want := t0.Add(-time.Hour); !cur.StartsAt.Equal(want) {
		t.Errorf("StartsAt = %s, want %s (now − 1 h, minute-truncated)", cur.StartsAt, want)
	}
	if cur.Scope != "zip:77494" {
		t.Errorf("scope = %s", cur.Scope)
	}
	if cur.TotalMS() > 6*3600*1000 {
		t.Errorf("version exceeds LineupHours: %d ms", cur.TotalMS())
	}
}

func TestVersionAt_ChainsVersionsUntilCovered(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 3) // 3 min per version; an hour of history needs ~20 versions
	s := testService(m, t0)

	cur, prev, err := s.versionAt(context.Background(), "zip:77494", t0)
	if err != nil {
		t.Fatal(err)
	}
	if !(cur.StartsAt.Before(t0) || cur.StartsAt.Equal(t0)) || !t0.Before(cur.EndsAt) {
		t.Fatalf("cur [%s, %s) does not cover %s", cur.StartsAt, cur.EndsAt, t0)
	}
	if prev == nil {
		t.Fatal("expected a predecessor")
	}
	if !prev.EndsAt.Equal(cur.StartsAt) {
		t.Errorf("chain gap: prev ends %s, cur starts %s", prev.EndsAt, cur.StartsAt)
	}
	if cur.StartSeq != prev.StartSeq+prev.Segments() {
		t.Errorf("StartSeq %d, want prev %d + %d", cur.StartSeq, prev.StartSeq, prev.Segments())
	}
	if cur.StartItem != prev.StartItem+int64(len(prev.ItemIDs)) {
		t.Errorf("StartItem %d, want prev %d + %d", cur.StartItem, prev.StartItem, len(prev.ItemIDs))
	}
	if cur.Version != prev.Version+1 {
		t.Errorf("versions %d after %d", cur.Version, prev.Version)
	}
	// Calling again is a no-op: same version served, nothing new stored.
	all, _ := m.ListVersions(context.Background(), "zip:77494")
	again, _, _ := s.versionAt(context.Background(), "zip:77494", t0)
	all2, _ := m.ListVersions(context.Background(), "zip:77494")
	if again.Version != cur.Version || len(all) != len(all2) {
		t.Errorf("second call changed state: v%d→v%d, %d→%d versions", cur.Version, again.Version, len(all), len(all2))
	}
}

func TestVersionAt_IdleChannelRestartsNearNow(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 3)
	s := testService(m, t0)
	first, _, err := s.versionAt(context.Background(), "zip:77494", t0)
	if err != nil {
		t.Fatal(err)
	}

	later := t0.Add(72 * time.Hour)
	s.now = func() time.Time { return later }
	cur, prev, err := s.versionAt(context.Background(), "zip:77494", later)
	if err != nil {
		t.Fatal(err)
	}
	if !later.Before(cur.EndsAt) || cur.StartsAt.After(later) {
		t.Fatalf("cur [%s, %s) does not cover %s", cur.StartsAt, cur.EndsAt, later)
	}
	// The restart happened at later − 1 h, not by chaining 72 h of dead air.
	all, _ := m.ListVersions(context.Background(), "zip:77494")
	if len(all) > 45 {
		t.Errorf("chained %d versions through idle time; expected a restart", len(all))
	}
	restart := all[first.Version] // the first version created after the idle gap
	if want := later.Add(-time.Hour); !restart.StartsAt.Equal(want) {
		t.Errorf("restart StartsAt = %s, want %s", restart.StartsAt, want)
	}
	if restart.StartSeq != first.StartSeq+first.Segments() {
		t.Errorf("counters must continue across the gap: %d vs %d", restart.StartSeq, first.StartSeq+first.Segments())
	}
	_ = prev
}

// racingStore makes the first InsertVersion lose to a rival row, as when a
// second instance created the same version concurrently.
type racingStore struct {
	*memStore
	raced bool
}

func (r *racingStore) InsertVersion(ctx context.Context, v *Version) (bool, error) {
	if !r.raced {
		r.raced = true
		rival := *v
		rival.ItemIDs = append([]int64(nil), v.ItemIDs...)
		// Rival chose a different (but valid) start: one minute earlier.
		rival.StartsAt = v.StartsAt.Add(-time.Minute)
		rival.EndsAt = v.EndsAt.Add(-time.Minute)
		_, _ = r.memStore.InsertVersion(ctx, &rival)
		return false, nil
	}
	return r.memStore.InsertVersion(ctx, v)
}

func TestVersionAt_ServesTheRowThatWonTheRace(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 200)
	rs := &racingStore{memStore: m}
	s := testService(rs, t0)
	cur, _, err := s.versionAt(context.Background(), "zip:77494", t0)
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(-time.Hour - time.Minute); !cur.StartsAt.Equal(want) {
		t.Errorf("served StartsAt = %s, want rival's %s", cur.StartsAt, want)
	}
}

func TestVersionAt_NoContent(t *testing.T) {
	s := testService(newMemStore(), t0)
	if _, _, err := s.versionAt(context.Background(), "us", t0); !errors.Is(err, ErrNoContent) {
		t.Errorf("err = %v, want ErrNoContent", err)
	}
}
