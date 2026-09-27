// Package testutil provides an in-memory S3 server for tests.
package testutil

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"sharemk/internal/config"
)

const Bucket = "test"

// NewS3 starts an in-memory S3 server and returns a client and a config
// pointing at it.
func NewS3(t *testing.T) (*s3.Client, *config.Config) {
	t.Helper()

	backend := s3mem.New()
	if err := backend.CreateBucket(Bucket); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(&taggingShim{next: gofakes3.New(backend).Server(), tags: map[string][]byte{}})
	t.Cleanup(ts.Close)

	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(ts.URL),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("x", "x", ""),
		UsePathStyle: true,
	})

	cfg := &config.Config{
		S3Bucket:        Bucket,
		S3ObjectPrefix:  "uploads/",
		TUSBasePath:     "/files/",
		TUSMaxSize:      1 << 30,
		PublicURL:       "https://share.test",
		RateLimitGlobal: 50,
		RateLimitPerIP:  5,

		TrustedProxies:      []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		MCPMaxBodyBytes:     1 << 20,
		MCPRateLimitGlobal:  10,
		MCPRateLimitPerIP:   2,
		IncompleteUploadTTL: 48 * time.Hour,
	}
	return client, cfg
}

// Exists reports whether key exists in the test bucket.
func Exists(t *testing.T, client *s3.Client, key string) bool {
	t.Helper()
	_, err := client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(Bucket),
		Key:    aws.String(key),
	})
	return err == nil
}

// taggingShim implements object tagging, which gofakes3 does not support
// (it would store the tagging XML as the object body).
type taggingShim struct {
	next http.Handler
	mu   sync.Mutex
	tags map[string][]byte
}

func (s *taggingShim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.URL.Query()["tagging"]; !ok {
		s.next.ServeHTTP(w, r)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		s.tags[r.URL.Path] = b
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/xml")
		if b, ok := s.tags[r.URL.Path]; ok {
			w.Write(b) //nolint:errcheck
		} else {
			io.WriteString(w, `<Tagging><TagSet></TagSet></Tagging>`) //nolint:errcheck
		}
	case http.MethodDelete:
		delete(s.tags, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}
}
