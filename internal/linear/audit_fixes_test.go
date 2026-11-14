package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
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

func (lookup *recordingGeo) Locate(ip net.IP) (geo.Location, bool, bool) {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	lookup.asked = append(lookup.asked, ip.String())
	if lookup.down {
		return geo.Location{}, false, false
	}
	return lookup.loc, lookup.loc != geo.Location{}, true
}

func trustedOpts(aud *fakeAudience, lookup geo.Lookup) ViewerOptions {
	opts := baseOpts(aud, lookup)
	opts.Hasher = platformHasher("s")
	return opts
}

// A stranger naming a home in ip= must neither learn what that home watched
// last nor make the service look that address up.
func TestResolve_IPParamFromAStrangerIsIgnored(t *testing.T) {
	memory := newMemStore()
	addClips(memory, katy, 1, 5)
	addClips(memory, Scope{Zip: "33101", City: "miami", State: "fl"}, 101, 5)
	home, stranger := "203.0.113.44", "198.51.100.9"
	hasher := viewer.NewHasher("s", false)
	aud := &fakeAudience{last: map[viewer.ID]string{hasher.IDAt("", home, t0): "state:fl"}}
	lookup := &recordingGeo{loc: geo.Location{Zip: "77494"}}
	opts := trustedOpts(aud, lookup)

	got := decodeResolve(t, serveViewers(t, memory, opts, "/channels/resolve?ip="+home, stranger))
	if got["source"] == "last_watched" || got["scope"] == "state:fl" {
		t.Fatalf("a stranger read the home's last-watched channel: %v", got)
	}
	if len(lookup.asked) != 1 || lookup.asked[0] != stranger {
		t.Fatalf("lookups = %v, want only the connecting address", lookup.asked)
	}
	for _, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"} {
		serveViewers(t, memory, opts, "/channels/resolve?ip="+ip, stranger)
	}
	for _, address := range lookup.asked {
		if address != stranger {
			t.Fatalf("an invented ip= was looked up: %v", lookup.asked)
		}
	}
	got = decodeResolve(t, serveViewers(t, memory, opts, "/channels/resolve?ip="+home, platformAddr))
	if got["source"] != "last_watched" || got["scope"] != "state:fl" {
		t.Errorf("a platform's ip= was not believed: %v", got)
	}
}

func TestBeat_RecordsTheConnectingAddressAndOnlyRealPlaces(t *testing.T) {
	memory := newMemStore()
	addClips(memory, katy, 1, 5)
	tr := &fakeTracker{}
	opts := trustedOpts(&fakeAudience{}, nil)
	opts.Tracker = tr
	stranger := "198.51.100.9"

	if rec := serveViewers(t, memory, opts, "/channels/beat?ip=203.0.113.44", stranger); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d", rec.Code)
	}
	beat := tr.beats[0]
	if beat.Client.IP != stranger || tr.from[0] != stranger || beat.Viewer != viewer.NewHasher("s", false).IDAt("", stranger, t0) {
		t.Errorf("a stranger's beat was recorded under the address it claimed: %+v", beat)
	}
	// From a platform the claimed address counts, and the platform's own
	// address is not what rows are limited by.
	serveViewers(t, memory, opts, "/channels/beat?ip=203.0.113.44", platformAddr)
	if beat := tr.beats[1]; beat.Client.IP != "203.0.113.44" || tr.from[1] != "" {
		t.Errorf("platform beat = %+v, limited by %q", beat, tr.from[1])
	}
	if rec := serveViewers(t, memory, opts, "/channels/beat?zip=00000", stranger); rec.Code != http.StatusNotFound || len(tr.beats) != 2 {
		t.Errorf("beat for an unknown ZIP: status %d, %d beats", rec.Code, len(tr.beats))
	}
	if rec := serveViewers(t, memory, opts, "/channels/beat?zip=77494", stranger); rec.Code != http.StatusNoContent || len(tr.beats) != 3 {
		t.Errorf("beat for a real ZIP: status %d, %d beats", rec.Code, len(tr.beats))
	}
}

// A home that first connects while the location lookup cannot answer gets
// national for the moment, not for good.
func TestEnsureHousehold_PendingDefaultIsSettledWhenTheLookupAnswers(t *testing.T) {
	ctx := context.Background()
	memory := newMemStore()
	addClips(memory, katy, 1, 40)
	addClips(memory, austin, 101, 40)
	now := t0
	svc := testService(memory, now)
	svc.now = func() time.Time { return now }
	household := viewer.NewHasher("salt", false).Household("203.0.113.5")

	spans, err := svc.EnsureHousehold(ctx, household, nil, false, now)
	if err != nil || len(spans) != 1 || spans[0].Scope != "us" || spans[0].Source != SourcePending {
		t.Fatalf("first contact during an outage: %+v, %v", spans, err)
	}
	now = t0.Add(time.Minute)
	if spans, err = svc.EnsureHousehold(ctx, household, nil, false, now); err != nil || len(spans) != 1 || spans[0].Source != SourcePending {
		t.Fatalf("second contact during the outage: %+v, %v", spans, err)
	}
	// The lookup answers: the home moves to its area from the next segment
	// boundary, as a choice would, marked as a location default.
	now = t0.Add(3 * time.Minute)
	spans, err = svc.EnsureHousehold(ctx, household, []Scope{katy}, true, now)
	if err != nil {
		t.Fatal(err)
	}
	last := spans[len(spans)-1]
	if len(spans) != 2 || last.Scope != katy.Key() || last.Source != SourceGeo || last.Requested != katy.Key() || !last.StartsAt.After(t0) {
		t.Fatalf("after the lookup answered: %+v", spans)
	}
	now = t0.Add(10 * time.Minute)
	if again, err := svc.EnsureHousehold(ctx, household, []Scope{austin}, true, now); err != nil || len(again) != 2 || again[1].Scope != katy.Key() {
		t.Fatalf("a settled home was moved again: %+v, %v", again, err)
	}
	if _, _, err := svc.PersonalPlaylist(ctx, household, now); err != nil {
		t.Fatalf("playlist after settling: %v", err)
	}
}

func TestEnsureHousehold_PendingDefaultStandsWhenNothingBetterIsKnown(t *testing.T) {
	ctx := context.Background()
	memory := newMemStore()
	addClips(memory, katy, 1, 40)
	svc := testService(memory, t0)
	household := viewer.NewHasher("salt", false).Household("203.0.113.6")
	if _, err := svc.EnsureHousehold(ctx, household, nil, false, t0); err != nil {
		t.Fatal(err)
	}
	// The lookup answers with nothing usable (abroad), or with a place that
	// has no channel of its own: national is now a fact about the home.
	spans, err := svc.EnsureHousehold(ctx, household, []Scope{{State: "wy"}}, true, t0.Add(time.Minute))
	if err != nil || len(spans) != 1 || spans[0].Scope != "us" || spans[0].Source != SourceDefault {
		t.Fatalf("settled default: %+v, %v", spans, err)
	}
	if !spans[0].StartsAt.Equal(t0) {
		t.Errorf("settling moved the span's start: %v", spans[0].StartsAt)
	}
}

func TestEnsureHousehold_LocatedHomeIsNeverPending(t *testing.T) {
	ctx := context.Background()
	memory := newMemStore()
	addClips(memory, katy, 1, 40)
	svc := testService(memory, t0)
	hasher := viewer.NewHasher("salt", false)
	located, err := svc.EnsureHousehold(ctx, hasher.Household("203.0.113.7"), []Scope{katy}, true, t0)
	if err != nil || located[0].Source != SourceGeo {
		t.Fatalf("%+v, %v", located, err)
	}
	unlocated, err := svc.EnsureHousehold(ctx, hasher.Household("203.0.113.8"), nil, true, t0)
	if err != nil || unlocated[0].Source != SourceDefault {
		t.Fatalf("%+v, %v", unlocated, err)
	}
}

// Through the routes: a home that meets an outage is not stuck, and a client
// never sees the internal "pending" marker.
func TestFeed_HomeThatMetALookupOutageGetsItsAreaLater(t *testing.T) {
	memory := newMemStore()
	addClips(memory, katy, 1, 40)
	lookup := &recordingGeo{loc: geo.Location{Zip: "77494"}, down: true}
	svc, mux := feedMuxGeo(memory, t0, lookup)
	now := t0
	svc.now = func() time.Time { return now }

	me := func() (scope, source string) {
		var feed struct{ Scope, Source string }
		rec := do(mux, "GET", "/feed/me", "203.0.113.5", "")
		if rec.Code != 200 {
			t.Fatalf("/feed/me: %d %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
			t.Fatal(err)
		}
		return feed.Scope, feed.Source
	}
	if scope, source := me(); scope != "us" || source != SourceDefault {
		t.Fatalf("during the outage: %s %s", scope, source)
	}
	lookup.mu.Lock()
	lookup.down = false
	lookup.mu.Unlock()
	now = t0.Add(3 * time.Minute)
	if scope, source := me(); scope != katy.Key() || source != SourceGeo {
		t.Fatalf("after the outage: %s %s", scope, source)
	}
}

func TestPurgeVersions_KeepsWhatTheChannelsNeed(t *testing.T) {
	ctx := context.Background()
	memory := newMemStore()
	addClips(memory, katy, 1, 40)
	memory.addZip("77001")
	now := t0
	svc := testService(memory, now)
	svc.now = func() time.Time { return now }

	// One busy channel over four days, and one ZIP somebody named once.
	if _, err := svc.Playlist(ctx, Scope{Zip: "77001"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for hours := 0; hours <= 4*24; hours += 3 {
		now = t0.Add(time.Duration(hours) * time.Hour)
		if _, err := svc.Playlist(ctx, katy, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	count := func(key string) int {
		vs, err := memory.ListVersions(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		return len(vs)
	}
	before := count(katy.Key())
	purged, err := svc.purgeVersions(ctx)
	if err != nil || purged == 0 {
		t.Fatalf("purged %d, %v", purged, err)
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
	playlist, err := svc.Playlist(ctx, katy, live)
	if err != nil {
		t.Fatalf("busy channel after the purge: %v", err)
	}
	if seq := parsePlaylist(t, playlist).mediaSeq; seq < 1000 {
		t.Errorf("media sequence restarted after the purge: %d", seq)
	}
	if _, err := svc.Playlist(ctx, Scope{Zip: "77001"}, live); err != nil {
		t.Fatalf("idle channel after the purge: %v", err)
	}
}

func TestRunMaintenance_PurgesAndStops(t *testing.T) {
	memory := newMemStore()
	old := Version{Key: "us", Version: 1, Scope: "us", StartsAt: t0.Add(-100 * time.Hour), EndsAt: t0.Add(-94 * time.Hour)}
	tip := Version{Key: "us", Version: 2, Scope: "us", StartsAt: t0.Add(-94 * time.Hour), EndsAt: t0.Add(-88 * time.Hour)}
	for _, version := range []Version{old, tip} {
		if _, err := memory.InsertVersion(context.Background(), &version); err != nil {
			t.Fatal(err)
		}
	}
	svc := testService(memory, t0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.RunMaintenance(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for {
		vs, _ := memory.ListVersions(context.Background(), "us")
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

// A settled home is not looked up again: the answer would be thrown away
// and the lookup budget is for homes not yet placed.
func TestFeed_KnownHomeCostsNoLookup(t *testing.T) {
	memory := newMemStore()
	addClips(memory, katy, 1, 40)
	lookup := &recordingGeo{loc: geo.Location{Zip: "77494"}}
	_, mux := feedMuxGeo(memory, t0, lookup)
	home := "203.0.113.5"
	for _, path := range []string{"/feed/me", "/feed/me", "/feed/master.m3u8", "/feed/qr.png", "/channels/cineplex/live.m3u8"} {
		if rec := do(mux, "GET", path, home, ""); rec.Code != 200 {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
	}
	if len(lookup.asked) != 1 {
		t.Fatalf("lookups = %v, want one, at first contact", lookup.asked)
	}
	// A pending home is asked about again until the lookup answers.
	lookup.down = true
	other := "203.0.113.6"
	do(mux, "GET", "/feed/me", other, "")
	do(mux, "GET", "/feed/me", other, "")
	if len(lookup.asked) != 3 {
		t.Fatalf("lookups = %v, want the pending home asked about on each request", lookup.asked)
	}
}

// failingAreas is a Store whose area check cannot reach the database.
type failingAreas struct{ Store }

func (failingAreas) AreaExists(context.Context, Scope) (bool, error) {
	return false, errors.New("database is away")
}

func TestBeat_IsKeptWhenTheAreaCheckCannotRun(t *testing.T) {
	tr := &fakeTracker{}
	opts := trustedOpts(&fakeAudience{}, nil)
	opts.Tracker = tr
	rec := serveViewers(t, failingAreas{newMemStore()}, opts, "/channels/beat?zip=77494&sid=tv-1", "203.0.113.5")
	if rec.Code != http.StatusNoContent || len(tr.beats) != 1 || tr.beats[0].Channel != "zip:77494" {
		t.Fatalf("status %d, beats %+v", rec.Code, tr.beats)
	}
}

type countingAreas struct {
	Store
	calls int
}

func (counting *countingAreas) AreaExists(ctx context.Context, sc Scope) (bool, error) {
	counting.calls++
	return counting.Store.AreaExists(ctx, sc)
}

func TestCheckArea_RemembersBothAnswers(t *testing.T) {
	ctx := context.Background()
	memory := newMemStore()
	addClips(memory, katy, 1, 5)
	store := &countingAreas{Store: memory}
	now := t0
	svc := testService(store, now)
	svc.now = func() time.Time { return now }
	unknown := Scope{Zip: "00001"}
	for range 3 {
		if err := svc.checkArea(ctx, katy); err != nil {
			t.Fatal(err)
		}
		if err := svc.checkArea(ctx, unknown); !errors.Is(err, ErrUnknownArea) {
			t.Fatalf("unknown ZIP: %v", err)
		}
	}
	if store.calls != 2 {
		t.Fatalf("store asked %d times, want once per place", store.calls)
	}
	// A place that starts to exist is found within a minute.
	memory.addZip("00001")
	now = t0.Add(unknownAreaTTL + time.Second)
	if err := svc.checkArea(ctx, unknown); err != nil {
		t.Fatalf("a ZIP that now exists is still refused: %v", err)
	}
}

func TestPurgeVersions_GoesThroughABacklogInBatches(t *testing.T) {
	memory := newMemStore()
	for number := 1; number <= 2*purgeBatch+10; number++ {
		start := t0.Add(-1000*time.Hour + time.Duration(number)*time.Minute)
		version := Version{Key: "us", Version: number, Scope: "us", StartsAt: start, EndsAt: start.Add(time.Minute)}
		if _, err := memory.InsertVersion(context.Background(), &version); err != nil {
			t.Fatal(err)
		}
	}
	svc := testService(memory, t0)
	purged, err := svc.purgeVersions(context.Background())
	if err != nil || purged != int64(2*purgeBatch+9) {
		t.Fatalf("purged %d, %v; want everything but the newest", purged, err)
	}
	if vs, _ := memory.ListVersions(context.Background(), "us"); len(vs) != 1 || vs[0].Version != 2*purgeBatch+10 {
		t.Fatalf("left %d versions", len(vs))
	}
}

func TestFeedCORSWriter_LetsTheServerWriterBeReached(t *testing.T) {
	rec := httptest.NewRecorder()
	var writer http.ResponseWriter = &feedCORSWriter{ResponseWriter: rec}
	unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
	if !ok || unwrapper.Unwrap() != http.ResponseWriter(rec) {
		t.Fatal("feedCORSWriter must unwrap to the writer it wraps")
	}
	// The controller reaches Flush through it.
	if err := http.NewResponseController(writer).Flush(); err != nil {
		t.Errorf("Flush through the wrapper: %v", err)
	}
}
