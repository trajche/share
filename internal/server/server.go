package server

import (
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
func inlineDisposition(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(&inlineWriter{ResponseWriter: w}, r)
	})
}

// inlineWriter intercepts WriteHeader to rewrite Content-Disposition before
// the response is sent. Go's http.ResponseWriter does not allow header changes
// after WriteHeader, so we must intercept it.
type inlineWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *inlineWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		h := w.ResponseWriter.Header()
		cd := h.Get("Content-Disposition")
		if strings.HasPrefix(cd, "attachment") {
			h.Set("Content-Disposition", "inline"+strings.TrimPrefix(cd, "attachment"))
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
