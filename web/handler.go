package web

import (
	"io/fs"
	"net/http"

	"github.com/wafhr/IF3110_K01-13/internal/access"
)

// NewHandler serves browser pages and static assets. Authentication and CSRF
// checks remain in access.HTTPHandler so the UI uses the same policy as the API.
func NewHandler(service *access.Service, auth *access.HTTPHandler) http.Handler {
	mux := http.NewServeMux()
	static, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /login", spaPage)
	mux.Handle("GET /{$}", auth.RequireBrowserPage(false, false, http.HandlerFunc(spaPage)))
	mux.Handle("GET /manage/", auth.RequireBrowserPage(true, false, http.HandlerFunc(spaPage)))
	mux.Handle("GET /workspace/", auth.RequireBrowserPage(false, false, http.HandlerFunc(spaPage)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

func spaPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Database Platform</title><link rel="stylesheet" href="/assets/css/app.css"></head><body><div id="app"><p>Loading…</p></div><script type="module" src="/assets/js/spa.js"></script></body></html>`))
}
