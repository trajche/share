package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"
	"github.com/tus/tusd/v2/pkg/s3store"
	"sharemk/internal/config"
	"sharemk/internal/expiry"
	"sharemk/internal/hooks"
	"sharemk/internal/mcpserver"
	"sharemk/internal/openapi"
	"sharemk/internal/ratelimit"
	"sharemk/internal/s3client"
	"sharemk/internal/server"
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

	// 3. Configure S3 store.
	store := s3store.New(cfg.S3Bucket, s3Client)
	store.ObjectPrefix = cfg.S3ObjectPrefix

	composer := handler.NewStoreComposer()
	store.UseIn(composer)

	// 4. Configure memory locker.
	locker := memorylocker.New()
	locker.UseIn(composer)

	// 5. Set up hooks.
	hooksHandler := hooks.New(cfg, s3Client)

	// 6. Create tusd handler.
	tusHandler, err := handler.NewHandler(handler.Config{
		BasePath:                cfg.TUSBasePath,
		StoreComposer:           composer,
		MaxSize:                 cfg.TUSMaxSize,
		RespectForwardedHeaders: true,
		NotifyCompleteUploads:   true,
		PreUploadCreateCallback: hooksHandler.PreCreate,
	})
	if err != nil {
		slog.Error("failed to create tusd handler", "error", err)
		os.Exit(1)
	}

	// 7. Drain CompleteUploads channel; call HandleComplete for each finished upload.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case event, ok := <-tusHandler.CompleteUploads:
				if !ok {
					return
				}
				go hooksHandler.HandleComplete(event)
			case <-ctx.Done():
				return
			}
		}
	}()

	// 8. Start background expiry worker.
	expiryWorker := expiry.New(cfg, s3Client)
	go expiryWorker.Start(ctx)

	// 9. Build MCP server and OpenAPI handler.
	mcpSrv := mcpserver.New(cfg, s3Client)
	openapiHandler := openapi.Handler()

	// 10. Build rate limiter and HTTP server.
	limiter := ratelimit.New(cfg.RateLimitGlobal, cfg.RateLimitPerIP)
	srv := server.New(cfg, tusHandler, limiter, mcpSrv.Handler(), openapiHandler)

	httpServer := &http.Server{
		Addr:        cfg.ServerAddr,
		Handler:     srv.Handler(),
		ReadTimeout: 0, // no read timeout — large uploads need unlimited time
		WriteTimeout: 0,
		IdleTimeout: 120 * time.Second,
	}

	// 11. Graceful shutdown on SIGTERM / SIGINT.
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

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}

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
