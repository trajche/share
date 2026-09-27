package files

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/tus/tusd/v2/pkg/handler"
	"sharemk/internal/config"
)

// S3API is the subset of *s3.Client used by this package and its callers.
type S3API interface {
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, opts ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
	GetObjectTagging(ctx context.Context, in *s3.GetObjectTaggingInput, opts ...func(*s3.Options)) (*s3.GetObjectTaggingOutput, error)
	PutObjectTagging(ctx context.Context, in *s3.PutObjectTaggingInput, opts ...func(*s3.Options)) (*s3.PutObjectTaggingOutput, error)
	AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, opts ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

// ErrNotFound is returned for unknown or malformed upload IDs.
var ErrNotFound = errors.New("upload not found")

// MCPMultipartID is the placeholder multipart ID for uploads written
// directly by the MCP server (tusd requires a non-empty value).
const MCPMultipartID = "mcp"

var objectIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// SplitID splits a tus upload ID ("objectId+multipartId") into its parts.
// A bare object ID is also accepted. It returns ok=false for IDs that could
// escape the object prefix.
func SplitID(id string) (objectID, multipartID string, ok bool) {
	objectID, multipartID, _ = strings.Cut(id, "+")
	return objectID, multipartID, objectIDPattern.MatchString(objectID)
}

// Service reads and deletes stored uploads.
type Service struct {
	cfg *config.Config
	s3  S3API
}

func NewService(cfg *config.Config, s3Client S3API) *Service {
	return &Service{cfg: cfg, s3: s3Client}
}

// S3 returns the underlying client.
func (s *Service) S3() S3API { return s.s3 }

// Bucket returns the configured bucket.
func (s *Service) Bucket() string { return s.cfg.S3Bucket }

// Key returns the S3 key of an upload's data object.
func (s *Service) Key(objectID string) string {
	return s.cfg.S3ObjectPrefix + objectID
}

// DownloadURL returns the public URL for an upload ID.
func (s *Service) DownloadURL(id string) string {
	return strings.TrimRight(s.cfg.PublicURL, "/") + s.cfg.TUSBasePath + id
}

// ManageURL returns the web page for managing an upload. The token goes in
// the fragment so it is never sent to the server or leaked via Referer.
func (s *Service) ManageURL(objectID, token string) string {
	return strings.TrimRight(s.cfg.PublicURL, "/") + "/manage/" + objectID + "#" + token
}

// Load reads the tusd .info object for an upload ID (full or object-only).
func (s *Service) Load(ctx context.Context, id string) (*handler.FileInfo, error) {
	objectID, _, ok := SplitID(id)
	if !ok {
		return nil, ErrNotFound
	}
	out, err := s.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.S3Bucket),
		Key:    aws.String(s.Key(objectID) + ".info"),
	})
	if err != nil {
		if IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer out.Body.Close()

	var info handler.FileInfo
	if err := json.NewDecoder(out.Body).Decode(&info); err != nil {
		return nil, err
	}
	if info.MetaData == nil {
		info.MetaData = handler.MetaData{}
	}
	return &info, nil
}

// IsNotFound reports whether err is an S3 "no such key" error.
func IsNotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	return HTTPStatus(err) == 404
}

// HTTPStatus returns the HTTP status code of an S3 response error, or 0.
func HTTPStatus(err error) int {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

// Complete reports whether all bytes of the upload have been received.
// s3store never updates the Offset in the .info object, so completion is
// detected by the presence of the data object, which s3store only creates
// when the multipart upload is finished.
func (s *Service) Complete(ctx context.Context, info *handler.FileInfo) bool {
	objectID, _, _ := SplitID(info.ID)
	_, err := s.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.S3Bucket),
		Key:    aws.String(s.Key(objectID)),
	})
	return err == nil
}

// Delete removes an upload's objects and aborts its multipart upload if it
// is still in progress.
func (s *Service) Delete(ctx context.Context, info *handler.FileInfo) error {
	objectID, multipartID, ok := SplitID(info.ID)
	if !ok {
		return ErrNotFound
	}
	key := s.Key(objectID)

	if multipartID != "" && multipartID != MCPMultipartID {
		// Best effort: fails harmlessly if the upload already completed.
		s.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{ //nolint:errcheck
			Bucket:   aws.String(s.cfg.S3Bucket),
			Key:      aws.String(key),
			UploadId: aws.String(multipartID),
		})
	}

	_, err := s.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(s.cfg.S3Bucket),
		Delete: &s3types.Delete{
			Objects: []s3types.ObjectIdentifier{
				{Key: aws.String(key)},
				{Key: aws.String(key + ".info")},
				{Key: aws.String(key + ".part")},
			},
			Quiet: aws.Bool(true),
		},
	})
	return err
}

// ExpiresAt returns the expires-at tag of an upload, or "" if not yet set.
func (s *Service) ExpiresAt(ctx context.Context, info *handler.FileInfo) string {
	objectID, _, _ := SplitID(info.ID)
	out, err := s.s3.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: aws.String(s.cfg.S3Bucket),
		Key:    aws.String(s.Key(objectID) + ".info"),
	})
	if err != nil {
		return ""
	}
	for _, t := range out.TagSet {
		if aws.ToString(t.Key) == "expires-at" {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

// Describe returns the public description of an upload. It never includes
// secrets.
func (s *Service) Describe(ctx context.Context, info *handler.FileInfo) map[string]any {
	disposition := info.MetaData[MetaDisposition]
	if disposition == "" {
		disposition = DispositionInline
	}
	return map[string]any{
		"file_id":            info.ID,
		"filename":           info.MetaData[MetaFilename],
		"content_type":       FileType(info.MetaData),
		"size_bytes":         info.Size,
		"complete":           s.Complete(ctx, info),
		"download_url":       s.DownloadURL(info.ID),
		"expires_at":         s.ExpiresAt(ctx, info),
		"disposition":        disposition,
		"password_protected": info.MetaData[MetaPasswordHash] != "",
	}
}
