package linear

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
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
