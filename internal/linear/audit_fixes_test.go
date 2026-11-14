package linear

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/geo"
	"github.com/dwellingtw/backend/internal/viewer"
)

// recordingGeo answers every address with loc and remembers what it was
// asked. While down it reports that it could not ask.
type recordingGeo struct {
	mu    sync.Mutex
	loc   geo.Location
	down  bool
	asked []string
}

func (g *recordingGeo) Locate(ip net.IP) (geo.Location, bool) {
	loc, ok, _ := g.LocateStatus(ip)
	return loc, ok
}

func (g *recordingGeo) LocateStatus(ip net.IP) (geo.Location, bool, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, ip.String())
	if g.down {
		return geo.Location{}, false, false
	}
	return g.loc, g.loc != geo.Location{}, true
}

func trustedOpts(aud *fakeAudience, g geo.Lookup) ViewerOptions {
	o := baseOpts(aud, g)
	nets, _ := viewer.ParseCIDRs(platformAddr)
	o.Hasher.TrustIPParamFrom(nets)
	return o
}

// A stranger naming a home in ip= must neither learn what that home watched
// last nor make the service look that address up.
func TestResolve_IPParamFromAStrangerIsIgnored(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 5)
	addClips(m, Scope{Zip: "33101", City: "miami", State: "fl"}, 101, 5)
	home, stranger := "203.0.113.44", "198.51.100.9"
	hasher := viewer.NewHasher("s", false)
	aud := &fakeAudience{last: map[viewer.ID]string{hasher.IDAt("", home, t0): "state:fl"}}
	g := &recordingGeo{loc: geo.Location{Zip: "77494"}}
	o := trustedOpts(aud, g)

	got := decodeResolve(t, serveViewers(t, m, o, "/channels/resolve?ip="+home, stranger))
	if got["source"] == "last_watched" || got["scope"] == "state:fl" {
		t.Fatalf("a stranger read the home's last-watched channel: %v", got)
	}
	if len(g.asked) != 1 || g.asked[0] != stranger {
		t.Fatalf("lookups = %v, want only the connecting address", g.asked)
	}
	for _, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"} {
		serveViewers(t, m, o, "/channels/resolve?ip="+ip, stranger)
	}
	for _, a := range g.asked {
		if a != stranger {
			t.Fatalf("an invented ip= was looked up: %v", g.asked)
		}
	}
	got = decodeResolve(t, serveViewers(t, m, o, "/channels/resolve?ip="+home, platformAddr))
	if got["source"] != "last_watched" || got["scope"] != "state:fl" {
		t.Errorf("a platform's ip= was not believed: %v", got)
	}
}

func TestBeat_RecordsTheConnectingAddressAndOnlyRealPlaces(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 5)
	tr := &fakeTracker{}
	o := trustedOpts(&fakeAudience{}, nil)
	o.Tracker = tr
	stranger := "198.51.100.9"

	if rec := serveViewers(t, m, o, "/channels/beat?ip=203.0.113.44", stranger); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d", rec.Code)
	}
	b := tr.beats[0]
	if b.Client.IP != stranger || b.Client.From != stranger || b.Viewer != viewer.NewHasher("s", false).IDAt("", stranger, t0) {
		t.Errorf("a stranger's beat was recorded under the address it claimed: %+v", b)
	}
	// From a platform the claimed address counts, and the platform's own
	// address is not what rows are limited by.
	serveViewers(t, m, o, "/channels/beat?ip=203.0.113.44", platformAddr)
	if b := tr.beats[1]; b.Client.IP != "203.0.113.44" || b.Client.From != "" {
		t.Errorf("platform beat = %+v", b)
	}
	if rec := serveViewers(t, m, o, "/channels/beat?zip=00000", stranger); rec.Code != http.StatusNotFound || len(tr.beats) != 2 {
		t.Errorf("beat for an unknown ZIP: status %d, %d beats", rec.Code, len(tr.beats))
	}
	if rec := serveViewers(t, m, o, "/channels/beat?zip=77494", stranger); rec.Code != http.StatusNoContent || len(tr.beats) != 3 {
		t.Errorf("beat for a real ZIP: status %d, %d beats", rec.Code, len(tr.beats))
	}
}

// A home that first connects while the location lookup cannot answer gets
// national for the moment, not for good.
func TestEnsureHousehold_PendingDefaultIsSettledWhenTheLookupAnswers(t *testing.T) {
	ctx := context.Background()
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	now := t0
	s := testService(m, now)
	s.now = func() time.Time { return now }
	h := viewer.NewHasher("salt", false).Household("203.0.113.5")

	spans, err := s.EnsureHousehold(ctx, h, nil, false, now)
	if err != nil || len(spans) != 1 || spans[0].Scope != "us" || spans[0].Source != SourcePending {
		t.Fatalf("first contact during an outage: %+v, %v", spans, err)
	}
	now = t0.Add(time.Minute)
	if spans, err = s.EnsureHousehold(ctx, h, nil, false, now); err != nil || len(spans) != 1 || spans[0].Source != SourcePending {
		t.Fatalf("second contact during the outage: %+v, %v", spans, err)
	}
	// The lookup answers: the home moves to its area from the next segment
	// boundary, as a choice would, marked as a location default.
	now = t0.Add(3 * time.Minute)
	spans, err = s.EnsureHousehold(ctx, h, []Scope{katy}, true, now)
	if err != nil {
		t.Fatal(err)
	}
	last := spans[len(spans)-1]
	if len(spans) != 2 || last.Scope != katy.Key() || last.Source != SourceGeo || last.Requested != katy.Key() || !last.StartsAt.After(t0) {
		t.Fatalf("after the lookup answered: %+v", spans)
	}
	now = t0.Add(10 * time.Minute)
	if again, err := s.EnsureHousehold(ctx, h, []Scope{austin}, true, now); err != nil || len(again) != 2 || again[1].Scope != katy.Key() {
		t.Fatalf("a settled home was moved again: %+v, %v", again, err)
	}
	if _, _, err := s.PersonalPlaylist(ctx, h, now); err != nil {
		t.Fatalf("playlist after settling: %v", err)
	}
}

func TestEnsureHousehold_PendingDefaultStandsWhenNothingBetterIsKnown(t *testing.T) {
	ctx := context.Background()
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	h := viewer.NewHasher("salt", false).Household("203.0.113.6")
	if _, err := s.EnsureHousehold(ctx, h, nil, false, t0); err != nil {
		t.Fatal(err)
	}
	// The lookup answers with nothing usable (abroad), or with a place that
	// has no channel of its own: national is now a fact about the home.
	spans, err := s.EnsureHousehold(ctx, h, []Scope{{State: "wy"}}, true, t0.Add(time.Minute))
	if err != nil || len(spans) != 1 || spans[0].Scope != "us" || spans[0].Source != SourceDefault {
		t.Fatalf("settled default: %+v, %v", spans, err)
	}
	if !spans[0].StartsAt.Equal(t0) {
		t.Errorf("settling moved the span's start: %v", spans[0].StartsAt)
	}
}

func TestEnsureHousehold_LocatedHomeIsNeverPending(t *testing.T) {
	ctx := context.Background()
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	hasher := viewer.NewHasher("salt", false)
	a, err := s.EnsureHousehold(ctx, hasher.Household("203.0.113.7"), []Scope{katy}, true, t0)
	if err != nil || a[0].Source != SourceGeo {
		t.Fatalf("%+v, %v", a, err)
	}
	b, err := s.EnsureHousehold(ctx, hasher.Household("203.0.113.8"), nil, true, t0)
	if err != nil || b[0].Source != SourceDefault {
		t.Fatalf("%+v, %v", b, err)
	}
}

// Through the routes: a home that meets an outage is not stuck, and a client
// never sees the internal "pending" marker.
func TestFeed_HomeThatMetALookupOutageGetsItsAreaLater(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	g := &recordingGeo{loc: geo.Location{Zip: "77494"}, down: true}
	svc := testService(m, t0)
	now := t0
	svc.now = func() time.Time { return now }
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.EnableViewers(ViewerOptions{Hasher: platformHasher(), Audience: feedTestAudience{}, Geo: g, PublicBaseURL: "https://api.example.test"})
	h.EnableFeeds(FeedOptions{MobileBaseURL: pageOrigin, QRShowSeconds: 60, QREverySeconds: 300})
	mux := http.NewServeMux()
	h.Register(mux)

	me := func() (scope, source string) {
		var f struct{ Scope, Source string }
		rec := do(mux, "GET", "/feed/me", "203.0.113.5", "")
		if rec.Code != 200 {
			t.Fatalf("/feed/me: %d %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
			t.Fatal(err)
		}
		return f.Scope, f.Source
	}
	if scope, source := me(); scope != "us" || source != SourceDefault {
		t.Fatalf("during the outage: %s %s", scope, source)
	}
	g.mu.Lock()
	g.down = false
	g.mu.Unlock()
	now = t0.Add(3 * time.Minute)
	if scope, source := me(); scope != katy.Key() || source != SourceGeo {
		t.Fatalf("after the outage: %s %s", scope, source)
	}
}

func TestPurgeVersions_KeepsWhatTheChannelsNeed(t *testing.T) {
	ctx := context.Background()
	m := newMemStore()
	addClips(m, katy, 1, 40)
	m.addZip("77001")
	now := t0
	s := testService(m, now)
	s.now = func() time.Time { return now }

	// One busy channel over four days, and one ZIP somebody named once.
	if _, err := s.Playlist(ctx, Scope{Zip: "77001"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for d := 0; d <= 4*24; d += 3 {
		now = t0.Add(time.Duration(d) * time.Hour)
		if _, err := s.Playlist(ctx, katy, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	count := func(key string) int {
		vs, err := m.ListVersions(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		return len(vs)
	}
	before := count(katy.Key())
	n, err := m.PurgeVersions(ctx, now.Add(-versionRetention))
	if err != nil || n == 0 {
		t.Fatalf("purged %d, %v", n, err)
	}
	after := count(katy.Key())
	if after >= before || after < 2 {
		t.Fatalf("busy channel: %d versions before, %d after", before, after)
	}
	// The ZIP nobody came back to keeps its newest version: the chain's
	// counters continue from it if it is ever requested again.
	if count("zip:77001") != 1 {
		t.Fatalf("idle channel has %d versions, want its newest only", count("zip:77001"))
	}
	live := now.Add(2 * time.Minute)
	a, err := s.Playlist(ctx, katy, live)
	if err != nil {
		t.Fatalf("busy channel after the purge: %v", err)
	}
	if seq := parsePlaylist(t, a).mediaSeq; seq < 1000 {
		t.Errorf("media sequence restarted after the purge: %d", seq)
	}
	if _, err := s.Playlist(ctx, Scope{Zip: "77001"}, live); err != nil {
		t.Fatalf("idle channel after the purge: %v", err)
	}
}

func TestRunMaintenance_PurgesAndStops(t *testing.T) {
	m := newMemStore()
	old := Version{Key: "us", Version: 1, Scope: "us", StartsAt: t0.Add(-100 * time.Hour), EndsAt: t0.Add(-94 * time.Hour)}
	tip := Version{Key: "us", Version: 2, Scope: "us", StartsAt: t0.Add(-94 * time.Hour), EndsAt: t0.Add(-88 * time.Hour)}
	for _, v := range []Version{old, tip} {
		if _, err := m.InsertVersion(context.Background(), &v); err != nil {
			t.Fatal(err)
		}
	}
	s := testService(m, t0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunMaintenance(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for {
		vs, _ := m.ListVersions(context.Background(), "us")
		if len(vs) == 1 && vs[0].Version == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("not purged: %d versions", len(vs))
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunMaintenance did not stop")
	}
}
