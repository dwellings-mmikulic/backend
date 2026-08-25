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
	body := rec.Body.String()
	if !strings.Contains(body, `"liveFeeds"`) || !strings.Contains(body, "channels/master.m3u8") {
		t.Errorf("feed lacks live entry:\n%s", body)
	}
	// The ready set is empty (no listing to borrow a thumbnail from), yet Roku
	// Direct Publisher requires a non-empty thumbnail on every liveFeeds entry.
	if strings.Contains(body, `"thumbnail":""`) {
		t.Errorf("liveFeeds thumbnail must not be empty when the ready set is empty:\n%s", body)
	}
	if !strings.Contains(body, liveChannelThumbnail) {
		t.Errorf("liveFeeds thumbnail must be the fixed channel poster, got:\n%s", body)
	}
}

func TestFeed_LiveThumbnailIsFixedRegardlessOfReadySet(t *testing.T) {
	// Two listings in different orders must not change which image is used
	// as the live channel's poster: it is fixed, not borrowed from whichever
	// listing happens to sort first.
	a := property.Property{ZPID: "1", ImageURLs: []string{"https://cdn/a.jpg"}}
	b := property.Property{ZPID: "2", ImageURLs: []string{"https://cdn/b.jpg"}}

	s1 := New(":0", "Dwellings", stubFeed{props: []property.Property{a, b}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s1.SetLiveFeedURL("https://api.example.com/channels/master.m3u8")
	rec1 := httptest.NewRecorder()
	s1.srv.Handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/roku/feed.json", nil))

	s2 := New(":0", "Dwellings", stubFeed{props: []property.Property{b, a}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s2.SetLiveFeedURL("https://api.example.com/channels/master.m3u8")
	rec2 := httptest.NewRecorder()
	s2.srv.Handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/roku/feed.json", nil))

	if !strings.Contains(rec1.Body.String(), liveChannelThumbnail) || !strings.Contains(rec2.Body.String(), liveChannelThumbnail) {
		t.Fatalf("both orderings must use the fixed channel poster:\n%s\n---\n%s", rec1.Body.String(), rec2.Body.String())
	}
}

type stubFeed struct{ props []property.Property }

func (s stubFeed) ListReadyForFeed(context.Context) ([]property.Property, error) { return s.props, nil }
