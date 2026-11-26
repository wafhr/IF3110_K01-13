package job

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/httpjson"
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
	h.routes.Handle("GET /databases/{databaseID}/attempts", guard.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.attempts)))
	h.routes.Handle("POST /databases/{databaseID}/jobs/{jobID}/resume", guard.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.resume)))
	h.routes.Handle("POST /databases/{databaseID}/jobs/{jobID}/cancel", guard.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.cancel)))
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
		httpjson.Error(w, http.StatusBadRequest, "limit must be between 1 and 100")
		return
	}
	jobs, err := h.service.Jobs(r.Context(), r.PathValue("databaseID"), limit)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (h *HTTPHandler) attempts(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit, ok := listLimit(query.Get("limit"))
	if !ok {
		httpjson.Error(w, http.StatusBadRequest, "limit must be between 1 and 100")
		return
	}
	var before int64
	if raw := query.Get("before"); raw != "" {
		cursor, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || cursor < 1 {
			httpjson.Error(w, http.StatusBadRequest, "before must be a positive integer")
			return
		}
		before = cursor
	}
	page, err := h.service.Attempts(r.Context(), r.PathValue("databaseID"), limit, before)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, page)
}

func (h *HTTPHandler) get(w http.ResponseWriter, r *http.Request) {
	job, err := h.service.Job(r.Context(), r.PathValue("databaseID"), r.PathValue("jobID"))
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"job": job})
}

func (h *HTTPHandler) resume(w http.ResponseWriter, r *http.Request) {
	h.requeue(w, r, h.service.Resume)
}

func (h *HTTPHandler) cancel(w http.ResponseWriter, r *http.Request) {
	h.requeue(w, r, h.service.Cancel)
}

func (h *HTTPHandler) requeue(w http.ResponseWriter, r *http.Request, action func(context.Context, string, string) (Job, error)) {
	job, err := action(r.Context(), r.PathValue("databaseID"), r.PathValue("jobID"))
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusAccepted, map[string]any{"job": job})
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
		httpjson.Error(w, http.StatusNotFound, "job not found")
	case errors.Is(err, ErrInvalidJob):
		httpjson.Error(w, http.StatusBadRequest, "invalid job request")
	case errors.Is(err, ErrNotResumable), errors.Is(err, ErrNotCancelable), errors.Is(err, ErrIrreversible):
		httpjson.Error(w, http.StatusConflict, err.Error())
	default:
		slog.ErrorContext(r.Context(), "job_request_failed", "status", "failed", "error", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal error")
	}
}
