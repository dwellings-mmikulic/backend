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

func (emptyFeed) ListReadyForFeed(context.Context, int, *property.FeedKey) ([]property.Property, bool, error) {
	return nil, false, nil
}

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
	s := New(":0", "Dwellings", &stubFeed{props: []property.Property{a}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	s := New(":0", "Dwellings", &stubFeed{props: []property.Property{a, b}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	s := New(":0", "Dwellings", &stubFeed{props: []property.Property{a}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetLiveFeed("https://api.example.com/channels/master.m3u8", "https://cdn/poster.jpg",
		func(context.Context) bool { return false })
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/roku/feed.json", nil))
	if lf := liveFeeds(t, rec.Body.String()); len(lf) != 0 {
		t.Errorf("liveFeeds = %+v, want none while the channel has no content", lf)
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

func decodeFeed(t *testing.T, body string) feed.Feed {
	t.Helper()
	var decoded feed.Feed
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode feed response: %v\n%s", err, body)
	}
	return decoded
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
	return New(":0", "Dwellings", repo, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func getFeed(t *testing.T, s *Server, target string) (*httptest.ResponseRecorder, feed.Feed) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "api.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	s.srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d:\n%s", target, rec.Code, rec.Body.String())
	}
	return rec, decodeFeed(t, rec.Body.String())
}

func TestFeed_FirstPageIsCappedAndLinksToTheNext(t *testing.T) {
	repo := &stubFeed{props: listings(250)}
	s := newFeedServer(repo)
	rec, f := getFeed(t, s, "/roku/feed.json")

	if len(f.ShortFormVideos) != defaultFeedLimit {
		t.Fatalf("first page has %d videos, want the default limit %d", len(f.ShortFormVideos), defaultFeedLimit)
	}
	if f.ShortFormVideos[0].ID != "1" || f.ShortFormVideos[99].ID != "100" {
		t.Errorf("page order wrong: first %s last %s", f.ShortFormVideos[0].ID, f.ShortFormVideos[99].ID)
	}
	if f.NextCursor == "" {
		t.Fatal("nextCursor missing on a page with more rows behind it")
	}
	want := "https://api.example.com/roku/feed.json?cursor=" + f.NextCursor + "&limit=100"
	if f.NextPage != want {
		t.Errorf("nextPage = %q, want %q", f.NextPage, want)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q, want a 5 minute public cache", cc)
	}
	if len(repo.calls) != 1 || repo.calls[0].limit != defaultFeedLimit || repo.calls[0].after != nil {
		t.Errorf("repo calls = %+v, want one first-page call with the default limit", repo.calls)
	}
}

func TestFeed_FollowsTheCursorToTheEnd(t *testing.T) {
	repo := &stubFeed{props: listings(25)}
	s := newFeedServer(repo)
	var seen []string
	next := "/roku/feed.json?limit=10"
	for pages := 0; next != ""; pages++ {
		if pages > 5 {
			t.Fatal("pagination never ended")
		}
		_, f := getFeed(t, s, next)
		for _, v := range f.ShortFormVideos {
			seen = append(seen, v.ID)
		}
		if f.NextPage == "" {
			if f.NextCursor != "" {
				t.Errorf("last page: nextCursor %q without nextPage", f.NextCursor)
			}
			break
		}
		u, err := url.Parse(f.NextPage)
		if err != nil {
			t.Fatalf("nextPage %q: %v", f.NextPage, err)
		}
		next = u.RequestURI()
	}
	if len(seen) != 25 {
		t.Fatalf("saw %d listings across pages, want all 25: %v", len(seen), seen)
	}
	for i, id := range seen {
		if id != strconv.Itoa(i+1) {
			t.Fatalf("listing %d = %s, want %d (pages must not skip or repeat)", i, id, i+1)
		}
	}
	// Three full pages and one last call that returned the final 5.
	if len(repo.calls) != 3 {
		t.Errorf("repo calls = %d, want 3 pages of 10", len(repo.calls))
	}
	if repo.calls[1].after == nil || repo.calls[1].after.ID != 10 {
		t.Errorf("second page must start after id 10, got %+v", repo.calls[1].after)
	}
}

func TestFeed_LastPageHasNoNextLink(t *testing.T) {
	s := newFeedServer(&stubFeed{props: listings(3)})
	rec, f := getFeed(t, s, "/roku/feed.json?limit=3")
	if f.NextCursor != "" || f.NextPage != "" {
		t.Errorf("exact fit must not advertise a next page: %+v", f)
	}
	if strings.Contains(rec.Body.String(), "nextCursor") || strings.Contains(rec.Body.String(), "nextPage") {
		t.Errorf("next keys must be omitted, not empty:\n%s", rec.Body.String())
	}
}

func TestFeed_LimitIsClamped(t *testing.T) {
	repo := &stubFeed{props: listings(1)}
	s := newFeedServer(repo)
	getFeed(t, s, "/roku/feed.json?limit=99999")
	if repo.calls[0].limit != maxFeedLimit {
		t.Errorf("limit above the cap must be clamped to %d, got %d", maxFeedLimit, repo.calls[0].limit)
	}
}

func TestFeed_BadParamsAre400(t *testing.T) {
	s := newFeedServer(&stubFeed{props: listings(1)})
	for _, target := range []string{"/roku/feed.json?limit=0", "/roku/feed.json?limit=abc", "/roku/feed.json?cursor=garbage!"} {
		rec := httptest.NewRecorder()
		s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", target, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET %s Content-Type = %q, want JSON", target, ct)
		}
	}
}

// The live channel belongs to the catalogue, not to every page of it.
func TestFeed_LiveEntryOnlyOnTheFirstPage(t *testing.T) {
	s := newFeedServer(&stubFeed{props: listings(5)})
	s.SetLiveFeed("https://api.example.com/channels/master.m3u8", "https://cdn/poster.jpg", available)
	_, first := getFeed(t, s, "/roku/feed.json?limit=2")
	if len(first.LiveFeeds) != 1 {
		t.Errorf("first page liveFeeds = %d, want 1", len(first.LiveFeeds))
	}
	_, second := getFeed(t, s, "/roku/feed.json?limit=2&cursor="+first.NextCursor)
	if len(second.LiveFeeds) != 0 {
		t.Errorf("second page liveFeeds = %d, want none", len(second.LiveFeeds))
	}
}

func TestSearchFeed_PagesWithNextPageURL(t *testing.T) {
	repo := &stubFeed{props: listings(7)}
	s := newFeedServer(repo)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/roku/search-feed.json?limit=5", nil)
	req.Host = "api.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	s.srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d:\n%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != feedCacheControl {
		t.Errorf("Cache-Control = %q", cc)
	}
	var page feed.SearchFeed
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	if page.Version != "1" || len(page.Assets) != 5 || page.Assets[0].ID != "1" || page.Assets[4].ID != "5" {
		t.Errorf("page = version %s, %d assets, first %s", page.Version, len(page.Assets), page.Assets[0].ID)
	}
	if !strings.HasPrefix(page.NextPageURL, "https://api.example.com/roku/search-feed.json?cursor=") || !strings.HasSuffix(page.NextPageURL, "&limit=5") {
		t.Fatalf("nextPageUrl = %q", page.NextPageURL)
	}
	if strings.Contains(rec.Body.String(), "liveFeeds") || strings.Contains(rec.Body.String(), "shortFormVideos") {
		t.Errorf("search feed must not carry Direct Publisher keys:\n%s", rec.Body.String())
	}

	// Follow it: the last two, and no further link.
	u, _ := url.Parse(page.NextPageURL)
	rec = httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
	var last feed.SearchFeed
	if err := json.Unmarshal(rec.Body.Bytes(), &last); err != nil {
		t.Fatal(err)
	}
	if len(last.Assets) != 2 || last.Assets[0].ID != "6" || last.NextPageURL != "" {
		t.Errorf("last page = %d assets starting %s, nextPageUrl %q", len(last.Assets), last.Assets[0].ID, last.NextPageURL)
	}
	if strings.Contains(rec.Body.String(), "nextPageUrl") {
		t.Errorf("last page must omit nextPageUrl, not blank it:\n%s", rec.Body.String())
	}
	if repo.calls[0].limit != 5 || repo.calls[1].after == nil || repo.calls[1].after.ID != 5 {
		t.Errorf("repo calls = %+v", repo.calls)
	}
}

func TestSearchFeed_DefaultLimitAndBadCursor(t *testing.T) {
	repo := &stubFeed{props: listings(1)}
	s := newFeedServer(repo)
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/roku/search-feed.json", nil))
	if rec.Code != http.StatusOK || repo.calls[0].limit != defaultSearchLimit {
		t.Errorf("status %d, limit %d, want 200 and %d", rec.Code, repo.calls[0].limit, defaultSearchLimit)
	}
	rec = httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/roku/search-feed.json?cursor=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
}
