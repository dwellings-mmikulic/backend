package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/api"
	"github.com/dwellingtw/backend/internal/budget"
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
	"github.com/dwellingtw/backend/internal/workqueue"
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
	// Several instances write to one log aggregate; every line says which.
	log = log.With("instance", cfg.InstanceID)

	// CRON_SCHEDULE defines the budget windows of the whole fleet, so a bad
	// one stops every role at startup rather than the first time it is asked.
	windows, err := budget.ParseWindows(cfg.CronSchedule)
	if err != nil {
		return fmt.Errorf("CRON_SCHEDULE %q: %w", cfg.CronSchedule, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
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

	windowStart, windowNext := windows.Current(time.Now())
	// One greppable line per boot: the budget variables must be identical on
	// every box, and this is how drift between them is found.
	log.Info("effective fleet config",
		"role", cfg.Role, "schedule", cfg.CronSchedule,
		"window_start", windowStart, "window_next", windowNext,
		"api_budget_per_window", cfg.APIBudgetPerCycle, "details_per_window", cfg.DetailsPerCycle,
		"queue_high_water", cfg.QueueHighWater, "db_max_conns", cfg.DBMaxConns,
		"listing_concurrency", cfg.Concurrency.Listings, "skip_existing", cfg.SkipExisting)

	bunnyClient := bunny.New(cfg.BunnyStorageZone, cfg.BunnyAPIKey, cfg.BunnyStorageHost, cfg.BunnyCDNBaseURL, cfg.BunnyTimeout)
	repo := property.NewRepository(pool)

	var renderer *video.Renderer
	if cfg.Video.Enabled {
		renderer, err = video.New(cfg.Video)
		if err != nil {
			return err
		}
		log.Info("video rendering enabled", "music_tracks", renderer.TrackCount(), "seconds_per_photo", cfg.Video.SecondsPerPhoto)
	}

	var sched *scheduler.Scheduler
	if cfg.Role.RunsWorkers() {
		sched, err = startWorkers(ctx, cfg, pool, repo, bunnyClient, renderer, windows, log)
		if err != nil {
			return err
		}
	}

	var shutdownHTTP func(context.Context) error
	if cfg.Role.ServesAPI() {
		shutdownHTTP = startAPI(ctx, cfg, pool, repo, bunnyClient, renderer, log)
	} else {
		shutdownHTTP = startHealth(cfg, sched, log)
	}

	<-ctx.Done()
	log.Info("shutting down")
	if sched != nil {
		// Waits for the loops: they stop claiming, cancel their renders and
		// hand every claim back, so other instances pick the work up at once.
		sched.Stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = shutdownHTTP(shutdownCtx)
	return nil
}

// startWorkers wires and starts the discovery, media and details loops.
func startWorkers(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, repo *property.Repository,
	bunnyClient *bunny.Client, renderer *video.Renderer, windows *budget.Windows, log *slog.Logger) (*scheduler.Scheduler, error) {
	zillowClient := zillow.New(cfg.ZillowBaseURL, cfg.ZillowAPIKey, cfg.HTTPTimeout)
	logZillowQuota(ctx, zillowClient, log)

	owner, err := claimOwner(cfg.InstanceID)
	if err != nil {
		return nil, err
	}
	sched := scheduler.New(cfg, scheduler.Deps{
		Zillow:  zillowClient,
		Bunny:   bunnyClient,
		Repo:    repo,
		Zips:    zipcode.NewRepository(pool),
		Queue:   workqueue.NewRepository(pool),
		Ledger:  budget.NewLedger(pool),
		Windows: windows,
		// nil-safe: a typed-nil renderer becomes an untyped nil when disabled.
		Render: rendererOrNil(renderer),
	}, owner, log)

	// Workers segment every new render for the linear channels whether or
	// not this instance serves them: LINEAR_ENABLED is what turns it on.
	if cfg.Linear.Enabled && cfg.Video.Enabled {
		sched.EnableHLS(hls.NewSegmenter(), linear.NewRepository(pool))
	}
	if err := sched.Start(ctx); err != nil {
		return nil, err
	}
	log.Info("worker loops started", "owner", owner)
	return sched, nil
}

// claimOwner names this process in every claim it takes. The random suffix
// makes it unique per boot, so two boxes given the same INSTANCE_ID, or a
// restarted process and its dead predecessor, can never pass each other's
// ownership guards.
func claimOwner(instanceID string) (string, error) {
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("claim owner nonce: %w", err)
	}
	return instanceID + "/" + hex.EncodeToString(nonce[:]), nil
}

// startAPI starts the public HTTP server (API, Roku feed, linear channels)
// and returns its shutdown function.
func startAPI(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, repo *property.Repository,
	bunnyClient *bunny.Client, renderer *video.Renderer, log *slog.Logger) func(context.Context) error {
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

	publicAPI := api.New(repo, mapEnsurerOrNil(mapSvc), log)
	publicAPI.SetAds(cfg.Ads.PrerollURL, cfg.Ads.MidrollURL)
	log.Info("ad tags", "pre_roll", cfg.Ads.PrerollURL != "", "mid_roll", cfg.Ads.MidrollURL != "")
	httpSrv := server.New(net.JoinHostPort("", cfg.HTTPPort), "DwellingTV", repo, publicAPI, log)
	if cfg.Linear.Enabled {
		linearRepo := linear.NewRepository(pool)
		svc := linear.New(linearRepo, linear.Options{
			LineupHours:     cfg.Linear.LineupHours,
			MinScopeClips:   cfg.Linear.MinScopeClips,
			EPGHorizonHours: cfg.Linear.EPGHorizonHours,
		}, log)
		h := linear.NewHandler(svc, log)
		h.SetAds(cfg.Ads.PrerollURL, cfg.Ads.MidrollURL)
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
	return httpSrv.Shutdown
}

// startHealth serves /healthz and nothing else: a worker has no public
// surface. It answers 503 while the worker's circuit breaker is open, which
// is how a box that cannot render shows up in `docker ps`.
func startHealth(cfg *config.Config, sched *scheduler.Scheduler, log *slog.Logger) func(context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if sched != nil && !sched.Healthy() {
			http.Error(w, "circuit breaker open", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{
		Addr:              net.JoinHostPort("", cfg.HTTPPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("health server started", "port", cfg.HTTPPort)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server stopped", "error", err)
		}
	}()
	return srv.Shutdown
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
		Clients:        repo,
		AdminKey:       cfg.Viewer.AdminKey,
	}
	if cfg.Viewer.GeoIPDBPath != "" {
		g, err := geo.Open(cfg.Viewer.GeoIPDBPath)
		if err != nil {
			log.Warn("geoip disabled: database unreadable; /channels/resolve will not use IP location", "path", cfg.Viewer.GeoIPDBPath, "error", err)
		} else {
			o.Geo = g
		}
	}
	log.Info("viewer tracking enabled", "rotate_daily", cfg.Viewer.RotateDaily, "retention_days", cfg.Viewer.RetentionDays, "geoip", o.Geo != nil, "admin_viewers", o.AdminKey != "")
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
