// Package openapi serves the OpenAPI spec, Swagger UI, and llms.txt.
package openapi

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed spec.json
var specJSON []byte

//go:embed swagger_ui.html
var swaggerUI []byte

//go:embed llms.txt
var llmsTXT []byte

// SetVersion stamps the running build's version (e.g. "v1.2.3") into the
// served spec's info.version so the docs never drift from the release.
// Development builds ("dev") keep the version written in spec.json.
func SetVersion(version string) {
	if version == "" || version == "dev" {
		return
	}
	var spec map[string]any
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return
	}
	info, ok := spec["info"].(map[string]any)
	if !ok {
		return
	}
	info["version"] = strings.TrimPrefix(version, "v")
	if b, err := json.MarshalIndent(spec, "", "  "); err == nil {
		specJSON = b
	}
}

// Version returns info.version of the served spec.
func Version() string {
	var spec struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	json.Unmarshal(specJSON, &spec) //nolint:errcheck
	return spec.Info.Version
}

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write(specJSON) //nolint:errcheck
	})
}

func SwaggerUIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(swaggerUI) //nolint:errcheck
	})
}

func LLMsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(llmsTXT) //nolint:errcheck
	})
}
