package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/feed"
	"github.com/dwellingtw/backend/internal/property"
)

func TestSwaggerUIServed(t *testing.T) {
	s := New(":0", nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
	s := New(":0", nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.Mount(stubMounter{})
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stub", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("GET /stub = %d %q", rec.Code, rec.Body.String())
	}
}

// The Direct Publisher feed was removed on 2026-10-18: the Roku app never
// read it (it reads /api/v1/properties), and Roku Search reads
// /roku/search-feed.json. Nothing may bring it back by accident.
func TestPublisherFeedIsGone(t *testing.T) {
	s := New(":0", &stubFeed{props: listings(1)}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/roku/feed.json", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /roku/feed.json = %d, want 404", rec.Code)
	}
}

// stubFeed serves props as the pinned set and counts the calls.
type stubFeed struct {
	props []property.Property
	err   error
	calls int
}

func (s *stubFeed) ListSearchFeed(context.Context) ([]property.Property, error) {
	s.calls++
	return s.props, s.err
}

func rendered(i int) *time.Time {
	t := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Minute)
	return &t
}

// listings returns n ready listings, newest render first, ids 1..n.
func listings(n int) []property.Property {
	out := make([]property.Property, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, property.Property{
			ID: int64(i), ZPID: strconv.Itoa(i), VideoURL: "https://cdn/videos/" + strconv.Itoa(i) + ".mp4",
			ImageURLs: []string{"https://cdn/" + strconv.Itoa(i) + ".jpg"}, VideoRenderedAt: rendered(i),
		})
	}
	return out
}

func newFeedServer(repo feedSource) *Server {
	return New(":0", repo, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func getSearch(t *testing.T, s *Server, target string) (*httptest.ResponseRecorder, feed.SearchFeed) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "api.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	s.srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d:\n%s", target, rec.Code, rec.Body.String())
	}
	var page feed.SearchFeed
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode %s: %v\n%s", target, err, rec.Body.String())
	}
	return rec, page
}

func TestSearchFeed_IsOnePageOfThePinnedSet(t *testing.T) {
	repo := &stubFeed{props: listings(2500)}
	s := newFeedServer(repo)
	rec, page := getSearch(t, s, "/roku/search-feed.json")

	if page.Version != "1" || len(page.Assets) != 2500 {
		t.Fatalf("version %s, %d assets, want \"1\" and all 2500 pinned listings", page.Version, len(page.Assets))
	}
	for i, a := range page.Assets {
		if a.ID != strconv.Itoa(i+1) {
			t.Fatalf("asset %d = %s, want %d (the pinned order)", i, a.ID, i+1)
		}
	}
	if strings.Contains(rec.Body.String(), "nextPageUrl") {
		t.Error("the feed is a single page and must not advertise a next one")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != feedCacheControl {
		t.Errorf("Cache-Control = %q", cc)
	}
	if repo.calls != 1 {
		t.Errorf("repo calls = %d, want 1", repo.calls)
	}
}

// Roku may still hold a paged URL from before the feed was one page; every
// URL gets the same page.
func TestSearchFeed_OldPagingParamsAreIgnored(t *testing.T) {
	s := newFeedServer(&stubFeed{props: listings(3)})
	for _, target := range []string{"/roku/search-feed.json?limit=1", "/roku/search-feed.json?cursor=garbage!&limit=0"} {
		_, page := getSearch(t, s, target)
		if len(page.Assets) != 3 {
			t.Errorf("GET %s = %d assets, want all 3", target, len(page.Assets))
		}
	}
}

func TestSearchFeed_RepoErrorIs500(t *testing.T) {
	s := newFeedServer(&stubFeed{err: errors.New("db down")})
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/roku/search-feed.json", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store so an error is not cached", cc)
	}
}
