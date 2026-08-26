package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/api"
	"github.com/dwellingtw/backend/internal/bunny"
	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/geo"
	"github.com/dwellingtw/backend/internal/hls"
	"github.com/dwellingtw/backend/internal/linear"
	"github.com/dwellingtw/backend/internal/locationiq"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/propertymap"
	"github.com/dwellingtw/backend/internal/scheduler"
	"github.com/dwellingtw/backend/internal/server"
	"github.com/dwellingtw/backend/internal/video"
	"github.com/dwellingtw/backend/internal/viewer"
	"github.com/dwellingtw/backend/internal/zillow"
	"github.com/dwellingtw/backend/internal/zipcode"
	"github.com/dwellingtw/backend/internal/zipseed"
)

// @title        Dwellings API
// @version      1.0
// @description  Public read-only API for browsing Dwellings property listings.
// @host         api.dwellings.tv
// @BasePath     /
func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	log.Info("database ready")

	seeded, err := zipseed.Seed(ctx, pool)
	if err != nil {
		return fmt.Errorf("seed zip codes: %w", err)
	}
	if seeded > 0 {
		log.Info("zip rotation table seeded", "zips", seeded)
	}

	zillowClient := zillow.New(cfg.ZillowBaseURL, cfg.ZillowAPIKey, cfg.HTTPTimeout)
	logZillowQuota(ctx, zillowClient, log)
	bunnyClient := bunny.New(cfg.BunnyStorageZone, cfg.BunnyAPIKey, cfg.BunnyStorageHost, cfg.BunnyCDNBaseURL, cfg.BunnyTimeout)
	repo := property.NewRepository(pool)

	// Property maps are optional: without a LocationIQ key the service stays
	// nil and the detail endpoint simply returns null map_image_url/map_image_dark_url.
	var mapSvc *propertymap.Service
	if cfg.LocationIQAPIKey != "" {
		mapSvc = propertymap.New(
			locationiq.New(cfg.LocationIQAPIKey, cfg.HTTPTimeout),
			bunnyClient, repo, log,
		)
		log.Info("property maps enabled")
	}

	var renderer *video.Renderer
	if cfg.Video.Enabled {
		renderer, err = video.New(cfg.Video)
		if err != nil {
			return err
		}
		log.Info("video rendering enabled", "music_tracks", renderer.TrackCount(), "seconds_per_photo", cfg.Video.SecondsPerPhoto)
	}

	// nil-safe: pass a typed-nil renderer through as an untyped nil when disabled.
	zipRepo := zipcode.NewRepository(pool)
	sched := scheduler.New(cfg, zillowClient, bunnyClient, repo, zipRepo, rendererOrNil(renderer), log)

	// Linear channels: segment new renders and serve the channel endpoints.
	var linearRepo *linear.Repository
	if cfg.Linear.Enabled {
		linearRepo = linear.NewRepository(pool)
		if cfg.Video.Enabled {
			sched.EnableHLS(hls.NewSegmenter(), linearRepo)
		}
	}

	if err := sched.Start(ctx); err != nil {
		return err
	}
	log.Info("scheduler started", "schedule", cfg.CronSchedule)

	publicAPI := api.New(repo, mapEnsurerOrNil(mapSvc), log)
	httpSrv := server.New(net.JoinHostPort("", cfg.HTTPPort), "DwellingTV", repo, publicAPI, log)
	if linearRepo != nil {
		svc := linear.New(linearRepo, linear.Options{
			LineupHours:     cfg.Linear.LineupHours,
			MinScopeClips:   cfg.Linear.MinScopeClips,
			EPGHorizonHours: cfg.Linear.EPGHorizonHours,
		}, log)
		h := linear.NewHandler(svc, log)
		if cfg.Viewer.Enabled {
			h.EnableViewers(viewerOptions(ctx, cfg, pool, log))
		}
		httpSrv.Mount(h)
		if cfg.PublicBaseURL != "" {
			httpSrv.SetLiveFeed(cfg.PublicBaseURL+"/channels/master.m3u8", cfg.Linear.LiveThumbnailURL, svc.HasContent)
		}
		log.Info("linear channels enabled", "lineup_hours", cfg.Linear.LineupHours, "min_scope_clips", cfg.Linear.MinScopeClips)
		warnLinearSetup(ctx, cfg, linearRepo, renderer, log)
	}
	go func() {
		log.Info("http server started", "port", cfg.HTTPPort)
		if err := httpSrv.Start(); err != nil {
			log.Error("http server stopped", "error", err)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	sched.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	return nil
}

// viewerOptions wires viewer tracking: the heartbeat recorder (flushed in
// the background until ctx ends), the geo database when configured, and the
// resolve/stats endpoints.
func viewerOptions(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger) linear.ViewerOptions {
	repo := viewer.NewRepository(pool)
	rec := viewer.NewRecorder(repo, viewer.Options{
		Retention: time.Duration(cfg.Viewer.RetentionDays) * 24 * time.Hour,
	}, log)
	go rec.Run(ctx)
	o := linear.ViewerOptions{
		Hasher:         viewer.NewHasher(cfg.Viewer.Salt, cfg.Viewer.RotateDaily),
		Tracker:        rec,
		Audience:       repo,
		PublicBaseURL:  cfg.PublicBaseURL,
		LastWatchedTTL: time.Duration(cfg.Viewer.RetentionDays) * 24 * time.Hour,
	}
	if cfg.Viewer.GeoIPDBPath != "" {
		g, err := geo.Open(cfg.Viewer.GeoIPDBPath)
		if err != nil {
			log.Warn("geoip disabled: database unreadable; /channels/resolve will not use IP location", "path", cfg.Viewer.GeoIPDBPath, "error", err)
		} else {
			o.Geo = g
		}
	}
	log.Info("viewer tracking enabled", "rotate_daily", cfg.Viewer.RotateDaily, "retention_days", cfg.Viewer.RetentionDays, "geoip", o.Geo != nil)
	return o
}

// warnLinearSetup surfaces the configurations in which the linear channels
// are mounted but cannot actually serve anything, at startup rather than as a
// silent empty stream.
func warnLinearSetup(ctx context.Context, cfg *config.Config, repo *linear.Repository, renderer *video.Renderer, log *slog.Logger) {
	if !cfg.Video.Enabled {
		log.Warn("linear channels are enabled but VIDEO_ENABLED=false: the channel routes are mounted, but no new render will ever be segmented")
	}
	if cfg.PublicBaseURL == "" {
		log.Warn("linear channels are enabled but PUBLIC_BASE_URL is empty: the Roku feed will not advertise the live channel")
	}
	if cfg.Video.Enabled && renderer != nil && renderer.TrackCount() == 0 {
		log.Warn("linear channels are enabled but no music tracks were found: renders have no audio track and segmentation rejects them", "music_dir", cfg.Video.MusicDir)
	}
	countCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	n, err := repo.CountCurrentClips(countCtx)
	switch {
	case err != nil:
		log.Warn("could not count segmented clips", "error", err)
	case n == 0:
		log.Warn("linear channels are enabled but no listing video has been segmented yet; run cmd/backfill-hls to fill the library")
	default:
		log.Info("linear channel library", "current_clips", n)
	}
}

// logZillowQuota queries the provider's usage endpoint and logs the remaining
// request quota at startup so quota exhaustion is visible without SSH. Failures
// are logged as a warning and never block startup.
func logZillowQuota(ctx context.Context, z *zillow.Client, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	usage, err := z.Usage(ctx)
	if err != nil {
		log.Warn("zillow quota check failed", "error", err)
		return
	}
	attrs := []any{"plan", usage.Plan.Nickname, "status", usage.Status}
	for _, q := range usage.Quotas {
		attrs = append(attrs,
			q.Name+"_limit", q.Limit,
			q.Name+"_used", q.Used,
			q.Name+"_remaining", q.Remaining,
			q.Name+"_reset_at", q.ResetAt,
		)
	}
	if usage.Status == "exceeded" {
		log.Warn("zillow quota exceeded", attrs...)
	} else {
		log.Info("zillow quota", attrs...)
	}
}

// rendererOrNil returns an interface-nil when the concrete renderer is nil, so
// the scheduler's nil check works correctly.
func rendererOrNil(r *video.Renderer) scheduler.Renderer {
	if r == nil {
		return nil
	}
	return r
}

// mapEnsurerOrNil returns an interface-nil when maps are disabled, so the API's
// nil check works correctly.
func mapEnsurerOrNil(s *propertymap.Service) api.MapEnsurer {
	if s == nil {
		return nil
	}
	return s
}
