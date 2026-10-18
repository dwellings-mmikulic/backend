package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// stubFeed pages over props in memory the way the repository does: after is
// the position of the previous page's last row, and limit+1 rows are read to
// learn whether another page exists.
type stubFeed struct {
	props []property.Property
	calls []feedCall
}

type feedCall struct {
	limit int
	after *property.FeedKey
}

func (s *stubFeed) ListReadyForFeed(_ context.Context, limit int, after *property.FeedKey) ([]property.Property, bool, error) {
	s.calls = append(s.calls, feedCall{limit, after})
	start := 0
	if after != nil {
		for i, p := range s.props {
			if p.ID == after.ID {
				start = i + 1
			}
		}
	}
	rest := s.props[start:]
	if len(rest) > limit {
		return rest[:limit], true, nil
	}
	return rest, false, nil
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

func TestSearchFeed_FirstPageIsCappedAndLinksToTheNext(t *testing.T) {
	repo := &stubFeed{props: listings(defaultSearchLimit + 50)}
	s := newFeedServer(repo)
	rec, page := getSearch(t, s, "/roku/search-feed.json")

	if page.Version != "1" || len(page.Assets) != defaultSearchLimit {
		t.Fatalf("version %s, %d assets, want \"1\" and the default limit %d", page.Version, len(page.Assets), defaultSearchLimit)
	}
	if page.Assets[0].ID != "1" || page.Assets[defaultSearchLimit-1].ID != strconv.Itoa(defaultSearchLimit) {
		t.Errorf("page order wrong: first %s last %s", page.Assets[0].ID, page.Assets[defaultSearchLimit-1].ID)
	}
	if !strings.HasPrefix(page.NextPageURL, "https://api.example.com/roku/search-feed.json?cursor=") ||
		!strings.HasSuffix(page.NextPageURL, "&limit="+strconv.Itoa(defaultSearchLimit)) {
		t.Errorf("nextPageUrl = %q", page.NextPageURL)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != feedCacheControl {
		t.Errorf("Cache-Control = %q", cc)
	}
	if len(repo.calls) != 1 || repo.calls[0].limit != defaultSearchLimit || repo.calls[0].after != nil {
		t.Errorf("repo calls = %+v, want one first-page call with the default limit", repo.calls)
	}
}

func TestSearchFeed_FollowsNextPageURLToTheEnd(t *testing.T) {
	repo := &stubFeed{props: listings(25)}
	s := newFeedServer(repo)
	var seen []string
	next := "/roku/search-feed.json?limit=10"
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination never ended")
		}
		rec, page := getSearch(t, s, next)
		for _, a := range page.Assets {
			seen = append(seen, a.ID)
		}
		if page.NextPageURL == "" {
			if strings.Contains(rec.Body.String(), "nextPageUrl") {
				t.Errorf("last page must omit nextPageUrl, not blank it:\n%s", rec.Body.String())
			}
			break
		}
		u, err := url.Parse(page.NextPageURL)
		if err != nil {
			t.Fatalf("nextPageUrl %q: %v", page.NextPageURL, err)
		}
		next = u.RequestURI()
	}
	if len(seen) != 25 {
		t.Fatalf("saw %d assets across pages, want all 25: %v", len(seen), seen)
	}
	for i, id := range seen {
		if id != strconv.Itoa(i+1) {
			t.Fatalf("asset %d = %s, want %d (pages must not skip or repeat)", i, id, i+1)
		}
	}
	if len(repo.calls) != 3 || repo.calls[1].after == nil || repo.calls[1].after.ID != 10 {
		t.Errorf("repo calls = %+v, want 3 pages of 10 with the second starting after id 10", repo.calls)
	}
}

func TestSearchFeed_ExactFitHasNoNextLink(t *testing.T) {
	s := newFeedServer(&stubFeed{props: listings(3)})
	rec, page := getSearch(t, s, "/roku/search-feed.json?limit=3")
	if page.NextPageURL != "" || strings.Contains(rec.Body.String(), "nextPageUrl") {
		t.Errorf("exact fit must not advertise a next page:\n%s", rec.Body.String())
	}
}

func TestSearchFeed_LimitIsClamped(t *testing.T) {
	repo := &stubFeed{props: listings(1)}
	s := newFeedServer(repo)
	getSearch(t, s, "/roku/search-feed.json?limit=99999")
	if repo.calls[0].limit != maxSearchLimit {
		t.Errorf("limit above the cap must be clamped to %d, got %d", maxSearchLimit, repo.calls[0].limit)
	}
}

func TestSearchFeed_BadParamsAre400(t *testing.T) {
	s := newFeedServer(&stubFeed{props: listings(1)})
	for _, target := range []string{"/roku/search-feed.json?limit=0", "/roku/search-feed.json?limit=abc", "/roku/search-feed.json?cursor=garbage!"} {
		rec := httptest.NewRecorder()
		s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", target, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET %s Content-Type = %q, want JSON", target, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", target, cc)
		}
	}
}
