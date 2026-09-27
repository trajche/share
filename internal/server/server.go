package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tus/tusd/v2/pkg/handler"
	"sharemk/internal/config"
	"sharemk/internal/files"
	"sharemk/internal/openapi"
	"sharemk/internal/ratelimit"
	"sharemk/internal/ui"
)

type Server struct {
	cfg     *config.Config
	files   *files.Service
	tus     http.Handler
	handler http.Handler
}

func New(cfg *config.Config, fs *files.Service, tusHandler *handler.Handler, limiter *ratelimit.Limiter, mcpHandler http.Handler, openapiHandler http.Handler) *Server {
	s := &Server{cfg: cfg, files: fs}

	mux := http.NewServeMux()

	mux.Handle("GET /{$}", ui.Handler())
	mux.Handle("GET /manage/{id}", ui.ManageHandler())
	mux.Handle("GET /ui/page.css", ui.CSSHandler())

	mux.HandleFunc("GET /health", healthHandler)

	// OpenAPI spec, Swagger UI, and LLM instructions.
	mux.Handle("GET /openapi.json", openapiHandler)
	mux.Handle("GET /docs", openapi.SwaggerUIHandler())
	mux.Handle("GET /llms.txt", openapi.LLMsHandler())

	// MCP Streamable HTTP transport (handles GET and POST).
	mux.Handle("/mcp", mcpHandler)

	// Management API (authenticated with the management token).
	mux.HandleFunc("GET /api/files/{id}", s.handleInfo)
	mux.HandleFunc("DELETE /api/files/{id}", s.handleDelete)

	// tusd's internal router does strings.Trim(path, "/") to detect the
	// creation endpoint (empty string = POST create). We must strip the base
	// path prefix before handing off so tusd sees "/" not "/files/".
	tusPrefix := strings.TrimSuffix(cfg.TUSBasePath, "/") // "/files/" → "/files"
	s.tus = http.StripPrefix(tusPrefix, tusHandler)
	mux.Handle(cfg.TUSBasePath, limiter.Middleware(http.HandlerFunc(s.handleFiles)))

	s.handler = mux
	return s
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleFiles routes requests under the tus base path. Downloads, deletion
// and password unlocking are handled here; everything else is tus.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, s.cfg.TUSBasePath)
	if id == "" {
		s.tus.ServeHTTP(w, r)
		return
	}
	if strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleDownload(w, r, id)
	case http.MethodPost:
		// tus only POSTs to the base path, so a POST to an upload URL is
		// the password form.
		s.handleUnlock(w, r, id)
	case http.MethodDelete:
		s.handleDelete(w, r)
	case http.MethodHead:
		s.tus.ServeHTTP(&metadataFilter{ResponseWriter: w}, r)
	default:
		s.tus.ServeHTTP(w, r)
	}
}

// metadataFilter removes secret values from the Upload-Metadata header that
// tusd sends in HEAD responses.
type metadataFilter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *metadataFilter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		h := w.Header()
		if raw := h.Get("Upload-Metadata"); raw != "" {
			public := files.PublicMetadata(handler.ParseMetadataHeader(raw))
			h.Set("Upload-Metadata", handler.SerializeMetadataHeader(public))
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *metadataFilter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *metadataFilter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
