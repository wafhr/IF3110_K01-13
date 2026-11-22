package app

import "net/http"

// Modules holds the HTTP handler of each backend module. A nil handler leaves
// its routes unmounted.
type Modules struct {
	Access    http.Handler
	Database  http.Handler
	Viewer    http.Handler
	Migration http.Handler
	Job       http.Handler
	Frontend  http.Handler
}

// RoutesWith mounts the frontend last; Go's ServeMux selects more-specific API routes.
func RoutesWith(modules Modules) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	if modules.Database != nil {
		mux.Handle("/admin/databases", modules.Database)
		mux.Handle("/admin/databases/", modules.Database)
	}
	if modules.Access != nil {
		mux.Handle("/auth/", modules.Access)
		mux.Handle("/admin/", modules.Access)
		if modules.Viewer == nil {
			mux.Handle("/databases/", modules.Access)
		} else {
			mux.Handle("GET /databases/{databaseID}/access", modules.Access)
		}
	}
	if modules.Viewer != nil {
		mux.Handle("/databases", modules.Viewer)
		mux.Handle("/databases/", modules.Viewer)
	}
	if modules.Migration != nil {
		mux.Handle("/databases/{databaseID}/migrations/", modules.Migration)
		mux.Handle("/databases/{databaseID}/migrations/explain", modules.Migration)
		mux.Handle("/databases/{databaseID}/migrations/schema-diff", modules.Migration)
	}
	if modules.Job != nil {
		mux.Handle("/databases/{databaseID}/jobs", modules.Job)
		mux.Handle("/databases/{databaseID}/jobs/", modules.Job)
		mux.Handle("/databases/{databaseID}/attempts", modules.Job)
	}
	if modules.Frontend != nil {
		mux.Handle("/", modules.Frontend)
	}
	return mux
}
