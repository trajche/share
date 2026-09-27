package expiry

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/tus/tusd/v2/pkg/handler"
	"sharemk/internal/config"
	"sharemk/internal/files"
)

type Worker struct {
	cfg      *config.Config
	s3Client *s3.Client
	files    *files.Service
	interval time.Duration
	now      func() time.Time
}

func New(cfg *config.Config, s3Client *s3.Client, fs *files.Service) *Worker {
	return &Worker{
		cfg:      cfg,
		s3Client: s3Client,
		files:    fs,
		interval: 10 * time.Minute,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

func (w *Worker) Start(ctx context.Context) {
	slog.Info("expiry: worker started", "interval", w.interval, "incomplete_ttl", w.cfg.IncompleteUploadTTL)
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

type stats struct {
	expired, repaired, abandoned, aborted int
}

// runOnce removes expired uploads, repairs uploads whose expiry tag was
// never written, removes uploads that were never finished, and aborts
// multipart uploads that nothing refers to any more.
func (w *Worker) runOnce(ctx context.Context) {
	slog.Info("expiry: scanning for expired objects")
	objects, err := w.list(ctx)
	if err != nil {
		slog.Error("expiry: failed to list objects", "error", err)
		return
	}

	var st stats
	now := w.now()
	for key, modified := range objects {
		if strings.HasSuffix(key, ".part") {
			continue
		}
		if base, ok := strings.CutSuffix(key, ".info"); ok {
			if _, complete := objects[base]; !complete {
				w.checkIncomplete(ctx, base, modified, now, &st)
			}
			continue
		}
		w.checkUpload(ctx, key, modified, now, &st)
	}
	w.abortStaleMultipart(ctx, now, &st)

	slog.Info("expiry: scan complete",
		"deleted_uploads", st.expired,
		"repaired_tags", st.repaired,
		"abandoned_uploads", st.abandoned,
		"aborted_multipart", st.aborted)
}

// list returns every object under the prefix with its last-modified time.
func (w *Worker) list(ctx context.Context) (map[string]time.Time, error) {
	objects := make(map[string]time.Time)
	paginator := s3.NewListObjectsV2Paginator(w.s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(w.cfg.S3Bucket),
		Prefix: aws.String(w.cfg.S3ObjectPrefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			objects[aws.ToString(obj.Key)] = aws.ToTime(obj.LastModified)
		}
	}
	return objects, nil
}

// checkUpload handles a finished upload (its data object exists).
func (w *Worker) checkUpload(ctx context.Context, key string, modified, now time.Time, st *stats) {
	objectID := strings.TrimPrefix(key, w.cfg.S3ObjectPrefix)

	tagsOut, err := w.s3Client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: aws.String(w.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		slog.Warn("expiry: failed to get tags", "key", key, "error", err)
		return
	}
	if v, ok := findTag(tagsOut.TagSet, "expires-at"); ok {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			slog.Warn("expiry: invalid expires-at tag", "key", key, "value", v)
			return
		}
		if now.After(t) {
			w.delete(ctx, objectID, "expired", &st.expired)
		}
		return
	}

	// No tag: the completion hook failed or the process stopped before it
	// ran. Derive the expiry from the upload's own setting and the time
	// the data object was written.
	info, err := w.files.Load(ctx, objectID)
	if err != nil {
		// Data without metadata cannot be served; drop it once it is old.
		if now.Sub(modified) > w.cfg.IncompleteUploadTTL {
			w.delete(ctx, objectID, "orphaned data", &st.abandoned)
		}
		return
	}
	dur, err := files.ParseExpiry(info.MetaData[files.MetaExpiresIn])
	if err != nil {
		dur, _ = files.ParseExpiry(files.DefaultExpiry)
	}
	expiresAt := modified.Add(dur).UTC()
	if now.After(expiresAt) {
		w.delete(ctx, objectID, "expired (untagged)", &st.expired)
		return
	}
	tags := &s3types.Tagging{TagSet: []s3types.Tag{
		{Key: aws.String("expires-at"), Value: aws.String(expiresAt.Format(time.RFC3339))},
	}}
	for _, k := range []string{key, key + ".info"} {
		if _, err := w.s3Client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
			Bucket: aws.String(w.cfg.S3Bucket), Key: aws.String(k), Tagging: tags,
		}); err != nil {
			slog.Warn("expiry: failed to repair tag", "key", k, "error", err)
			return
		}
	}
	st.repaired++
}

// checkIncomplete removes an upload whose data never arrived once its
// metadata is older than IncompleteUploadTTL.
func (w *Worker) checkIncomplete(ctx context.Context, dataKey string, created, now time.Time, st *stats) {
	if now.Sub(created) <= w.cfg.IncompleteUploadTTL {
		return
	}
	w.delete(ctx, strings.TrimPrefix(dataKey, w.cfg.S3ObjectPrefix), "abandoned", &st.abandoned)
}

// delete removes an upload (aborting its multipart upload if needed).
func (w *Worker) delete(ctx context.Context, objectID, reason string, counter *int) {
	info, err := w.files.Load(ctx, objectID)
	if err != nil {
		// No readable metadata: remove whatever objects remain.
		info = &handler.FileInfo{ID: objectID}
	}
	if err := w.files.Delete(ctx, info); err != nil {
		slog.Error("expiry: failed to delete upload", "object_id", objectID, "reason", reason, "error", err)
		return
	}
	*counter++
}

// abortStaleMultipart aborts multipart uploads older than the incomplete
// TTL. It catches uploads whose .info is already gone.
func (w *Worker) abortStaleMultipart(ctx context.Context, now time.Time, st *stats) {
	paginator := s3.NewListMultipartUploadsPaginator(w.s3Client, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(w.cfg.S3Bucket),
		Prefix: aws.String(w.cfg.S3ObjectPrefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			slog.Warn("expiry: failed to list multipart uploads", "error", err)
			return
		}
		for _, u := range page.Uploads {
			if now.Sub(aws.ToTime(u.Initiated)) <= w.cfg.IncompleteUploadTTL {
				continue
			}
			if _, err := w.s3Client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(w.cfg.S3Bucket),
				Key:      u.Key,
				UploadId: u.UploadId,
			}); err != nil {
				slog.Warn("expiry: failed to abort multipart upload", "key", aws.ToString(u.Key), "error", err)
				continue
			}
			st.aborted++
		}
	}
}

func findTag(tags []s3types.Tag, key string) (string, bool) {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value), true
		}
	}
	return "", false
}
