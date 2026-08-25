package linear

import (
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
