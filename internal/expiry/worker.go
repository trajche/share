package expiry

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"sharemk/internal/config"
)

type Worker struct {
	cfg      *config.Config
	s3Client *s3.Client
	interval time.Duration
}

func New(cfg *config.Config, s3Client *s3.Client) *Worker {
	return &Worker{
		cfg:      cfg,
		s3Client: s3Client,
		interval: 10 * time.Minute,
	}
}

func (w *Worker) Start(ctx context.Context) {
	slog.Info("expiry: worker started", "interval", w.interval)
	w.runOnce(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("expiry: worker stopping")
			return
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

func (w *Worker) runOnce(ctx context.Context) {
	slog.Info("expiry: scanning for expired objects")
	now := time.Now().UTC()
	deleted := 0

	paginator := s3.NewListObjectsV2Paginator(w.s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(w.cfg.S3Bucket),
		Prefix: aws.String(w.cfg.S3ObjectPrefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			slog.Error("expiry: failed to list objects", "error", err)
			return
		}

		var toDelete []s3types.ObjectIdentifier

		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)

			// Only process data objects; skip metadata and multipart parts.
			if strings.HasSuffix(key, ".info") || strings.HasSuffix(key, ".part") {
				continue
			}

			tagsOut, err := w.s3Client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
				Bucket: aws.String(w.cfg.S3Bucket),
				Key:    aws.String(key),
			})
			if err != nil {
				slog.Warn("expiry: failed to get tags", "key", key, "error", err)
				continue
			}

			expiresAt, found := findTag(tagsOut.TagSet, "expires-at")
			if !found {
				continue
			}

			t, err := time.Parse(time.RFC3339, expiresAt)
			if err != nil {
				slog.Warn("expiry: invalid expires-at tag", "key", key, "value", expiresAt)
				continue
			}

			if now.After(t) {
				toDelete = append(toDelete,
					s3types.ObjectIdentifier{Key: aws.String(key)},
					s3types.ObjectIdentifier{Key: aws.String(key + ".info")},
				)
			}
		}

		if len(toDelete) == 0 {
			continue
		}

		_, err = w.s3Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(w.cfg.S3Bucket),
			Delete: &s3types.Delete{Objects: toDelete, Quiet: aws.Bool(true)},
		})
		if err != nil {
			slog.Error("expiry: failed to delete objects", "error", err)
			continue
		}

		deleted += len(toDelete) / 2
	}

	slog.Info("expiry: scan complete", "deleted_uploads", deleted)
}

func findTag(tags []s3types.Tag, key string) (string, bool) {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value), true
		}
	}
	return "", false
}
