package scheduler

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/hls"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/video"
)

// enableHLS turns segmentation on with a recorder that shares the harness's
// event log, so its calls can be ordered against the store's.
func (h *harness) enableHLS(seg Segmenter) *fakeHLSRecorder {
	rec := &fakeHLSRecorder{events: h.events}
	h.s.EnableHLS(seg, rec)
	return rec
}

// countingServer serves a JPEG and counts the requests it got.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	data := jpegBytes(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
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
	h := newHarness(t, baseConfig())

	urls := h.s.uploadPhotos(context.Background(), "ZP1", local)
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

// The gallery's order is the order the provider gave, and the video's photos
// follow it. The downloads run concurrently, so each one has to land in its
// own slot rather than wherever it finished: a listing whose photos come back
// out of order shows the kitchen first and ends the video on the front door.
// Each source serves an image of a distinct size, which is what the decoded
// result is checked against.
func TestDownloadPhotos_PreservesOrder(t *testing.T) {
	const n = 8
	// A later photo is served more slowly, so completion order is the reverse
	// of source order unless the code places each result by its index.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".jpg"))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		time.Sleep(time.Duration(n-idx) * time.Millisecond)
		var buf bytes.Buffer
		// Width identifies the source: 8, 16, 24 … (JPEG needs a few pixels).
		if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8*(idx+1), 8)), nil); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	var srcs []string
	for i := 0; i < n; i++ {
		srcs = append(srcs, srv.URL+"/"+strconv.Itoa(i)+".jpg")
	}
	h := newHarness(t, baseConfig())

	local := h.s.downloadPhotos(context.Background(), srcs, t.TempDir())

	if len(local) != n {
		t.Fatalf("got %d local photos, want %d", len(local), n)
	}
	for i, p := range local {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := jpeg.DecodeConfig(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if want := 8 * (i + 1); cfg.Width != want {
			t.Errorf("local[%d] is %dpx wide, i.e. came from source %d, want source %d: the gallery order is the provider's order",
				i, cfg.Width, cfg.Width/8-1, i)
		}
	}
}

// IMAGE_CONCURRENCY is what keeps one listing from opening a socket per photo
// against the photo CDN and Bunny at once; a dense listing has dozens, and
// LISTING_CONCURRENCY of them run side by side on the same box.
func TestPhotos_ConcurrencyIsBoundedByTheImageLimit(t *testing.T) {
	const (
		limit  = 3
		photos = 9
	)
	cfg := baseConfig()
	cfg.Concurrency.Images = limit

	t.Run("download", func(t *testing.T) {
		started := make(chan struct{}, photos)
		release := make(chan struct{})
		data := jpegBytes(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			started <- struct{}{}
			<-release
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(data)
		}))
		defer srv.Close()
		var srcs []string
		for i := 0; i < photos; i++ {
			srcs = append(srcs, srv.URL+"/"+strconv.Itoa(i)+".jpg")
		}
		h := newHarness(t, cfg)

		done := make(chan []string, 1)
		go func() { done <- h.s.downloadPhotos(context.Background(), srcs, t.TempDir()) }()

		assertAtMostInFlight(t, started, limit, "photo downloads")
		close(release)
		if got := recv(t, done, "the downloads to finish"); len(got) != photos {
			t.Errorf("downloaded %d photos, want %d", len(got), photos)
		}
	})

	t.Run("upload", func(t *testing.T) {
		dir := t.TempDir()
		var local []string
		for i := 0; i < photos; i++ {
			p := filepath.Join(dir, strconv.Itoa(i)+".jpg")
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			local = append(local, p)
		}
		h := newHarness(t, cfg)
		started := make(chan struct{}, photos)
		release := make(chan struct{})
		h.bunny.onUpload = func(string) {
			started <- struct{}{}
			<-release
		}

		done := make(chan []string, 1)
		go func() { done <- h.s.uploadPhotos(context.Background(), "ZP1", local) }()

		assertAtMostInFlight(t, started, limit, "photo uploads")
		close(release)
		if got := recv(t, done, "the uploads to finish"); len(got) != photos {
			t.Errorf("uploaded %d photos, want %d", len(got), photos)
		}
		if peak := h.bunny.peak.Load(); peak != int64(limit) {
			t.Errorf("peak concurrent uploads = %d, want %d", peak, limit)
		}
	})
}

// assertAtMostInFlight waits for want tokens (they must arrive: the work is
// parked) and then checks that no further one does while they are all held.
func assertAtMostInFlight(t *testing.T, started <-chan struct{}, want int, what string) {
	t.Helper()
	for i := 0; i < want; i++ {
		recv(t, started, what)
	}
	select {
	case <-started:
		t.Fatalf("more than %d %s ran at once: the IMAGE_CONCURRENCY limit is gone", want, what)
	case <-time.After(100 * time.Millisecond):
	}
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

	h := newHarness(t, baseConfig())
	dir := t.TempDir()

	local := h.s.downloadPhotos(context.Background(), []string{srv.URL + "/photo.webp"}, dir)
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

	h := newHarness(t, baseConfig())

	local := h.s.downloadPhotos(context.Background(), []string{srv.URL + "/broken.jpg"}, t.TempDir())
	if len(local) != 0 {
		t.Errorf("got %d local photos, want 0 (undecodable data must be skipped)", len(local))
	}
}

func TestProcessListing_NewListingGoesThroughTheWholePipeline(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	p := listing("ZP1", img.URL+"/a.jpg", img.URL+"/b.jpg")

	skipped, err := h.s.processListing(context.Background(), &p, false)
	if err != nil || skipped {
		t.Fatalf("processListing = (%v, %v), want (false, nil)", skipped, err)
	}

	up := h.store.upsertedProps()
	if len(up) != 1 {
		t.Fatalf("upserts = %d, want 1", len(up))
	}
	wantURLs := []string{"https://cdn.example/properties/ZP1/0.jpg", "https://cdn.example/properties/ZP1/1.jpg"}
	if !reflect.DeepEqual(up[0].ImageURLs, wantURLs) {
		t.Errorf("stored image urls = %v, want the CDN urls %v", up[0].ImageURLs, wantURLs)
	}
	if got := h.store.ready(); !reflect.DeepEqual(got, []string{"ZP1"}) {
		t.Errorf("video ready = %v, want [ZP1]", got)
	}
	if got := h.bunny.pathsWithPrefix("videos/"); !reflect.DeepEqual(got, []string{"videos/ZP1.mp4"}) {
		t.Errorf("video uploads = %v", got)
	}
	if got := h.store.videoFailed(); len(got) != 0 {
		t.Errorf("video failed = %v, want none", got)
	}
}

func TestProcessListing_EmptyZPIDIsAnError(t *testing.T) {
	h := newHarness(t, baseConfig())
	p := listing("")
	if _, err := h.s.processListing(context.Background(), &p, false); err == nil {
		t.Fatal("want an error for a listing without a zpid")
	}
	if h.store.upsertCount() != 0 {
		t.Error("a listing without a zpid must not be stored")
	}
}

func TestProcessListing_SkipExisting(t *testing.T) {
	img := jpegServer(t)

	t.Run("stored with a ready video: skipped untouched", func(t *testing.T) {
		cfg := baseConfig()
		cfg.SkipExisting = true
		h := newHarness(t, cfg)
		h.store.existing = map[string]bool{"ZP1": true}
		p := listing("ZP1", img.URL+"/a.jpg")

		skipped, err := h.s.processListing(context.Background(), &p, false)
		if err != nil || !skipped {
			t.Fatalf("processListing = (%v, %v), want (true, nil)", skipped, err)
		}
		if h.store.upsertCount() != 0 || len(h.bunny.paths()) != 0 || h.render.calls.Load() != 0 {
			t.Error("a skipped listing must not be touched")
		}
	})

	// A stored listing whose render failed previously must be revisited so the
	// video can be retried. Before this, SkipExisting returned before
	// renderVideo ran, so a single failed render stranded that listing without
	// a video for good — which is how 24 production listings ended up
	// permanently videoless. The revisit renders only: it must not re-upload
	// images or re-upsert the row.
	t.Run("stored without a ready video: re-rendered, nothing else", func(t *testing.T) {
		cfg := baseConfig()
		cfg.SkipExisting = true
		h := newHarness(t, cfg)
		h.store.existing = map[string]bool{"ZP1": true}
		h.store.needsVideo = map[string]bool{"ZP1": true}
		p := listing("ZP1", img.URL+"/a.jpg")

		skipped, err := h.s.processListing(context.Background(), &p, false)
		if err != nil || skipped {
			t.Fatalf("processListing = (%v, %v), want (false, nil)", skipped, err)
		}
		if got := h.store.ready(); !reflect.DeepEqual(got, []string{"ZP1"}) {
			t.Errorf("video ready = %v, want [ZP1]", got)
		}
		if got := h.store.upsertCount(); got != 0 {
			t.Errorf("upserts = %d, want 0 — a video revisit must not rewrite the row", got)
		}
		for _, path := range h.bunny.paths() {
			if !strings.HasPrefix(path, "videos/") {
				t.Errorf("unexpected upload %q — a video revisit must not re-upload photos", path)
			}
		}
	})

	t.Run("stored without a video on a worker that renders none: skipped", func(t *testing.T) {
		cfg := baseConfig()
		cfg.SkipExisting = true
		cfg.Video.Enabled = false
		h := newHarness(t, cfg)
		h.store.existing = map[string]bool{"ZP1": true}
		h.store.needsVideo = map[string]bool{"ZP1": true}
		p := listing("ZP1", img.URL+"/a.jpg")

		skipped, err := h.s.processListing(context.Background(), &p, false)
		if err != nil || !skipped {
			t.Fatalf("processListing = (%v, %v), want (true, nil)", skipped, err)
		}
	})

	t.Run("SKIP_EXISTING off refreshes a stored listing", func(t *testing.T) {
		h := newHarness(t, baseConfig())
		h.store.existing = map[string]bool{"ZP1": true}
		p := listing("ZP1", img.URL+"/a.jpg")

		if skipped, err := h.s.processListing(context.Background(), &p, false); err != nil || skipped {
			t.Fatalf("processListing = (%v, %v), want (false, nil)", skipped, err)
		}
		if got := h.store.upsertCount(); got != 1 {
			t.Errorf("upserts = %d, want 1", got)
		}
	})
}

// A revisit payload (cmd/backfill-videos) takes the video-only path whatever
// SKIP_EXISTING says: the tool selected the listing because it is stored.
func TestProcessListing_RevisitPayloadForcesTheVideoOnlyPath(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())                  // SkipExisting is false
	h.store.needsVideo = map[string]bool{"ZP1": true} // stored, still without a video
	p := listing("ZP1", img.URL+"/a.jpg")

	skipped, err := h.s.processListing(context.Background(), &p, true)
	if err != nil || skipped {
		t.Fatalf("processListing = (%v, %v), want (false, nil)", skipped, err)
	}
	if got := h.store.upsertCount(); got != 0 {
		t.Errorf("upserts = %d, want 0 on a revisit", got)
	}
	if got := h.bunny.pathsWithPrefix("properties/"); len(got) != 0 {
		t.Errorf("photo uploads = %v, want none on a revisit", got)
	}
	if got := h.store.ready(); !reflect.DeepEqual(got, []string{"ZP1"}) {
		t.Errorf("video ready = %v, want [ZP1]", got)
	}
}

// Spec 3.2: "The media worker re-checks, as processListing does today", and
// the branch's goal: no listing rendered twice. A revisit item whose listing
// has got its video since it was enqueued — its first run rendered it and then
// lost the Complete (a DB blip inside the bookkeeping timeout, a SIGKILL
// between SetVideoReady and Complete), so the item is claimed again after its
// lease — must not be rendered, uploaded and segmented a second time. The
// revisit path never loads the stored status and hash, so renderVideo's
// "unchanged, skip" cannot catch this one.
func TestProcessListing_RevisitOfAnAlreadyReadyListingIsSkipped(t *testing.T) {
	tests := []struct {
		name       string
		needsVideo map[string]bool
	}{
		{"video ready since it was enqueued", map[string]bool{"ZP1": false}},
		{"listing no longer stored at all", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img := jpegServer(t)
			cfg := baseConfig()
			cfg.SkipExisting = true
			h := newHarness(t, cfg)
			h.store.needsVideo = tt.needsVideo
			p := listing("ZP1", img.URL+"/a.jpg")

			skipped, err := h.s.processListing(context.Background(), &p, true)

			if err != nil || !skipped {
				t.Fatalf("processListing = (%v, %v), want (true, nil): nothing left to do", skipped, err)
			}
			if n := h.render.calls.Load(); n != 0 {
				t.Errorf("rendered %d time(s) a listing that does not need a video", n)
			}
			if got := h.bunny.paths(); len(got) != 0 {
				t.Errorf("uploaded %v, want nothing", got)
			}
			if got := h.store.ready(); len(got) != 0 {
				t.Errorf("SetVideoReady for %v, want none", got)
			}
		})
	}
}

func TestProcessListing_RevisitOnAWorkerWithoutVideo(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		noRenderer bool
	}{
		{"video disabled", false, false},
		{"no renderer", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Video.Enabled = tt.enabled
			h := newHarness(t, cfg)
			if tt.noRenderer {
				h.s.render = nil
			}
			p := listing("ZP1", "https://photos.example/a.jpg")

			_, err := h.s.processListing(context.Background(), &p, true)
			if !errors.Is(err, errVideoDisabledRevisit) {
				t.Fatalf("err = %v, want errVideoDisabledRevisit", err)
			}
			if h.store.upsertCount() != 0 || len(h.bunny.paths()) != 0 {
				t.Error("nothing may be touched when the revisit cannot be done here")
			}
		})
	}
}

// A listing the provider has no photos for is not a failure of this box: it
// is stored (there is a price and an address), its video is marked failed,
// and the item is done.
func TestProcessListing_NoSourcePhotosIsTerminalNotAnError(t *testing.T) {
	tests := []struct {
		name            string
		images, video   bool
		revisit         bool
		wantUpserts     int
		wantVideoFailed int
	}{
		{name: "images and video", images: true, video: true, wantUpserts: 1, wantVideoFailed: 1},
		{name: "video only", images: false, video: true, wantUpserts: 1, wantVideoFailed: 1},
		{name: "images only", images: true, video: false, wantUpserts: 1, wantVideoFailed: 0},
		{name: "neither", images: false, video: false, wantUpserts: 1, wantVideoFailed: 0},
		{name: "revisit", images: true, video: true, revisit: true, wantUpserts: 0, wantVideoFailed: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.ImagesEnabled, cfg.Video.Enabled = tt.images, tt.video
			h := newHarness(t, cfg)
			h.store.needsVideo = map[string]bool{"ZP1": true}
			p := listing("ZP1")

			skipped, err := h.s.processListing(context.Background(), &p, tt.revisit)
			if err != nil || skipped {
				t.Fatalf("processListing = (%v, %v), want (false, nil)", skipped, err)
			}
			if got := h.store.upsertCount(); got != tt.wantUpserts {
				t.Errorf("upserts = %d, want %d", got, tt.wantUpserts)
			}
			if got := len(h.store.videoFailed()); got != tt.wantVideoFailed {
				t.Errorf("SetVideoFailed calls = %d, want %d", got, tt.wantVideoFailed)
			}
			if h.render.calls.Load() != 0 {
				t.Error("nothing to render without photos")
			}
			if tt.video {
				if recs := h.logs.find("no photos to render video"); len(recs) != 1 || recs[0].level != slog.LevelWarn {
					t.Errorf("want one Warn about the missing photos, got %+v", recs)
				}
			}
		})
	}
}

// Source photos exist but not one could be fetched: that is this box (blocked
// IP) or the photo CDN, not the listing. Persisting it now would store an
// empty gallery that SKIP_EXISTING then keeps forever.
func TestProcessListing_NoPhotoDownloadableIsAnInfrastructureError(t *testing.T) {
	blocked := statusServer(t, http.StatusForbidden)
	for _, images := range []bool{true, false} {
		t.Run("images enabled "+strconv.FormatBool(images), func(t *testing.T) {
			cfg := baseConfig()
			cfg.ImagesEnabled = images
			h := newHarness(t, cfg)
			p := listing("ZP1", blocked.URL+"/a.jpg", blocked.URL+"/b.jpg")

			_, err := h.s.processListing(context.Background(), &p, false)
			if err == nil {
				t.Fatal("want an error when no source photo could be downloaded")
			}
			if got := h.store.upsertCount(); got != 0 {
				t.Errorf("upserts = %d, want 0: never persist a listing whose photos could not be fetched", got)
			}
			if got := h.store.videoFailed(); len(got) != 0 {
				t.Errorf("video failed = %v: the render was never reached", got)
			}
		})
	}
}

func TestProcessListing_NoPhotoUploadedFailsBeforeTheUpsert(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	h.bunny.failPrefix = "properties/"
	p := listing("ZP1", img.URL+"/a.jpg", img.URL+"/b.jpg")

	_, err := h.s.processListing(context.Background(), &p, false)
	if err == nil {
		t.Fatal("want an error when no photo could be uploaded")
	}
	if got := h.store.upsertCount(); got != 0 {
		t.Errorf("upserts = %d, want 0: an empty gallery must never be persisted", got)
	}
	if h.render.calls.Load() != 0 {
		t.Error("the render must not run for a listing that was not stored")
	}
}

func TestProcessListing_PartialPhotoSetIsAccepted(t *testing.T) {
	data := jpegBytes(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "missing.jpg") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	h := newHarness(t, baseConfig())
	p := listing("ZP1", srv.URL+"/a.jpg", srv.URL+"/missing.jpg", srv.URL+"/c.jpg")

	if _, err := h.s.processListing(context.Background(), &p, false); err != nil {
		t.Fatalf("a partial photo set must be accepted, got %v", err)
	}
	up := h.store.upsertedProps()
	if len(up) != 1 || len(up[0].ImageURLs) != 2 {
		t.Fatalf("upserted %+v, want one row with the 2 photos that exist", up)
	}
	recs := h.logs.find("some photos could not be downloaded")
	if len(recs) != 1 || recs[0].level != slog.LevelWarn ||
		recs[0].attrs["downloaded"] != int64(2) || recs[0].attrs["source"] != int64(3) {
		t.Errorf("want one Warn with the counts, got %+v", recs)
	}
}

func TestProcessListing_Modes(t *testing.T) {
	t.Run("images disabled: source urls stored, video still rendered", func(t *testing.T) {
		img := jpegServer(t)
		cfg := baseConfig()
		cfg.ImagesEnabled = false
		h := newHarness(t, cfg)
		src := []string{img.URL + "/a.jpg"}
		p := listing("ZP1", src...)

		if _, err := h.s.processListing(context.Background(), &p, false); err != nil {
			t.Fatal(err)
		}
		if up := h.store.upsertedProps(); len(up) != 1 || !reflect.DeepEqual(up[0].ImageURLs, src) {
			t.Errorf("upserted %+v, want the source urls kept", up)
		}
		if got := h.bunny.pathsWithPrefix("properties/"); len(got) != 0 {
			t.Errorf("photo uploads = %v, want none", got)
		}
		if got := h.store.ready(); len(got) != 1 {
			t.Errorf("video ready = %v, want [ZP1]", got)
		}
	})

	t.Run("video disabled: photos uploaded and stored, no render", func(t *testing.T) {
		img := jpegServer(t)
		cfg := baseConfig()
		cfg.Video.Enabled = false
		h := newHarness(t, cfg)
		p := listing("ZP1", img.URL+"/a.jpg")

		if _, err := h.s.processListing(context.Background(), &p, false); err != nil {
			t.Fatal(err)
		}
		if h.store.upsertCount() != 1 || len(h.bunny.pathsWithPrefix("properties/")) != 1 {
			t.Error("the listing should be stored with its uploaded photo")
		}
		if h.render.calls.Load() != 0 || len(h.store.ready())+len(h.store.videoFailed()) != 0 {
			t.Error("video state must not be touched with video disabled")
		}
	})

	t.Run("both disabled: stored as found, nothing downloaded", func(t *testing.T) {
		img, hits := countingServer(t)
		cfg := baseConfig()
		cfg.ImagesEnabled, cfg.Video.Enabled = false, false
		h := newHarness(t, cfg)
		p := listing("ZP1", img.URL+"/a.jpg")

		if _, err := h.s.processListing(context.Background(), &p, false); err != nil {
			t.Fatal(err)
		}
		if h.store.upsertCount() != 1 {
			t.Error("the listing should be stored")
		}
		if hits.Load() != 0 || len(h.bunny.paths()) != 0 {
			t.Error("nothing needs the photos locally: no download, no upload")
		}
	})
}

// An unchanged listing with a ready video is not rendered again.
func TestProcessListing_UnchangedReadyVideoIsNotReRendered(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	p := listing("ZP1", img.URL+"/a.jpg")

	stored := p
	stored.ImageURLs = []string{"https://cdn.example/properties/ZP1/0.jpg"}
	h.store.storedStatus = map[string]property.VideoStatus{"ZP1": property.VideoReady}
	h.store.storedHash = map[string]string{"ZP1": video.ContentHash(&stored, h.cfg.Video.SecondsPerPhoto)}

	if _, err := h.s.processListing(context.Background(), &p, false); err != nil {
		t.Fatal(err)
	}
	if h.render.calls.Load() != 0 {
		t.Error("rendered although the stored video is ready and the content hash unchanged")
	}
}

// The skip needs BOTH halves: the stored video is ready AND its content hash
// still matches. Before this branch SetVideoFailed was unguarded, so rows that
// carry a matching hash next to a 'failed' or 'pending' status exist in
// production; on the hash alone they would never be rendered again.
func TestProcessListing_OnlyAReadyVideoWithAMatchingHashIsSkipped(t *testing.T) {
	for _, status := range []property.VideoStatus{property.VideoFailed, property.VideoPending} {
		t.Run(string(status), func(t *testing.T) {
			img := jpegServer(t)
			h := newHarness(t, baseConfig())
			p := listing("ZP1", img.URL+"/a.jpg")

			stored := p
			stored.ImageURLs = []string{"https://cdn.example/properties/ZP1/0.jpg"}
			h.store.storedStatus = map[string]property.VideoStatus{"ZP1": status}
			h.store.storedHash = map[string]string{"ZP1": video.ContentHash(&stored, h.cfg.Video.SecondsPerPhoto)}

			if _, err := h.s.processListing(context.Background(), &p, false); err != nil {
				t.Fatal(err)
			}
			if got := h.render.calls.Load(); got != 1 {
				t.Errorf("renders = %d, want 1: the hash matches but the stored video is %s, not ready", got, status)
			}
			if got := h.store.ready(); !reflect.DeepEqual(got, []string{"ZP1"}) {
				t.Errorf("video ready = %v, want [ZP1]", got)
			}
		})
	}
}

// Two error paths the pipeline has to return rather than swallow: an unusable
// database answer must fail the item so it is retried, not be read as "this
// listing is fine".
func TestProcessListing_StoreErrorsAreReturned(t *testing.T) {
	dbDown := errors.New("db down")

	// Swallowing this would download and re-upload the photos of every stored
	// listing on every pass, which is what SKIP_EXISTING exists to prevent.
	t.Run("the existence check fails", func(t *testing.T) {
		img := jpegServer(t)
		cfg := baseConfig()
		cfg.SkipExisting = true
		h := newHarness(t, cfg)
		h.store.existsErr = dbDown
		p := listing("ZP1", img.URL+"/a.jpg")

		_, err := h.s.processListing(context.Background(), &p, false)

		if !errors.Is(err, dbDown) {
			t.Fatalf("err = %v, want the lookup failure returned", err)
		}
		if h.store.upsertCount() != 0 || len(h.bunny.paths()) != 0 {
			t.Error("nothing may be written when it is not known whether the listing is stored")
		}
	})

	// Swallowing this would leave the row 'pending' for ever: NeedsVideo keeps
	// saying yes, so every pass revisits a listing that has no photos to render.
	t.Run("marking the photo-less video failed does not stick", func(t *testing.T) {
		h := newHarness(t, baseConfig())
		h.store.setFailedErr = dbDown
		p := listing("BARE")

		_, err := h.s.processListing(context.Background(), &p, false)

		if !errors.Is(err, dbDown) {
			t.Fatalf("err = %v, want the write failure returned so the item is retried", err)
		}
		if h.store.upsertCount() != 1 {
			t.Error("the listing itself was stored and must stay stored")
		}
	})
}

func TestProcessListing_VideoFailuresAreReturnedAndRecorded(t *testing.T) {
	boom := errors.New("ffmpeg exploded")
	tests := []struct {
		name  string
		setup func(h *harness)
	}{
		{"render fails", func(h *harness) {
			h.render.setHook(func(context.Context, *property.Property) error { return boom })
		}},
		{"mp4 upload fails", func(h *harness) { h.bunny.failPrefix = "videos/" }},
		{"recording the video fails", func(h *harness) { h.store.setReadyErr = boom }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img := jpegServer(t)
			h := newHarness(t, baseConfig())
			tt.setup(h)
			p := listing("ZP1", img.URL+"/a.jpg")

			_, err := h.s.processListing(context.Background(), &p, false)
			if err == nil {
				t.Fatal("want the video failure returned, not only logged")
			}
			if got := h.store.videoFailed(); !reflect.DeepEqual(got, []string{"ZP1"}) {
				t.Errorf("video failed = %v, want [ZP1]", got)
			}
			if got := h.store.upsertCount(); got != 1 {
				t.Errorf("upserts = %d, want 1: the listing itself was stored before the render", got)
			}
		})
	}
}

// A shutdown is not a failed render: the item is released and rendered
// elsewhere, and the listing must not be left 'failed' in the meantime.
func TestProcessListing_ShutdownDuringRenderDoesNotMarkTheVideoFailed(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.render.setHook(func(ctx context.Context, _ *property.Property) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	})
	p := listing("ZP1", img.URL+"/a.jpg")

	_, err := h.s.processListing(ctx, &p, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context error", err)
	}
	if got := h.store.videoFailed(); len(got) != 0 {
		t.Errorf("video failed = %v, want none on shutdown", got)
	}
}

// The listing deadline running out IS a failed render, and by then the work's
// context is dead: the failure has to be recorded on a bookkeeping context.
func TestProcessListing_DeadlineDuringRenderMarksTheVideoFailed(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	ctx, expire := context.WithCancelCause(context.Background())
	defer expire(nil)
	h.render.setHook(func(ctx context.Context, _ *property.Property) error {
		expire(errListingDeadline) // what WithTimeoutCause does when the timer fires
		<-ctx.Done()
		return ctx.Err()
	})
	p := listing("ZP1", img.URL+"/a.jpg")

	if _, err := h.s.processListing(ctx, &p, false); err == nil {
		t.Fatal("want an error")
	}
	if got := h.store.videoFailed(); !reflect.DeepEqual(got, []string{"ZP1"}) {
		t.Errorf("video failed = %v, want [ZP1] recorded despite the dead context", got)
	}
}

func TestProcessListing_SegmentsBeforeTheVideoGoesReady(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	seg := &fakeSegmenter{}
	rec := h.enableHLS(seg)
	p := listing("ZP1", img.URL+"/a.jpg")

	if _, err := h.s.processListing(context.Background(), &p, false); err != nil {
		t.Fatal(err)
	}
	if seg.calls.Load() != 1 {
		t.Errorf("segmenter calls = %d, want 1", seg.calls.Load())
	}
	if base := rec.base("ZP1"); !strings.HasPrefix(base, "https://cdn.example/hls/v1/ZP1/") {
		t.Errorf("recorded base url = %q", base)
	}
	var segs, idx int
	for _, path := range h.bunny.paths() {
		switch {
		case strings.HasPrefix(path, "hls/v1/ZP1/") && strings.HasSuffix(path, ".ts"):
			segs++
		case strings.HasPrefix(path, "hls/v1/ZP1/") && strings.HasSuffix(path, "/"+hls.IndexName):
			idx++
		}
	}
	if segs != 2 || idx != 1 {
		t.Errorf("uploaded %d segments and %d index files, want 2 and 1: %v", segs, idx, h.bunny.paths())
	}
	// A shutdown between the two would otherwise strand a ready listing that
	// no channel can play and nothing retries.
	want := []string{"upsert:ZP1", "hls:ZP1", "ready:ZP1"}
	if got := h.events.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v: segments are recorded BEFORE the video goes ready", got, want)
	}
}

func TestProcessListing_WithoutHLSDoesNotSegment(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	p := listing("ZP1", img.URL+"/a.jpg")

	if _, err := h.s.processListing(context.Background(), &p, false); err != nil {
		t.Fatal(err)
	}
	if got := h.bunny.pathsWithPrefix("hls/"); len(got) != 0 {
		t.Errorf("unexpected hls uploads %v", got)
	}
}

// A clip that cannot be segmented is still a perfectly good VOD render: the
// MP4 is uploaded and the listing goes ready. Only the channels miss it, and
// cmd/backfill-hls retries later. Marking the video failed here would drop it
// from the Roku feed over a channels-only problem.
func TestProcessListing_SegmentFailureLeavesTheRenderReady(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	seg := &fakeSegmenter{hook: func(context.Context) error { return errors.New("segmenter exploded") }}
	rec := h.enableHLS(seg)
	p := listing("ZP1", img.URL+"/a.jpg")

	if _, err := h.s.processListing(context.Background(), &p, false); err != nil {
		t.Fatalf("a segmentation failure must stay non-fatal, got %v", err)
	}
	if seg.calls.Load() != 1 {
		t.Errorf("segmenter calls = %d, want 1", seg.calls.Load())
	}
	if got := h.store.ready(); len(got) != 1 {
		t.Errorf("SetVideoReady calls = %d, want 1 despite the segmentation failure", len(got))
	}
	if got := h.store.videoFailed(); len(got) != 0 {
		t.Errorf("SetVideoFailed calls = %d, want 0: the MP4 rendered fine", len(got))
	}
	if rec.count() != 0 {
		t.Error("nothing should be recorded when segmentation failed")
	}
	if got := h.bunny.pathsWithPrefix("hls/"); len(got) != 0 {
		t.Errorf("unexpected hls uploads %v after a segmentation failure", got)
	}
	if recs := h.logs.find("video segment failed"); len(recs) != 1 || recs[0].level != slog.LevelError {
		t.Errorf("want one Error about the segmentation, got %+v", recs)
	}
}

// When the context ends during segmentation the failure says nothing about
// the clip. Going ready now would strand it unsegmented, so the whole item is
// retried instead.
func TestProcessListing_CancelDuringSegmentationRetriesTheWholeItem(t *testing.T) {
	stages := map[string]func(h *harness, cancel context.CancelFunc) Segmenter{
		"while segmenting": func(_ *harness, cancel context.CancelFunc) Segmenter {
			return &fakeSegmenter{hook: func(ctx context.Context) error {
				cancel()
				return ctx.Err()
			}}
		},
		"while uploading segments": func(h *harness, cancel context.CancelFunc) Segmenter {
			h.bunny.onUpload = func(path string) {
				if strings.HasPrefix(path, "hls/") {
					cancel()
				}
			}
			return &fakeSegmenter{}
		},
	}
	for name, build := range stages {
		t.Run(name, func(t *testing.T) {
			img := jpegServer(t)
			h := newHarness(t, baseConfig())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec := h.enableHLS(build(h, cancel))
			p := listing("ZP1", img.URL+"/a.jpg")

			_, err := h.s.processListing(ctx, &p, false)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the context error", err)
			}
			if got := h.store.ready(); len(got) != 0 {
				t.Errorf("SetVideoReady called (%v) although segmentation was cut short", got)
			}
			if got := h.store.videoFailed(); len(got) != 0 {
				t.Errorf("SetVideoFailed called (%v): a shutdown is not a failed render", got)
			}
			if rec.count() != 0 {
				t.Error("no segments should be recorded")
			}
		})
	}
}

// The same guard, but reached through the listing's work deadline rather than
// a shutdown. With the context gone the segmentation failure says nothing
// about the clip, so the item is retried whole: going ready now would strand a
// listing that no channel can play and nothing ever segments, and marking the
// video failed would take a good render off the feed over a timeout.
func TestProcessListing_DeadlineDuringSegmentationRetriesTheWholeItem(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	ctx, expire := context.WithCancelCause(context.Background())
	defer expire(nil)
	rec := h.enableHLS(&fakeSegmenter{hook: func(ctx context.Context) error {
		expire(errListingDeadline) // what WithTimeoutCause does when the timer fires
		return ctx.Err()
	}})
	p := listing("ZP1", img.URL+"/a.jpg")

	_, err := h.s.processListing(ctx, &p, false)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context error returned", err)
	}
	if got := h.store.ready(); len(got) != 0 {
		t.Errorf("SetVideoReady called (%v) although segmentation was cut short", got)
	}
	if got := h.store.videoFailed(); len(got) != 0 {
		t.Errorf("SetVideoFailed called (%v): the clip was never judged", got)
	}
	if rec.count() != 0 {
		t.Error("no segments should be recorded")
	}
}

func TestContentTypeForExt(t *testing.T) {
	tests := map[string]string{".png": "image/png", ".PNG": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", "": "image/jpeg"}
	for ext, want := range tests {
		if got := contentTypeForExt(ext); got != want {
			t.Errorf("contentTypeForExt(%q) = %q, want %q", ext, got, want)
		}
	}
}

// nilGalleryStore records every Upsert handed a nil ImageURLs. The package's
// fakeStore cannot see it: it copies the slice with append(nil, ...).
type nilGalleryStore struct {
	*fakeStore
	mu         sync.Mutex
	nilGallery []string
}

func (s *nilGalleryStore) Upsert(ctx context.Context, p *property.Property) error {
	if p.ImageURLs == nil {
		s.mu.Lock()
		s.nilGallery = append(s.nilGallery, p.ZPID)
		s.mu.Unlock()
	}
	return s.fakeStore.Upsert(ctx, p)
}

// properties.image_urls is TEXT[] NOT NULL and pgx sends a nil slice as NULL.
// The provider reports "no photos" as nil (zillow.photoURLs), the payload
// round-trips it as null, and the old scheduler never upserted it because it
// always replaced the gallery with uploadPhotos' non-nil result. Handing nil
// to the real Upsert fails every photo-less listing three times into a dead
// queue row.
func TestProcessListing_PhotolessListingIsUpsertedWithAnEmptyNotANilGallery(t *testing.T) {
	h := newHarness(t, baseConfig()) // IMAGES_ENABLED=true, video on: production
	store := &nilGalleryStore{fakeStore: h.store}
	h.s.repo = store
	h.closeBreaker()

	p := listing("ZP1")
	p.ImageURLs = nil
	h.queue.put(t, p, false)

	h.s.drainQueue(context.Background())

	if got := h.store.upsertCount(); got != 1 {
		t.Fatalf("upserts = %d, want 1", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.nilGallery) != 0 {
		t.Errorf("Upsert was handed ImageURLs == nil for %v; want an empty, non-nil gallery", store.nilGallery)
	}
	if got := h.queue.historyOf("complete"); len(got) != 1 {
		t.Errorf("completions = %d, want the photo-less listing done for good", len(got))
	}
}
