package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/property"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- fakes ---

type fakeSearch struct{ props []property.Property }

func (f fakeSearch) Search(context.Context, config.SearchCriteria) ([]property.Property, error) {
	return f.props, nil
}

// fakeUploader records peak concurrency and echoes the path into the URL so
// ordering can be asserted.
type fakeUploader struct {
	cur, peak atomic.Int64
}

func (u *fakeUploader) Upload(_ context.Context, path string, content io.Reader, _ string) (string, error) {
	n := u.cur.Add(1)
	for {
		p := u.peak.Load()
		if n <= p || u.peak.CompareAndSwap(p, n) {
			break
		}
	}
	_, _ = io.Copy(io.Discard, content)
	u.cur.Add(-1)
	return "https://cdn.example/" + path, nil
}

type fakeStore struct {
	upserts, ready, failed atomic.Int64
	existing               map[string]bool
}

func (s *fakeStore) Exists(_ context.Context, zpid string) (bool, error) {
	return s.existing[zpid], nil
}

func (s *fakeStore) Upsert(_ context.Context, p *property.Property) error {
	s.upserts.Add(1)
	p.VideoStatus = property.VideoPending
	return nil
}
func (s *fakeStore) SetVideoReady(context.Context, string, string, string, int) error {
	s.ready.Add(1)
	return nil
}
func (s *fakeStore) SetVideoFailed(context.Context, string) error {
	s.failed.Add(1)
	return nil
}

type fakeRenderer struct{ peak, cur atomic.Int64 }

func (r *fakeRenderer) Render(_ context.Context, _ *property.Property, imgs []string, _, outPath string) (int, error) {
	n := r.cur.Add(1)
	for {
		p := r.peak.Load()
		if n <= p || r.peak.CompareAndSwap(p, n) {
			break
		}
	}
	defer r.cur.Add(-1)
	if err := os.WriteFile(outPath, []byte("video"), 0o644); err != nil {
		return 0, err
	}
	return len(imgs) * 2, nil
}

func baseConfig() *config.Config {
	return &config.Config{
		ImagesEnabled: true,
		Video:         config.VideoConfig{Enabled: true, SecondsPerPhoto: 2},
		Concurrency:   config.ConcurrencyConfig{Listings: 4, Images: 4},
	}
}

func TestUploadPhotos_PreservesOrder(t *testing.T) {
	dir := t.TempDir()
	var local []string
	for i := 0; i < 12; i++ {
		p := dir + "/" + strconv.Itoa(i) + ".jpg"
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		local = append(local, p)
	}
	s := New(baseConfig(), fakeSearch{}, &fakeUploader{}, &fakeStore{}, &fakeRenderer{}, testLogger())

	urls := s.uploadPhotos(context.Background(), "ZP1", local)
	if len(urls) != 12 {
		t.Fatalf("got %d urls, want 12", len(urls))
	}
	for i, u := range urls {
		want := "properties/ZP1/" + strconv.Itoa(i) + ".jpg"
		if !strings.HasSuffix(u, want) {
			t.Errorf("url[%d] = %q, want suffix %q (order not preserved)", i, u, want)
		}
	}
}

func TestRunCycle_AllListingsRenderedConcurrently(t *testing.T) {
	// Image server returns a JPEG for any path.
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xd9")) // minimal JPEG marker bytes
	}))
	defer imgSrv.Close()

	var props []property.Property
	for i := 0; i < 8; i++ {
		props = append(props, property.Property{
			ZPID:      fmt.Sprintf("ZP%d", i),
			Address:   "addr",
			ImageURLs: []string{imgSrv.URL + "/a.jpg", imgSrv.URL + "/b.jpg"},
			DetailURL: "https://www.zillow.com/x/",
		})
	}

	store := &fakeStore{}
	render := &fakeRenderer{}
	s := New(baseConfig(), fakeSearch{props: props}, &fakeUploader{}, store, render, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := store.ready.Load(); got != 8 {
		t.Errorf("video ready = %d, want 8", got)
	}
	if got := store.failed.Load(); got != 0 {
		t.Errorf("video failed = %d, want 0", got)
	}
	if got := store.upserts.Load(); got != 8 {
		t.Errorf("upserts = %d, want 8", got)
	}
	// With 8 listings and a limit of 4, more than one render must have overlapped.
	if peak := render.peak.Load(); peak < 2 {
		t.Errorf("expected concurrent renders, peak = %d", peak)
	}
	if peak := render.peak.Load(); peak > 4 {
		t.Errorf("render concurrency exceeded limit: peak = %d", peak)
	}
}

func TestRunCycle_SkipsExisting(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xd9"))
	}))
	defer imgSrv.Close()

	var props []property.Property
	for i := 0; i < 5; i++ {
		props = append(props, property.Property{
			ZPID:      fmt.Sprintf("ZP%d", i),
			ImageURLs: []string{imgSrv.URL + "/a.jpg"},
		})
	}

	cfg := baseConfig()
	cfg.SkipExisting = true
	// ZP0, ZP1, ZP2 already exist → should be skipped.
	store := &fakeStore{existing: map[string]bool{"ZP0": true, "ZP1": true, "ZP2": true}}
	s := New(cfg, fakeSearch{props: props}, &fakeUploader{}, store, &fakeRenderer{}, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Only ZP3, ZP4 are new → 2 upserts and 2 videos.
	if got := store.upserts.Load(); got != 2 {
		t.Errorf("upserts = %d, want 2 (existing should be skipped)", got)
	}
	if got := store.ready.Load(); got != 2 {
		t.Errorf("video ready = %d, want 2", got)
	}
}
