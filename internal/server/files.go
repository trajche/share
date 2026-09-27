package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/tus/tusd/v2/pkg/handler"
	"sharemk/internal/files"
	"sharemk/internal/hooks"
	"sharemk/internal/ui"
)

const (
	unlockCookiePrefix = "sm_unlock_"
	unlockCookieMaxAge = 12 * time.Hour
)

// handleDownload serves a completed upload straight from S3, honouring Range
// requests (needed for video seeking and Safari playback), the inline vs.
// download policy, and password protection.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, id string) {
	info, ok := s.loadOrFail(w, r, id)
	if !ok {
		return
	}
	objectID, _, _ := files.SplitID(info.ID)
	protected := info.MetaData[files.MetaPasswordHash] != ""
	if protected && !s.passwordOK(r, info.MetaData[files.MetaPasswordHash], objectID) {
		s.passwordRequired(w, r, id, "")
		return
	}

	in := &s3.GetObjectInput{
		Bucket: aws.String(s.files.Bucket()),
		Key:    aws.String(s.files.Key(objectID)),
	}
	// Ignore Range when If-Range is present: S3 cannot evaluate it for us,
	// and a full response is always a correct answer.
	if rg := r.Header.Get("Range"); rg != "" && r.Header.Get("If-Range") == "" {
		in.Range = aws.String(rg)
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		in.IfNoneMatch = aws.String(inm)
	}

	out, err := s.files.S3().GetObject(r.Context(), in)
	if err != nil {
		switch status := files.HTTPStatus(err); {
		case status == http.StatusNotModified:
			w.WriteHeader(http.StatusNotModified)
		case status == http.StatusRequestedRangeNotSatisfiable:
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
			writeError(w, status, "requested range not satisfiable")
		case files.IsNotFound(err):
			// The .info exists but the data object does not: s3store only
			// creates it when the multipart upload completes.
			writeError(w, http.StatusConflict, "upload is not complete yet")
		default:
			slog.Error("download: GetObject failed", "id", id, "error", err)
			writeError(w, http.StatusBadGateway, "failed to read file")
		}
		return
	}
	defer out.Body.Close()

	p := files.Present(info.MetaData, r.URL.Query().Get("dl") == "1")

	h := w.Header()
	h.Set("Content-Type", p.ContentType)
	h.Set("Content-Disposition", p.ContentDisposition)
	h.Set("Content-Security-Policy", p.ContentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Accept-Ranges", "bytes")
	if protected {
		h.Set("Cache-Control", "private, no-store")
	} else {
		h.Set("Cache-Control", "private, no-cache")
		if r.Header.Get("Origin") != "" {
			h.Set("Access-Control-Allow-Origin", "*")
		}
	}
	if out.ContentLength != nil {
		h.Set("Content-Length", strconv.FormatInt(*out.ContentLength, 10))
	}
	if out.ETag != nil {
		h.Set("ETag", *out.ETag)
	}
	if out.LastModified != nil {
		h.Set("Last-Modified", out.LastModified.UTC().Format(http.TimeFormat))
	}

	status := http.StatusOK
	if out.ContentRange != nil {
		h.Set("Content-Range", *out.ContentRange)
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)

	if _, err := io.Copy(w, out.Body); err != nil && !errors.Is(err, context.Canceled) {
		slog.Debug("download: copy interrupted", "id", id, "error", err)
	}
}

// passwordOK accepts either HTTP Basic auth (any username) or the unlock
// cookie set by the password form.
func (s *Server) passwordOK(r *http.Request, hash, objectID string) bool {
	if _, pw, ok := r.BasicAuth(); ok {
		return files.VerifyPassword(hash, pw)
	}
	if c, err := r.Cookie(unlockCookiePrefix + objectID); err == nil {
		return files.VerifyUnlock(hash, objectID, c.Value)
	}
	return false
}

// passwordRequired shows the password form to browsers and a JSON error to
// API clients.
func (s *Server) passwordRequired(w http.ResponseWriter, r *http.Request, id, errMsg string) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		ui.RenderUnlock(w, http.StatusUnauthorized, ui.UnlockData{
			Action:   s.cfg.TUSBasePath + id,
			Error:    errMsg,
			Download: r.URL.Query().Get("dl") == "1" || r.PostFormValue("dl") == "1",
		})
		return
	}
	if errMsg == "" {
		errMsg = "this file is password protected; send the password with HTTP Basic auth (any username)"
	}
	writeError(w, http.StatusUnauthorized, errMsg)
}

// handleUnlock processes the password form and sets the unlock cookie.
func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request, id string) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form")
		return
	}

	info, ok := s.loadOrFail(w, r, id)
	if !ok {
		return
	}
	target := s.cfg.TUSBasePath + id
	if r.PostFormValue("dl") == "1" {
		target += "?dl=1"
	}

	hash := info.MetaData[files.MetaPasswordHash]
	if hash == "" {
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	if !files.VerifyPassword(hash, r.PostFormValue("password")) {
		s.passwordRequired(w, r, id, "Wrong password. Try again.")
		return
	}

	objectID, _, _ := files.SplitID(info.ID)
	http.SetCookie(w, &http.Cookie{
		Name:     unlockCookiePrefix + objectID,
		Value:    files.UnlockValue(hash, objectID),
		Path:     s.cfg.TUSBasePath,
		MaxAge:   int(unlockCookieMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.cfg.PublicURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// managementToken extracts the token from "Authorization: Bearer <token>"
// or the Upload-Management-Token header.
func managementToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return r.Header.Get(hooks.HeaderManagementToken)
}

// authorize loads an upload and checks the management token. Unknown IDs
// and wrong tokens get the same response to prevent ID enumeration.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, id string) (*handler.FileInfo, bool) {
	token := managementToken(r)
	if token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="share.mk"`)
		writeError(w, http.StatusUnauthorized, "management token required (Authorization: Bearer <token>)")
		return nil, false
	}
	info, err := s.files.Load(r.Context(), id)
	if err != nil && !errors.Is(err, files.ErrNotFound) {
		slog.Error("files: load failed", "id", id, "error", err)
		writeError(w, http.StatusBadGateway, "failed to read file metadata")
		return nil, false
	}
	if err != nil || !files.TokenMatches(info.MetaData, token) {
		writeError(w, http.StatusNotFound, "invalid file id or management token")
		return nil, false
	}
	return info, true
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	info, ok := s.authorize(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.files.Describe(r.Context(), info))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = strings.TrimPrefix(r.URL.Path, s.cfg.TUSBasePath)
	}
	info, ok := s.authorize(w, r, id)
	if !ok {
		return
	}
	if err := s.files.Delete(r.Context(), info); err != nil {
		slog.Error("files: delete failed", "id", id, "error", err)
		writeError(w, http.StatusBadGateway, "failed to delete file")
		return
	}
	slog.Info("files: deleted by owner", "id", info.ID)
	w.Header().Set("Tus-Resumable", "1.0.0")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) loadOrFail(w http.ResponseWriter, r *http.Request, id string) (*handler.FileInfo, bool) {
	info, err := s.files.Load(r.Context(), id)
	if errors.Is(err, files.ErrNotFound) {
		writeError(w, http.StatusNotFound, "file not found")
		return nil, false
	}
	if err != nil {
		slog.Error("files: load failed", "id", id, "error", err)
		writeError(w, http.StatusBadGateway, "failed to read file metadata")
		return nil, false
	}
	return info, true
}
