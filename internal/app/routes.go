package app

import "net/http"

func Routes() http.Handler {
	return routes(nil, nil, nil, nil, nil)
}

// RoutesWithModulesAndFrontend wires every API module and the browser UI.
// The frontend is mounted last; Go's ServeMux selects more-specific API routes.
func RoutesWithModulesAndFrontend(accessHandler, databaseHandler, viewerHandler, migrationHandler, frontendHandler http.Handler) http.Handler {
	return routes(accessHandler, databaseHandler, viewerHandler, migrationHandler, frontendHandler)
}

func routes(accessHandler, databaseHandler, viewerHandler, migrationHandler, frontendHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	if databaseHandler != nil {
		mux.Handle("/admin/databases", databaseHandler)
		mux.Handle("/admin/databases/", databaseHandler)
	}
	if accessHandler != nil {
		mux.Handle("/auth/", accessHandler)
		mux.Handle("/admin/", accessHandler)
		if viewerHandler == nil {
			mux.Handle("/databases/", accessHandler)
		} else {
			mux.Handle("GET /databases/{databaseID}/access", accessHandler)
		}
	}
	if viewerHandler != nil {
		mux.Handle("/databases", viewerHandler)
		mux.Handle("/databases/", viewerHandler)
	}
	if migrationHandler != nil {
		mux.Handle("/databases/{databaseID}/migrations", migrationHandler)
		mux.Handle("/databases/{databaseID}/migrations/explain", migrationHandler)
		mux.Handle("/databases/{databaseID}/migrations/{version}", migrationHandler)
	}
	if frontendHandler != nil {
		mux.Handle("/", frontendHandler)
	}
	return mux
}
