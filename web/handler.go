package web

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/wafhr/IF3110_K01-13/internal/access"
)

//go:embed static
var assets embed.FS

//go:embed index.html
var shell []byte

// NewHandler serves the single-page application shell and its static assets.
// Authentication and CSRF checks remain in access.HTTPHandler so the UI uses
// the same policy as the API.
func NewHandler(auth *access.HTTPHandler) http.Handler {
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

func spaPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(shell)
}
