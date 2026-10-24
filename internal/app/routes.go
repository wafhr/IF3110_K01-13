package app

import "net/http"

func Routes() http.Handler {
	return routes(nil, nil)
}

func RoutesWithAccess(accessHandler http.Handler) http.Handler {
	return routes(accessHandler, nil)
}

func RoutesWithMigration(accessHandler, migrationHandler http.Handler) http.Handler {
	return routes(accessHandler, migrationHandler)
}

func routes(accessHandler, migrationHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	if accessHandler != nil {
		mux.Handle("/auth/", accessHandler)
		mux.Handle("/admin/", routeByPrefix("/admin/databases", migrationHandler, accessHandler))
		mux.Handle("/databases/", routeByPrefix("/databases/", migrationHandler, accessHandler))
	}
	return mux
}

func routeByPrefix(prefix string, preferred, fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if preferred != nil && (prefix == "/databases/" && len(r.URL.Path) >= len(prefix) && containsMigrationPath(r.URL.Path) || prefix != "/databases/" && r.URL.Path == prefix) {
			preferred.ServeHTTP(w, r)
			return
		}
		fallback.ServeHTTP(w, r)
	})
}

func containsMigrationPath(path string) bool {
	for i := 0; i+len("/migrations") <= len(path); i++ {
		if path[i:i+len("/migrations")] == "/migrations" && (i+len("/migrations") == len(path) || path[i+len("/migrations")] == '/') {
			return true
		}
	}
	return false
}
