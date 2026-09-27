package hooks

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/tus/tusd/v2/pkg/handler"
	"sharemk/internal/config"
	"sharemk/internal/files"
)

// Response headers set on a successful upload creation (POST /files/).
const (
	HeaderManagementToken = "Upload-Management-Token"
	HeaderManageURL       = "Upload-Manage-URL"
)

type Hooks struct {
	cfg   *config.Config
	files *files.Service
}

func New(cfg *config.Config, fs *files.Service) *Hooks {
	return &Hooks{cfg: cfg, files: fs}
}

// PreCreate validates upload metadata before tusd creates the upload. It
// applies defaults, hashes an optional password, and issues the management
// token, which is returned once in the Upload-Management-Token header.
func (h *Hooks) PreCreate(event handler.HookEvent) (handler.HTTPResponse, handler.FileInfoChanges, error) {
	meta := make(handler.MetaData, len(event.Upload.MetaData)+2)
	for k, v := range event.Upload.MetaData {
		meta[k] = v
	}
	files.StripServerOwned(meta)

	if meta[files.MetaExpiresIn] == "" {
		meta[files.MetaExpiresIn] = files.DefaultExpiry
	}
	if _, err := files.ParseExpiry(meta[files.MetaExpiresIn]); err != nil {
		return reject("ERR_INVALID_EXPIRES_IN", err.Error())
	}

	if !files.ValidDisposition(meta[files.MetaDisposition]) {
		return reject("ERR_INVALID_DISPOSITION", `invalid disposition; valid values: inline, attachment`)
	}

	if pw, ok := meta[files.MetaPassword]; ok {
		delete(meta, files.MetaPassword)
		if pw != "" {
			hash, err := files.HashPassword(pw)
			if err != nil {
				return reject("ERR_INVALID_PASSWORD", err.Error())
			}
			meta[files.MetaPasswordHash] = hash
		}
	}

	token, err := files.NewToken()
	if err != nil {
		return handler.HTTPResponse{}, handler.FileInfoChanges{}, err
	}
	objectID, err := files.NewObjectID()
	if err != nil {
		return handler.HTTPResponse{}, handler.FileInfoChanges{}, err
	}
	meta[files.MetaTokenHash] = files.HashToken(token)

	resp := handler.HTTPResponse{
		Header: handler.HTTPHeader{
			HeaderManagementToken: token,
			HeaderManageURL:       h.files.ManageURL(objectID, token),
		},
	}
	return resp, handler.FileInfoChanges{ID: objectID, MetaData: meta}, nil
}

func reject(code, msg string) (handler.HTTPResponse, handler.FileInfoChanges, error) {
	return handler.HTTPResponse{}, handler.FileInfoChanges{}, handler.NewError(code, msg, http.StatusBadRequest)
}

// HandleComplete tags the S3 object with its expiry time after a successful upload.
func (h *Hooks) HandleComplete(event handler.HookEvent) {
	key, ok := event.Upload.Storage["Key"]
	if !ok || key == "" {
		slog.Error("hooks: missing S3 key in upload storage", "upload_id", event.Upload.ID)
		return
	}

	dur, err := files.ParseExpiry(event.Upload.MetaData[files.MetaExpiresIn])
	if err != nil {
		slog.Error("hooks: invalid expires-in in metadata", "error", err, "upload_id", event.Upload.ID)
		dur, _ = files.ParseExpiry(files.DefaultExpiry)
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
		_, err := h.files.S3().PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
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
