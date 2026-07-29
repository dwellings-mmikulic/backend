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

func (f *fakeClient) StaticMap(_ context.Context, _, _ float64) ([]byte, error) {
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
	gotPath  string
	uploaded int
	err      error
}

func (f *fakeUploader) Upload(_ context.Context, path string, _ io.Reader, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploaded++
	f.gotPath = path
	if f.err != nil {
		return "", f.err
	}
	return "https://cdn.example/" + path, nil
}

type fakeStore struct {
	mu        sync.Mutex
	mapURLs   map[string]string
	coords    map[string][2]float64
	mapCalls  int
	coordCall int
}

func newFakeStore() *fakeStore {
	return &fakeStore{mapURLs: map[string]string{}, coords: map[string][2]float64{}}
}

func (f *fakeStore) SetMapImage(_ context.Context, zpid, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mapCalls++
	f.mapURLs[zpid] = url
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
	p.MapImageURL = "https://cdn.example/maps/Z1.png"

	url, pending := svc.Ensure(context.Background(), p)
	if url != "https://cdn.example/maps/Z1.png" || pending {
		t.Errorf("Ensure = (%q, %v)", url, pending)
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

	url, pending := svc.Ensure(context.Background(), p)
	if url != "" || pending {
		t.Errorf("Ensure = (%q, %v), want empty and not pending", url, pending)
	}
	if c.geocodeCalls.Load() != 0 || c.staticCalls.Load() != 0 {
		t.Error("unmappable row must not call LocationIQ")
	}
}

func TestEnsure_GeocodesThenGeneratesAndPersists(t *testing.T) {
	c := &fakeClient{lat: 30.2672, lon: -97.7431}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	url, pending := svc.Ensure(context.Background(), sampleProp())
	if pending {
		t.Fatal("fast fakes should finish inside the deadline")
	}
	if url != "https://cdn.example/maps/Z1.png" {
		t.Errorf("url = %q", url)
	}
	if u.gotPath != "maps/Z1.png" {
		t.Errorf("upload path = %q, want maps/Z1.png", u.gotPath)
	}
	if got := s.coords["Z1"]; got != [2]float64{30.2672, -97.7431} {
		t.Errorf("coords written = %v", got)
	}
	if s.mapURLs["Z1"] != url {
		t.Errorf("stored url = %q, want %q", s.mapURLs["Z1"], url)
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
	if c.staticCalls.Load() != 1 {
		t.Errorf("static map calls = %d, want 1", c.staticCalls.Load())
	}
	if s.coordCall != 0 {
		t.Error("must not rewrite coordinates it did not fetch")
	}
}

func TestEnsure_NoMatchRecordsUnmappableRow(t *testing.T) {
	c := &fakeClient{geocodeErr: locationiq.ErrNoMatch}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	url, pending := svc.Ensure(context.Background(), sampleProp())
	if url != "" || pending {
		t.Errorf("Ensure = (%q, %v)", url, pending)
	}
	if s.mapCalls != 1 || s.mapURLs["Z1"] != "" {
		t.Errorf("want one SetMapImage with an empty url, got %d calls %q", s.mapCalls, s.mapURLs["Z1"])
	}
	if c.staticCalls.Load() != 0 {
		t.Error("must not fetch a map without coordinates")
	}
}

func TestEnsure_TransientFailureLeavesRowAloneAndCoolsDown(t *testing.T) {
	c := &fakeClient{lat: 1, lon: 2, staticMapErr: errors.New("locationiq 503")}
	u, s := &fakeUploader{}, newFakeStore()
	svc := newTestService(c, u, s)

	if url, _ := svc.Ensure(context.Background(), sampleProp()); url != "" {
		t.Errorf("url = %q, want empty", url)
	}
	if s.mapCalls != 0 {
		t.Error("a transient failure must not stamp map_generated_at")
	}

	// Second attempt is refused by the cooldown, so no new API calls.
	before := c.staticCalls.Load()
	if url, pending := svc.Ensure(context.Background(), sampleProp()); url != "" || pending {
		t.Errorf("Ensure during cooldown = (%q, %v)", url, pending)
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

	url, pending := svc.Ensure(context.Background(), sampleProp())
	if url != "" || !pending {
		t.Fatalf("Ensure = (%q, %v), want empty and pending", url, pending)
	}

	// Generation continues in the background and still persists.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		done := s.mapURLs["Z1"] != ""
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
		done := s.mapURLs["Z1"] != ""
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
	if got := c.staticCalls.Load(); got != 1 {
		t.Errorf("static map calls = %d, want 1", got)
	}
	if u.uploaded != 1 {
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
		url, _ := svc.Ensure(context.Background(), p)
		if i < 2 && url == "" {
			t.Errorf("call %d denied, want allowed", i)
		}
		if i == 2 && url != "" {
			t.Error("third call should exceed the budget")
		}
	}
	if got := c.staticCalls.Load(); got != 2 {
		t.Errorf("static map calls = %d, want 2", got)
	}
}

func TestEnsure_NilServiceIsDisabled(t *testing.T) {
	var svc *Service
	if url, pending := svc.Ensure(context.Background(), sampleProp()); url != "" || pending {
		t.Errorf("nil service Ensure = (%q, %v)", url, pending)
	}
}
