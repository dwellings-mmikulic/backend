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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/hls"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/workqueue"
	"github.com/dwellingtw/backend/internal/zillow"
	"github.com/dwellingtw/backend/internal/zipcode"
)

const testOwner = "box-1/nonce"

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

// statusServer answers every request with the given status and no image.
func statusServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// failAfter bounds every channel wait in the tests. It is never slept through:
// it only turns a hang into a failure when the code under test is broken.
const failAfter = 10 * time.Second

// recv receives from ch, failing the test instead of hanging.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(failAfter):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// send sends on ch, failing the test instead of hanging.
func send[T any](t *testing.T, ch chan<- T, v T, what string) {
	t.Helper()
	select {
	case ch <- v:
	case <-time.After(failAfter):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// --- log recorder ---

type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

// logRecorder is a slog.Handler that keeps every record, for the behaviours
// whose only observable effect is a log line (a pushed-back ZIP, the status
// line).
type logRecorder struct {
	mu      sync.Mutex
	records []logRecord
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logRecorder) WithGroup(string) slog.Handler            { return l }

func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, msg: r.Message, attrs: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Any()
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, rec)
	return nil
}

// find returns the records whose message contains substr.
func (l *logRecorder) find(substr string) []logRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []logRecord
	for _, r := range l.records {
		if strings.Contains(r.msg, substr) {
			out = append(out, r)
		}
	}
	return out
}

// --- event log ---

// eventLog records calls across several fakes in one sequence, for the tests
// that pin an order (SetVideoHLS before SetVideoReady).
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (e *eventLog) add(ev string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *eventLog) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

// --- zillow ---

type searchCall struct {
	location  string
	startPage int
	maxPages  int
}

// fakeZillow pages like the real client: the permit is asked before every
// request, a search ends with the request that comes back empty (so a ZIP with
// n pages of listings costs n+1 requests, and an empty ZIP one), a denial
// stops early with NextPage and no error, and a failure returns the pages
// fetched so far together with the error.
type fakeZillow struct {
	mu sync.Mutex

	// props is page 1 of every location that has no entry in pages.
	props []property.Property
	pages map[string][][]property.Property
	// pageErr[location][page] is returned instead of that page, after the
	// request for it was paid.
	pageErr map[string]map[int]error
	// beforePage runs before each page's context check, afterSearch just
	// before a search returns successfully: hooks for shutdown tests.
	beforePage  func(location string, page int)
	afterSearch func(location string)
	searches    []searchCall

	detailsErr   map[string]error
	detailsCalls []string
	// beforeDetails runs before each details call's context check,
	// afterDetails just before a record is returned.
	beforeDetails func(zpid string)
	afterDetails  func(zpid string)

	// usage is returned by Usage; usageErr takes precedence. With neither,
	// Usage reports "ok" with ample quota.
	usage      *zillow.Usage
	usageErr   error
	usageCalls int
}

func (f *fakeZillow) SearchPages(ctx context.Context, c config.SearchCriteria, startPage int, permit zillow.Permit) (zillow.SearchResult, error) {
	f.mu.Lock()
	f.searches = append(f.searches, searchCall{c.Location, startPage, c.MaxPages})
	pages, ok := f.pages[c.Location]
	if !ok && f.props != nil {
		pages = [][]property.Property{f.props}
	}
	pageErr := f.pageErr[c.Location]
	beforePage, afterSearch := f.beforePage, f.afterSearch
	f.mu.Unlock()

	if startPage <= 0 {
		startPage = 1
	}
	var res zillow.SearchResult
	for page := startPage; ; page++ {
		if beforePage != nil {
			beforePage(c.Location, page)
		}
		if err := ctx.Err(); err != nil {
			res.NextPage = page
			return res, fmt.Errorf("search page %d: %w", page, err)
		}
		if permit != nil && !permit(ctx) {
			res.NextPage = page
			if err := ctx.Err(); err != nil {
				return res, fmt.Errorf("search page %d: %w", page, err)
			}
			return res, nil
		}
		res.Requests++
		if err := pageErr[page]; err != nil {
			res.NextPage = page
			return res, fmt.Errorf("search page %d: %w", page, err)
		}
		if page > len(pages) {
			break // the empty page that ends every search
		}
		res.Properties = append(res.Properties, pages[page-1]...)
	}
	if afterSearch != nil {
		afterSearch(c.Location)
	}
	return res, nil
}

func (f *fakeZillow) PropertyDetails(ctx context.Context, zpid string, permit zillow.Permit) (*property.Details, []byte, error) {
	f.mu.Lock()
	before := f.beforeDetails
	f.mu.Unlock()
	if before != nil {
		before(zpid)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if permit != nil && !permit(ctx) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		return nil, nil, zillow.ErrBudgetExhausted
	}
	f.mu.Lock()
	f.detailsCalls = append(f.detailsCalls, zpid)
	err := f.detailsErr[zpid]
	after := f.afterDetails
	f.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	if after != nil {
		after(zpid)
	}
	pt := "SINGLE_FAMILY"
	return &property.Details{PropertyType: &pt}, []byte(`{"status":"OK"}`), nil
}

func (f *fakeZillow) Usage(ctx context.Context) (*zillow.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usageCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.usageErr != nil {
		return nil, f.usageErr
	}
	if f.usage != nil {
		return f.usage, nil
	}
	return usageWith("ok", 10000), nil
}

func usageWith(status string, remaining int) *zillow.Usage {
	u := &zillow.Usage{Status: status}
	u.Quotas = []zillow.QuotaMetric{{Name: "Requests", Limit: 10000, Used: 10000 - remaining, Remaining: remaining}}
	return u
}

func (f *fakeZillow) searchCalls() []searchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]searchCall(nil), f.searches...)
}

func (f *fakeZillow) searchedLocations() []string {
	var out []string
	for _, c := range f.searchCalls() {
		out = append(out, c.location)
	}
	return out
}

func (f *fakeZillow) usageCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usageCalls
}

func (f *fakeZillow) detailsCalled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.detailsCalls...)
}

func (f *fakeZillow) setUsage(u *zillow.Usage, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage, f.usageErr = u, err
}

// --- uploader ---

// fakeUploader records peak concurrency and echoes the path into the URL so
// ordering can be asserted.
type fakeUploader struct {
	cur, peak atomic.Int64

	mu       sync.Mutex
	uploaded []string // every path stored, in completion order
	// failPrefix makes every upload under that path prefix fail.
	failPrefix string
	// onUpload runs before an upload's context check.
	onUpload func(path string)
}

func (u *fakeUploader) Upload(ctx context.Context, path string, content io.Reader, _ string) (string, error) {
	n := u.cur.Add(1)
	for {
		p := u.peak.Load()
		if n <= p || u.peak.CompareAndSwap(p, n) {
			break
		}
	}
	defer u.cur.Add(-1)

	u.mu.Lock()
	failPrefix, onUpload := u.failPrefix, u.onUpload
	u.mu.Unlock()
	if onUpload != nil {
		onUpload(path)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if failPrefix != "" && strings.HasPrefix(path, failPrefix) {
		return "", fmt.Errorf("bunny: status 503 for %s", path)
	}
	_, _ = io.Copy(io.Discard, content)

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

func (u *fakeUploader) pathsWithPrefix(prefix string) []string {
	var out []string
	for _, p := range u.paths() {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	return out
}

// --- store ---

// fakeStore is property.Repository in memory. Like every fake here it refuses
// a dead context, which is what makes the bookkeeping-context tests mean
// something.
type fakeStore struct {
	mu     sync.Mutex
	events *eventLog

	existing map[string]bool
	// onExists runs inside Exists, before its context check; it may block.
	onExists func(ctx context.Context)
	// needsVideo is what NeedsVideo / VideoStates report for a stored listing.
	needsVideo map[string]bool
	// storedStatus/storedHash are what Upsert reads back into the property
	// (default: pending, no hash).
	storedStatus map[string]property.VideoStatus
	storedHash   map[string]string

	upserted       []property.Property
	upsertErr      error
	readyZPIDs     []string
	setReadyErr    error
	afterReady     func() // runs once SetVideoReady has recorded the video
	failedZPIDs    []string
	videoStatesErr error
	videoStatesN   int

	// missingDetails are the rows ClaimMissingDetails offers, oldest first.
	missingDetails  []string
	detailsTaken    map[string]bool // leased, stored or failed: not offered again
	claimDetailsErr error
	claimLimits     []int
	detailsSet      []string // zpids passed to SetDetails, in order
	detailsGot      map[string]*property.Details
	setDetailsErr   map[string]error
	detailsReleased [][]string
	detailsFailed   []string
}

func (s *fakeStore) Exists(ctx context.Context, zpid string) (bool, error) {
	s.mu.Lock()
	onExists := s.onExists
	s.mu.Unlock()
	if onExists != nil {
		onExists(ctx)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.existing[zpid], nil
}

func (s *fakeStore) NeedsVideo(ctx context.Context, zpid string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsVideo[zpid], nil
}

func (s *fakeStore) VideoStates(ctx context.Context, zpids []string) (map[string]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.videoStatesN++
	if s.videoStatesErr != nil {
		return nil, s.videoStatesErr
	}
	out := map[string]bool{}
	for _, z := range zpids {
		if s.existing[z] {
			out[z] = s.needsVideo[z]
		}
	}
	return out, nil
}

func (s *fakeStore) Upsert(ctx context.Context, p *property.Property) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upsertErr != nil {
		return s.upsertErr
	}
	cp := *p
	cp.ImageURLs = append([]string(nil), p.ImageURLs...)
	s.upserted = append(s.upserted, cp)
	s.events.add("upsert:" + p.ZPID)
	p.VideoStatus = property.VideoPending
	if st, ok := s.storedStatus[p.ZPID]; ok {
		p.VideoStatus = st
	}
	p.VideoContentHash = s.storedHash[p.ZPID]
	return nil
}

func (s *fakeStore) SetVideoReady(ctx context.Context, zpid, _, _ string, _ int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setReadyErr != nil {
		return s.setReadyErr
	}
	s.readyZPIDs = append(s.readyZPIDs, zpid)
	s.events.add("ready:" + zpid)
	if s.afterReady != nil {
		s.afterReady()
	}
	return nil
}

func (s *fakeStore) SetVideoFailed(ctx context.Context, zpid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failedZPIDs = append(s.failedZPIDs, zpid)
	s.events.add("video-failed:" + zpid)
	return nil
}

func (s *fakeStore) ClaimMissingDetails(ctx context.Context, limit int, _ time.Duration) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimLimits = append(s.claimLimits, limit)
	if s.claimDetailsErr != nil {
		return nil, s.claimDetailsErr
	}
	if s.detailsTaken == nil {
		s.detailsTaken = map[string]bool{}
	}
	var out []string
	for _, z := range s.missingDetails {
		if len(out) == limit {
			break
		}
		if !s.detailsTaken[z] {
			s.detailsTaken[z] = true
			out = append(out, z)
		}
	}
	return out, nil
}

func (s *fakeStore) ReleaseDetails(ctx context.Context, zpids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detailsReleased = append(s.detailsReleased, append([]string(nil), zpids...))
	for _, z := range zpids {
		delete(s.detailsTaken, z)
	}
	return nil
}

// FailDetails keeps the lease, as the real one does: the row is not offered
// again within a test.
func (s *fakeStore) FailDetails(ctx context.Context, zpid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detailsFailed = append(s.detailsFailed, zpid)
	return nil
}

func (s *fakeStore) SetDetails(ctx context.Context, zpid string, d *property.Details, _ []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.setDetailsErr[zpid]; err != nil {
		return err
	}
	s.detailsSet = append(s.detailsSet, zpid)
	if s.detailsGot == nil {
		s.detailsGot = map[string]*property.Details{}
	}
	s.detailsGot[zpid] = d
	return nil
}

func (s *fakeStore) upsertedProps() []property.Property {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]property.Property(nil), s.upserted...)
}

func (s *fakeStore) upsertCount() int { return len(s.upsertedProps()) }

func (s *fakeStore) ready() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.readyZPIDs...)
}

func (s *fakeStore) videoFailed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.failedZPIDs...)
}

func (s *fakeStore) videoStatesCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.videoStatesN
}

func (s *fakeStore) detailsState() (set []string, released [][]string, failed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.detailsSet...),
		append([][]string(nil), s.detailsReleased...),
		append([]string(nil), s.detailsFailed...)
}

// --- renderer / HLS ---

// fakeRenderer writes a stand-in MP4. hook, when set, runs first and may
// block, fail or panic.
type fakeRenderer struct {
	peak, cur, calls atomic.Int64

	mu   sync.Mutex
	hook func(ctx context.Context, p *property.Property) error
}

func (r *fakeRenderer) Render(ctx context.Context, p *property.Property, imgs []string, _, outPath string) (int, error) {
	r.calls.Add(1)
	n := r.cur.Add(1)
	for {
		pk := r.peak.Load()
		if n <= pk || r.peak.CompareAndSwap(pk, n) {
			break
		}
	}
	defer r.cur.Add(-1)

	r.mu.Lock()
	hook := r.hook
	r.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, p); err != nil {
			return 0, err
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := os.WriteFile(outPath, []byte("video"), 0o644); err != nil {
		return 0, err
	}
	return len(imgs) * 2, nil
}

func (r *fakeRenderer) setHook(h func(ctx context.Context, p *property.Property) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hook = h
}

type fakeSegmenter struct {
	calls atomic.Int64
	// hook, when set, runs first; its error is the segmentation failure.
	hook func(ctx context.Context) error
}

func (f *fakeSegmenter) Segment(ctx context.Context, _ string, outDir string) (hls.Clip, error) {
	f.calls.Add(1)
	if f.hook != nil {
		if err := f.hook(ctx); err != nil {
			return hls.Clip{}, err
		}
	}
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(filepath.Join(outDir, hls.SegmentName(i)), []byte("ts"), 0o644); err != nil {
			return hls.Clip{}, err
		}
	}
	if err := os.WriteFile(filepath.Join(outDir, hls.IndexName), []byte("#EXTM3U\n"), 0o644); err != nil {
		return hls.Clip{}, err
	}
	return hls.Clip{SegmentMS: []int{3000, 2000}, TotalMS: 5000}, nil
}

type fakeHLSRecorder struct {
	mu       sync.Mutex
	events   *eventLog
	recorded map[string]string // zpid → base URL
}

func (r *fakeHLSRecorder) SetVideoHLS(ctx context.Context, zpid, _ string, baseURL string, _ hls.Clip) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recorded == nil {
		r.recorded = map[string]string{}
	}
	r.recorded[zpid] = baseURL
	r.events.add("hls:" + zpid)
	return nil
}

func (r *fakeHLSRecorder) base(zpid string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recorded[zpid]
}

func (r *fakeHLSRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.recorded)
}

// --- zips ---

type zipRow struct {
	marked       bool // stamped searched: at the back of the rotation
	listingCount int
	failures     int
	resumePage   int
	hiddenUntil  time.Time // Defer/Fail backoff, or a live claim's lease
	claimed      bool
	token        time.Time
}

type zipTransition struct {
	kind       string // "mark" | "defer" | "fail" | "release"
	zip        string
	until      time.Time
	resumePage int
	count      int
	pushedBack bool
}

// fakeZips is the ZIP rotation in memory: claims with fencing tokens, resume
// pages, backoffs on the fake clock, and the five-failure push-back. A ZIP
// that was stamped searched is not offered again (one pass of the rotation),
// so RunOnce's discovery terminates.
type fakeZips struct {
	mu    sync.Mutex
	now   func() time.Time
	order []string
	rows  map[string]*zipRow
	seq   int

	claimErr    error
	claims      int
	transitions []zipTransition
}

func newFakeZips(now func() time.Time, zips ...string) *fakeZips {
	f := &fakeZips{now: now, order: zips, rows: map[string]*zipRow{}}
	for _, z := range zips {
		f.rows[z] = &zipRow{}
	}
	return f
}

func (f *fakeZips) Claim(ctx context.Context, _ string, lease time.Duration) (*zipcode.Claim, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	now := f.now()
	for _, z := range f.order {
		r := f.rows[z]
		if r.marked || r.hiddenUntil.After(now) {
			continue
		}
		f.seq++
		r.claimed = true
		r.hiddenUntil = now.Add(lease)
		r.token = r.hiddenUntil.Add(time.Duration(f.seq)) // unique per claim
		return &zipcode.Claim{Zip: z, ResumePage: r.resumePage, Token: r.token}, nil
	}
	return nil, nil
}

// complete applies fn when c is the row's live claim, and gives the claim up.
func (f *fakeZips) complete(ctx context.Context, c zipcode.Claim, fn func(r *zipRow) zipTransition) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[c.Zip]
	if r == nil || !r.claimed || !r.token.Equal(c.Token) {
		return zipcode.ErrLeaseLost
	}
	r.claimed = false
	r.hiddenUntil = time.Time{}
	tr := fn(r)
	tr.zip = c.Zip
	f.transitions = append(f.transitions, tr)
	return nil
}

func (f *fakeZips) MarkSearched(ctx context.Context, c zipcode.Claim, _ string, listingCount int) error {
	return f.complete(ctx, c, func(r *zipRow) zipTransition {
		r.marked, r.listingCount, r.failures, r.resumePage = true, listingCount, 0, 0
		return zipTransition{kind: "mark", count: listingCount}
	})
}

func (f *fakeZips) Defer(ctx context.Context, c zipcode.Claim, _ string, until time.Time, resumePage int) error {
	return f.complete(ctx, c, func(r *zipRow) zipTransition {
		r.hiddenUntil, r.resumePage = until, resumePage
		return zipTransition{kind: "defer", until: until, resumePage: resumePage}
	})
}

func (f *fakeZips) Fail(ctx context.Context, c zipcode.Claim, _ string, until time.Time, resumePage int) (bool, error) {
	var pushedBack bool
	err := f.complete(ctx, c, func(r *zipRow) zipTransition {
		r.failures++
		if r.failures >= zipcode.MaxFailures {
			pushedBack = true
			r.marked, r.failures, r.resumePage = true, 0, 0
		} else {
			r.hiddenUntil, r.resumePage = until, resumePage
		}
		return zipTransition{kind: "fail", until: until, resumePage: resumePage, pushedBack: pushedBack}
	})
	return pushedBack, err
}

func (f *fakeZips) Release(ctx context.Context, c zipcode.Claim, _ string) error {
	return f.complete(ctx, c, func(*zipRow) zipTransition {
		return zipTransition{kind: "release"}
	})
}

func (f *fakeZips) claimCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims
}

func (f *fakeZips) history() []zipTransition {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]zipTransition(nil), f.transitions...)
}

// markedZips maps every stamped ZIP to the listing count it was stamped with.
func (f *fakeZips) markedZips() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for z, r := range f.rows {
		if r.marked {
			out[z] = r.listingCount
		}
	}
	return out
}

// setFailures presets a ZIP's consecutive-failure count.
func (f *fakeZips) setFailures(zip string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[zip].failures = n
}

// setResume presets the page a ZIP's next claim resumes at.
func (f *fakeZips) setResume(zip string, page int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[zip].resumePage = page
}

// steal re-claims a held ZIP for somebody else, as happens when a lease runs
// out under a paused process: the old holder's token no longer matches.
func (f *fakeZips) steal(zip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.rows[zip].token = f.rows[zip].token.Add(time.Duration(f.seq) * time.Hour)
}

// --- queue ---

type queueRow struct {
	zpid         string
	payload      []byte
	sourceZip    string
	attempts     int
	availableAt  time.Time
	claimedBy    string
	claimedUntil time.Time
	token        time.Time
	lastError    string
	seq          int
}

type queueTransition struct {
	kind       string // "complete" | "fail" | "release"
	zpid       string
	attempts   int // the item's Attempts as the caller held it
	msg        string
	retryAfter time.Duration
	refund     bool
	delay      time.Duration
}

// fakeQueue is listing_queue in memory, with the rules the dispatcher and the
// breaker depend on: attempts are spent at claim time and refunded by Release
// and Fail(refund), available_at hides a row until the fake clock reaches it,
// dead rows (MaxAttempts) are never offered, every claim has its own token and
// a completion with a stale one is ErrLeaseLost.
type fakeQueue struct {
	mu   sync.Mutex
	now  func() time.Time
	rows map[string]*queueRow
	seq  int

	// enqueueErrs are returned by successive Enqueue calls (nil = succeed).
	// A call given an error stores nothing, unless the error wraps
	// workqueue.ErrUnstorable: then every item is stored and the count comes
	// back together with the error, as the real queue does.
	enqueueErrs  []error
	enqueueCalls int
	claimErr     error
	claimLimits  []int
	depthErr     error
	depthCalls   int
	transitions  []queueTransition
}

func newFakeQueue(now func() time.Time) *fakeQueue {
	return &fakeQueue{now: now, rows: map[string]*queueRow{}}
}

func (q *fakeQueue) Enqueue(ctx context.Context, items []workqueue.NewItem) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueueCalls++
	var injected error
	if len(q.enqueueErrs) > 0 {
		injected, q.enqueueErrs = q.enqueueErrs[0], q.enqueueErrs[1:]
	}
	if injected != nil && !errors.Is(injected, workqueue.ErrUnstorable) {
		return 0, injected
	}
	n := 0
	now := q.now()
	for _, it := range items {
		if it.ZPID == "" {
			continue
		}
		if row, ok := q.rows[it.ZPID]; ok {
			dead := row.attempts >= workqueue.MaxAttempts && !q.liveClaim(row, now)
			if !dead {
				continue // a live row is left alone
			}
		}
		q.seq++
		q.rows[it.ZPID] = &queueRow{
			zpid: it.ZPID, payload: append([]byte(nil), it.Payload...),
			sourceZip: it.SourceZip, availableAt: now, seq: q.seq,
		}
		n++
	}
	return n, injected
}

func (q *fakeQueue) liveClaim(r *queueRow, now time.Time) bool {
	return r.claimedBy != "" && r.claimedUntil.After(now)
}

func (q *fakeQueue) claimable(r *queueRow, now time.Time) bool {
	return r.attempts < workqueue.MaxAttempts && !r.availableAt.After(now) && !q.liveClaim(r, now)
}

func (q *fakeQueue) Claim(ctx context.Context, owner string, limit int, lease time.Duration) ([]workqueue.Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.claimLimits = append(q.claimLimits, limit)
	if q.claimErr != nil {
		return nil, q.claimErr
	}
	now := q.now()
	var ready []*queueRow
	for _, r := range q.rows {
		if q.claimable(r, now) {
			ready = append(ready, r)
		}
	}
	sort.Slice(ready, func(i, j int) bool {
		if !ready[i].availableAt.Equal(ready[j].availableAt) {
			return ready[i].availableAt.Before(ready[j].availableAt)
		}
		return ready[i].seq < ready[j].seq
	})
	if len(ready) > limit {
		ready = ready[:limit]
	}
	var out []workqueue.Item
	for _, r := range ready {
		q.seq++
		r.attempts++
		r.claimedBy = owner
		r.claimedUntil = now.Add(lease)
		r.token = r.claimedUntil.Add(time.Duration(q.seq))
		out = append(out, workqueue.Item{
			ZPID: r.zpid, Payload: append([]byte(nil), r.payload...),
			Attempts: r.attempts, Token: r.token,
		})
	}
	return out, nil
}

// held returns the row when it is still claimed by owner under its token.
func (q *fakeQueue) held(it workqueue.Item, owner string) *queueRow {
	r := q.rows[it.ZPID]
	if r == nil || r.claimedBy != owner || !r.token.Equal(it.Token) {
		return nil
	}
	return r
}

func (q *fakeQueue) Complete(ctx context.Context, it workqueue.Item, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.held(it, owner) == nil {
		return workqueue.ErrLeaseLost
	}
	delete(q.rows, it.ZPID)
	q.transitions = append(q.transitions, queueTransition{kind: "complete", zpid: it.ZPID, attempts: it.Attempts})
	return nil
}

func (q *fakeQueue) Fail(ctx context.Context, it workqueue.Item, owner, errMsg string, retryAfter time.Duration, refundAttempt bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	r := q.held(it, owner)
	if r == nil {
		return workqueue.ErrLeaseLost
	}
	r.claimedBy, r.lastError = "", errMsg
	r.availableAt = q.now().Add(retryAfter)
	if refundAttempt {
		r.attempts--
	}
	q.transitions = append(q.transitions, queueTransition{
		kind: "fail", zpid: it.ZPID, attempts: it.Attempts,
		msg: errMsg, retryAfter: retryAfter, refund: refundAttempt,
	})
	return nil
}

func (q *fakeQueue) Release(ctx context.Context, it workqueue.Item, owner string, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	r := q.held(it, owner)
	if r == nil {
		return workqueue.ErrLeaseLost
	}
	r.claimedBy = ""
	r.attempts--
	r.availableAt = q.now().Add(delay)
	q.transitions = append(q.transitions, queueTransition{kind: "release", zpid: it.ZPID, attempts: it.Attempts, delay: delay})
	return nil
}

func (q *fakeQueue) Depth(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.depthCalls++
	if q.depthErr != nil {
		return 0, q.depthErr
	}
	n, now := 0, q.now()
	for _, r := range q.rows {
		if q.claimable(r, now) {
			n++
		}
	}
	return n, nil
}

// steal re-claims a held item for another owner, as happens when a lease runs
// out under a paused process.
func (q *fakeQueue) steal(zpid string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	r := q.rows[zpid]
	r.claimedBy = "other-box/nonce"
	r.token = r.token.Add(time.Duration(q.seq) * time.Hour)
}

// put enqueues one listing the way discovery (or backfill-videos) would.
func (q *fakeQueue) put(t *testing.T, p property.Property, revisit bool) {
	t.Helper()
	payload, err := EncodeListing(&p, revisit)
	if err != nil {
		t.Fatal(err)
	}
	q.putRaw(t, p.ZPID, payload)
}

func (q *fakeQueue) putRaw(t *testing.T, zpid string, payload []byte) {
	t.Helper()
	n, err := q.Enqueue(context.Background(), []workqueue.NewItem{{ZPID: zpid, Payload: payload, SourceZip: "00000"}})
	if err != nil || n != 1 {
		t.Fatalf("enqueue %s: n=%d err=%v", zpid, n, err)
	}
}

func (q *fakeQueue) history() []queueTransition {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]queueTransition(nil), q.transitions...)
}

func (q *fakeQueue) historyOf(kind string) []queueTransition {
	var out []queueTransition
	for _, tr := range q.history() {
		if tr.kind == kind {
			out = append(out, tr)
		}
	}
	return out
}

func (q *fakeQueue) limits() []int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]int(nil), q.claimLimits...)
}

// queued returns the zpids in the queue (any state), sorted.
func (q *fakeQueue) queued() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []string
	for z := range q.rows {
		out = append(out, z)
	}
	sort.Strings(out)
	return out
}

func (q *fakeQueue) row(zpid string) (queueRow, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	r, ok := q.rows[zpid]
	if !ok {
		return queueRow{}, false
	}
	return *r, true
}

func (q *fakeQueue) calls() (enqueues, depths int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.enqueueCalls, q.depthCalls
}

// --- ledger / windows ---

type ledgerKey struct {
	window time.Time
	kind   string
}

// fakeLedger is a counter with a limit per (window, kind), like api_budget.
type fakeLedger struct {
	mu         sync.Mutex
	spent      map[ledgerKey]int
	reserveErr error
	spentErr   error
	limitsSeen map[string][]int // kind → every limit TryReserve was given
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{spent: map[ledgerKey]int{}, limitsSeen: map[string][]int{}}
}

func (l *fakeLedger) TryReserve(ctx context.Context, window time.Time, kind string, limit int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limitsSeen[kind] = append(l.limitsSeen[kind], limit)
	if l.reserveErr != nil {
		return false, l.reserveErr
	}
	k := ledgerKey{window.UTC(), kind}
	if limit <= 0 || l.spent[k] >= limit {
		return false, nil
	}
	l.spent[k]++
	return true, nil
}

func (l *fakeLedger) Spent(ctx context.Context, window time.Time, kind string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.spentErr != nil {
		return 0, l.spentErr
	}
	return l.spent[ledgerKey{window.UTC(), kind}], nil
}

func (l *fakeLedger) set(window time.Time, kind string, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spent[ledgerKey{window.UTC(), kind}] = n
}

func (l *fakeLedger) get(window time.Time, kind string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.spent[ledgerKey{window.UTC(), kind}]
}

// fakeWindows cuts time into fixed windows anchored at base, so advancing the
// fake clock past the end of one opens the next, with a fresh budget.
type fakeWindows struct {
	base   time.Time
	length time.Duration
}

func (w *fakeWindows) Current(now time.Time) (start, next time.Time) {
	n := now.Sub(w.base) / w.length
	start = w.base.Add(n * w.length)
	return start, start.Add(w.length)
}

// --- harness ---

type harness struct {
	cfg     *config.Config
	clock   *fakeClock
	zillow  *fakeZillow
	bunny   *fakeUploader
	store   *fakeStore
	zips    *fakeZips
	queue   *fakeQueue
	ledger  *fakeLedger
	windows *fakeWindows
	render  *fakeRenderer
	logs    *logRecorder
	events  *eventLog
	s       *Scheduler

	sleepMu sync.Mutex
	sleeps  []time.Duration
}

const testWindow = 12 * time.Hour

func baseConfig() *config.Config {
	return &config.Config{
		ImagesEnabled:     true,
		Video:             config.VideoConfig{Enabled: true, SecondsPerPhoto: 2},
		Concurrency:       config.ConcurrencyConfig{Listings: 4, Images: 4},
		APIBudgetPerCycle: 1000,
	}
}

// leanConfig is for the tests that never touch media.
func leanConfig(budget, details int) *config.Config {
	return &config.Config{
		APIBudgetPerCycle: budget,
		DetailsPerCycle:   details,
		Concurrency:       config.ConcurrencyConfig{Listings: 1, Images: 1},
	}
}

// newHarness wires a Scheduler to in-memory fakes that share one fake clock.
// The injected sleep records the wait and returns at once.
func newHarness(t *testing.T, cfg *config.Config, zips ...string) *harness {
	t.Helper()
	clock := newFakeClock()
	events := &eventLog{}
	h := &harness{
		cfg:     cfg,
		clock:   clock,
		zillow:  &fakeZillow{},
		bunny:   &fakeUploader{},
		store:   &fakeStore{events: events},
		zips:    newFakeZips(clock.now, zips...),
		queue:   newFakeQueue(clock.now),
		ledger:  newFakeLedger(),
		windows: &fakeWindows{base: clock.now(), length: testWindow},
		render:  &fakeRenderer{},
		logs:    &logRecorder{},
		events:  events,
	}
	h.s = New(cfg, Deps{
		Zillow: h.zillow, Bunny: h.bunny, Repo: h.store, Zips: h.zips,
		Queue: h.queue, Ledger: h.ledger, Windows: h.windows, Render: h.render,
	}, testOwner, slog.New(h.logs))
	h.s.now = clock.now
	h.s.sleep = func(_ context.Context, d time.Duration) {
		h.sleepMu.Lock()
		defer h.sleepMu.Unlock()
		h.sleeps = append(h.sleeps, d)
	}
	return h
}

// windowStart is the start of the budget window the fake clock is in.
func (h *harness) windowStart() time.Time {
	start, _ := h.windows.Current(h.clock.now())
	return start
}

func (h *harness) nextWindow() time.Time {
	_, next := h.windows.Current(h.clock.now())
	return next
}

// closeBreaker puts the breaker in the closed state, for tests that are not
// about the boot probe.
func (h *harness) closeBreaker() { h.s.breaker.success() }

func listing(zpid string, imageURLs ...string) property.Property {
	return property.Property{
		ZPID: zpid, Address: "1 Main St", City: "Punta Gorda", State: "FL", Zip: "33950",
		DetailURL: "https://www.zillow.com/homedetails/" + zpid + "_zpid/",
		SalePrice: 350000, Bedrooms: 3, Bathrooms: 2.5,
		ImageURLs: imageURLs,
	}
}
