package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/dwellingtw/backend/internal/hls"
	"github.com/dwellingtw/backend/internal/imaging"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/video"
	"golang.org/x/sync/errgroup"
)

var (
	// errVideoDisabledRevisit is returned for a revisit item (render the
	// video of a stored listing) on a worker that renders no video. The item
	// is fine and so is the worker; it just has to go to another box.
	errVideoDisabledRevisit = errors.New("revisit item on a worker without video rendering")

	// errListingDeadline is the cancellation cause of a listing's work
	// deadline. It is what tells "this listing took too long" (a failed
	// render) from "the process is shutting down" (not one).
	errListingDeadline = errors.New("listing deadline exceeded")
)

// processListing handles a single property: download photos, upload images,
// persist, then render and upload its video. It returns skipped=true when the
// listing already exists and SkipExisting is set.
//
// revisit is the queue payload's flag (set by cmd/backfill-videos): the
// listing is known to be stored and only its video is wanted, whatever
// SkipExisting says.
//
// A nil error means the item is done for good. Every error is returned rather
// than swallowed: the caller decides between retry, release and the breaker.
func (s *Scheduler) processListing(ctx context.Context, p *property.Property, revisit bool) (bool, error) {
	if p.ZPID == "" {
		return false, fmt.Errorf("empty zpid (address %q)", p.Address)
	}
	videoWanted := s.videoWanted()

	// revisitForVideo means the listing is already stored and would normally be
	// left alone, but has no ready video. Without this, a listing whose render
	// failed once could never retry under SkipExisting: we would return here,
	// before renderVideo, on every subsequent pass. When set, we do the work
	// the render needs (download the photos) and nothing else — no re-upload of
	// images, no upsert.
	revisitForVideo := false

	switch {
	case revisit:
		if !videoWanted {
			return false, errVideoDisabledRevisit
		}
		// The media worker re-checks (spec 3.2). cmd/backfill-videos may have
		// enqueued this hours ago, and the listing may have got its video
		// since — from another instance, or from this very item's earlier run
		// whose Complete was lost. The revisit path never loads the stored
		// status and hash (there is no Upsert), so renderVideo's "unchanged,
		// skip" cannot catch it: without this the video would be rendered,
		// uploaded and segmented a second time. A listing that is no longer
		// stored at all reports the same and is skipped too.
		needsVideo, err := s.repo.NeedsVideo(ctx, p.ZPID)
		if err != nil {
			return false, fmt.Errorf("check listing video: %w", err)
		}
		if !needsVideo {
			s.log.Debug("skipping revisit of a listing that already has its video", "zpid", p.ZPID)
			return true, nil
		}
		revisitForVideo = true
	case s.cfg.SkipExisting:
		// Discovery filtered on the same predicate, but that was when the item
		// was enqueued: it may have been rendered since.
		exists, err := s.repo.Exists(ctx, p.ZPID)
		if err != nil {
			return false, fmt.Errorf("check listing exists: %w", err)
		}
		if exists {
			if videoWanted {
				if revisitForVideo, err = s.repo.NeedsVideo(ctx, p.ZPID); err != nil {
					return false, fmt.Errorf("check listing video: %w", err)
				}
			}
			if !revisitForVideo {
				s.log.Debug("skipping existing listing", "zpid", p.ZPID)
				return true, nil
			}
			s.log.Info("existing listing has no ready video, re-rendering", "zpid", p.ZPID)
		}
	}

	// No source photos at all is a fact about the listing, not a failure of
	// this box: it is stored (it still has a price and an address), its video
	// is marked failed, and the item is done. Retrying would change nothing.
	if len(p.ImageURLs) == 0 {
		return false, s.storeWithoutPhotos(ctx, p, revisitForVideo)
	}

	var workDir string
	var localPhotos []string
	if s.cfg.ImagesEnabled || videoWanted {
		var err error
		workDir, err = os.MkdirTemp("", "dwellings-"+p.ZPID+"-")
		if err != nil {
			return false, fmt.Errorf("create work dir: %w", err)
		}
		defer os.RemoveAll(workDir)

		localPhotos = s.downloadPhotos(ctx, p.ImageURLs, workDir)
		// The source has photos and we got none: a blocked IP, the photo CDN
		// down, a full disk. Storing the listing now would persist an empty
		// gallery, which SkipExisting then keeps forever.
		if len(localPhotos) == 0 {
			return false, fmt.Errorf("none of %d source photos could be downloaded", len(p.ImageURLs))
		}
		if len(localPhotos) < len(p.ImageURLs) {
			s.log.Warn("some photos could not be downloaded", "zpid", p.ZPID,
				"downloaded", len(localPhotos), "source", len(p.ImageURLs))
		}
	}

	// Upload images to Bunny (replacing source URLs) when enabled. On a
	// video-only revisit the stored photos are already on the CDN, so this and
	// the upsert below are skipped: the listing's data is not being refreshed,
	// only its missing video rendered.
	if s.cfg.ImagesEnabled && !revisitForVideo {
		cdnURLs := s.uploadPhotos(ctx, p.ZPID, localPhotos)
		if len(cdnURLs) == 0 {
			return false, fmt.Errorf("none of %d photos could be uploaded", len(localPhotos))
		}
		if len(cdnURLs) < len(localPhotos) {
			s.log.Warn("some photos could not be uploaded", "zpid", p.ZPID,
				"uploaded", len(cdnURLs), "downloaded", len(localPhotos))
		}
		p.ImageURLs = cdnURLs
	}

	if !revisitForVideo {
		// Persist; Upsert populates p.VideoStatus and p.VideoContentHash from the DB.
		if err := s.repo.Upsert(ctx, p); err != nil {
			return false, fmt.Errorf("upsert listing: %w", err)
		}
	}

	if videoWanted {
		if err := s.renderVideo(ctx, p, localPhotos, workDir); err != nil {
			return false, err
		}
	}
	return false, nil
}

// storeWithoutPhotos is the terminal path of a listing the provider has no
// photos for: stored like any other (unless this is a video-only revisit),
// and with nothing to render its video is marked failed.
func (s *Scheduler) storeWithoutPhotos(ctx context.Context, p *property.Property, revisitForVideo bool) error {
	if !revisitForVideo {
		// pgx sends a nil slice as NULL and properties.image_urls is NOT NULL.
		// The provider gives no photos as nil, so without this every
		// photo-less listing would fail its upsert.
		if p.ImageURLs == nil {
			p.ImageURLs = []string{}
		}
		if err := s.repo.Upsert(ctx, p); err != nil {
			return fmt.Errorf("upsert listing: %w", err)
		}
	}
	if !s.videoWanted() {
		return nil
	}
	hash := video.ContentHash(p, s.cfg.Video.SecondsPerPhoto, s.cfg.Video.FPS)
	if p.VideoStatus == property.VideoReady && p.VideoContentHash == hash {
		return nil // unchanged — skip
	}
	s.log.Warn("no photos to render video", "zpid", p.ZPID)
	if err := s.repo.SetVideoFailed(ctx, p.ZPID); err != nil {
		return fmt.Errorf("record video failed: %w", err)
	}
	return nil
}

// renderVideo renders, uploads, segments and records the listing video. It is
// idempotent: a ready video with an unchanged content hash is left alone.
//
// The order matters. Segments are cut, uploaded and recorded BEFORE the video
// is marked ready: a shutdown in between then leaves a listing that is simply
// not ready yet and is retried whole, instead of a ready listing no channel
// can play and nothing ever segments.
//
// Failures are returned. They also mark the video 'failed', except when the
// process is shutting down: a render that was killed is not a failed render.
func (s *Scheduler) renderVideo(ctx context.Context, p *property.Property, localPhotos []string, workDir string) error {
	hash := video.ContentHash(p, s.cfg.Video.SecondsPerPhoto, s.cfg.Video.FPS)
	if p.VideoStatus == property.VideoReady && p.VideoContentHash == hash {
		return nil // unchanged — skip
	}

	outPath := filepath.Join(workDir, "video.mp4")
	dur, err := s.render.Render(ctx, p, localPhotos, workDir, outPath)
	if err != nil {
		return s.videoFailed(ctx, p.ZPID, fmt.Errorf("render video: %w", err))
	}

	f, err := os.Open(outPath)
	if err != nil {
		return s.videoFailed(ctx, p.ZPID, fmt.Errorf("open rendered video: %w", err))
	}
	defer f.Close()

	cdnURL, err := s.bunny.Upload(ctx, path.Join("videos", p.ZPID+".mp4"), f, "video/mp4")
	if err != nil {
		return s.videoFailed(ctx, p.ZPID, fmt.Errorf("upload video: %w", err))
	}

	if s.segmenter != nil {
		if err := s.segmentVideo(ctx, p.ZPID, hash, outPath, workDir); err != nil {
			// With the context gone the failure says nothing about the clip.
			// Going ready now would strand it unsegmented, so the item is
			// retried whole instead.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("segment video: %w", ctxErr)
			}
			// A clip ffmpeg will not remux is still a good VOD render: only
			// the channels miss it, and cmd/backfill-hls retries it.
			s.log.Error("video segment failed", "zpid", p.ZPID, "error", err)
		}
	}

	if err := s.repo.SetVideoReady(ctx, p.ZPID, cdnURL, hash, dur); err != nil {
		return s.videoFailed(ctx, p.ZPID, fmt.Errorf("record video: %w", err))
	}
	s.log.Info("video ready", "zpid", p.ZPID, "url", cdnURL, "duration_secs", dur)
	return nil
}

// videoFailed records a failed render and hands cause back for returning.
// Nothing is written while shutting down: the item is about to be released
// and rendered elsewhere, and 'failed' would be a lie in the meantime. The
// write runs on a bookkeeping context because the usual reason to be here is
// the listing deadline, and then ctx is already dead.
func (s *Scheduler) videoFailed(ctx context.Context, zpid string, cause error) error {
	if shuttingDown(ctx) {
		return cause
	}
	bctx, cancel := s.bookkeeping(ctx)
	defer cancel()
	if err := s.repo.SetVideoFailed(bctx, zpid); err != nil {
		s.log.Error("mark video failed failed", "zpid", zpid, "error", err)
	}
	return cause
}

// shuttingDown reports whether ctx ended for a reason other than the listing's
// own work deadline, i.e. because whatever is above it was cancelled.
func shuttingDown(ctx context.Context) bool {
	return ctx.Err() != nil && !errors.Is(context.Cause(ctx), errListingDeadline)
}

// segmentVideo cuts the rendered MP4 into HLS segments, uploads them under an
// immutable prefix and records the layout.
func (s *Scheduler) segmentVideo(ctx context.Context, zpid, hash, mp4Path, workDir string) error {
	dir := filepath.Join(workDir, "hls")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("hls work dir: %w", err)
	}
	clip, err := s.segmenter.Segment(ctx, mp4Path, dir)
	if err != nil {
		return fmt.Errorf("segment: %w", err)
	}
	base, err := hls.Upload(ctx, safeUploader{s.bunny, s.log}, dir, hls.Prefix(zpid, hash), clip, s.cfg.Concurrency.Images)
	if err != nil {
		return fmt.Errorf("upload segments: %w", err)
	}
	if err := s.hls.SetVideoHLS(ctx, zpid, hash, base, clip); err != nil {
		return fmt.Errorf("record segments: %w", err)
	}
	s.log.Info("video segmented", "zpid", zpid, "segments", len(clip.SegmentMS), "base_url", base)
	return nil
}

// downloadPhotos fetches each source image into workDir concurrently, returning
// the local file paths in original order. Individual failures are logged and
// skipped (their slots are dropped).
func (s *Scheduler) downloadPhotos(ctx context.Context, srcURLs []string, workDir string) []string {
	results := make([]string, len(srcURLs))
	var g errgroup.Group
	g.SetLimit(s.cfg.Concurrency.Images)
	for idx, src := range srcURLs {
		g.Go(func() error {
			defer s.dropPhotoOnPanic("image download", src)
			data, err := s.download(ctx, src)
			if err != nil {
				s.log.Warn("image download failed", "src", src, "error", err)
				return nil
			}
			// Guarantee JPEG/PNG: pass through compliant images untouched,
			// transcode anything else, drop undecodable responses.
			data, ext, _, err := imaging.Normalize(data)
			if err != nil {
				s.log.Warn("image normalize failed", "src", src, "error", err)
				return nil
			}
			dest := filepath.Join(workDir, strconv.Itoa(idx)+ext)
			if err := os.WriteFile(dest, data, 0o644); err != nil {
				s.log.Warn("image write failed", "dest", dest, "error", err)
				return nil
			}
			results[idx] = dest
			return nil
		})
	}
	_ = g.Wait()
	return compact(results)
}

// uploadPhotos uploads local photo files to Bunny CDN concurrently, returning the
// CDN URLs in original order.
func (s *Scheduler) uploadPhotos(ctx context.Context, zpid string, localPhotos []string) []string {
	results := make([]string, len(localPhotos))
	var g errgroup.Group
	g.SetLimit(s.cfg.Concurrency.Images)
	for idx, lp := range localPhotos {
		g.Go(func() error {
			defer s.dropPhotoOnPanic("image upload", lp)
			f, err := os.Open(lp)
			if err != nil {
				s.log.Warn("open local image failed", "path", lp, "error", err)
				return nil
			}
			defer f.Close()
			dest := path.Join("properties", zpid, strconv.Itoa(idx)+filepath.Ext(lp))
			cdnURL, err := s.bunny.Upload(ctx, dest, f, contentTypeForExt(filepath.Ext(lp)))
			if err != nil {
				s.log.Warn("image upload failed", "zpid", zpid, "dest", dest, "error", err)
				return nil
			}
			results[idx] = cdnURL
			return nil
		})
	}
	_ = g.Wait()
	return compact(results)
}

// dropPhotoOnPanic recovers a panic raised in one photo's goroutine, logs it
// with its stack and drops that photo — exactly what happens to a photo that
// fails for any other reason.
//
// processListingSafely's recover only covers the item's own goroutine, and
// errgroup (x/sync) explicitly does not pass a panic on to Wait. So every
// goroutine the pipeline starts has to recover for itself, or the image the
// decoder chokes on kills the process: every other listing in flight is then
// left claimed for its whole 65 min lease with an attempt already spent, and
// the box that claims the poison listing next dies the same way.
func (s *Scheduler) dropPhotoOnPanic(what, src string) {
	if r := recover(); r != nil {
		s.log.Error(what+" panicked, photo dropped", "src", src,
			"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
	}
}

// safeUploader turns a panic inside an upload into an error. hls.Upload runs
// the segment uploads in errgroup goroutines of its own, which no recover of
// ours covers; wrapping the uploader puts the recover on those goroutines.
type safeUploader struct {
	up  uploader
	log *slog.Logger
}

func (u safeUploader) Upload(ctx context.Context, path string, content io.Reader, contentType string) (url string, err error) {
	defer func() {
		if r := recover(); r != nil {
			u.log.Error("upload panicked", "path", path,
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			url, err = "", fmt.Errorf("panic: %v", r)
		}
	}()
	return u.up.Upload(ctx, path, content, contentType)
}

// compact drops empty strings while preserving order.
func compact(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (s *Scheduler) download(ctx context.Context, src string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	res, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", res.StatusCode)
	}
	return io.ReadAll(res.Body)
}

// contentTypeForExt maps a local photo extension to its upload content type.
// downloadPhotos normalizes every photo to .jpg or .png before this runs.
func contentTypeForExt(ext string) string {
	if strings.ToLower(ext) == ".png" {
		return "image/png"
	}
	return "image/jpeg"
}
