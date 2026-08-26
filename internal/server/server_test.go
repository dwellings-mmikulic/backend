package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dwellingtw/backend/internal/feed"
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

func liveFeeds(t *testing.T, body string) []feed.LiveFeed {
	t.Helper()
	var decoded feed.Feed
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode feed response: %v\n%s", err, body)
	}
	return decoded.LiveFeeds
}

func available(context.Context) bool { return true }

func TestFeed_UsesTheConfiguredLiveThumbnail(t *testing.T) {
	const poster = "https://cdn.example/branding/live-poster.jpg"
	a := property.Property{ZPID: "1", ImageURLs: []string{"https://cdn/a.jpg"}}
	s := New(":0", "Dwellings", stubFeed{props: []property.Property{a}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetLiveFeed("https://api.example.com/channels/master.m3u8", poster, available)
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/roku/feed.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	lf := liveFeeds(t, rec.Body.String())
	if len(lf) != 1 {
		t.Fatalf("liveFeeds = %d entries, want 1:\n%s", len(lf), rec.Body.String())
	}
	if lf[0].Thumbnail != poster {
		t.Errorf("thumbnail = %q, want the configured poster %q", lf[0].Thumbnail, poster)
	}
	if !strings.Contains(rec.Body.String(), "channels/master.m3u8") {
		t.Errorf("feed lacks the live stream url:\n%s", rec.Body.String())
	}
}

// Roku Direct Publisher requires a non-empty thumbnail on every liveFeeds
// entry. Without a configured one, borrow the first ready listing's first
// image rather than pointing at a URL nobody ever uploaded.
func TestFeed_FallsBackToTheFirstListingImage(t *testing.T) {
	a := property.Property{ZPID: "1", ImageURLs: []string{"https://cdn/a.jpg"}}
	b := property.Property{ZPID: "2", ImageURLs: []string{"https://cdn/b.jpg"}}
	s := New(":0", "Dwellings", stubFeed{props: []property.Property{a, b}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetLiveFeed("https://api.example.com/channels/master.m3u8", "", available)
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/roku/feed.json", nil))
	lf := liveFeeds(t, rec.Body.String())
	if len(lf) != 1 {
		t.Fatalf("liveFeeds = %d entries, want 1:\n%s", len(lf), rec.Body.String())
	}
	if lf[0].Thumbnail != "https://cdn/a.jpg" {
		t.Errorf("thumbnail = %q, want the first ready listing's first image", lf[0].Thumbnail)
	}
}

// No configured thumbnail and no listing image to borrow: the entry must be
// omitted, not published with an empty thumbnail Roku would reject.
func TestFeed_OmitsLiveEntryWithoutAnyThumbnail(t *testing.T) {
	s := New(":0", "Dwellings", emptyFeed{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetLiveFeed("https://api.example.com/channels/master.m3u8", "", available)
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/roku/feed.json", nil))
	if lf := liveFeeds(t, rec.Body.String()); len(lf) != 0 {
		t.Errorf("liveFeeds = %+v, want none without a thumbnail", lf)
	}
	if strings.Contains(rec.Body.String(), "liveFeeds") {
		t.Errorf("liveFeeds key must be omitted entirely:\n%s", rec.Body.String())
	}
}

// Before the backfill has segmented anything the channels have nothing to
// play; advertising them to Roku would ship a stream that 503s.
func TestFeed_OmitsLiveEntryWhenNoContentIsAvailable(t *testing.T) {
	a := property.Property{ZPID: "1", ImageURLs: []string{"https://cdn/a.jpg"}}
	s := New(":0", "Dwellings", stubFeed{props: []property.Property{a}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetLiveFeed("https://api.example.com/channels/master.m3u8", "https://cdn/poster.jpg",
		func(context.Context) bool { return false })
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/roku/feed.json", nil))
	if lf := liveFeeds(t, rec.Body.String()); len(lf) != 0 {
		t.Errorf("liveFeeds = %+v, want none while the channel has no content", lf)
	}
}

type stubFeed struct{ props []property.Property }

func (s stubFeed) ListReadyForFeed(context.Context) ([]property.Property, error) { return s.props, nil }
