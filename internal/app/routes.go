package app

import "net/http"

func Routes() http.Handler {
	return routes(nil, nil)
}

func RoutesWithAccess(accessHandler http.Handler) http.Handler {
	return routes(accessHandler, nil)
}

func RoutesWithDatabaseManagement(accessHandler, databaseHandler http.Handler) http.Handler {
	return routes(accessHandler, databaseHandler)
}

func routes(accessHandler, databaseHandler http.Handler) http.Handler {
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
		mux.Handle("/databases/", accessHandler)
	}
	return mux
}
