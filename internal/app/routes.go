package app

import "net/http"

func Routes() http.Handler {
	return routes(nil, nil)
}

func RoutesWithAccess(accessHandler http.Handler) http.Handler {
	return routes(accessHandler, nil)
}

func RoutesWithFrontend(accessHandler, frontendHandler http.Handler) http.Handler {
	return routes(accessHandler, frontendHandler)
}

func routes(accessHandler, frontendHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	if accessHandler != nil {
		mux.Handle("/auth/", accessHandler)
		mux.Handle("/admin/", accessHandler)
		mux.Handle("/databases/", accessHandler)
	}
	if frontendHandler != nil {
		mux.Handle("/", frontendHandler)
	}
	return mux
}
