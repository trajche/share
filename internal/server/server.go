package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tus/tusd/v2/pkg/handler"
	"sharemk/internal/config"
	"sharemk/internal/openapi"
	"sharemk/internal/ratelimit"
	"sharemk/internal/ui"
)

type Server struct {
	cfg     *config.Config
	handler http.Handler
}

func New(cfg *config.Config, tusHandler *handler.Handler, limiter *ratelimit.Limiter, mcpHandler http.Handler, openapiHandler http.Handler) *Server {
	mux := http.NewServeMux()

	mux.Handle("GET /{$}", ui.Handler())

	mux.HandleFunc("GET /health", healthHandler)

	// OpenAPI spec, Swagger UI, and LLM instructions.
	mux.Handle("GET /openapi.json", openapiHandler)
	mux.Handle("GET /docs", openapi.SwaggerUIHandler())
	mux.Handle("GET /llms.txt", openapi.LLMsHandler())

	// MCP Streamable HTTP transport (handles GET and POST).
	mux.Handle("/mcp", mcpHandler)

	// tusd's internal router does strings.Trim(path, "/") to detect the
	// creation endpoint (empty string = POST create). We must strip the base
	// path prefix before handing off so tusd sees "/" not "/files/".
	tusPrefix := strings.TrimSuffix(cfg.TUSBasePath, "/") // "/files/" → "/files"
	strippedTus := http.StripPrefix(tusPrefix, tusHandler)
	mux.Handle("/files/", limiter.Middleware(inlineDisposition(strippedTus)))

	return &Server{cfg: cfg, handler: mux}
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// inlineDisposition wraps a handler and rewrites Content-Disposition from
// "attachment" to "inline" on GET responses so that AI tools and browsers
// render the file content directly instead of treating it as a binary download.
// Pass ?dl=1 to force attachment (download) behaviour instead.
func inlineDisposition(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		dl := r.URL.Query().Get("dl") == "1"
		next.ServeHTTP(&inlineWriter{ResponseWriter: w, forceDownload: dl}, r)
	})
}

// inlineWriter intercepts WriteHeader to rewrite Content-Disposition and
// Content-Type before the response is sent. Go's http.ResponseWriter does not
// allow header changes after WriteHeader, so we must intercept it.
type inlineWriter struct {
	http.ResponseWriter
	forceDownload bool
	wroteHeader   bool
}

func (w *inlineWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		h := w.ResponseWriter.Header()

		if w.forceDownload {
			// Ensure attachment regardless of what tusd set.
			cd := h.Get("Content-Disposition")
			if strings.HasPrefix(cd, "inline") {
				h.Set("Content-Disposition", "attachment"+strings.TrimPrefix(cd, "inline"))
			} else if cd == "" {
				h.Set("Content-Disposition", "attachment")
			}
		} else {
			// Rewrite attachment → inline so browsers and AI tools render inline.
			cd := h.Get("Content-Disposition")
			if strings.HasPrefix(cd, "attachment") {
				h.Set("Content-Disposition", "inline"+strings.TrimPrefix(cd, "attachment"))
			}

			// Fix Content-Type when tusd falls back to binary/octet-stream.
			// Uploaders often send the MIME type as "content-type" metadata key
			// instead of the tusd-preferred "filetype" key.
			ct := h.Get("Content-Type")
			if ct == "application/octet-stream" || ct == "binary/octet-stream" {
				meta := parseTusdMeta(h.Get("Upload-Metadata"))
				if mime, ok := meta["filetype"]; ok && mime != "" {
					h.Set("Content-Type", mime)
				} else if mime, ok := meta["content-type"]; ok && mime != "" {
					h.Set("Content-Type", mime)
				}
			}
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *inlineWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// parseTusdMeta decodes the Upload-Metadata header value (comma-separated
// "key base64value" pairs) into a plain map.
func parseTusdMeta(raw string) map[string]string {
	m := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		kv := strings.SplitN(pair, " ", 2)
		if len(kv) == 2 {
			if b, err := base64.StdEncoding.DecodeString(kv[1]); err == nil {
				m[kv[0]] = string(b)
			}
		}
	}
	return m
}
