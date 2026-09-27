package expiry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/tus/tusd/v2/pkg/handler"
	"sharemk/internal/files"
	"sharemk/internal/testutil"
)

type fixture struct {
	t      *testing.T
	client *s3.Client
	worker *Worker
}

func newFixture(t *testing.T) *fixture {
	client, cfg := testutil.NewS3(t)
	cfg.IncompleteUploadTTL = 48 * time.Hour
	return &fixture{t: t, client: client, worker: New(cfg, client, files.NewService(cfg, client))}
}

// runAt runs one scan as if the current time were now+offset.
func (f *fixture) runAt(offset time.Duration) {
	f.worker.now = func() time.Time { return time.Now().UTC().Add(offset) }
	f.worker.runOnce(context.Background())
}

func (f *fixture) put(key, body string) {
	f.t.Helper()
	if _, err := f.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(testutil.Bucket), Key: aws.String(key), Body: strings.NewReader(body),
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) putInfo(objectID, id string, meta handler.MetaData) {
	f.t.Helper()
	b, _ := json.Marshal(handler.FileInfo{ID: id, MetaData: meta})
	f.put("uploads/"+objectID+".info", string(b))
}

func (f *fixture) tag(key, expiresAt string) {
	f.t.Helper()
	if _, err := f.client.PutObjectTagging(context.Background(), &s3.PutObjectTaggingInput{
		Bucket: aws.String(testutil.Bucket), Key: aws.String(key),
		Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{Key: aws.String("expires-at"), Value: aws.String(expiresAt)}}},
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) expectExists(want map[string]bool) {
	f.t.Helper()
	for key, exists := range want {
		if got := testutil.Exists(f.t, f.client, key); got != exists {
			f.t.Errorf("%s exists = %v, want %v", key, got, exists)
		}
	}
}

func (f *fixture) expiresTag(key string) string {
	f.t.Helper()
	out, err := f.client.GetObjectTagging(context.Background(), &s3.GetObjectTaggingInput{
		Bucket: aws.String(testutil.Bucket), Key: aws.String(key),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	v, _ := findTag(out.TagSet, "expires-at")
	return v
}

func TestDeletesExpiredUploads(t *testing.T) {
	f := newFixture(t)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	for _, k := range []string{"uploads/old", "uploads/old.info", "uploads/new", "uploads/new.info"} {
		f.put(k, "x")
	}
	f.tag("uploads/old", past)
	f.tag("uploads/new", future)
	f.put("other/old", "x") // outside the prefix
	f.tag("other/old", past)

	f.runAt(0)
	f.expectExists(map[string]bool{
		"uploads/old": false, "uploads/old.info": false,
		"uploads/new": true, "uploads/new.info": true,
		"other/old": true,
	})
}

func TestRepairsMissingExpiryTag(t *testing.T) {
	f := newFixture(t)
	f.put("uploads/untagged", "x")
	f.putInfo("untagged", "untagged+mp", handler.MetaData{files.MetaExpiresIn: "6h"})

	f.runAt(0)
	f.expectExists(map[string]bool{"uploads/untagged": true})
	for _, k := range []string{"uploads/untagged", "uploads/untagged.info"} {
		at, err := time.Parse(time.RFC3339, f.expiresTag(k))
		if err != nil {
			t.Fatalf("%s: no repaired tag: %v", k, err)
		}
		if d := time.Until(at); d < 5*time.Hour || d > 6*time.Hour+time.Minute {
			t.Errorf("%s: repaired expiry in %v, want ~6h", k, d)
		}
	}
}

func TestDeletesUntaggedUploadPastItsExpiry(t *testing.T) {
	f := newFixture(t)
	f.put("uploads/stale", "x")
	f.putInfo("stale", "stale+mp", handler.MetaData{files.MetaExpiresIn: "1h"})

	f.runAt(2 * time.Hour)
	f.expectExists(map[string]bool{"uploads/stale": false, "uploads/stale.info": false})
}

func TestRemovesAbandonedIncompleteUploads(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	mp, err := f.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(testutil.Bucket), Key: aws.String("uploads/pending"),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.putInfo("pending", "pending+"+aws.ToString(mp.UploadId), handler.MetaData{})
	f.put("uploads/pending.part", "partial")

	// Recent: left alone.
	f.runAt(time.Hour)
	f.expectExists(map[string]bool{"uploads/pending.info": true, "uploads/pending.part": true})

	// Past the TTL: metadata removed and the multipart upload aborted.
	f.runAt(49 * time.Hour)
	f.expectExists(map[string]bool{"uploads/pending.info": false, "uploads/pending.part": false})
	if n := f.multipartUploads(); n != 0 {
		t.Errorf("%d multipart uploads left", n)
	}
}

func TestAbortsOrphanedMultipartUploads(t *testing.T) {
	f := newFixture(t)
	if _, err := f.client.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String(testutil.Bucket), Key: aws.String("uploads/orphan"),
	}); err != nil {
		t.Fatal(err)
	}

	f.runAt(time.Hour)
	if n := f.multipartUploads(); n != 1 {
		t.Fatalf("recent multipart upload aborted early (%d left)", n)
	}
	f.runAt(49 * time.Hour)
	if n := f.multipartUploads(); n != 0 {
		t.Errorf("%d multipart uploads left", n)
	}
}

func TestDeletesOrphanedDataObjects(t *testing.T) {
	f := newFixture(t)
	f.put("uploads/nometa", "x")

	f.runAt(time.Hour)
	f.expectExists(map[string]bool{"uploads/nometa": true})
	f.runAt(49 * time.Hour)
	f.expectExists(map[string]bool{"uploads/nometa": false})
}

func (f *fixture) multipartUploads() int {
	f.t.Helper()
	out, err := f.client.ListMultipartUploads(context.Background(), &s3.ListMultipartUploadsInput{
		Bucket: aws.String(testutil.Bucket),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return len(out.Uploads)
}
