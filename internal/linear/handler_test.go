package linear

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, store Store, now time.Time, path string) *httptest.ResponseRecorder {
	t.Helper()
	svc := testService(store, now)
	mux := http.NewServeMux()
	NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHandler_LivePlaylist(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	rec := serve(t, m, t0, "/channels/live.m3u8?zip=77494")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("content type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=2" {
		t.Errorf("cache control = %q", cc)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing CORS header")
	}
	if !strings.HasPrefix(rec.Body.String(), "#EXTM3U\n") {
		t.Errorf("body:\n%s", rec.Body.String())
	}
}

func TestHandler_Master(t *testing.T) {
	rec := serve(t, newMemStore(), t0, "/channels/master.m3u8?zip=77494")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "\nlive.m3u8?zip=77494\n") {
		t.Errorf("master must reference the media playlist with the same query:\n%s", rec.Body.String())
	}
	rec = serve(t, newMemStore(), t0, "/channels/master.m3u8")
	if !strings.Contains(rec.Body.String(), "\nlive.m3u8\n") {
		t.Errorf("national master:\n%s", rec.Body.String())
	}
	// The media URI is rebuilt from the parsed scope, never echoed from the
	// raw query: unknown parameters and odd casing must not be reflected.
	rec = serve(t, newMemStore(), t0, "/channels/master.m3u8?city=KATY&state=TX&junk=%3Cx%3E")
	if !strings.Contains(rec.Body.String(), "\nlive.m3u8?city=katy&state=tx\n") {
		t.Errorf("master must re-encode the scope:\n%s", rec.Body.String())
	}
	// Viewer parameters ride along to the media playlist, which is the
	// request that is tracked; invalid ones (an unfilled macro) are dropped.
	rec = serve(t, newMemStore(), t0, "/channels/master.m3u8?state=TX&ip=203.0.113.5&sid=roku-1")
	if !strings.Contains(rec.Body.String(), "\nlive.m3u8?ip=203.0.113.5&sid=roku-1&state=tx\n") {
		t.Errorf("master must carry sid and ip:\n%s", rec.Body.String())
	}
	rec = serve(t, newMemStore(), t0, "/channels/master.m3u8?ip=%7BRokuIP%7D&sid=%3Cx%3E")
	if !strings.Contains(rec.Body.String(), "\nlive.m3u8\n") {
		t.Errorf("master must drop invalid sid and ip:\n%s", rec.Body.String())
	}
}

func TestHandler_BadFilterIs400(t *testing.T) {
	rec := serve(t, newMemStore(), t0, "/channels/live.m3u8?city=Katy")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_NoContentIs503(t *testing.T) {
	rec := serve(t, newMemStore(), t0, "/channels/live.m3u8")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", rec.Code)
	}
}

func TestHandler_EPG(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	rec := serve(t, m, t0, "/channels/epg.json?zip=77494")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("cache control = %q", cc)
	}
	if !strings.Contains(rec.Body.String(), `"programs":[{"start":"`) {
		t.Errorf("body:\n%s", rec.Body.String())
	}
}

// A stored lineup whose item segment counts disagree with the clips it
// references is a store inconsistency: the request must fail with a logged
// 500, never take the process down with a panic.
func TestHandler_InconsistentLineupIs500(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	v := &Version{
		Key: "zip:77494", Version: 1, Scope: "zip:77494",
		StartsAt: t0.Add(-time.Hour), EndsAt: t0.Add(time.Hour),
		ItemIDs:  []int64{1},
		ItemMS:   []int{2 * 60 * 60 * 1000},
		ItemSegs: []int{99}, // clip 1 really has 20 segments
	}
	if _, err := m.InsertVersion(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	rec := serve(t, m, t0, "/channels/live.m3u8?zip=77494")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"internal error"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

// The channel endpoints are unauthenticated: a filter naming a place that
// does not exist must 404 before anything is created for it, so a crawler
// cannot mint an unbounded number of lineup chains and cache entries.
func TestHandler_UnknownAreaIs404AndCreatesNothing(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	paths := []string{
		"/channels/live.m3u8?zip=00000",
		"/channels/epg.json?zip=00000",
		"/channels/live.m3u8?city=Nowhere&state=tx",
		"/channels/live.m3u8?state=zz",
	}
	for _, p := range paths {
		rec := serve(t, m, t0, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404: %s", p, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"unknown area"`) {
			t.Errorf("GET %s body = %s", p, rec.Body.String())
		}
	}
	for _, key := range []string{"zip:00000", "city:nowhere|tx", "state:zz"} {
		if vs, _ := m.ListVersions(context.Background(), key); len(vs) != 0 {
			t.Errorf("channel %s got %d lineup versions; nothing should be created for an unknown area", key, len(vs))
		}
	}
}

// A real but empty ZIP is not an unknown area: it falls back up the
// hierarchy like any thin scope.
func TestHandler_RealButEmptyZipFallsBack(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	m.addZip("77493") // exists in zip_codes, no listings of its own
	rec := serve(t, m, t0, "/channels/live.m3u8?zip=77493")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
}
