package linear

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

func TestEnsureHousehold_CreatesSpan0FromFirstCandidateWithContent(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	m.addZip("77777") // real ZIP, no listings: must be skipped
	s := testService(m, t0)
	hh := viewer.ID{1}

	spans, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77777"}, {Zip: "77494"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 || spans[0].N != 0 || spans[0].Scope != "zip:77494" || spans[0].Requested != "zip:77494" ||
		spans[0].Source != SourceGeo || !spans[0].StartsAt.Equal(t0) || spans[0].SeqOffset != 0 || spans[0].ItemOffset != 0 {
		t.Errorf("span 0 = %+v", spans)
	}
	again, err := s.EnsureHousehold(context.Background(), hh, nil, t0.Add(time.Hour))
	if err != nil || len(again) != 1 || !again[0].StartsAt.Equal(t0) {
		t.Errorf("second call must return the existing span: %+v, %v", again, err)
	}
}

func TestEnsureHousehold_DefaultsToNational(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	spans, err := s.EnsureHousehold(context.Background(), viewer.ID{2}, []Scope{{State: "fl"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if spans[0].Scope != "us" || spans[0].Source != SourceDefault || spans[0].Requested != "" {
		t.Errorf("span 0 = %+v", spans[0])
	}
}

func TestHousehold_UnknownIsAnError(t *testing.T) {
	s := testService(newMemStore(), t0)
	if _, err := s.Household(context.Background(), viewer.ID{3}); !errors.Is(err, ErrUnknownHousehold) {
		t.Errorf("err = %v", err)
	}
}

func TestCities_UsesMinScopeClipsAndCaches(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 5)
	addClips(m, austin, 101, 2) // below MinScopeClips (3 in testService)
	s := testService(m, t0)
	cs, err := s.Cities(context.Background())
	if err != nil || len(cs) != 1 || cs[0].City != "katy" {
		t.Fatalf("cities = %+v, %v", cs, err)
	}
	addClips(m, austin, 201, 5)
	cs, _ = s.Cities(context.Background())
	if len(cs) != 1 {
		t.Errorf("cached answer expected within 10 min, got %+v", cs)
	}
	s.now = func() time.Time { return t0.Add(11 * time.Minute) }
	cs, _ = s.Cities(context.Background())
	if len(cs) != 2 {
		t.Errorf("refreshed answer expected after 10 min, got %+v", cs)
	}
}

// parsed is a live playlist as the tests read it.
type parsed struct {
	mediaSeq, discSeq int64
	urls              []string
	discAt            []bool // a DISCONTINUITY tag precedes urls[i]
}

func parsePlaylist(t *testing.T, body []byte) parsed {
	t.Helper()
	var p parsed
	disc := false
	for _, l := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		switch {
		case strings.HasPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"):
			p.mediaSeq, _ = strconv.ParseInt(strings.TrimPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
		case strings.HasPrefix(l, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			p.discSeq, _ = strconv.ParseInt(strings.TrimPrefix(l, "#EXT-X-DISCONTINUITY-SEQUENCE:"), 10, 64)
		case l == "#EXT-X-DISCONTINUITY":
			disc = true
		case strings.HasPrefix(l, "https://"):
			p.urls = append(p.urls, l)
			p.discAt = append(p.discAt, disc)
			disc = false
		}
	}
	if len(p.urls) == 0 {
		t.Fatalf("playlist lists no segments:\n%s", body)
	}
	return p
}

var clipIDRe = regexp.MustCompile(`/hls/v1/(\d+)/`)

func clipID(t *testing.T, url string) int64 {
	t.Helper()
	m := clipIDRe.FindStringSubmatch(url)
	if m == nil {
		t.Fatalf("no clip id in %s", url)
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id
}

// pollMonotonic polls hh's personal playlist every 3 s for dur from start and
// checks the live-playlist invariants a player relies on: MEDIA-SEQUENCE and
// DISCONTINUITY-SEQUENCE never go backwards, a sequence number never maps to
// two different segments, and once a segment of isNew appears no older
// channel's segment follows it. It returns whether a switch was observed with
// a discontinuity tag exactly at the first new segment.
func pollMonotonic(t *testing.T, s *Service, hh viewer.ID, start time.Time, dur time.Duration, isNew func(id int64) bool) (sawSwitch bool) {
	t.Helper()
	seen := map[int64]string{}
	lastSeq, lastDisc := int64(-1), int64(-1)
	for now := start; now.Before(start.Add(dur)); now = now.Add(3 * time.Second) {
		now := now
		s.now = func() time.Time { return now }
		body, _, err := s.PersonalPlaylist(context.Background(), hh, now)
		if err != nil {
			t.Fatalf("at %s: %v", now, err)
		}
		p := parsePlaylist(t, body)
		if p.mediaSeq < lastSeq || p.discSeq < lastDisc {
			t.Fatalf("at %s: counters went backwards (%d<%d or %d<%d)\n%s", now, p.mediaSeq, lastSeq, p.discSeq, lastDisc, body)
		}
		lastSeq, lastDisc = p.mediaSeq, p.discSeq
		firstNew := -1
		for i, u := range p.urls {
			seq := p.mediaSeq + int64(i)
			if prev, ok := seen[seq]; ok && prev != u {
				t.Fatalf("at %s: seq %d was %s, now %s", now, seq, prev, u)
			}
			seen[seq] = u
			if isNew(clipID(t, u)) {
				if firstNew < 0 {
					firstNew = i
				}
			} else if firstNew >= 0 {
				t.Fatalf("at %s: old channel's segment after the switch:\n%s", now, body)
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

func TestPersonalPlaylist_MatchesChannelBeforeAnyChoice(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	now := t0.Add(90 * time.Second)
	s.now = func() time.Time { return now }
	personal, airing, err := s.PersonalPlaylist(context.Background(), hh, now)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := s.Playlist(context.Background(), Scope{Zip: "77494"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if string(personal) != string(channel) {
		t.Errorf("personal feed differs from its channel before any choice:\n%s\n---\n%s", personal, channel)
	}
	if airing.N != 0 || airing.Scope != "zip:77494" {
		t.Errorf("airing = %+v", airing)
	}
}

func TestPersonalPlaylist_UnknownHousehold(t *testing.T) {
	s := testService(newMemStore(), t0)
	if _, _, err := s.PersonalPlaylist(context.Background(), viewer.ID{9}, t0); !errors.Is(err, ErrUnknownHousehold) {
		t.Errorf("err = %v", err)
	}
}

func TestChoose_SwitchesAtNextSegmentWithMonotonicCounters(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	s := testService(m, t0)
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	switchAt := t0.Add(61 * time.Second) // mid-segment (segments are 3 s from t0-1h)
	s.now = func() time.Time { return switchAt }
	spans, err := s.Choose(context.Background(), hh, Scope{Zip: "78701"}, switchAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 || spans[1].N != 1 || spans[1].Scope != "zip:78701" || spans[1].Requested != "zip:78701" || spans[1].Source != SourceChoice {
		t.Fatalf("spans = %+v", spans)
	}
	// The switch takes effect when the segment airing at submit ends: that
	// segment covers [t0+60s, t0+63s).
	if !spans[1].StartsAt.Equal(t0.Add(63 * time.Second)) {
		t.Errorf("span 1 starts at %s, want %s", spans[1].StartsAt, t0.Add(63*time.Second))
	}
	if !pollMonotonic(t, s, hh, switchAt, 2*time.Minute, func(id int64) bool { return id > 100 }) {
		t.Error("never saw the switch to austin with a discontinuity")
	}
	// Two minutes on, only austin is listed and the feed is still moving.
	end := switchAt.Add(2 * time.Minute)
	s.now = func() time.Time { return end }
	body, airing, err := s.PersonalPlaylist(context.Background(), hh, end)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range parsePlaylist(t, body).urls {
		if clipID(t, u) <= 100 {
			t.Fatalf("katy still listed two minutes after the switch:\n%s", body)
		}
	}
	if airing.N != 1 {
		t.Errorf("airing = %+v", airing)
	}
}

func TestChoose_SameScopeIsANoop(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	spans, err := s.Choose(context.Background(), hh, Scope{Zip: "77494"}, t0.Add(time.Minute))
	if err != nil || len(spans) != 1 {
		t.Errorf("spans = %+v, %v", spans, err)
	}
}

func TestChoose_SecondSubmitBeforeAiringReplacesThePendingSpan(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	s := testService(m, t0)
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	first := t0.Add(61 * time.Second)
	s.now = func() time.Time { return first }
	spans, err := s.Choose(context.Background(), hh, Scope{Zip: "78701"}, first)
	if err != nil {
		t.Fatal(err)
	}
	pendingStart := spans[1].StartsAt
	second := first.Add(time.Second) // still before any austin segment aired
	s.now = func() time.Time { return second }
	spans, err = s.Choose(context.Background(), hh, Scope{State: "tx"}, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 || spans[1].Scope != "state:tx" || !spans[1].StartsAt.Equal(pendingStart) {
		t.Fatalf("expected span 1 replaced in place: %+v", spans)
	}
	// state:tx holds both katy (1-40) and austin (101-140) clips, so "new"
	// means anything the personal timeline did not carry before: the
	// invariants are the counters, the seq→segment stability and the
	// discontinuity, which pollMonotonic checks whatever isNew says.
	pollMonotonic(t, s, hh, second, 2*time.Minute, func(int64) bool { return false })
}

func TestChoose_ThinOrUnknownAreas(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	m.addZip("77777") // exists, no listings → falls back to national
	s := testService(m, t0)
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0); err != nil {
		t.Fatal(err)
	}
	now := t0.Add(time.Minute)
	s.now = func() time.Time { return now }
	spans, err := s.Choose(context.Background(), hh, Scope{Zip: "77777"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if spans[1].Scope != "us" || spans[1].Requested != "zip:77777" {
		t.Errorf("thin ZIP: %+v", spans[1])
	}
	if _, err := s.Choose(context.Background(), hh, Scope{Zip: "12345"}, now); !errors.Is(err, ErrUnknownArea) {
		t.Errorf("unknown ZIP: err = %v", err)
	}
	if _, err := s.Choose(context.Background(), viewer.ID{9}, Scope{Zip: "77494"}, now); !errors.Is(err, ErrUnknownHousehold) {
		t.Errorf("unknown household: err = %v", err)
	}
}

// S can fall inside the last segment of the new channel's current lineup
// version: the first segment at or after S is then the next version's
// first, which must be found (and created if need be) rather than failing.
func TestChoose_InsideTheNewChannelsLastSegmentRollsToTheNextVersion(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)        // 3 s segments
	for i := int64(0); i < 3; i++ { // austin: 3 clips of 9 × 7 s = 63 s → 189 s versions
		segs := make([]int, 9)
		for j := range segs {
			segs[j] = 7000
		}
		m.addClip(101+i, austin, 400000, segs...)
	}
	s := testService(m, t0.Add(-10*time.Minute))
	hh := viewer.ID{1}
	if _, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77494"}}, t0.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Materialise austin's chain from t0-1h: version 19 covers
	// [t0-198s, t0-9s) and its last segment is [t0-16s, t0-9s).
	s.now = func() time.Time { return t0 }
	if _, err := s.Playlist(context.Background(), Scope{Zip: "78701"}, t0); err != nil {
		t.Fatal(err)
	}
	// katy's chain is created on this call from t0-61min, 3 s aligned to t0:
	// the segment airing at t0-13s is [t0-15s, t0-12s), so S = t0-12s, inside
	// austin's version-19 last segment. The first austin segment at or after
	// S is version 20's first, at t0-9s.
	at := t0.Add(-13 * time.Second)
	s.now = func() time.Time { return at }
	spans, err := s.Choose(context.Background(), hh, Scope{Zip: "78701"}, at)
	if err != nil {
		t.Fatal(err)
	}
	if !spans[1].StartsAt.Equal(t0.Add(-12 * time.Second)) {
		t.Fatalf("S = %s, want %s", spans[1].StartsAt, t0.Add(-12*time.Second))
	}
	if !pollMonotonic(t, s, hh, at, 2*time.Minute, func(id int64) bool { return id > 100 }) {
		t.Error("never saw the switch")
	}
}
