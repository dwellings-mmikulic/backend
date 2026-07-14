package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dwellingtw/backend/internal/bunny"
	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/db"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/scheduler"
	"github.com/dwellingtw/backend/internal/server"
	"github.com/dwellingtw/backend/internal/video"
	"github.com/dwellingtw/backend/internal/zillow"
)

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

	zillowClient := zillow.New(cfg.ZillowBaseURL, cfg.ZillowAPIKey, cfg.HTTPTimeout)
	logZillowQuota(ctx, zillowClient, log)
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

	// nil-safe: pass a typed-nil renderer through as an untyped nil when disabled.
	sched := scheduler.New(cfg, zillowClient, bunnyClient, repo, rendererOrNil(renderer), log)
	if err := sched.Start(ctx); err != nil {
		return err
	}
	log.Info("scheduler started", "schedule", cfg.CronSchedule)

	httpSrv := server.New(net.JoinHostPort("", cfg.HTTPPort), "DwellingTV", repo, log)
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
