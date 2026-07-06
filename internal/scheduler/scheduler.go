// Package scheduler runs the periodic property-collection cycle.
package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/video"
	"github.com/robfig/cron/v3"
	"golang.org/x/sync/errgroup"
)

// zillowSearcher discovers properties matching criteria.
type zillowSearcher interface {
	Search(ctx context.Context, s config.SearchCriteria) ([]property.Property, error)
}

// uploader stores content and returns its public CDN URL.
type uploader interface {
	Upload(ctx context.Context, path string, content io.Reader, contentType string) (string, error)
}

// store persists properties and their video state.
type store interface {
	Exists(ctx context.Context, zpid string) (bool, error)
	Upsert(ctx context.Context, p *property.Property) error
	SetVideoReady(ctx context.Context, zpid, videoURL, contentHash string, durationSecs int) error
	SetVideoFailed(ctx context.Context, zpid string) error
}

// Renderer turns a property + local photos into an MP4, returning its duration.
type Renderer interface {
	Render(ctx context.Context, p *property.Property, imagePaths []string, workDir, outPath string) (int, error)
}

// Scheduler wires the collection cycle together and runs it on a cron schedule.
type Scheduler struct {
	cfg    *config.Config
	zillow zillowSearcher
	bunny  uploader
	repo   store
	render Renderer
	http   *http.Client
	log    *slog.Logger
	cron   *cron.Cron
}

// New creates a Scheduler. render may be nil when video rendering is disabled.
func New(cfg *config.Config, z zillowSearcher, b uploader, repo store, render Renderer, log *slog.Logger) *Scheduler {
	return &Scheduler{
		cfg:    cfg,
		zillow: z,
		bunny:  b,
		repo:   repo,
		render: render,
		http:   &http.Client{Timeout: cfg.HTTPTimeout},
		log:    log,
		cron:   cron.New(),
	}
}

// Start schedules the cycle and runs one immediately. It is non-blocking.
func (s *Scheduler) Start(ctx context.Context) error {
	if _, err := s.cron.AddFunc(s.cfg.CronSchedule, func() {
		if err := s.RunCycle(ctx); err != nil {
			s.log.Error("collection cycle failed", "error", err)
		}
	}); err != nil {
		return fmt.Errorf("schedule cycle: %w", err)
	}
	s.cron.Start()

	go func() {
		if err := s.RunCycle(ctx); err != nil {
			s.log.Error("initial collection cycle failed", "error", err)
		}
	}()
	return nil
}

// Stop halts the cron scheduler, waiting for any running job to finish.
func (s *Scheduler) Stop() {
	ctx := s.cron.Stop()
	<-ctx.Done()
}

// RunCycle performs one full cycle: for each configured location, discover
// listings and process each. Locations run sequentially; a single location's
// search failure is logged and skipped so the others still run. Tallies
// aggregate across all locations.
func (s *Scheduler) RunCycle(ctx context.Context) error {
	start := time.Now()
	s.log.Info("collection cycle started", "locations", s.cfg.SearchLocations)

	var saved, skipped, failed atomic.Int64
	for _, loc := range s.cfg.SearchLocations {
		if ctx.Err() != nil {
			break // shutting down — stop searching new locations
		}
		criteria := s.cfg.Search
		criteria.Location = loc
		s.runLocation(ctx, criteria, &saved, &skipped, &failed)
	}

	s.log.Info("collection cycle finished",
		"saved", saved.Load(), "skipped", skipped.Load(), "failed", failed.Load(),
		"duration", time.Since(start).String())
	return nil
}

// runLocation discovers and processes all listings for a single location,
// adding to the shared tallies. A search failure is logged and returns without
// affecting other locations.
func (s *Scheduler) runLocation(ctx context.Context, criteria config.SearchCriteria, saved, skipped, failed *atomic.Int64) {
	props, err := s.zillow.Search(ctx, criteria)
	if err != nil {
		s.log.Error("search failed", "location", criteria.Location, "error", err)
		return
	}
	s.log.Info("properties discovered", "location", criteria.Location,
		"count", len(props), "listing_concurrency", s.cfg.Concurrency.Listings)

	// Process listings concurrently (each does a CPU-heavy render), bounded by
	// LISTING_CONCURRENCY. No errgroup context: one listing's failure must not
	// cancel its siblings, so each task handles its own error and we tally with
	// atomics.
	var g errgroup.Group
	g.SetLimit(s.cfg.Concurrency.Listings)
	for i := range props {
		if ctx.Err() != nil {
			break // shutting down — stop scheduling new work
		}
		p := &props[i]
		g.Go(func() error {
			wasSkipped, err := s.processListing(ctx, p)
			switch {
			case err != nil:
				s.log.Error("listing failed", "zpid", p.ZPID, "error", err)
				failed.Add(1)
			case wasSkipped:
				skipped.Add(1)
			default:
				saved.Add(1)
			}
			return nil
		})
	}
	_ = g.Wait()
}

// processListing handles a single property: download photos, upload images,
// persist, then render and upload its video. It returns skipped=true when the
// listing already exists and SkipExisting is set.
func (s *Scheduler) processListing(ctx context.Context, p *property.Property) (bool, error) {
	if p.ZPID == "" {
		return false, fmt.Errorf("empty zpid (address %q)", p.Address)
	}

	if s.cfg.SkipExisting {
		exists, err := s.repo.Exists(ctx, p.ZPID)
		if err != nil {
			return false, err
		}
		if exists {
			s.log.Debug("skipping existing listing", "zpid", p.ZPID)
			return true, nil
		}
	}

	needLocalPhotos := s.cfg.ImagesEnabled || (s.cfg.Video.Enabled && s.render != nil)

	var workDir string
	var localPhotos []string
	if needLocalPhotos {
		var err error
		workDir, err = os.MkdirTemp("", "dwellings-"+p.ZPID+"-")
		if err != nil {
			return false, fmt.Errorf("create work dir: %w", err)
		}
		defer os.RemoveAll(workDir)
		localPhotos = s.downloadPhotos(ctx, p.ImageURLs, workDir)
	}

	// Upload images to Bunny (replacing source URLs) when enabled.
	if s.cfg.ImagesEnabled {
		p.ImageURLs = s.uploadPhotos(ctx, p.ZPID, localPhotos)
	}

	// Persist; Upsert populates p.VideoStatus and p.VideoContentHash from the DB.
	if err := s.repo.Upsert(ctx, p); err != nil {
		return false, err
	}

	// Render + upload the video.
	if s.cfg.Video.Enabled && s.render != nil {
		s.renderVideo(ctx, p, localPhotos, workDir)
	}
	return false, nil
}

// renderVideo renders, uploads, and records the listing video. It is idempotent:
// a ready video with an unchanged content hash is left alone. Failures are
// logged and recorded as 'failed' (retried on a later cycle), never fatal.
func (s *Scheduler) renderVideo(ctx context.Context, p *property.Property, localPhotos []string, workDir string) {
	hash := video.ContentHash(p, s.cfg.Video.SecondsPerPhoto)
	if p.VideoStatus == property.VideoReady && p.VideoContentHash == hash {
		return // unchanged — skip
	}
	if len(localPhotos) == 0 {
		s.log.Warn("no photos to render video", "zpid", p.ZPID)
		_ = s.repo.SetVideoFailed(ctx, p.ZPID)
		return
	}

	outPath := filepath.Join(workDir, "video.mp4")
	dur, err := s.render.Render(ctx, p, localPhotos, workDir, outPath)
	if err != nil {
		s.log.Error("video render failed", "zpid", p.ZPID, "error", err)
		_ = s.repo.SetVideoFailed(ctx, p.ZPID)
		return
	}

	f, err := os.Open(outPath)
	if err != nil {
		s.log.Error("open rendered video", "zpid", p.ZPID, "error", err)
		_ = s.repo.SetVideoFailed(ctx, p.ZPID)
		return
	}
	defer f.Close()

	cdnURL, err := s.bunny.Upload(ctx, path.Join("videos", p.ZPID+".mp4"), f, "video/mp4")
	if err != nil {
		s.log.Error("video upload failed", "zpid", p.ZPID, "error", err)
		_ = s.repo.SetVideoFailed(ctx, p.ZPID)
		return
	}

	if err := s.repo.SetVideoReady(ctx, p.ZPID, cdnURL, hash, dur); err != nil {
		s.log.Error("record video failed", "zpid", p.ZPID, "error", err)
		return
	}
	s.log.Info("video ready", "zpid", p.ZPID, "url", cdnURL, "duration_secs", dur)
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
			data, contentType, err := s.download(ctx, src)
			if err != nil {
				s.log.Warn("image download failed", "src", src, "error", err)
				return nil
			}
			dest := filepath.Join(workDir, strconv.Itoa(idx)+imageExt(src, contentType))
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

func (s *Scheduler) download(ctx context.Context, src string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, "", err
	}
	res, err := s.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("status %d", res.StatusCode)
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, "", err
	}
	return data, res.Header.Get("Content-Type"), nil
}

func imageExt(src, contentType string) string {
	if ext := path.Ext(strings.SplitN(path.Base(src), "?", 2)[0]); ext != "" {
		return ext
	}
	switch {
	case strings.Contains(contentType, "png"):
		return ".png"
	case strings.Contains(contentType, "webp"):
		return ".webp"
	default:
		return ".jpg"
	}
}

func contentTypeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}
