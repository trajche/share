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
	mux.Handle("/files/", limiter.Middleware(strippedTus))

	return &Server{cfg: cfg, handler: mux}
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
