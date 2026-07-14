package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/zillow"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- fakes ---

type fakeSearch struct {
	props []property.Property
	// byLocation, when set, returns per-location props and records each queried
	// location in order. Takes precedence over props.
	byLocation map[string][]property.Property
	queried    []string
	// detailsErr, when set for a zpid, is returned by PropertyDetails.
	detailsErr map[string]error
	mu         sync.Mutex
}

func (f *fakeSearch) Search(_ context.Context, c config.SearchCriteria) ([]property.Property, error) {
	if f.byLocation != nil {
		f.mu.Lock()
		f.queried = append(f.queried, c.Location)
		f.mu.Unlock()
		return f.byLocation[c.Location], nil
	}
	return f.props, nil
}

func (f *fakeSearch) PropertyDetails(_ context.Context, zpid string) (*property.Details, []byte, error) {
	if err := f.detailsErr[zpid]; err != nil {
		return nil, nil, err
	}
	pt := "SINGLE_FAMILY"
	return &property.Details{PropertyType: &pt}, []byte(`{"status":"OK"}`), nil
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
	// missingDetails is what ListZPIDsMissingDetails returns (up to limit).
	missingDetails []string
	mu             sync.Mutex
	detailsSet     []string // zpids passed to SetDetails, in order
	detailsGot     map[string]*property.Details
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

func (s *fakeStore) ListZPIDsMissingDetails(_ context.Context, limit int) ([]string, error) {
	if len(s.missingDetails) > limit {
		return s.missingDetails[:limit], nil
	}
	return s.missingDetails, nil
}

func (s *fakeStore) SetDetails(_ context.Context, zpid string, d *property.Details, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detailsSet = append(s.detailsSet, zpid)
	if s.detailsGot == nil {
		s.detailsGot = map[string]*property.Details{}
	}
	s.detailsGot[zpid] = d
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
		ImagesEnabled:   true,
		Video:           config.VideoConfig{Enabled: true, SecondsPerPhoto: 2},
		Concurrency:     config.ConcurrencyConfig{Listings: 4, Images: 4},
		SearchLocations: []string{"33950"},
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
	s := New(baseConfig(), &fakeSearch{}, &fakeUploader{}, &fakeStore{}, &fakeRenderer{}, testLogger())

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
	s := New(baseConfig(), &fakeSearch{props: props}, &fakeUploader{}, store, render, testLogger())

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
	s := New(cfg, &fakeSearch{props: props}, &fakeUploader{}, store, &fakeRenderer{}, testLogger())

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

func TestRunCycle_SearchesEachLocation(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xd9"))
	}))
	defer imgSrv.Close()

	// prop builds n listings for a location, with zpids prefixed by the location.
	prop := func(loc string, n int) []property.Property {
		var out []property.Property
		for i := 0; i < n; i++ {
			out = append(out, property.Property{
				ZPID:      fmt.Sprintf("%s-ZP%d", loc, i),
				ImageURLs: []string{imgSrv.URL + "/a.jpg"},
			})
		}
		return out
	}

	search := &fakeSearch{byLocation: map[string][]property.Property{
		"33950": prop("33950", 3),
		"33948": prop("33948", 2),
		"33983": prop("33983", 4),
	}}

	cfg := baseConfig()
	cfg.SearchLocations = []string{"33950", "33948", "33983"}
	store := &fakeStore{}
	s := New(cfg, search, &fakeUploader{}, store, &fakeRenderer{}, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Every configured location must be searched, in order.
	want := []string{"33950", "33948", "33983"}
	if len(search.queried) != len(want) {
		t.Fatalf("queried %v, want %v", search.queried, want)
	}
	for i, loc := range want {
		if search.queried[i] != loc {
			t.Errorf("queried[%d] = %q, want %q", i, search.queried[i], loc)
		}
	}

	// All 3+2+4 = 9 listings across the three ZIPs must be processed.
	if got := store.upserts.Load(); got != 9 {
		t.Errorf("upserts = %d, want 9 (across all locations)", got)
	}
	if got := store.ready.Load(); got != 9 {
		t.Errorf("video ready = %d, want 9", got)
	}
}

func TestRunCycle_EnrichesDetailsUpToCap(t *testing.T) {
	cfg := baseConfig()
	cfg.SearchLocations = nil // no search work — isolate enrichment
	cfg.DetailsPerCycle = 2

	store := &fakeStore{missingDetails: []string{"Z1", "Z2", "Z3"}}
	s := New(cfg, &fakeSearch{}, &fakeUploader{}, store, &fakeRenderer{}, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.detailsSet) != 2 {
		t.Fatalf("SetDetails calls = %v, want exactly the 2-cap", store.detailsSet)
	}
	if store.detailsSet[0] != "Z1" || store.detailsSet[1] != "Z2" {
		t.Errorf("enriched %v, want [Z1 Z2] (oldest first)", store.detailsSet)
	}
	if d := store.detailsGot["Z1"]; d == nil || d.PropertyType == nil || *d.PropertyType != "SINGLE_FAMILY" {
		t.Errorf("details not stored: %+v", store.detailsGot["Z1"])
	}
}

func TestRunCycle_EnrichmentDisabledWhenCapZero(t *testing.T) {
	cfg := baseConfig()
	cfg.SearchLocations = nil
	cfg.DetailsPerCycle = 0

	store := &fakeStore{missingDetails: []string{"Z1"}}
	s := New(cfg, &fakeSearch{}, &fakeUploader{}, store, &fakeRenderer{}, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.detailsSet) != 0 {
		t.Errorf("enrichment ran with cap 0: %v", store.detailsSet)
	}
}

func TestRunCycle_EnrichmentErrorHandling(t *testing.T) {
	cfg := baseConfig()
	cfg.SearchLocations = nil
	cfg.DetailsPerCycle = 10

	search := &fakeSearch{detailsErr: map[string]error{
		"DEAD":  zillow.ErrDetailsNotFound,  // definitive: mark fetched
		"FLAKY": errors.New("500 whatever"), // transient: leave for retry
	}}
	store := &fakeStore{missingDetails: []string{"DEAD", "FLAKY", "OK1"}}
	s := New(cfg, search, &fakeUploader{}, store, &fakeRenderer{}, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	// DEAD gets empty details recorded (no infinite retry); FLAKY is skipped;
	// OK1 is enriched normally.
	got := map[string]bool{}
	for _, z := range store.detailsSet {
		got[z] = true
	}
	if !got["DEAD"] || !got["OK1"] || got["FLAKY"] {
		t.Errorf("SetDetails calls = %v, want DEAD and OK1 only", store.detailsSet)
	}
	if d := store.detailsGot["DEAD"]; d == nil || d.PropertyType != nil {
		t.Errorf("DEAD must be recorded with empty details, got %+v", d)
	}
}
