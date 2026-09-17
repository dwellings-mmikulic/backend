package linear

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/geo"
	"github.com/dwellingtw/backend/internal/viewer"
)

type fakeAudience struct {
	last  map[viewer.ID]string
	stats viewer.Stats
}

func (f *fakeAudience) LastChannel(_ context.Context, id viewer.ID, _ time.Time) (string, bool, error) {
	ch, ok := f.last[id]
	return ch, ok, nil
}
func (f *fakeAudience) Stats(context.Context, string, time.Time) (viewer.Stats, error) {
	return f.stats, nil
}

type fakeGeo struct{ loc geo.Location }

func (g fakeGeo) Locate(net.IP) (geo.Location, bool) { return g.loc, g.loc != geo.Location{} }

type fakeTracker struct{ beats []viewer.Heartbeat }

func (t *fakeTracker) Record(id viewer.ID, ch string, at time.Time) {
	t.beats = append(t.beats, viewer.Heartbeat{Viewer: id, Channel: ch, Minute: at})
}

func serveViewers(t *testing.T, store Store, o ViewerOptions, path, ip string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(testService(store, t0), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.EnableViewers(o)
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Real-IP", ip)
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeResolve(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func baseOpts(aud *fakeAudience, g geo.Lookup) ViewerOptions {
	return ViewerOptions{
		Hasher:        viewer.NewHasher("s", false),
		Audience:      aud,
		Geo:           g,
		PublicBaseURL: "https://api.test",
	}
}

func TestResolve_LastWatchedWins(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	addClips(m, Scope{State: "fl"}, 100, 40)
	id := viewer.NewHasher("s", false).IDAt("", "203.0.113.5", t0)
	aud := &fakeAudience{last: map[viewer.ID]string{id: "state:fl"}}
	got := decodeResolve(t, serveViewers(t, m, baseOpts(aud, fakeGeo{geo.Location{Zip: "77494"}}), "/channels/resolve", "203.0.113.5"))
	if got["source"] != "last_watched" || got["scope"] != "state:fl" {
		t.Errorf("got %v", got)
	}
	if got["master"] != "https://api.test/channels/master.m3u8?state=fl" || got["epg"] != "https://api.test/channels/epg.json?state=fl" {
		t.Errorf("urls: %v", got)
	}
	if got["name"] != "Homes for sale in Florida" {
		t.Errorf("name = %q", got["name"])
	}
}

func TestResolve_GeoZipWithFallback(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	aud := &fakeAudience{last: map[viewer.ID]string{}}
	// Exact ZIP has content.
	got := decodeResolve(t, serveViewers(t, m, baseOpts(aud, fakeGeo{geo.Location{Zip: "77494", City: "Katy", State: "tx"}}), "/channels/resolve", "203.0.113.5"))
	if got["source"] != "geo" || got["scope"] != "zip:77494" {
		t.Errorf("got %v", got)
	}
	// Unknown ZIP but known city → city scope via the geo city, not national.
	got = decodeResolve(t, serveViewers(t, m, baseOpts(aud, fakeGeo{geo.Location{Zip: "77000", City: "Katy", State: "tx"}}), "/channels/resolve", "203.0.113.5"))
	if got["source"] != "geo" || got["scope"] != "city:katy|tx" {
		t.Errorf("got %v", got)
	}
	// Thin ZIP falls back through the normal chain (zip → city → state → us).
	m2 := newMemStore()
	addClips(m2, katy, 1, 2) // below MinScopeClips (3)
	addClips(m2, Scope{State: "tx"}, 100, 40)
	got = decodeResolve(t, serveViewers(t, m2, baseOpts(aud, fakeGeo{geo.Location{Zip: "77494"}}), "/channels/resolve", "203.0.113.5"))
	if got["source"] != "geo" || got["scope"] != "state:tx" {
		t.Errorf("got %v", got)
	}
}

func TestResolve_DefaultAndNoContent(t *testing.T) {
	m := newMemStore()
	addClips(m, Scope{State: "fl"}, 100, 40)
	aud := &fakeAudience{last: map[viewer.ID]string{}}
	got := decodeResolve(t, serveViewers(t, m, baseOpts(aud, nil), "/channels/resolve", "203.0.113.5"))
	if got["source"] != "default" || got["scope"] != "us" || got["master"] != "https://api.test/channels/master.m3u8" {
		t.Errorf("got %v", got)
	}
	rec := serveViewers(t, newMemStore(), baseOpts(aud, nil), "/channels/resolve", "203.0.113.5")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("empty library: status %d", rec.Code)
	}
	rec = serveViewers(t, m, baseOpts(aud, nil), "/channels/resolve", "203.0.113.5")
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("resolve must not be cached: %q", cc)
	}
}

func TestResolve_BadLastChannelIsIgnored(t *testing.T) {
	m := newMemStore()
	addClips(m, Scope{State: "fl"}, 100, 40)
	id := viewer.NewHasher("s", false).IDAt("", "203.0.113.5", t0)
	aud := &fakeAudience{last: map[viewer.ID]string{id: "garbage"}}
	got := decodeResolve(t, serveViewers(t, m, baseOpts(aud, nil), "/channels/resolve", "203.0.113.5"))
	if got["source"] != "default" {
		t.Errorf("got %v", got)
	}
}

func TestStats(t *testing.T) {
	aud := &fakeAudience{stats: viewer.Stats{Concurrent: 3, Unique24h: 10, Unique7d: 50}}
	rec := serveViewers(t, newMemStore(), baseOpts(aud, nil), "/channels/stats?state=TX", "203.0.113.5")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Scope string `json:"scope"`
		viewer.Stats
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Scope != "state:tx" || got.Concurrent != 3 || got.Unique7d != 50 {
		t.Errorf("got %+v", got)
	}
	if rec.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Errorf("cache control = %q", rec.Header().Get("Cache-Control"))
	}
	if rec = serveViewers(t, newMemStore(), baseOpts(aud, nil), "/channels/stats?zip=1", "1.1.1.1"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad scope: %d", rec.Code)
	}
}

func TestLive_RecordsHeartbeat(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	tr := &fakeTracker{}
	o := baseOpts(&fakeAudience{}, nil)
	o.Tracker = tr
	rec := serveViewers(t, m, o, "/channels/live.m3u8?zip=77494&sid=roku-1", "203.0.113.5")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if len(tr.beats) != 1 || tr.beats[0].Channel != "zip:77494" {
		t.Fatalf("beats = %+v", tr.beats)
	}
	if tr.beats[0].Viewer != viewer.NewHasher("s", false).IDAt("roku-1", "", t0) {
		t.Error("sid must identify the viewer")
	}
	// master and a bad scope are not heartbeats.
	serveViewers(t, m, o, "/channels/master.m3u8", "203.0.113.5")
	serveViewers(t, m, o, "/channels/live.m3u8?zip=x", "203.0.113.5")
	if len(tr.beats) != 1 {
		t.Errorf("beats after master/bad = %d", len(tr.beats))
	}
}

func TestViewersNotEnabled(t *testing.T) {
	rec := serve(t, newMemStore(), t0, "/channels/resolve")
	if rec.Code != http.StatusNotFound {
		t.Errorf("resolve without EnableViewers: %d", rec.Code)
	}
}

func TestBeat(t *testing.T) {
	m := newMemStore()
	tr := &fakeTracker{}
	o := baseOpts(&fakeAudience{}, nil)
	o.Tracker = tr
	rec := serveViewers(t, m, o, "/channels/beat?state=TX&sid=roku-1", "203.0.113.5")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("headers: %v", rec.Header())
	}
	if len(tr.beats) != 1 || tr.beats[0].Channel != "state:tx" || tr.beats[0].Viewer != viewer.NewHasher("s", false).IDAt("roku-1", "", t0) {
		t.Errorf("beats = %+v", tr.beats)
	}
	// National needs no filter; a bad filter is rejected and not recorded.
	if rec = serveViewers(t, m, o, "/channels/beat", "203.0.113.5"); rec.Code != http.StatusNoContent || tr.beats[1].Channel != "us" {
		t.Errorf("national beat: %d %+v", rec.Code, tr.beats)
	}
	if rec = serveViewers(t, m, o, "/channels/beat?zip=abc", "203.0.113.5"); rec.Code != http.StatusBadRequest || len(tr.beats) != 2 {
		t.Errorf("bad beat: %d, beats %d", rec.Code, len(tr.beats))
	}
}

func TestResolve_CarriesAdTags(t *testing.T) {
	const pre = "https://ads.example.com/vast?pod=pre&did=ROKU_ADS_TRACKING_ID"
	m := newMemStore()
	addClips(m, Scope{State: "fl"}, 100, 40)
	h := NewHandler(testService(m, t0), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.EnableViewers(baseOpts(&fakeAudience{last: map[viewer.ID]string{}}, nil))
	h.SetAds(pre, "")
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/channels/resolve", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]*string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if v, ok := body["pre_roll_ad"]; !ok || v == nil || *v != pre {
		t.Errorf("pre_roll_ad = %v: %s", v, rec.Body.String())
	}
	if v, ok := body["mid_roll_ad"]; !ok || v != nil {
		t.Errorf("mid_roll_ad must be present and null: %s", rec.Body.String())
	}
}
