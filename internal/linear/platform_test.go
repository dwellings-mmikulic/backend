package linear

import (
	"net/http"
	"strings"
	"testing"
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
