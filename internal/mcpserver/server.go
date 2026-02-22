// Package mcpserver exposes an MCP (Model Context Protocol) server so AI
// assistants can upload, inspect, and delete files without speaking tus.
package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"sharemk/internal/config"
)

var validExpiries = map[string]time.Duration{
	"1h":  1 * time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// fileInfo mirrors the subset of tusd's FileInfo that s3store serialises to
// the .info object, so the tusd GET handler can serve MCP-uploaded files.
type fileInfo struct {
	ID             string            `json:"ID"`
	Size           int64             `json:"Size"`
	SizeIsDeferred bool              `json:"SizeIsDeferred"`
	Offset         int64             `json:"Offset"`
	MetaData       map[string]string `json:"MetaData"`
	IsPartial      bool              `json:"IsPartial"`
	IsFinal        bool              `json:"IsFinal"`
	PartialUploads []string          `json:"PartialUploads"`
	Storage        map[string]string `json:"Storage"`
}

// MCPServer wraps an MCP server instance and holds shared dependencies.
type MCPServer struct {
	cfg      *config.Config
	s3Client *s3.Client
	mcp      *server.MCPServer
}

// New creates an MCPServer and registers all tools.
func New(cfg *config.Config, s3Client *s3.Client) *MCPServer {
	ms := &MCPServer{cfg: cfg, s3Client: s3Client}

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
				"Practical size limit for MCP calls is ~10 MB.",
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
			mcp.Description("How long until the file is deleted. One of: 1h, 6h, 24h (default), 7d, 30d."),
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
		expiresIn = "24h"
	}

	dur, ok := validExpiries[expiresIn]
	if !ok {
		return mcp.NewToolResultError("expires_in must be one of: 1h, 6h, 24h, 7d, 30d"), nil
	}

	objectId := uuid.New().String()
	// tusd's s3store.GetUpload splits the ID on '+' and requires both parts
	// to be non-empty (objectId + multipartId).  Using a plain UUID results
	// in an empty multipartId and an immediate ErrNotFound.  Appending
	// "+mcp" satisfies the check while clearly marking MCP-originated files.
	tusID := objectId + "+mcp"
	key := ms.cfg.S3ObjectPrefix + objectId
	expiresAt := time.Now().UTC().Add(dur).Format(time.RFC3339)

	// Generate a cryptographically random management token (256-bit entropy).
	// This is the only mechanism that proves upload ownership for the
	// get_file_info and delete_file tools.  It is returned once here and
	// never exposed again — not even by get_file_info.
	mgmtToken, err := generateToken()
	if err != nil {
		slog.Error("mcp: upload_file failed to generate management token", "error", err)
		return mcp.NewToolResultError("internal error generating management token"), nil
	}

	opCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Upload the file data.
	size := int64(len(data))
	_, err = ms.s3Client.PutObject(opCtx, &s3.PutObjectInput{
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

	// Create the tusd-compatible .info file so the GET /files/{id} handler
	// can serve the file without knowing it was uploaded via MCP.
	// The ID field must be the full tus ID (objectId+multipartId) so that
	// tusd's s3store.GetUpload can look it up by the URL path segment.
	info := fileInfo{
		ID:     tusID,
		Size:   size,
		Offset: size,
		MetaData: map[string]string{
			"filename":   filename,
			"filetype":   contentType,
			"expires-in": expiresIn,
			// mgmt-token is stored server-side only and never returned by
			// any endpoint except this upload response.
			"mgmt-token": mgmtToken,
		},
		Storage: map[string]string{
			"Type":   "s3store",
			"Bucket": ms.cfg.S3Bucket,
			"Key":    key,
		},
	}
	infoJSON, _ := json.Marshal(info)

	_, err = ms.s3Client.PutObject(opCtx, &s3.PutObjectInput{
		Bucket:      aws.String(ms.cfg.S3Bucket),
		Key:         aws.String(key + ".info"),
		Body:        bytes.NewReader(infoJSON),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		slog.Error("mcp: upload_file PutObject(.info) failed", "error", err)
		// Best-effort cleanup of the data object.
		ms.s3Client.DeleteObject(opCtx, &s3.DeleteObjectInput{ //nolint:errcheck
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
		if _, terr := ms.s3Client.PutObjectTagging(opCtx, &s3.PutObjectTaggingInput{
			Bucket:  aws.String(ms.cfg.S3Bucket),
			Key:     aws.String(k),
			Tagging: tags,
		}); terr != nil {
			slog.Warn("mcp: failed to tag object", "key", k, "error", terr)
		}
	}

	downloadURL := strings.TrimRight(ms.cfg.PublicURL, "/") + ms.cfg.TUSBasePath + tusID

	result := map[string]any{
		"file_id":          tusID,
		"management_token": mgmtToken,
		"download_url":     downloadURL,
		"expires_at":       expiresAt,
		"filename":         filename,
		"size_bytes":       size,
	}
	return toolResultJSON(result)
}

func (ms *MCPServer) handleGetFileInfo(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, _ := args["file_id"].(string)
	if id == "" {
		return mcp.NewToolResultError("file_id is required"), nil
	}

	providedToken, _ := args["management_token"].(string)

	key := ms.objectKey(id)
	opCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Read the .info file.  Use the same error for "not found" and "wrong
	// token" to prevent callers from enumerating valid file IDs.
	out, err := ms.s3Client.GetObject(opCtx, &s3.GetObjectInput{
		Bucket: aws.String(ms.cfg.S3Bucket),
		Key:    aws.String(key + ".info"),
	})
	if err != nil {
		return mcp.NewToolResultError("invalid file_id or management_token"), nil
	}
	defer out.Body.Close()

	var info fileInfo
	if err := json.NewDecoder(out.Body).Decode(&info); err != nil {
		return mcp.NewToolResultError("invalid file_id or management_token"), nil
	}

	// Constant-time comparison prevents timing-oracle attacks on the token.
	// Same error whether the file doesn't exist, has no token, or the token
	// doesn't match.
	if !tokenMatches(info.MetaData["mgmt-token"], providedToken) {
		return mcp.NewToolResultError("invalid file_id or management_token"), nil
	}

	// Read expiry tag.
	tagsOut, _ := ms.s3Client.GetObjectTagging(opCtx, &s3.GetObjectTaggingInput{
		Bucket: aws.String(ms.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	expiresAt := ""
	if tagsOut != nil {
		for _, t := range tagsOut.TagSet {
			if aws.ToString(t.Key) == "expires-at" {
				expiresAt = aws.ToString(t.Value)
				break
			}
		}
	}

	downloadURL := strings.TrimRight(ms.cfg.PublicURL, "/") + ms.cfg.TUSBasePath + info.ID

	// Never return the stored mgmt-token in responses.
	result := map[string]any{
		"file_id":      info.ID,
		"filename":     info.MetaData["filename"],
		"content_type": info.MetaData["filetype"],
		"size_bytes":   info.Size,
		"download_url": downloadURL,
		"expires_at":   expiresAt,
	}
	return toolResultJSON(result)
}

func (ms *MCPServer) handleDeleteFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, _ := args["file_id"].(string)
	if id == "" {
		return mcp.NewToolResultError("file_id is required"), nil
	}

	providedToken, _ := args["management_token"].(string)

	key := ms.objectKey(id)
	opCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Verify ownership before deleting.  Read the .info file to get the
	// stored token.  Same error for "not found" vs "wrong token" to prevent
	// file-ID enumeration via the delete endpoint.
	out, err := ms.s3Client.GetObject(opCtx, &s3.GetObjectInput{
		Bucket: aws.String(ms.cfg.S3Bucket),
		Key:    aws.String(key + ".info"),
	})
	if err != nil {
		return mcp.NewToolResultError("invalid file_id or management_token"), nil
	}
	defer out.Body.Close()

	var info fileInfo
	if err := json.NewDecoder(out.Body).Decode(&info); err != nil {
		return mcp.NewToolResultError("invalid file_id or management_token"), nil
	}

	if !tokenMatches(info.MetaData["mgmt-token"], providedToken) {
		return mcp.NewToolResultError("invalid file_id or management_token"), nil
	}

	_, err = ms.s3Client.DeleteObjects(opCtx, &s3.DeleteObjectsInput{
		Bucket: aws.String(ms.cfg.S3Bucket),
		Delete: &s3types.Delete{
			Objects: []s3types.ObjectIdentifier{
				{Key: aws.String(key)},
				{Key: aws.String(key + ".info")},
			},
			Quiet: aws.Bool(true),
		},
	})
	if err != nil {
		return mcp.NewToolResultError("failed to delete file: " + err.Error()), nil
	}

	return toolResultJSON(map[string]any{"deleted": true, "file_id": id})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// generateToken returns a cryptographically random 32-byte token as a
// lowercase hex string (64 characters, 256-bit entropy).
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// tokenMatches compares stored and provided tokens in constant time to
// prevent timing-oracle attacks.  Both strings must be non-empty AND equal
// for the function to return true.  Using crypto/subtle instead of == or
// strings.Compare ensures the comparison time does not leak information
// about how many characters matched.
func tokenMatches(stored, provided string) bool {
	if stored == "" || provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(provided)) == 1
}

// objectKey converts a tus upload ID (possibly in "objectId+multipartId"
// format) to the S3 object key for the data file.
func (ms *MCPServer) objectKey(id string) string {
	objectId := id
	if i := strings.IndexByte(id, '+'); i >= 0 {
		objectId = id[:i]
	}
	return ms.cfg.S3ObjectPrefix + objectId
}

func toolResultJSON(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("failed to marshal result"), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}
