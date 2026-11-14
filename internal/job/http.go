package job

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/wafhr/IF3110_K01-13/internal/access"
)

const defaultListLimit = 50

type DatabaseGuard interface {
	RequireDatabaseAccess(access.Permission, http.Handler) http.Handler
}

type HTTPHandler struct {
	service *Service
	routes  *http.ServeMux
}

// NewHTTPHandler exposes job status to every user who can view the database,
// so View Only members can follow migrations submitted by others.
func NewHTTPHandler(service *Service, guard DatabaseGuard) *HTTPHandler {
	h := &HTTPHandler{service: service, routes: http.NewServeMux()}
	h.routes.Handle("GET /databases/{databaseID}/jobs", guard.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.list)))
	h.routes.Handle("GET /databases/{databaseID}/jobs/{jobID}", guard.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.get)))
	h.routes.Handle("POST /databases/{databaseID}/jobs/{jobID}/resume", guard.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.resume)))
	return h
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h.routes.ServeHTTP(w, r)
}

func (h *HTTPHandler) list(w http.ResponseWriter, r *http.Request) {
	limit, ok := listLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeError(w, http.StatusBadRequest, "limit must be between 1 and 100")
		return
	}
	jobs, err := h.service.Jobs(r.Context(), r.PathValue("databaseID"), limit)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (h *HTTPHandler) get(w http.ResponseWriter, r *http.Request) {
	job, err := h.service.Job(r.Context(), r.PathValue("databaseID"), r.PathValue("jobID"))
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

func (h *HTTPHandler) resume(w http.ResponseWriter, r *http.Request) {
	job, err := h.service.Resume(r.Context(), r.PathValue("databaseID"), r.PathValue("jobID"))
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func listLimit(value string) (int, bool) {
	if value == "" {
		return defaultListLimit, true
	}
	limit, err := strconv.Atoi(value)
	return limit, err == nil && limit >= 1 && limit <= MaxListLimit
}

func writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "job not found")
	case errors.Is(err, ErrInvalidJob):
		writeError(w, http.StatusBadRequest, "invalid job request")
	case errors.Is(err, ErrNotPartial):
		writeError(w, http.StatusConflict, ErrNotPartial.Error())
	default:
		slog.ErrorContext(r.Context(), "job_request_failed", "status", "failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
