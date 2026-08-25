package propertymap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/locationiq"
	"github.com/dwellingtw/backend/internal/property"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeClient counts calls and returns canned results, optionally blocking
// StaticMap so a test can observe the deadline path.
type fakeClient struct {
	lat, lon      float64
	geocodeErr    error
	staticMapErr  error
	geocodeCalls  atomic.Int32
	staticCalls   atomic.Int32
	blockStaticOn chan struct{} // when non-nil, StaticMap waits on it
}

func (f *fakeClient) Geocode(_ context.Context, _ locationiq.Address) (float64, float64, error) {
	f.geocodeCalls.Add(1)
	if f.geocodeErr != nil {
		return 0, 0, f.geocodeErr
	}
	return f.lat, f.lon, nil
}

func (f *fakeClient) StaticMap(_ context.Context, _, _ float64, _ locationiq.Style) ([]byte, error) {
	f.staticCalls.Add(1)
	if f.blockStaticOn != nil {
		<-f.blockStaticOn
	}
	if f.staticMapErr != nil {
		return nil, f.staticMapErr
	}
	return []byte("PNG"), nil
}

type fakeUploader struct {
	mu       sync.Mutex
	gotPaths []string
	uploaded int
	err      error
}

func (f *fakeUploader) Upload(_ context.Context, path string, _ io.Reader, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploaded++
	f.gotPaths = append(f.gotPaths, path)
	if f.err != nil {
		return "", f.err
	}
	return "https://cdn.example/" + path, nil
}

type fakeStore struct {
	mu        sync.Mutex
	mapURLs   map[string]property.MapURLs
	coords    map[string][2]float64
	mapCalls  int
	coordCall int
}

func newFakeStore() *fakeStore {
	return &fakeStore{mapURLs: map[string]property.MapURLs{}, coords: map[string][2]float64{}}
}

func (f *fakeStore) SetMapImage(_ context.Context, zpid string, m property.MapURLs) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mapCalls++
	f.mapURLs[zpid] = m
	return nil
}

func (f *fakeStore) SetCoordinates(_ context.Context, zpid string, lat, lon float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.coordCall++
	f.coords[zpid] = [2]float64{lat, lon}
	return nil
}

func f64p(v float64) *float64 { return &v }

func sampleProp() *property.Property {
	return &property.Property{
		ZPID: "Z1", Address: "1234 Hilltop Drive",
		City: "Austin", State: "TX", Zip: "78746",
	}
}

func newTestService(c Client, u Uploader, s Store) *Service {
	svc := New(c, u, s, testLogger())
	svc.deadline = 2 * time.Second // generous: these fakes are fast
	return svc
}

func TestEnsure_ReturnsExistingURLWithoutAPICalls(t *testing.T) {
	c, u, s := &fakeClient{}, &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	p := sampleProp()
	p.MapImageURL = "https://cdn.example/maps/" + locationiq.StyleVersion + "/Z1.png"
	p.MapImageDarkURL = "https://cdn.example/maps/" + locationiq.StyleVersion + "/Z1-dark.png"

	m, pending := svc.Ensure(context.Background(), p)
	if m.Light != "https://cdn.example/maps/"+locationiq.StyleVersion+"/Z1.png" || pending {
		t.Errorf("Ensure = (%q, %v)", m.Light, pending)
	}
	if c.geocodeCalls.Load() != 0 || c.staticCalls.Load() != 0 {
		t.Error("cached map must not call LocationIQ")
	}
}

func TestEnsure_PermanentlyUnmappableMakesNoAPICalls(t *testing.T) {
	c, u, s := &fakeClient{}, &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	now := time.Now()
	p := sampleProp()
	p.MapGeneratedAt = &now // stamped, but MapImageURL empty

	m, pending := svc.Ensure(context.Background(), p)
	if m.Light != "" || pending {
		t.Errorf("Ensure = (%q, %v), want empty and not pending", m.Light, pending)
	}
	if c.geocodeCalls.Load() != 0 || c.staticCalls.Load() != 0 {
		t.Error("unmappable row must not call LocationIQ")
	}
}

func TestEnsure_GeocodesThenGeneratesBothStylesAndPersists(t *testing.T) {
	c := &fakeClient{lat: 30.2672, lon: -97.7431}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	m, pending := svc.Ensure(context.Background(), sampleProp())
	if pending {
		t.Fatal("fast fakes should finish inside the deadline")
	}
	// Built from the constant, not hardcoded: a restyle bumps StyleVersion, and
	// the invariant under test is that the version is IN the path (so restyled
	// maps get a fresh CDN URL), not which version happens to be current. The
	// light map keeps the suffix-free name so pre-dark-map rows stay valid.
	wantLight := "maps/" + locationiq.StyleVersion + "/Z1.png"
	wantDark := "maps/" + locationiq.StyleVersion + "/Z1-dark.png"
	want := property.MapURLs{Light: "https://cdn.example/" + wantLight, Dark: "https://cdn.example/" + wantDark}
	if m != want {
		t.Errorf("maps = %+v, want %+v", m, want)
	}
	if len(u.gotPaths) != 2 || u.gotPaths[0] != wantLight || u.gotPaths[1] != wantDark {
		t.Errorf("upload paths = %q, want [%q %q]", u.gotPaths, wantLight, wantDark)
	}
	if c.geocodeCalls.Load() != 1 || c.staticCalls.Load() != 2 {
		t.Errorf("geocode=%d static=%d calls, want 1 and 2", c.geocodeCalls.Load(), c.staticCalls.Load())
	}
	if got := s.coords["Z1"]; got != [2]float64{30.2672, -97.7431} {
		t.Errorf("coords written = %v", got)
	}
	if s.mapCalls != 1 || s.mapURLs["Z1"] != want {
		t.Errorf("stored = %+v in %d calls, want %+v in 1", s.mapURLs["Z1"], s.mapCalls, want)
	}
}

func TestEnsure_FillsOnlyTheMissingDarkMap(t *testing.T) {
	c := &fakeClient{}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	// A row from before dark maps existed: light map stored and stamped.
	p := sampleProp()
	p.Latitude, p.Longitude = f64p(30.2672), f64p(-97.7431)
	p.MapImageURL = "https://cdn.example/maps/old/Z1.png"
	stamp := time.Now()
	p.MapGeneratedAt = &stamp

	m, pending := svc.Ensure(context.Background(), p)
	if pending {
		t.Fatal("unexpected pending")
	}
	wantDark := "https://cdn.example/" + ObjectPath("Z1", locationiq.StyleDark)
	if m.Light != p.MapImageURL || m.Dark != wantDark {
		t.Errorf("maps = %+v, want light kept and dark %q", m, wantDark)
	}
	if c.geocodeCalls.Load() != 0 || c.staticCalls.Load() != 1 {
		t.Errorf("geocode=%d static=%d calls, want 0 and 1", c.geocodeCalls.Load(), c.staticCalls.Load())
	}
	if s.mapURLs["Z1"] != m {
		t.Errorf("stored = %+v, want %+v", s.mapURLs["Z1"], m)
	}
}

func TestEnsure_SkipsGeocodeWhenCoordsPresent(t *testing.T) {
	c := &fakeClient{}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	p := sampleProp()
	p.Latitude, p.Longitude = f64p(30.2672), f64p(-97.7431)

	if _, pending := svc.Ensure(context.Background(), p); pending {
		t.Fatal("unexpected pending")
	}
	if c.geocodeCalls.Load() != 0 {
		t.Error("must not geocode when coordinates are already known")
	}
	if c.staticCalls.Load() != 2 {
		t.Errorf("static map calls = %d, want one per style", c.staticCalls.Load())
	}
	if s.coordCall != 0 {
		t.Error("must not rewrite coordinates it did not fetch")
	}
}

func TestEnsure_NoMatchRecordsUnmappableRow(t *testing.T) {
	c := &fakeClient{geocodeErr: locationiq.ErrNoMatch}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	m, pending := svc.Ensure(context.Background(), sampleProp())
	if m != (property.MapURLs{}) || pending {
		t.Errorf("Ensure = (%+v, %v)", m, pending)
	}
	if s.mapCalls != 1 || s.mapURLs["Z1"] != (property.MapURLs{}) {
		t.Errorf("want one SetMapImage with empty urls, got %d calls %+v", s.mapCalls, s.mapURLs["Z1"])
	}
	if c.staticCalls.Load() != 0 {
		t.Error("must not fetch a map without coordinates")
	}
}

func TestEnsure_TransientFailureLeavesRowAloneAndCoolsDown(t *testing.T) {
	c := &fakeClient{lat: 1, lon: 2, staticMapErr: errors.New("locationiq 503")}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	if m, _ := svc.Ensure(context.Background(), sampleProp()); m != (property.MapURLs{}) {
		t.Errorf("maps = %+v, want empty", m)
	}
	if s.mapCalls != 0 {
		t.Error("a transient failure must not stamp map_generated_at")
	}

	// Second attempt is refused by the cooldown, so no new API calls.
	before := c.staticCalls.Load()
	if m, pending := svc.Ensure(context.Background(), sampleProp()); m.Light != "" || pending {
		t.Errorf("Ensure during cooldown = (%+v, %v)", m, pending)
	}
	if c.staticCalls.Load() != before {
		t.Error("cooldown must suppress the retry")
	}
}

func TestEnsure_ReturnsPendingWhenGenerationOutlastsDeadline(t *testing.T) {
	release := make(chan struct{})
	c := &fakeClient{lat: 1, lon: 2, blockStaticOn: release}
	u, s := &fakeUploader{}, newFakeStore()

	svc := New(c, u, s, testLogger())
	svc.deadline = 20 * time.Millisecond

	m, pending := svc.Ensure(context.Background(), sampleProp())
	if m.Light != "" || !pending {
		t.Fatalf("Ensure = (%q, %v), want empty and pending", m.Light, pending)
	}

	// Generation continues in the background and still persists.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		done := s.mapURLs["Z1"].Complete()
		s.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background generation never persisted the map")
}

func TestEnsure_CancelledRequestDoesNotKillGeneration(t *testing.T) {
	release := make(chan struct{})
	c := &fakeClient{lat: 1, lon: 2, blockStaticOn: release}
	u, s := &fakeUploader{}, newFakeStore()

	svc := New(c, u, s, testLogger())
	svc.deadline = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	if _, pending := svc.Ensure(ctx, sampleProp()); !pending {
		t.Fatal("want pending")
	}
	cancel() // viewer navigates away
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		done := s.mapURLs["Z1"].Complete()
		s.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("generation must survive request cancellation")
}

func TestEnsure_SingleFlightCollapsesConcurrentViews(t *testing.T) {
	release := make(chan struct{})
	c := &fakeClient{lat: 1, lon: 2, blockStaticOn: release}
	u, s := &fakeUploader{}, newFakeStore()

	svc := New(c, u, s, testLogger())
	svc.deadline = 2 * time.Second

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.Ensure(context.Background(), sampleProp())
		}()
	}
	// Let all ten reach the single-flight before releasing the blocked call.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := c.geocodeCalls.Load(); got != 1 {
		t.Errorf("geocode calls = %d, want 1", got)
	}
	if got := c.staticCalls.Load(); got != 2 {
		t.Errorf("static map calls = %d, want one per style", got)
	}
	if u.uploaded != 2 {
		t.Errorf("uploads = %d, want 1", u.uploaded)
	}
}

func TestEnsure_HourlyBudgetDeniesGeneration(t *testing.T) {
	c := &fakeClient{lat: 1, lon: 2}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)
	svc.budget = 2

	for i, zpid := range []string{"A", "B", "C"} {
		p := sampleProp()
		p.ZPID = zpid
		m, _ := svc.Ensure(context.Background(), p)
		if i < 2 && m.Light == "" {
			t.Errorf("call %d denied, want allowed", i)
		}
		if i == 2 && m.Light != "" {
			t.Error("third call should exceed the budget")
		}
	}
	if got := c.staticCalls.Load(); got != 4 {
		t.Errorf("static map calls = %d, want 2", got)
	}
}

func TestEnsure_NilServiceIsDisabled(t *testing.T) {
	var svc *Service
	if m, pending := svc.Ensure(context.Background(), sampleProp()); m.Light != "" || pending {
		t.Errorf("nil service Ensure = (%q, %v)", m.Light, pending)
	}
}

// sometimesFailClient fails its first N StaticMap calls, then succeeds. It
// drives the cooldown-expiry test, where the transient failure must clear
// once the guard readmits a retry.
type sometimesFailClient struct {
	lat, lon    float64
	failTimes   atomic.Int32 // StaticMap calls left that should fail
	staticCalls atomic.Int32
}

func (c *sometimesFailClient) Geocode(_ context.Context, _ locationiq.Address) (float64, float64, error) {
	return c.lat, c.lon, nil
}

func (c *sometimesFailClient) StaticMap(_ context.Context, _, _ float64, _ locationiq.Style) ([]byte, error) {
	c.staticCalls.Add(1)
	if c.failTimes.Add(-1) >= 0 {
		return nil, errors.New("locationiq 503")
	}
	return []byte("PNG"), nil
}

// TestEnsure_CooldownExpiryReadmitsRetry drives svc.now with a controllable
// clock to cover the cooldown-expiry path: a transient failure denies
// retries until the cooldown elapses, then the next attempt is admitted and,
// since the underlying failure has cleared, succeeds.
func TestEnsure_CooldownExpiryReadmitsRetry(t *testing.T) {
	c := &sometimesFailClient{lat: 1, lon: 2}
	c.failTimes.Store(1) // first StaticMap call fails, the rest succeed
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	clock := time.Now()
	svc.now = func() time.Time { return clock } // set before any Ensure call, per allow()'s locking contract

	zpid := "COOL1"
	newProp := func() *property.Property {
		p := sampleProp()
		p.ZPID = zpid
		return p
	}

	// First attempt: transient failure, puts the zpid on cooldown.
	if m, _ := svc.Ensure(context.Background(), newProp()); m.Light != "" {
		t.Errorf("first attempt url = %q, want empty", m.Light)
	}
	if s.mapCalls != 0 {
		t.Error("a transient failure must not stamp map_generated_at")
	}

	// Second attempt, same instant: cooldown still active, denied outright.
	before := c.staticCalls.Load()
	if m, pending := svc.Ensure(context.Background(), newProp()); m.Light != "" || pending {
		t.Errorf("Ensure during cooldown = (%q, %v)", m.Light, pending)
	}
	if c.staticCalls.Load() != before {
		t.Error("cooldown must suppress the retry")
	}

	// Advance the clock past the cooldown: the next attempt is admitted, and
	// since the client's failure budget is spent, it now succeeds.
	clock = clock.Add(svc.cooldown + time.Second)

	m, pending := svc.Ensure(context.Background(), newProp())
	if pending {
		t.Fatal("fast fake should finish inside the deadline")
	}
	if m.Light == "" {
		t.Error("attempt after cooldown expiry should be admitted and succeed")
	}
	if s.mapURLs[zpid].Light != m.Light {
		t.Errorf("stored url = %q, want %q", s.mapURLs[zpid].Light, m.Light)
	}
}

// TestEnsure_BudgetWindowRolloverReadmitsAfterAnHour drives svc.now with a
// controllable clock to cover the window-rollover path: once the budget is
// exhausted within a window, generation is denied until the clock advances
// past the window, at which point the count resets and generation resumes.
func TestEnsure_BudgetWindowRolloverReadmitsAfterAnHour(t *testing.T) {
	c := &fakeClient{lat: 1, lon: 2}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)
	svc.budget = 1

	clock := time.Now()
	svc.now = func() time.Time { return clock } // set before any Ensure call, per allow()'s locking contract

	first := sampleProp()
	first.ZPID = "WIN1"
	if m, pending := svc.Ensure(context.Background(), first); m.Light == "" || pending {
		t.Fatalf("first call = (%q, %v), want admitted and successful", m.Light, pending)
	}

	second := sampleProp()
	second.ZPID = "WIN2"
	if m, pending := svc.Ensure(context.Background(), second); m.Light != "" || pending {
		t.Errorf("Ensure over budget = (%q, %v), want denied", m.Light, pending)
	}
	if got := c.staticCalls.Load(); got != 2 {
		t.Errorf("static map calls = %d, want 2 (one per style) before rollover", got)
	}

	// Advance the clock past the tumbling window: the budget resets.
	clock = clock.Add(time.Hour + time.Second)

	third := sampleProp()
	third.ZPID = "WIN3"
	m, pending := svc.Ensure(context.Background(), third)
	if pending {
		t.Fatal("fast fake should finish inside the deadline")
	}
	if m.Light == "" {
		t.Error("call after window rollover should be admitted")
	}
	if got := c.staticCalls.Load(); got != 4 {
		t.Errorf("static map calls = %d, want 4 after rollover", got)
	}
}
