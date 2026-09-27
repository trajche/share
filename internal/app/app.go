// Package app assembles the HTTP handler stack (tusd, hooks, downloads,
// management API, MCP) so that main and the integration tests share it.
package app

import (
	"context"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"
	"github.com/tus/tusd/v2/pkg/s3store"
	"sharemk/internal/config"
	"sharemk/internal/files"
	"sharemk/internal/hooks"
	"sharemk/internal/mcpserver"
	"sharemk/internal/openapi"
	"sharemk/internal/ratelimit"
	"sharemk/internal/server"
)

type App struct {
	Handler http.Handler
	Files   *files.Service
	Hooks   *hooks.Hooks
	Tus     *handler.Handler
}

func New(cfg *config.Config, s3Client *s3.Client) (*App, error) {
	// S3 store with in-memory locking.
	store := s3store.New(cfg.S3Bucket, s3Client)
	store.ObjectPrefix = cfg.S3ObjectPrefix

	composer := handler.NewStoreComposer()
	store.UseIn(composer)
	memorylocker.New().UseIn(composer)

	fileService := files.NewService(cfg, s3Client)
	hooksHandler := hooks.New(cfg, fileService)

	cors := handler.DefaultCorsConfig
	cors.ExposeHeaders += ", " + hooks.HeaderManagementToken + ", " + hooks.HeaderManageURL
	cors.AllowHeaders += ", " + hooks.HeaderManagementToken

	tusHandler, err := handler.NewHandler(handler.Config{
		BasePath:                cfg.TUSBasePath,
		StoreComposer:           composer,
		MaxSize:                 cfg.TUSMaxSize,
		RespectForwardedHeaders: true,
		NotifyCompleteUploads:   true,
		PreUploadCreateCallback: hooksHandler.PreCreate,
		Cors:                    &cors,
		// Downloads and deletion are served by internal/server, which
		// enforces passwords, management tokens and the inline policy.
		DisableDownload:    true,
		DisableTermination: true,
	})
	if err != nil {
		return nil, err
	}

	mcpSrv := mcpserver.New(cfg, fileService)
	limiter := ratelimit.New(cfg.RateLimitGlobal, cfg.RateLimitPerIP)
	srv := server.New(cfg, fileService, tusHandler, limiter, mcpSrv.Handler(), openapi.Handler())

	return &App{
		Handler: srv.Handler(),
		Files:   fileService,
		Hooks:   hooksHandler,
		Tus:     tusHandler,
	}, nil
}

// ProcessCompletions calls HandleComplete for each finished upload until ctx
// is cancelled.
func (a *App) ProcessCompletions(ctx context.Context) {
	for {
		select {
		case event, ok := <-a.Tus.CompleteUploads:
			if !ok {
				return
			}
			go a.Hooks.HandleComplete(event)
		case <-ctx.Done():
			return
		}
	}
}
