package ui

import (
	_ "embed"
	"html/template"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

//go:embed manage.html
var manageHTML []byte

//go:embed page.css
var pageCSS string

//go:embed unlock.html
var unlockHTML string

//go:embed message.html
var messageHTML string

var (
	unlockTmpl  = template.Must(template.New("unlock").Parse(unlockHTML))
	messageTmpl = template.Must(template.New("message").Parse(messageHTML))
)

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML) //nolint:errcheck
	})
}

// ManageHandler serves the page that lets an uploader view and delete a file
// using the management token from the URL fragment.
func ManageHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Write(manageHTML) //nolint:errcheck
	})
}

// CSSHandler serves the stylesheet shared by the manage and unlock pages.
func CSSHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write([]byte(pageCSS)) //nolint:errcheck
	})
}

// UnlockData fills the password form.
type UnlockData struct {
	Action   string
	Error    string
	Download bool
	CSS      template.CSS
}

// RenderUnlock writes the password form for a protected file.
func RenderUnlock(w http.ResponseWriter, status int, data UnlockData) {
	data.CSS = template.CSS(pageCSS)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	w.WriteHeader(status)
	unlockTmpl.Execute(w, data) //nolint:errcheck
}

// MessageData fills a simple status page (e.g. an expired link).
type MessageData struct {
	Title string
	Text  string
	CSS   template.CSS
}

// RenderMessage writes a status page for browser visitors.
func RenderMessage(w http.ResponseWriter, status int, data MessageData) {
	data.CSS = template.CSS(pageCSS)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	messageTmpl.Execute(w, data) //nolint:errcheck
}
