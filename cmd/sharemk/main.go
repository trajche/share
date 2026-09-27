package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sharemk/internal/app"
	"sharemk/internal/config"
	"sharemk/internal/expiry"
	"sharemk/internal/s3client"
)

// version is set at build time via -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	// 1. Load configuration.
	cfg := config.Load()

	setupLogger(cfg.LogLevel)
	slog.Info("starting share.mk", "version", version)

	// 2. Build S3 client.
	s3Client, err := s3client.New(cfg)
	if err != nil {
		slog.Error("failed to create S3 client", "error", err)
		os.Exit(1)
	}

	// 3. Assemble tusd, hooks, downloads, management API and MCP.
	application, err := app.New(cfg, s3Client)
	if err != nil {
		slog.Error("failed to build application", "error", err)
		os.Exit(1)
	}

	// 4. Tag finished uploads with their expiry.
	ctx, cancel := context.WithCancel(context.Background())
	go application.ProcessCompletions(ctx)

	// 5. Start background expiry worker.
	expiryWorker := expiry.New(cfg, s3Client, application.Files)
	go expiryWorker.Start(ctx)

	// 6. HTTP server.
	httpServer := &http.Server{
		Addr:    cfg.ServerAddr,
		Handler: application.Handler,
		// Headers must arrive promptly (slowloris protection); bodies may
		// take as long as they need because large uploads are slow.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	// 7. Graceful shutdown on SIGTERM / SIGINT.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		slog.Info("server starting", "addr", cfg.ServerAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-quit
	slog.Info("shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Stop accepting requests first so uploads finishing during shutdown
	// still reach ProcessCompletions and get their expiry tag.
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}
	cancel()

	slog.Info("server stopped")
}

func setupLogger(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})))
}
