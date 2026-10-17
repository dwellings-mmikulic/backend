package mp4purge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dwellingtw/backend/internal/property"
)

const cdn = "https://cdn.example"

type fakeStorage struct {
	mu       sync.Mutex
	deleted  []string
	failPath string
}

func (s *fakeStorage) ObjectPath(cdnURL string) (string, error) {
	rest, ok := strings.CutPrefix(cdnURL, cdn+"/")
	if !ok || rest == "" {
		return "", fmt.Errorf("not an object of %s: %q", cdn, cdnURL)
	}
	return rest, nil
}

func (s *fakeStorage) Delete(ctx context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if path == s.failPath {
		return errors.New("bunny delete returned status 400")
	}
	s.deleted = append(s.deleted, path)
	return nil
}

type fakeStore struct {
	mu      sync.Mutex
	swaps   []string // "zpid from->to @hash"
	changed bool
}

func (s *fakeStore) SwapVideoURL(ctx context.Context, zpid, from, to, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed {
		return fmt.Errorf("swap: %w", property.ErrVideoChanged)
	}
	s.swaps = append(s.swaps, fmt.Sprintf("%s %s->%s @%s", zpid, from, to, hash))
	return nil
}

type fakeProbe struct {
	err    error
	probed []string
}

func (p *fakeProbe) Playable(ctx context.Context, url string) error {
	p.probed = append(p.probed, url)
	return p.err
}

func swap(zpid string) property.VideoSwap {
	return property.VideoSwap{
		ZPID:        zpid,
		MP4URL:      cdn + "/videos/" + zpid + ".mp4",
		HLSURL:      cdn + "/hls/v1/" + zpid + "/abcd1234/index.m3u8",
		ContentHash: "abcd1234",
	}
}

func newPurger(st *fakeStorage, store *fakeStore, probe *fakeProbe) *Purger {
	return New(st, store, probe, slog.New(slog.NewTextHandler(nil, nil)))
}

func TestPurge_DeletesTheMP4ThenSwapsTheRow(t *testing.T) {
	st, store, probe := &fakeStorage{}, &fakeStore{}, &fakeProbe{}
	s := swap("42")

	if err := newPurger(st, store, probe).Purge(context.Background(), s); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if len(st.deleted) != 1 || st.deleted[0] != "videos/42.mp4" {
		t.Errorf("deleted = %v, want videos/42.mp4", st.deleted)
	}
	if want := "42 " + s.MP4URL + "->" + s.HLSURL + " @abcd1234"; len(store.swaps) != 1 || store.swaps[0] != want {
		t.Errorf("swaps = %v, want %q", store.swaps, want)
	}
	if len(probe.probed) != 1 || probe.probed[0] != s.HLSURL {
		t.Errorf("probed = %v, want the playlist", probe.probed)
	}
}

func TestPurge_KeepsTheMP4WhenThePlaylistIsNotPlayable(t *testing.T) {
	st, store := &fakeStorage{}, &fakeStore{}
	probe := &fakeProbe{err: errors.New("status 404")}

	err := newPurger(st, store, probe).Purge(context.Background(), swap("42"))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Purge = %v, want the probe's error", err)
	}
	if len(st.deleted) != 0 || len(store.swaps) != 0 {
		t.Errorf("deleted %v, swapped %v; want nothing touched", st.deleted, store.swaps)
	}
}

func TestPurge_DoesNotSwapWhenTheDeleteFails(t *testing.T) {
	st := &fakeStorage{failPath: "videos/42.mp4"}
	store, probe := &fakeStore{}, &fakeProbe{}

	if err := newPurger(st, store, probe).Purge(context.Background(), swap("42")); err == nil {
		t.Fatal("Purge = nil, want the delete error")
	}
	if len(store.swaps) != 0 {
		t.Errorf("row swapped after a failed delete: %v", store.swaps)
	}
}

func TestPurge_ReportsARowChangedUnderneath(t *testing.T) {
	st, store, probe := &fakeStorage{}, &fakeStore{changed: true}, &fakeProbe{}

	err := newPurger(st, store, probe).Purge(context.Background(), swap("42"))
	if !errors.Is(err, property.ErrVideoChanged) {
		t.Fatalf("Purge = %v, want ErrVideoChanged", err)
	}
}

func TestCheck_RefusesWhatIsNotTheListingsOwnFiles(t *testing.T) {
	cases := map[string]func(*property.VideoSwap){
		"mp4 on another host":      func(s *property.VideoSwap) { s.MP4URL = "https://zillowstatic.com/v.mp4" },
		"another listing's mp4":    func(s *property.VideoSwap) { s.MP4URL = cdn + "/videos/43.mp4" },
		"mp4 outside videos/":      func(s *property.VideoSwap) { s.MP4URL = cdn + "/properties/42/42.mp4" },
		"already a playlist":       func(s *property.VideoSwap) { s.MP4URL = s.HLSURL },
		"playlist on another host": func(s *property.VideoSwap) { s.HLSURL = "https://other.example/hls/v1/42/x/index.m3u8" },
		"another listing's clip":   func(s *property.VideoSwap) { s.HLSURL = cdn + "/hls/v1/43/x/index.m3u8" },
		"not a playlist":           func(s *property.VideoSwap) { s.HLSURL = cdn + "/hls/v1/42/x/seg-000.ts" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			st, store, probe := &fakeStorage{}, &fakeStore{}, &fakeProbe{}
			s := swap("42")
			mutate(&s)
			p := newPurger(st, store, probe)
			if err := p.Check(s); err == nil {
				t.Error("Check = nil, want a refusal")
			}
			if err := p.Purge(context.Background(), s); err == nil {
				t.Error("Purge = nil, want a refusal")
			}
			if len(st.deleted) != 0 || len(store.swaps) != 0 || len(probe.probed) != 0 {
				t.Errorf("a refused listing was touched: deleted %v, swapped %v, probed %v", st.deleted, store.swaps, probe.probed)
			}
		})
	}
	if err := newPurger(&fakeStorage{}, &fakeStore{}, &fakeProbe{}).Check(swap("42")); err != nil {
		t.Errorf("Check of the listing's own files = %v, want nil", err)
	}
}

func TestHTTPProber(t *testing.T) {
	const vod = "#EXTM3U\n#EXT-X-VERSION:6\n#EXTINF:5.000000,\nseg-000.ts\n#EXT-X-ENDLIST\n"
	pages := map[string]string{
		"/vod":   vod,
		"/live":  "#EXTM3U\n#EXTINF:5.0,\nseg-000.ts\n",
		"/empty": "#EXTM3U\n#EXT-X-ENDLIST\n",
		"/html":  "<html>not found</html>",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	p := HTTPProber{Client: srv.Client()}

	if err := p.Playable(context.Background(), srv.URL+"/vod"); err != nil {
		t.Errorf("a complete VOD playlist = %v, want nil", err)
	}
	for _, path := range []string{"/live", "/empty", "/html", "/missing"} {
		if err := p.Playable(context.Background(), srv.URL+path); err == nil {
			t.Errorf("%s = nil, want an error", path)
		}
	}
}
