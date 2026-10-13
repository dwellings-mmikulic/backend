package linear

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

// pollAnchored is pollMonotonic for the channel route: channel sc with ip=
// for hh, from start for dur, asserting the live-playlist invariants and
// returning whether the switch to isNew was seen with a discontinuity at
// its first segment.
func pollAnchored(t *testing.T, s *Service, hh viewer.ID, sc Scope, start time.Time, dur time.Duration, isNew func(id int64) bool) (sawSwitch bool) {
	t.Helper()
	seen := map[int64]string{}
	labels := map[string][2]int64{} // url → (seq, discontinuity domain): a hole or overlap at the switch relabels a url
	lastSeq, lastDisc := int64(-1), int64(-1)
	for now := start; now.Before(start.Add(dur)); now = now.Add(3 * time.Second) {
		now := now
		s.now = func() time.Time { return now }
		body, err := s.AnchoredPlaylist(context.Background(), hh, sc, now)
		if err != nil {
			t.Fatalf("at %s: %v", now, err)
		}
		p := parsePlaylist(t, body)
		if p.mediaSeq < lastSeq || p.discSeq < lastDisc {
			t.Fatalf("at %s: counters went backwards (%d<%d or %d<%d)\n%s", now, p.mediaSeq, lastSeq, p.discSeq, lastDisc, body)
		}
		lastSeq, lastDisc = p.mediaSeq, p.discSeq
		if len(p.urls) < liveWindow.MinSegments {
			t.Fatalf("at %s: only %d segments\n%s", now, len(p.urls), body)
		}
		firstNew := -1
		domain := p.discSeq
		for i, u := range p.urls {
			seq := p.mediaSeq + int64(i)
			if prev, ok := seen[seq]; ok && prev != u {
				t.Fatalf("at %s: seq %d was %s, now %s", now, seq, prev, u)
			}
			seen[seq] = u
			if p.discAt[i] {
				domain++
			}
			if prev, ok := labels[u]; ok && prev != [2]int64{seq, domain} {
				t.Fatalf("at %s: %s was seq %d in domain %d, now seq %d in domain %d\n%s", now, u, prev[0], prev[1], seq, domain, body)
			}
			labels[u] = [2]int64{seq, domain}
			if isNew(clipID(t, u)) {
				if firstNew < 0 {
					firstNew = i
				}
			} else if firstNew >= 0 {
				t.Fatalf("at %s: old segment after the switch:\n%s", now, body)
			}
		}
		if firstNew > 0 {
			if !p.discAt[firstNew] {
				t.Fatalf("at %s: no DISCONTINUITY at the switch:\n%s", now, body)
			}
			sawSwitch = true
		}
	}
	return sawSwitch
}

func TestAnchoredPlaylist_UnknownHouseholdIsAnError(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	_, err := s.AnchoredPlaylist(context.Background(), viewer.ID{9}, Scope{}, t0)
	if !errors.Is(err, ErrUnknownHousehold) {
		t.Errorf("err = %v", err)
	}
	if _, err := s.AnchoredPlaylist(context.Background(), viewer.ID{9}, Scope{Zip: "00000"}, t0); !errors.Is(err, ErrUnknownArea) {
		t.Errorf("unknown area: err = %v", err)
	}
}

func TestAnchoredPlaylist_SwitchesWithMonotonicCounters(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)     // the household's default (its city)
	addClips(m, austin, 101, 40) // the channel the platform carries
	s := testService(m, t0)
	hh := viewer.ID{1}
	// The phone opened the page at t0: household defaulted to katy.
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	// The platform has been playing the austin channel for a while.
	channel := Scope{Zip: "78701"}
	s.now = func() time.Time { return t0.Add(-2 * time.Minute) }
	if _, err := s.Playlist(context.Background(), channel, t0.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The first ip= request comes at t0+61s, mid-segment.
	first := t0.Add(61 * time.Second)
	s.now = func() time.Time { return first }
	chanBody, err := s.Playlist(context.Background(), channel, first)
	if err != nil {
		t.Fatal(err)
	}
	chanP := parsePlaylist(t, chanBody)
	body, err := s.AnchoredPlaylist(context.Background(), hh, channel, first)
	if err != nil {
		t.Fatal(err)
	}
	// Right after anchoring nothing of the personal feed has aired past S,
	// so the anchored playlist equals the channel's.
	if string(body) != string(chanBody) {
		t.Errorf("anchored playlist differs from the channel before the switch:\n%s\n---\n%s", body, chanBody)
	}
	a, _ := m.Anchor(context.Background(), hh, channel.Key())
	if a == nil || !a.StartsAt.Equal(t0.Add(63*time.Second)) {
		t.Fatalf("anchor = %+v, want starts_at %s", a, t0.Add(63*time.Second))
	}
	if !pollAnchored(t, s, hh, channel, first, 3*time.Minute, func(id int64) bool { return id > 0 && id <= 100 }) {
		t.Error("never saw the switch to katy with a discontinuity")
	}
	// Three minutes on: only katy, and the media sequence continued the
	// channel's numbering.
	end := first.Add(3 * time.Minute)
	s.now = func() time.Time { return end }
	body, err = s.AnchoredPlaylist(context.Background(), hh, channel, end)
	if err != nil {
		t.Fatal(err)
	}
	p := parsePlaylist(t, body)
	for _, u := range p.urls {
		if id := clipID(t, u); id > 100 {
			t.Fatalf("austin still listed three minutes after the switch:\n%s", body)
		}
	}
	if p.mediaSeq <= chanP.mediaSeq {
		t.Errorf("media sequence %d did not continue the channel's %d", p.mediaSeq, chanP.mediaSeq)
	}
	// A later choice keeps the served counters monotonic.
	if _, err := s.Choose(context.Background(), hh, Scope{Zip: "78701"}, end); err != nil {
		t.Fatal(err)
	}
	if !pollAnchored(t, s, hh, channel, end, 2*time.Minute, func(id int64) bool { return id > 100 }) {
		t.Error("never saw the switch back to austin")
	}
}

func TestAnchoredPlaylist_PendingSpanAtSwitch(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	s := testService(m, t0)
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	// The platform carries katy (no austin clips), so an austin segment on the
	// anchored feed can only come from the household's choice.
	channel := Scope{Zip: "77494"}
	s.now = func() time.Time { return t0.Add(-time.Minute) }
	if _, err := s.Playlist(context.Background(), channel, t0.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The viewer picks austin at t0+61s (pending until t0+63s) and the
	// platform's first ip= request lands at t0+62s, inside that pending gap.
	s.now = func() time.Time { return t0.Add(61 * time.Second) }
	if _, err := s.Choose(context.Background(), hh, Scope{Zip: "78701"}, t0.Add(61*time.Second)); err != nil {
		t.Fatal(err)
	}
	first := t0.Add(62 * time.Second)
	if !pollAnchored(t, s, hh, channel, first, 3*time.Minute, func(id int64) bool { return id > 100 }) {
		t.Error("never saw austin on the anchored feed")
	}
}

func TestAnchoredPlaylist_ReusesTheStoredAnchor(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	ch := Scope{Zip: "77494"}
	s.now = func() time.Time { return t0.Add(time.Minute) }
	if _, err := s.AnchoredPlaylist(context.Background(), hh, ch, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	a1, _ := m.Anchor(context.Background(), hh, ch.Key())
	s.now = func() time.Time { return t0.Add(5 * time.Minute) }
	if _, err := s.AnchoredPlaylist(context.Background(), hh, ch, t0.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	a2, _ := m.Anchor(context.Background(), hh, ch.Key())
	if a1 == nil || a2 == nil || *a1 != *a2 {
		t.Errorf("anchor changed: %+v vs %+v", a1, a2)
	}
}
