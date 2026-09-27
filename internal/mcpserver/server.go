// Package mcpserver exposes an MCP (Model Context Protocol) server so AI
// assistants can upload, inspect, and delete files without speaking tus.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/tus/tusd/v2/pkg/handler"
	"sharemk/internal/config"
	"sharemk/internal/files"
)

// MCPServer wraps an MCP server instance and holds shared dependencies.
type MCPServer struct {
	cfg   *config.Config
	files *files.Service
	mcp   *server.MCPServer
}

// New creates an MCPServer and registers all tools.
func New(cfg *config.Config, fs *files.Service) *MCPServer {
	ms := &MCPServer{cfg: cfg, files: fs}

	s := server.NewMCPServer(
		"share.mk",
		"1.0.0",
		server.WithToolCapabilities(false),
	)

	s.AddTool(ms.uploadFileTool(), ms.handleUploadFile)
	s.AddTool(ms.getFileInfoTool(), ms.handleGetFileInfo)
	s.AddTool(ms.deleteFileTool(), ms.handleDeleteFile)

	ms.mcp = s
	return ms
}

// Handler returns an http.Handler for the MCP Streamable HTTP transport.
func (ms *MCPServer) Handler() http.Handler {
	return server.NewStreamableHTTPServer(ms.mcp)
}

// ---------------------------------------------------------------------------
// Tool definitions
// ---------------------------------------------------------------------------

func (ms *MCPServer) uploadFileTool() mcp.Tool {
	return mcp.NewTool("upload_file",
		mcp.WithDescription(
			"Upload a file to share.mk and get back a download URL. "+
				"The file content must be base64-encoded. "+
				"Practical size limit for MCP calls is ~10 MB. "+
				"Images, video, audio, PDF and text open in the browser by default.",
		),
		mcp.WithString("filename",
			mcp.Required(),
			mcp.Description("Original filename, e.g. report.pdf"),
		),
		mcp.WithString("content",
			mcp.Required(),
			mcp.Description("Base64-encoded file content (standard or URL-safe encoding accepted)"),
		),
		mcp.WithString("content_type",
			mcp.Description("MIME type, e.g. application/pdf. Defaults to application/octet-stream."),
		),
		mcp.WithString("expires_in",
			mcp.Description("How long until the file is deleted. One of: "+files.ExpiryChoices+" (default 24h)."),
		),
		mcp.WithString("disposition",
			mcp.Description(`"inline" (default) previews safe types in the browser; "attachment" always downloads.`),
			mcp.Enum(files.DispositionInline, files.DispositionAttachment),
		),
		mcp.WithString("password",
			mcp.Description("Optional password required to download the file. Share it separately from the link."),
		),
	)
}

func (ms *MCPServer) getFileInfoTool() mcp.Tool {
	return mcp.NewTool("get_file_info",
		mcp.WithDescription(
			"Return metadata and the download URL for a previously uploaded file. "+
				"Requires the management_token returned by upload_file.",
		),
		mcp.WithString("file_id",
			mcp.Required(),
			mcp.Description("The file ID returned by upload_file"),
		),
		mcp.WithString("management_token",
			mcp.Required(),
			mcp.Description("The management token returned by upload_file — proves ownership"),
		),
	)
}

func (ms *MCPServer) deleteFileTool() mcp.Tool {
	return mcp.NewTool("delete_file",
		mcp.WithDescription(
			"Permanently delete an uploaded file. "+
				"Requires the management_token returned by upload_file.",
		),
		mcp.WithString("file_id",
			mcp.Required(),
			mcp.Description("The file ID returned by upload_file"),
		),
		mcp.WithString("management_token",
			mcp.Required(),
			mcp.Description("The management token returned by upload_file — proves ownership"),
		),
	)
}

// ---------------------------------------------------------------------------
// Tool handlers
// ---------------------------------------------------------------------------

func (ms *MCPServer) handleUploadFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	filename, _ := args["filename"].(string)
	if filename == "" {
		return mcp.NewToolResultError("filename is required"), nil
	}

	contentB64, _ := args["content"].(string)
	if contentB64 == "" {
		return mcp.NewToolResultError("content is required"), nil
	}

	data, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		// Fall back to URL-safe encoding.
		data, err = base64.URLEncoding.DecodeString(contentB64)
		if err != nil {
			return mcp.NewToolResultError("content must be valid base64"), nil
		}
	}

	contentType, _ := args["content_type"].(string)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	expiresIn, _ := args["expires_in"].(string)
	if expiresIn == "" {
		expiresIn = files.DefaultExpiry
	}
	dur, err := files.ParseExpiry(expiresIn)
	if err != nil {
		return mcp.NewToolResultError("expires_in must be one of: " + files.ExpiryChoices), nil
	}

	disposition, _ := args["disposition"].(string)
	if !files.ValidDisposition(disposition) {
		return mcp.NewToolResultError(`disposition must be "inline" or "attachment"`), nil
	}

	objectID, err := files.NewObjectID()
	if err != nil {
		return mcp.NewToolResultError("internal error generating file id"), nil
	}
	// tusd's s3store.GetUpload splits the ID on '+' and requires both parts
	// to be non-empty (objectId + multipartId), so HEAD requests need a
	// multipart ID. "mcp" satisfies the check and marks MCP-originated files.
	tusID := objectID + "+" + files.MCPMultipartID
	key := ms.files.Key(objectID)
	expiresAt := time.Now().UTC().Add(dur).Format(time.RFC3339)

	// The management token is the only proof of ownership for get_file_info
	// and delete_file. It is returned once here; only its hash is stored.
	mgmtToken, err := files.NewToken()
	if err != nil {
		slog.Error("mcp: upload_file failed to generate management token", "error", err)
		return mcp.NewToolResultError("internal error generating management token"), nil
	}

	meta := handler.MetaData{
		files.MetaFilename:  filename,
		files.MetaFiletype:  contentType,
		files.MetaExpiresIn: expiresIn,
		files.MetaTokenHash: files.HashToken(mgmtToken),
	}
	if disposition != "" {
		meta[files.MetaDisposition] = disposition
	}
	password, _ := args["password"].(string)
	if password != "" {
		hash, err := files.HashPassword(password)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		meta[files.MetaPasswordHash] = hash
	}

	opCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s3c := ms.files.S3()

	// Upload the file data.
	size := int64(len(data))
	_, err = s3c.PutObject(opCtx, &s3.PutObjectInput{
		Bucket:        aws.String(ms.cfg.S3Bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		slog.Error("mcp: upload_file PutObject failed", "error", err)
		return mcp.NewToolResultError("failed to upload file: " + err.Error()), nil
	}

	// Create the tusd-compatible .info file so tus HEAD requests and the
	// download handler treat MCP uploads like any other upload.
	info := handler.FileInfo{
		ID:       tusID,
		Size:     size,
		Offset:   size,
		MetaData: meta,
		Storage: map[string]string{
			"Type":   "s3store",
			"Bucket": ms.cfg.S3Bucket,
			"Key":    key,
		},
	}
	infoJSON, _ := json.Marshal(info)

	_, err = s3c.PutObject(opCtx, &s3.PutObjectInput{
		Bucket:      aws.String(ms.cfg.S3Bucket),
		Key:         aws.String(key + ".info"),
		Body:        bytes.NewReader(infoJSON),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		slog.Error("mcp: upload_file PutObject(.info) failed", "error", err)
		// Best-effort cleanup of the data object.
		s3c.DeleteObject(opCtx, &s3.DeleteObjectInput{ //nolint:errcheck
			Bucket: aws.String(ms.cfg.S3Bucket),
			Key:    aws.String(key),
		})
		return mcp.NewToolResultError("failed to write upload metadata: " + err.Error()), nil
	}

	// Tag both objects with the expiry timestamp.
	tags := &s3types.Tagging{
		TagSet: []s3types.Tag{
			{Key: aws.String("expires-at"), Value: aws.String(expiresAt)},
		},
	}
	for _, k := range []string{key, key + ".info"} {
		if _, terr := s3c.PutObjectTagging(opCtx, &s3.PutObjectTaggingInput{
			Bucket:  aws.String(ms.cfg.S3Bucket),
			Key:     aws.String(k),
			Tagging: tags,
		}); terr != nil {
			slog.Warn("mcp: failed to tag object", "key", k, "error", terr)
		}
	}

	result := map[string]any{
		"file_id":            tusID,
		"management_token":   mgmtToken,
		"manage_url":         ms.files.ManageURL(objectID, mgmtToken),
		"download_url":       ms.files.DownloadURL(tusID),
		"expires_at":         expiresAt,
		"filename":           filename,
		"size_bytes":         size,
		"password_protected": password != "",
	}
	return toolResultJSON(result)
}

func (ms *MCPServer) handleGetFileInfo(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	info, errResult := ms.authorize(req)
	if errResult != nil {
		return errResult, nil
	}
	opCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return toolResultJSON(ms.files.Describe(opCtx, info))
}

func (ms *MCPServer) handleDeleteFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	info, errResult := ms.authorize(req)
	if errResult != nil {
		return errResult, nil
	}
	opCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ms.files.Delete(opCtx, info); err != nil {
		return mcp.NewToolResultError("failed to delete file: " + err.Error()), nil
	}
	return toolResultJSON(map[string]any{"deleted": true, "file_id": info.ID})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// authorize loads the upload named by file_id and verifies management_token.
// Unknown IDs and wrong tokens produce the same error to prevent callers
// from enumerating valid file IDs.
func (ms *MCPServer) authorize(req mcp.CallToolRequest) (*handler.FileInfo, *mcp.CallToolResult) {
	args := req.GetArguments()
	id, _ := args["file_id"].(string)
	if id == "" {
		return nil, mcp.NewToolResultError("file_id is required")
	}
	token, _ := args["management_token"].(string)

	opCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	info, err := ms.files.Load(opCtx, id)
	if err != nil && !errors.Is(err, files.ErrNotFound) {
		slog.Error("mcp: failed to load upload", "id", id, "error", err)
		return nil, mcp.NewToolResultError("failed to read file metadata")
	}
	if err != nil || !files.TokenMatches(info.MetaData, token) {
		return nil, mcp.NewToolResultError("invalid file_id or management_token")
	}
	return info, nil
}

func toolResultJSON(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("failed to marshal result"), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}
