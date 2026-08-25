package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dwellingtw/backend/internal/property"
)

func TestSwaggerUIServed(t *testing.T) {
	s := New(":0", "Dwellings", nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	tests := []struct {
		path string
		want string
	}{
		{"/swagger/index.html", "swagger-ui"},
		{"/swagger/doc.json", `"Dwellings API"`},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		rec := httptest.NewRecorder()
		s.srv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want %d", tt.path, rec.Code, http.StatusOK)
			continue
		}
		if !strings.Contains(rec.Body.String(), tt.want) {
			t.Errorf("GET %s body does not contain %q", tt.path, tt.want)
		}
	}
}

type stubMounter struct{}

func (stubMounter) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /stub", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
}

func TestMount_RegistersRoutes(t *testing.T) {
	s := New(":0", "Dwellings", nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.Mount(stubMounter{})
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stub", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("GET /stub = %d %q", rec.Code, rec.Body.String())
	}
}

type emptyFeed struct{}

func (emptyFeed) ListReadyForFeed(context.Context) ([]property.Property, error) { return nil, nil }

func TestFeed_IncludesLiveFeedWhenConfigured(t *testing.T) {
	s := New(":0", "Dwellings", emptyFeed{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetLiveFeedURL("https://api.example.com/channels/master.m3u8")
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/roku/feed.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"liveFeeds"`) || !strings.Contains(rec.Body.String(), "channels/master.m3u8") {
		t.Errorf("feed lacks live entry:\n%s", rec.Body.String())
	}
}
