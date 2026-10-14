package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rfbatista/spillway/internal/configsource"
	"github.com/rfbatista/spillway/internal/infrastructure"
	"github.com/rfbatista/spillway/internal/proxy"
	"github.com/rfbatista/spillway/internal/quote"
	"github.com/rfbatista/spillway/internal/repositories"
)

func main() {
	cfg := infrastructure.NewConfig()
	brd := infrastructure.NewBroadcaster()
	modelrepo := repositories.NewInMemoryModelRepository()

	saveusage, err := quote.NewSaveUsageService(modelrepo, brd)
	if err != nil {
		log.Fatal("missing save usage service", err)
	}
	getusage, err := quote.NewGetUsageService(modelrepo, brd)
	if err != nil {
		log.Fatal("missing get usage service", err)
	}

	reqBroadcaster := infrastructure.NewRequestBroadcaster()
	tracker := proxy.NewRequestTracker(reqBroadcaster)

	handler, err := proxy.NewHandler(slog.With(slog.String("module", "proxy")))
	if err != nil {
		log.Fatal("cant create the proxy handler", err)
	}

	// One QueueManager for the process lifetime, shared by every pool generation.
	// A per-pool manager would strand its worker goroutines on each config
	// rebuild — that was the source of a ~4MB/hour memory leak.
	queueManager := proxy.NewQueueManager()

	// Build and set the initial pool using the chosen ConfigSource.
	buildPool := func(cfgs []proxy.BackendConfig) error {
		pool, err := proxy.NewBackendPool(saveusage, getusage, cfgs, tracker, queueManager)
		if err != nil {
			return fmt.Errorf("building backend pool: %w", err)
		}
		handler.SetPool(pool)
		return nil
	}

	src, pollInterval := selectSource()

	cfgs, err := bootLoad(src)
	if err != nil {
		log.Fatalf("failed to load initial config: %v", err)
	}
	if err := buildPool(cfgs); err != nil {
		log.Fatalf("failed to build initial pool: %v", err)
	}
	slog.Info("initial config loaded", slog.Int("backends", len(cfgs)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	heartbeatInterval := 30 * time.Second
	if raw := os.Getenv("HEARTBEAT_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			heartbeatInterval = d
		} else {
			slog.Warn("invalid HEARTBEAT_INTERVAL, using default 30s", slog.String("raw", raw))
		}
	}
	go proxy.NewHeartbeat(queueManager, heartbeatInterval).Run(ctx)

	if pollInterval > 0 {
		watcher := configsource.NewWatcher(src, pollInterval, buildPool)
		// The pool was already built from cfgs above; seed the watcher so an
		// unchanged config on the first tick doesn't trigger a redundant rebuild.
		watcher.Seed(cfgs)
		go watcher.Run(ctx)
	}

	adminPort := os.Getenv("ADMIN_PORT")
	if adminPort == "" {
		adminPort = "9090"
	}
	adminServer := infrastructure.NewAdminServer(":"+adminPort, handler.GetPool)
	go func() {
		slog.Info("admin server started", slog.String("addr", ":"+adminPort))
		if err := adminServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("admin server error", slog.Any("err", err))
		}
	}()

	server := infrastructure.NewServer(cfg, brd, reqBroadcaster, handler)

	go func() {
		<-ctx.Done()
		slog.Info("shutting down gracefully...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("proxy shutdown error", slog.Any("err", err))
		}
		if err := adminServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("admin shutdown error", slog.Any("err", err))
		}
	}()

	slog.Info("proxy started", slog.String("port", cfg.Port))
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// selectSource returns the ConfigSource to use and the Watcher poll interval (0 = no polling).
// If APPCONFIG_APP_ID is set, it uses AppConfigAgentSource with polling; otherwise EnvConfigSource.
func selectSource() (configsource.ConfigSource, time.Duration) {
	appID := os.Getenv("APPCONFIG_APP_ID")
	if appID == "" {
		slog.Info("APPCONFIG_APP_ID not set — using PROXY_CONFIG env var as config source")
		return configsource.NewEnvConfigSource(), 0
	}

	envID := os.Getenv("APPCONFIG_ENV_ID")
	profileID := os.Getenv("APPCONFIG_PROFILE_ID")
	if envID == "" || profileID == "" {
		log.Fatal("APPCONFIG_APP_ID is set but APPCONFIG_ENV_ID and/or APPCONFIG_PROFILE_ID are missing")
	}

	pollInterval := 30 * time.Second
	if raw := os.Getenv("APPCONFIG_POLL_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			pollInterval = d
		} else {
			slog.Warn("invalid APPCONFIG_POLL_INTERVAL, using default 30s", slog.String("raw", raw))
		}
	}

	src := configsource.NewAppConfigAgentSource(appID, envID, profileID)
	slog.Info("AppConfig mode enabled",
		slog.String("app_id", appID),
		slog.String("env_id", envID),
		slog.String("profile_id", profileID),
		slog.Duration("poll_interval", pollInterval),
	)
	return src, pollInterval
}

// bootLoad retries source.Load until it succeeds or APPCONFIG_BOOT_TIMEOUT expires.
func bootLoad(src configsource.ConfigSource) ([]proxy.BackendConfig, error) {
	bootTimeout := 10 * time.Second
	if raw := os.Getenv("APPCONFIG_BOOT_TIMEOUT"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			bootTimeout = d
		} else {
			slog.Warn("invalid APPCONFIG_BOOT_TIMEOUT, using default 10s", slog.String("raw", raw))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), bootTimeout)
	defer cancel()

	const retryInterval = 500 * time.Millisecond
	for {
		cfgs, err := src.Load(ctx)
		if err == nil {
			return cfgs, nil
		}
		slog.Warn("config load failed, retrying", slog.Any("error", err), slog.Duration("retry_in", retryInterval))
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("boot timeout after %s: %w", bootTimeout, err)
		case <-time.After(retryInterval):
		}
	}
}
