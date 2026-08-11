package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// jpegBytes returns a real, decodable JPEG image.
func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// jpegServer serves a real JPEG for any path.
func jpegServer(t *testing.T) *httptest.Server {
	t.Helper()
	data := jpegBytes(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- fakes ---

type fakeSearch struct {
	props []property.Property
	// byLocation, when set, returns per-location props and records each queried
	// location in order. Takes precedence over props.
	byLocation map[string][]property.Property
	queried    []string
	// maxPages records each call's criteria.MaxPages, in call order — lets
	// tests assert the per-search page cap tracked the remaining budget.
	maxPages []int
	// searchErr, when set for a location, is returned by SearchPages for it.
	searchErr map[string]error
	// pagesFor, when set for a location, is the page count SearchPages reports
	// for it (default 1).
	pagesFor map[string]int
	// detailsErr, when set for a zpid, is returned by PropertyDetails.
	detailsErr map[string]error
	// usage is returned by Usage; usageErr takes precedence. A nil usage with
	// nil usageErr returns an "ok" report with ample remaining quota.
	usage    *zillow.Usage
	usageErr error
	// blockSearch, when non-nil, is closed-waited inside SearchPages after
	// signalling searchEntered — for overlap-guard tests.
	blockSearch   chan struct{}
	searchEntered chan struct{}
	mu            sync.Mutex
}

func (f *fakeSearch) SearchPages(_ context.Context, c config.SearchCriteria) ([]property.Property, int, error) {
	if f.searchEntered != nil {
		f.searchEntered <- struct{}{}
	}
	if f.blockSearch != nil {
		<-f.blockSearch
	}
	f.mu.Lock()
	f.queried = append(f.queried, c.Location)
	f.maxPages = append(f.maxPages, c.MaxPages)
	f.mu.Unlock()

	pages := 1
	if p, ok := f.pagesFor[c.Location]; ok {
		pages = p
	}
	if err := f.searchErr[c.Location]; err != nil {
		return nil, pages, err
	}
	if f.byLocation != nil {
		return f.byLocation[c.Location], pages, nil
	}
	return f.props, pages, nil
}

func (f *fakeSearch) Usage(_ context.Context) (*zillow.Usage, error) {
	if f.usageErr != nil {
		return nil, f.usageErr
	}
	if f.usage != nil {
		return f.usage, nil
	}
	u := &zillow.Usage{Status: "ok"}
	u.Quotas = []zillow.QuotaMetric{{Name: "Requests", Limit: 10000, Used: 0, Remaining: 10000}}
	return u, nil
}

func (f *fakeSearch) queriedLocations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queried...)
}

func (f *fakeSearch) maxPagesRecorded() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.maxPages...)
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

	mu       sync.Mutex
	uploaded []string // every path passed to Upload, in completion order
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

	u.mu.Lock()
	u.uploaded = append(u.uploaded, path)
	u.mu.Unlock()
	return "https://cdn.example/" + path, nil
}

// paths returns a copy of the recorded upload paths.
func (u *fakeUploader) paths() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.uploaded...)
}

type fakeStore struct {
	upserts, ready, failed atomic.Int64
	existing               map[string]bool
	// needsVideo is what NeedsVideo reports for an already-stored listing.
	needsVideo map[string]bool
	// missingDetails is what ListZPIDsMissingDetails returns (up to limit).
	missingDetails []string
	mu             sync.Mutex
	detailsSet     []string // zpids passed to SetDetails, in order
	detailsGot     map[string]*property.Details
}

func (s *fakeStore) Exists(_ context.Context, zpid string) (bool, error) {
	return s.existing[zpid], nil
}

func (s *fakeStore) NeedsVideo(_ context.Context, zpid string) (bool, error) {
	return s.needsVideo[zpid], nil
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

// fakeZips serves a fixed queue in order, skipping already-marked ZIPs —
// mirroring the real rotation query, where marking pushes a ZIP to the back.
type fakeZips struct {
	mu     sync.Mutex
	queue  []string
	marked map[string]int // zip → listing count recorded by MarkSearched
}

func (f *fakeZips) NextBatch(_ context.Context, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, z := range f.queue {
		if _, done := f.marked[z]; done {
			continue
		}
		out = append(out, z)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeZips) MarkSearched(_ context.Context, zip string, listingCount int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.marked == nil {
		f.marked = map[string]int{}
	}
	f.marked[zip] = listingCount
	return nil
}

func (f *fakeZips) markedZips() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for k, v := range f.marked {
		out[k] = v
	}
	return out
}

func baseConfig() *config.Config {
	return &config.Config{
		ImagesEnabled:     true,
		Video:             config.VideoConfig{Enabled: true, SecondsPerPhoto: 2},
		Concurrency:       config.ConcurrencyConfig{Listings: 4, Images: 4},
		APIBudgetPerCycle: 1000,
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
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(baseConfig(), &fakeSearch{}, &fakeUploader{}, &fakeStore{}, zips, &fakeRenderer{}, testLogger())

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
	imgSrv := jpegServer(t)

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
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(baseConfig(), &fakeSearch{props: props}, &fakeUploader{}, store, zips, render, testLogger())

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
	imgSrv := jpegServer(t)

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
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(cfg, &fakeSearch{props: props}, &fakeUploader{}, store, zips, &fakeRenderer{}, testLogger())

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

// A stored listing whose render failed previously must be revisited so the
// video can be retried. Before this, SkipExisting returned before renderVideo
// ran, so a single failed render stranded that listing without a video for
// good — which is how 24 production listings ended up permanently videoless.
// The revisit renders only: it must not re-upload images or re-upsert the row.
func TestRunCycle_ExistingListingWithoutVideoIsReRendered(t *testing.T) {
	imgSrv := jpegServer(t)

	var props []property.Property
	for i := 0; i < 4; i++ {
		props = append(props, property.Property{
			ZPID:      fmt.Sprintf("ZP%d", i),
			ImageURLs: []string{imgSrv.URL + "/a.jpg"},
		})
	}

	cfg := baseConfig()
	cfg.SkipExisting = true
	store := &fakeStore{
		// All four are stored already.
		existing: map[string]bool{"ZP0": true, "ZP1": true, "ZP2": true, "ZP3": true},
		// ZP1 and ZP2 have no ready video — only those two get revisited.
		needsVideo: map[string]bool{"ZP1": true, "ZP2": true},
	}
	up := &fakeUploader{}
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(cfg, &fakeSearch{props: props}, up, store, zips, &fakeRenderer{}, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := store.ready.Load(); got != 2 {
		t.Errorf("video ready = %d, want 2 (ZP1 and ZP2 re-rendered)", got)
	}
	if got := store.upserts.Load(); got != 0 {
		t.Errorf("upserts = %d, want 0 — a video revisit must not rewrite the row", got)
	}
	// Only the rendered videos should be uploaded; no listing photos.
	for _, path := range up.paths() {
		if !strings.HasPrefix(path, "videos/") {
			t.Errorf("unexpected upload %q — a video revisit must not re-upload photos", path)
		}
	}
}

func TestRunCycle_RotatesUntilBudgetExhausted(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 3, // no details reserve → 3 search pages
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{byLocation: map[string][]property.Property{}} // every zip: 0 listings, 1 page
	zips := &fakeZips{queue: []string{"11111", "22222", "33333", "44444", "55555"}}
	store := &fakeStore{}
	s := New(cfg, search, &fakeUploader{}, store, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	marked := zips.markedZips()
	if len(marked) != 3 {
		t.Fatalf("marked %d zips, want 3 (budget): %v", len(marked), marked)
	}
	for _, z := range []string{"11111", "22222", "33333"} {
		if _, ok := marked[z]; !ok {
			t.Errorf("zip %s not marked; queue order should win", z)
		}
	}
}

func TestRunCycle_DetailsReserveShrinksSearchBudget(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 3,
		DetailsPerCycle:   2, // search budget = 3 - 2 = 1
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{byLocation: map[string][]property.Property{}}
	zips := &fakeZips{queue: []string{"11111", "22222"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(zips.markedZips()); got != 1 {
		t.Fatalf("marked %d zips, want 1 (search budget 1)", got)
	}
}

func TestRunCycle_FailedSearchNotMarkedButBudgetSpent(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 2,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{
		byLocation: map[string][]property.Property{},
		searchErr:  map[string]error{"11111": errors.New("boom")},
	}
	zips := &fakeZips{queue: []string{"11111", "22222"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	marked := zips.markedZips()
	if _, ok := marked["11111"]; ok {
		t.Error("failed zip 11111 must not be marked (retries next cycle)")
	}
	if _, ok := marked["22222"]; !ok {
		t.Error("zip 22222 should be searched with the remaining budget")
	}
}

// TestRunCycle_FailedZipAttemptedOnceThenSkipped covers the "tried" guard: a
// failed ZIP stays unmarked (genuinely retries next cycle) but must not be
// re-attempted within the SAME cycle, or a persistently failing ZIP at the
// rotation front would burn the whole budget retrying just it.
func TestRunCycle_FailedZipAttemptedOnceThenSkipped(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 5,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{
		byLocation: map[string][]property.Property{},
		searchErr:  map[string]error{"11111": errors.New("boom")},
	}
	zips := &fakeZips{queue: []string{"11111", "22222"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	var attempts int
	for _, z := range search.queriedLocations() {
		if z == "11111" {
			attempts++
		}
	}
	if attempts != 1 {
		t.Errorf("11111 queried %d times, want exactly 1 (must not retry within the cycle)", attempts)
	}

	marked := zips.markedZips()
	if _, ok := marked["11111"]; ok {
		t.Error("failed zip 11111 must not be marked")
	}
	if _, ok := marked["22222"]; !ok {
		t.Error("zip 22222 should still be marked")
	}
}

// TestRunCycle_MultiPageSearchChargesBudget covers multi-page budget
// accounting: a search reporting more than one page must deduct that many
// requests, and each search's MaxPages must reflect the budget remaining
// when it was issued.
func TestRunCycle_MultiPageSearchChargesBudget(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 3,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{
		byLocation: map[string][]property.Property{},
		pagesFor:   map[string]int{"11111": 2},
	}
	zips := &fakeZips{queue: []string{"11111", "22222", "33333"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	marked := zips.markedZips()
	if _, ok := marked["11111"]; !ok {
		t.Error("zip 11111 (2 pages) should be marked")
	}
	if _, ok := marked["22222"]; !ok {
		t.Error("zip 22222 (1 page) should be marked")
	}
	if _, ok := marked["33333"]; ok {
		t.Error("zip 33333 should never be queried — budget exhausted by 11111+22222 (2+1=3)")
	}

	want := []int{3, 1}
	got := search.maxPagesRecorded()
	if len(got) != len(want) {
		t.Fatalf("maxPages recorded = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("maxPages[%d] = %d, want %d (MaxPages must equal budget remaining at call time)", i, got[i], want[i])
		}
	}
}

// TestRunCycle_BudgetExhaustsMidBatch covers a batch of ZIPs where the first
// one alone spends the entire budget: the rest of that same batch must be
// left untouched, not queried.
func TestRunCycle_BudgetExhaustsMidBatch(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 3,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{
		byLocation: map[string][]property.Property{},
		pagesFor:   map[string]int{"11111": 3},
	}
	zips := &fakeZips{queue: []string{"11111", "22222", "33333"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := search.queriedLocations(); len(got) != 1 || got[0] != "11111" {
		t.Fatalf("queried = %v, want only [11111]", got)
	}
	marked := zips.markedZips()
	if _, ok := marked["11111"]; !ok {
		t.Error("zip 11111 should be marked")
	}
	if _, ok := marked["22222"]; ok {
		t.Error("zip 22222 must not be touched — budget exhausted by 11111 alone")
	}
	if _, ok := marked["33333"]; ok {
		t.Error("zip 33333 must not be touched — budget exhausted by 11111 alone")
	}
}

func TestRunCycle_MarksListingCount(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 10,
		SkipExisting:      true,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{byLocation: map[string][]property.Property{
		"33950": {{ZPID: "a"}, {ZPID: "b"}},
	}}
	store := &fakeStore{existing: map[string]bool{"a": true, "b": true}}
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(cfg, search, &fakeUploader{}, store, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := zips.markedZips()["33950"]; got != 2 {
		t.Errorf("last_listing_count = %d, want 2", got)
	}
}

func TestRunCycle_SkipsWhenQuotaExceeded(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 10,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{usage: &zillow.Usage{Status: "exceeded"}}
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := search.queriedLocations(); len(got) != 0 {
		t.Errorf("searched %v, want none when quota exceeded", got)
	}
}

func TestRunCycle_SkipsWhenRemainingBelowBudget(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 150,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	u := &zillow.Usage{Status: "ok"}
	u.Quotas = []zillow.QuotaMetric{{Name: "Requests", Limit: 10000, Used: 9900, Remaining: 100}}
	search := &fakeSearch{usage: u}
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := search.queriedLocations(); len(got) != 0 {
		t.Errorf("searched %v, want none when remaining < budget", got)
	}
}

func TestRunCycle_ProceedsWhenUsageCheckFails(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 10,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{
		byLocation: map[string][]property.Property{},
		usageErr:   errors.New("usage endpoint down"),
	}
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := zips.markedZips()["33950"]; !ok {
		t.Error("cycle should proceed (fail-open) when the usage check errors")
	}
}

func TestRunCycle_SkipsOverlappingCycle(t *testing.T) {
	cfg := &config.Config{
		APIBudgetPerCycle: 10,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
	search := &fakeSearch{
		byLocation:    map[string][]property.Property{},
		blockSearch:   make(chan struct{}),
		searchEntered: make(chan struct{}, 1),
	}
	zips := &fakeZips{queue: []string{"33950"}}
	s := New(cfg, search, &fakeUploader{}, &fakeStore{}, zips, nil, testLogger())

	done := make(chan struct{})
	go func() {
		_ = s.RunCycle(context.Background())
		close(done)
	}()
	<-search.searchEntered // first cycle is now mid-search

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(search.queriedLocations()); got != 0 {
		// queried is appended after the block, so at this point the first
		// cycle hasn't recorded its search yet; any entry means the second
		// cycle ran a search.
		t.Errorf("second cycle performed %d searches, want 0", got)
	}

	close(search.blockSearch)
	<-done
}

func TestDownloadPhotos_NormalizesWebPToJPEG(t *testing.T) {
	webp, err := os.ReadFile("../imaging/testdata/opaque.webp")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/webp")
		_, _ = w.Write(webp)
	}))
	defer srv.Close()

	zips := &fakeZips{queue: []string{"33950"}}
	s := New(baseConfig(), &fakeSearch{}, &fakeUploader{}, &fakeStore{}, zips, &fakeRenderer{}, testLogger())
	dir := t.TempDir()

	local := s.downloadPhotos(context.Background(), []string{srv.URL + "/photo.webp"}, dir)
	if len(local) != 1 {
		t.Fatalf("got %d local photos, want 1", len(local))
	}
	if ext := filepath.Ext(local[0]); ext != ".jpg" {
		t.Errorf("local file ext = %q, want .jpg (webp must be transcoded)", ext)
	}
	data, err := os.ReadFile(local[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Errorf("local file is not valid JPEG: %v", err)
	}
}

func TestDownloadPhotos_SkipsUndecodableData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("<html>error page pretending to be an image</html>"))
	}))
	defer srv.Close()

	zips := &fakeZips{queue: []string{"33950"}}
	s := New(baseConfig(), &fakeSearch{}, &fakeUploader{}, &fakeStore{}, zips, &fakeRenderer{}, testLogger())

	local := s.downloadPhotos(context.Background(), []string{srv.URL + "/broken.jpg"}, t.TempDir())
	if len(local) != 0 {
		t.Errorf("got %d local photos, want 0 (undecodable data must be skipped)", len(local))
	}
}

func TestRunCycle_EnrichesDetailsUpToCap(t *testing.T) {
	cfg := baseConfig()
	cfg.DetailsPerCycle = 2

	store := &fakeStore{missingDetails: []string{"Z1", "Z2", "Z3"}}
	zips := &fakeZips{} // no search work — isolate enrichment
	s := New(cfg, &fakeSearch{}, &fakeUploader{}, store, zips, &fakeRenderer{}, testLogger())

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
	cfg.DetailsPerCycle = 0

	store := &fakeStore{missingDetails: []string{"Z1"}}
	zips := &fakeZips{}
	s := New(cfg, &fakeSearch{}, &fakeUploader{}, store, zips, &fakeRenderer{}, testLogger())

	if err := s.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.detailsSet) != 0 {
		t.Errorf("enrichment ran with cap 0: %v", store.detailsSet)
	}
}

func TestRunCycle_EnrichmentErrorHandling(t *testing.T) {
	cfg := baseConfig()
	cfg.DetailsPerCycle = 10

	search := &fakeSearch{detailsErr: map[string]error{
		"DEAD":  zillow.ErrDetailsNotFound,  // definitive: mark fetched
		"FLAKY": errors.New("500 whatever"), // transient: leave for retry
	}}
	store := &fakeStore{missingDetails: []string{"DEAD", "FLAKY", "OK1"}}
	zips := &fakeZips{}
	s := New(cfg, search, &fakeUploader{}, store, zips, &fakeRenderer{}, testLogger())

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
