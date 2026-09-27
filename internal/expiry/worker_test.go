package expiry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"sharemk/internal/testutil"
)

func TestRunOnceDeletesExpiredUploads(t *testing.T) {
	client, cfg := testutil.NewS3(t)
	ctx := context.Background()

	put := func(key, expiresAt string) {
		t.Helper()
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(testutil.Bucket), Key: aws.String(key), Body: strings.NewReader("x"),
		}); err != nil {
			t.Fatal(err)
		}
		if expiresAt == "" {
			return
		}
		if _, err := client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
			Bucket: aws.String(testutil.Bucket), Key: aws.String(key),
			Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{Key: aws.String("expires-at"), Value: aws.String(expiresAt)}}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	put("uploads/old", past)
	put("uploads/old.info", past)
	put("uploads/new", future)
	put("uploads/new.info", future)
	put("uploads/untagged", "")
	put("uploads/untagged.info", "")
	put("other/old", past) // outside the prefix

	New(cfg, client).runOnce(ctx)

	want := map[string]bool{
		"uploads/old": false, "uploads/old.info": false,
		"uploads/new": true, "uploads/new.info": true,
		"uploads/untagged": true, "uploads/untagged.info": true,
		"other/old": true,
	}
	for key, exists := range want {
		if got := testutil.Exists(t, client, key); got != exists {
			t.Errorf("%s exists = %v, want %v", key, got, exists)
		}
	}
}
