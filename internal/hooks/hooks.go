package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/tus/tusd/v2/pkg/handler"
	"sharemk/internal/config"
)

var validExpiries = map[string]time.Duration{
	"1h":  1 * time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

type Hooks struct {
	cfg      *config.Config
	s3Client *s3.Client
}

func New(cfg *config.Config, s3Client *s3.Client) *Hooks {
	return &Hooks{cfg: cfg, s3Client: s3Client}
}

// PreCreate validates the expires-in metadata and injects a default if absent.
func (h *Hooks) PreCreate(event handler.HookEvent) (handler.HTTPResponse, handler.FileInfoChanges, error) {
	expiry := event.Upload.MetaData["expires-in"]

	if expiry == "" {
		expiry = "24h"
		// Inject the default back so PostFinish can read it.
		changes := handler.FileInfoChanges{
			MetaData: event.Upload.MetaData,
		}
		if changes.MetaData == nil {
			changes.MetaData = make(handler.MetaData)
		}
		changes.MetaData["expires-in"] = expiry
		return handler.HTTPResponse{}, changes, nil
	}

	if _, ok := validExpiries[expiry]; !ok {
		body, _ := json.Marshal(map[string]string{
			"error": fmt.Sprintf("invalid expires-in %q; valid values: 1h, 6h, 24h, 7d, 30d", expiry),
		})
		return handler.HTTPResponse{
			StatusCode: 400,
			Header:     handler.HTTPHeader{"Content-Type": "application/json"},
			Body:       string(body),
		}, handler.FileInfoChanges{}, nil
	}

	return handler.HTTPResponse{}, handler.FileInfoChanges{}, nil
}

// HandleComplete tags the S3 object with its expiry time after a successful upload.
func (h *Hooks) HandleComplete(event handler.HookEvent) {
	key, ok := event.Upload.Storage["Key"]
	if !ok || key == "" {
		slog.Error("hooks: missing S3 key in upload storage", "upload_id", event.Upload.ID)
		return
	}

	expiry := event.Upload.MetaData["expires-in"]
	if expiry == "" {
		expiry = "24h"
	}

	dur, ok := validExpiries[expiry]
	if !ok {
		slog.Error("hooks: invalid expires-in in metadata", "value", expiry, "upload_id", event.Upload.ID)
		return
	}

	expiresAt := time.Now().UTC().Add(dur).Format(time.RFC3339)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tags := &s3types.Tagging{
		TagSet: []s3types.Tag{
			{Key: aws.String("expires-at"), Value: aws.String(expiresAt)},
		},
	}

	for _, k := range []string{key, key + ".info"} {
		_, err := h.s3Client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
			Bucket:  aws.String(h.cfg.S3Bucket),
			Key:     aws.String(k),
			Tagging: tags,
		})
		if err != nil {
			slog.Error("hooks: failed to tag object", "key", k, "error", err)
		}
	}

	slog.Info("hooks: tagged upload with expiry", "upload_id", event.Upload.ID, "expires_at", expiresAt)
}
