package scheduler

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dwellingtw/backend/internal/property"
)

// The photos a listing was rendered from are purged from the CDN once its
// video is ready — and only then: a failed render keeps its photos for the
// retry, and the purge runs after SetVideoReady so a crash in between leaves a
// listing that is ready with its full gallery, which the sweep finishes.
func TestProcessListing_PurgesThePhotosOnceTheVideoIsReady(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	readyWhenPurged := false
	h.purger.onPurge = func(zpid string) {
		readyWhenPurged = reflect.DeepEqual(h.store.ready(), []string{"ZP1"})
	}
	p := listing("ZP1", img.URL+"/a.jpg", img.URL+"/b.jpg")

	if _, err := h.s.processListing(context.Background(), &p, false); err != nil {
		t.Fatalf("processListing: %v", err)
	}
	if got := h.purger.purged(); !reflect.DeepEqual(got, []string{"ZP1"}) {
		t.Fatalf("purged = %v, want [ZP1]", got)
	}
	if !readyWhenPurged {
		t.Error("purge ran before the video was recorded as ready")
	}
}

func TestProcessListing_AFailedRenderKeepsItsPhotos(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	h.render.hook = func(context.Context, *property.Property) error { return errors.New("ffmpeg exited 1") }
	p := listing("ZP1", img.URL+"/a.jpg", img.URL+"/b.jpg")

	if _, err := h.s.processListing(context.Background(), &p, false); err == nil {
		t.Fatal("processListing = nil, want the render error")
	}
	if got := h.purger.purged(); len(got) != 0 {
		t.Errorf("purged = %v, want none", got)
	}
}

// The purge is housekeeping. The listing is stored, rendered and on the feed;
// failing the item now would re-render it for nothing, and the sweep gets the
// photos later.
func TestProcessListing_APurgeFailureDoesNotFailTheListing(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	h.purger.err = errors.New("bunny delete returned status 500")
	p := listing("ZP1", img.URL+"/a.jpg", img.URL+"/b.jpg")

	skipped, err := h.s.processListing(context.Background(), &p, false)
	if err != nil || skipped {
		t.Fatalf("processListing = (%v, %v), want (false, nil)", skipped, err)
	}
	if got := h.store.ready(); !reflect.DeepEqual(got, []string{"ZP1"}) {
		t.Errorf("video ready = %v, want [ZP1]", got)
	}
	if len(h.logs.find("photo purge failed")) == 0 {
		t.Error("purge failure not logged")
	}
}

// A shutdown that lands between the first delete and the trim must not leave
// a row that lists objects that are gone: under SKIP_EXISTING nothing ever
// visits the listing again, and the public would read dead links until a
// manual sweep. The purge therefore runs detached from the shutdown, and
// Stop waits for it like for every item goroutine.
func TestProcessListing_ThePurgeOutlivesAShutdown(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.purger.onPurge = func(string) { cancel() } // SIGTERM while purging
	p := listing("ZP1", img.URL+"/a.jpg", img.URL+"/b.jpg")

	if _, err := h.s.processListing(ctx, &p, false); err != nil {
		t.Fatalf("processListing: %v", err)
	}
	if got := h.purger.purged(); !reflect.DeepEqual(got, []string{"ZP1"}) {
		t.Errorf("purged = %v, want [ZP1]: the purge was abandoned on shutdown", got)
	}
}

// A revisit (render only) purges too: the worker holds the provider's source
// URLs, but the purger reads the row, where the CDN gallery is.
func TestProcessListing_ARevisitPurgesTheStoredGallery(t *testing.T) {
	img := jpegServer(t)
	cfg := baseConfig()
	cfg.SkipExisting = true
	h := newHarness(t, cfg)
	h.store.existing = map[string]bool{"ZP1": true}
	h.store.needsVideo = map[string]bool{"ZP1": true}
	p := listing("ZP1", img.URL+"/a.jpg", img.URL+"/b.jpg")

	if _, err := h.s.processListing(context.Background(), &p, false); err != nil {
		t.Fatalf("processListing: %v", err)
	}
	if got := h.purger.purged(); !reflect.DeepEqual(got, []string{"ZP1"}) {
		t.Errorf("purged = %v, want [ZP1]", got)
	}
}

// Without video there is nothing the photos are "done" for.
func TestProcessListing_NoPurgeWithoutVideo(t *testing.T) {
	img := jpegServer(t)
	cfg := baseConfig()
	cfg.Video.Enabled = false
	h := newHarness(t, cfg)
	p := listing("ZP1", img.URL+"/a.jpg", img.URL+"/b.jpg")

	if _, err := h.s.processListing(context.Background(), &p, false); err != nil {
		t.Fatalf("processListing: %v", err)
	}
	if got := h.purger.purged(); len(got) != 0 {
		t.Errorf("purged = %v, want none", got)
	}
}

// PHOTO_PURGE=false leaves Deps.Purger nil, and the pipeline must simply not
// purge.
func TestProcessListing_NoPurgerConfigured(t *testing.T) {
	img := jpegServer(t)
	h := newHarness(t, baseConfig())
	h.s.purger = nil
	p := listing("ZP1", img.URL+"/a.jpg", img.URL+"/b.jpg")

	if _, err := h.s.processListing(context.Background(), &p, false); err != nil {
		t.Fatalf("processListing: %v", err)
	}
	if got := h.store.ready(); !reflect.DeepEqual(got, []string{"ZP1"}) {
		t.Errorf("video ready = %v, want [ZP1]", got)
	}
}
