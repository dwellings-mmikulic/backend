package linear

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/geo"
	"github.com/dwellingtw/backend/internal/viewer"
)

func TestPlatformRoutes_ServeTheChannel(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	// The master's media reference is relative, so a player resolves it to
	// /channels/cineplex/live.m3u8 and stays on the platform's feed.
	rec := serve(t, m, t0, "/channels/cineplex/master.m3u8")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "\nlive.m3u8\n") {
		t.Fatalf("master: %d\n%s", rec.Code, rec.Body.String())
	}
	for _, path := range []string{
		"/channels/cineplex/live.m3u8",
		"/channels/cineplex/live.m3u8?zip=77494",
		"/channels/cineplex/epg.json",
		"/channels/cineplex/epg.xml",
	} {
		if rec := serve(t, m, t0, path); rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestPlatformRoutes_UnknownPlatformIs404(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	for _, path := range []string{
		"/channels/pluto/master.m3u8",
		"/channels/pluto/live.m3u8",
		"/channels/Cineplex/live.m3u8",
		"/channels/pluto/epg.xml",
	} {
		if rec := serve(t, m, t0, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, rec.Code)
		}
	}
	// Viewer-only routes have no platform form.
	if rec := serve(t, m, t0, "/channels/cineplex/resolve"); rec.Code != http.StatusNotFound {
		t.Errorf("platform resolve: %d, want 404", rec.Code)
	}
}

func TestPlatformRoutes_TrackThePlatform(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	tr := &fakeTracker{}
	o := baseOpts(&fakeAudience{}, nil)
	o.Tracker = tr
	serveViewers(t, m, o, "/channels/cineplex/live.m3u8", "203.0.113.5")
	serveViewers(t, m, o, "/channels/live.m3u8", "203.0.113.6")
	serveViewers(t, m, o, "/channels/pluto/live.m3u8", "203.0.113.7")
	if len(tr.beats) != 2 {
		t.Fatalf("beats = %+v", tr.beats)
	}
	if tr.beats[0].Client.Platform != "cineplex" || tr.beats[0].Channel != "us" {
		t.Errorf("platform beat = %+v", tr.beats[0])
	}
	if tr.beats[1].Client.Platform != "" {
		t.Errorf("own feed beat platform = %q", tr.beats[1].Client.Platform)
	}
}

func TestPlatformRoutes_Stats(t *testing.T) {
	aud := &fakeAudience{}
	if rec := serveViewers(t, newMemStore(), baseOpts(aud, nil), "/channels/cineplex/stats", "1.1.1.1"); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if aud.platform != "cineplex" {
		t.Errorf("stats platform = %q", aud.platform)
	}
	if serveViewers(t, newMemStore(), baseOpts(aud, nil), "/channels/stats", "1.1.1.1"); aud.platform != "" {
		t.Errorf("own stats platform = %q", aud.platform)
	}
}

// platformMux is feedMux with an IP geo lookup, as production has.
func platformMux(store Store, now time.Time, g geo.Lookup) (*Service, *http.ServeMux) {
	svc := testService(store, now)
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.EnableViewers(ViewerOptions{Hasher: viewer.NewHasher("salt", false), Audience: feedTestAudience{}, Geo: g, PublicBaseURL: "https://api.example.test"})
	h.EnableFeeds(FeedOptions{MobileBaseURL: "https://tv.example.test", QRShowSeconds: 60, QREverySeconds: 300})
	mux := http.NewServeMux()
	h.Register(mux)
	return svc, mux
}

// A platform's player (a Roku box in the Cineplex app) fetches the playlist
// itself, with no ip= in the URL, so the connecting address is the home.
func TestPlatformLive_FollowsTheConnectingHousehold(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	svc, mux := platformMux(m, t0, nil)
	ip := "203.0.113.7"
	// The phone on the home's network picks austin.
	var me struct{ ID string }
	_ = json.Unmarshal(do(mux, "GET", "/feed/me", ip, "").Body.Bytes(), &me)
	svc.now = func() time.Time { return t0.Add(time.Minute) }
	if rec := do(mux, "POST", "/feed/"+me.ID, ip, `{"zip":"78701"}`); rec.Code != 200 {
		t.Fatalf("choose: %d %s", rec.Code, rec.Body.String())
	}
	// The box polls the platform feed: anchored on first contact…
	svc.now = func() time.Time { return t0.Add(2 * time.Minute) }
	if rec := do(mux, "GET", "/channels/cineplex/live.m3u8", ip, ""); rec.Code != 200 {
		t.Fatalf("first poll: %d %s", rec.Code, rec.Body.String())
	}
	// …and from then on, through the whole lineup, it lists austin only.
	for min := 5; min < 40; min++ {
		svc.now = func() time.Time { return t0.Add(time.Duration(min) * time.Minute) }
		rec := do(mux, "GET", "/channels/cineplex/live.m3u8", ip, "")
		if rec.Code != 200 {
			t.Fatalf("t0+%dm: status %d: %s", min, rec.Code, rec.Body.String())
		}
		for _, u := range parsePlaylist(t, rec.Body.Bytes()).urls {
			if clipID(t, u) <= 100 {
				t.Fatalf("t0+%dm: katy listed on the home's platform feed:\n%s", min, rec.Body.String())
			}
		}
	}
	// The own channel route never keys on the connection: the same home
	// sees what any other address sees.
	own := do(mux, "GET", "/channels/live.m3u8", ip, "").Body.String()
	other := do(mux, "GET", "/channels/live.m3u8", "198.51.100.1", "").Body.String()
	if own != other {
		t.Fatalf("/channels/live.m3u8 was personalized by the connecting address:\n%s\n---\n%s", own, other)
	}
}

// A home that never scanned the QR gets its own area by IP, as the personal
// feeds do: the household is created on first contact with the geo default.
func TestPlatformLive_NewHouseholdDefaultsToItsArea(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	g := fakeGeo{loc: geo.Location{Zip: "78701", City: "austin", State: "tx"}}
	svc, mux := platformMux(m, t0, g)
	ip := "203.0.113.9"
	svc.now = func() time.Time { return t0.Add(time.Minute) }
	if rec := do(mux, "GET", "/channels/cineplex/live.m3u8", ip, ""); rec.Code != 200 {
		t.Fatalf("first poll: %d %s", rec.Code, rec.Body.String())
	}
	spans, _ := m.Spans(context.Background(), viewer.NewHasher("salt", false).Household(ip))
	if len(spans) != 1 || spans[0].Source != SourceGeo || spans[0].Scope != austin.Key() {
		t.Fatalf("spans = %+v", spans)
	}
	for min := 4; min < 40; min++ {
		svc.now = func() time.Time { return t0.Add(time.Duration(min) * time.Minute) }
		rec := do(mux, "GET", "/channels/cineplex/live.m3u8", ip, "")
		if rec.Code != 200 {
			t.Fatalf("t0+%dm: status %d: %s", min, rec.Code, rec.Body.String())
		}
		for _, u := range parsePlaylist(t, rec.Body.Bytes()).urls {
			if clipID(t, u) <= 100 {
				t.Fatalf("t0+%dm: katy listed on an austin home's platform feed:\n%s", min, rec.Body.String())
			}
		}
	}
}

// A non-public connecting address (a stitcher on a private network, a
// misconfigured proxy) is nobody's home: the channel as advertised, and no
// household is created for it.
func TestPlatformLive_NonPublicConnectingAddressIsTheChannel(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, austin, 101, 40)
	g := fakeGeo{loc: geo.Location{Zip: "78701", City: "austin", State: "tx"}}
	svc, mux := platformMux(m, t0, g)
	svc.now = func() time.Time { return t0.Add(time.Minute) }
	plain := do(mux, "GET", "/channels/live.m3u8", "198.51.100.1", "").Body.String()
	for _, ip := range []string{"10.0.0.5", "100.64.1.1", "127.0.0.1"} {
		rec := do(mux, "GET", "/channels/cineplex/live.m3u8", ip, "")
		if rec.Code != 200 || rec.Body.String() != plain {
			t.Errorf("from %s: status %d, body differs from the channel", ip, rec.Code)
		}
		if spans, _ := m.Spans(context.Background(), viewer.NewHasher("salt", false).Household(ip)); len(spans) != 0 {
			t.Errorf("from %s: household created: %+v", ip, spans)
		}
	}
}
