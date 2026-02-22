// Package openapi serves the OpenAPI spec, Swagger UI, and llms.txt.
package openapi

import (
	_ "embed"
	"net/http"
)

//go:embed spec.json
var specJSON []byte

//go:embed swagger_ui.html
var swaggerUI []byte

//go:embed llms.txt
var llmsTXT []byte

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
