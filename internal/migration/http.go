package migration

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/wafhr/IF3110_K01-13/internal/access"
)

type HTTPHandler struct {
	service *Service
	routes  *http.ServeMux
}

func NewHTTPHandler(service *Service, auth *access.HTTPHandler) *HTTPHandler {
	h := &HTTPHandler{service: service, routes: http.NewServeMux()}
	h.routes.Handle("GET /databases/{databaseID}/migrations", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.list)))
	h.routes.Handle("POST /databases/{databaseID}/migrations", auth.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.execute)))
	h.routes.Handle("GET /databases/{databaseID}/migrations/{version}", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.detail)))
	return h
}
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.routes.ServeHTTP(w, r) }

func (h *HTTPHandler) list(w http.ResponseWriter, r *http.Request) {
	rows, err := h.service.List(r.Context(), r.PathValue("databaseID"))
	if err != nil {
		migrationError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"migrations": rows})
}
func (h *HTTPHandler) detail(w http.ResponseWriter, r *http.Request) {
	v, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil || v < 1 {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid version"})
		return
	}
	row, err := h.service.Get(r.Context(), r.PathValue("databaseID"), v)
	if err != nil {
		migrationError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"migration": row})
}
func (h *HTTPHandler) execute(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
		SQL  string `json:"sql"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if !decodeRequestJSON(w, r, &input) {
		return
	}
	user := access.CurrentUser(r.Context())
	row, err := h.service.Execute(r.Context(), r.PathValue("databaseID"), user.ID, input.Name, input.SQL)
	if err != nil {
		if row.Version > 0 {
			jsonResponse(w, http.StatusUnprocessableEntity, map[string]any{"migration": row, "error": "migration failed and was rolled back"})
			return
		}
		migrationError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]any{"migration": row})
}
func decodeRequestJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "request must contain one JSON value"})
		return false
	}
	return true
}

func migrationError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrInvalidMigration) {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid migration request"})
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		jsonResponse(w, http.StatusNotFound, map[string]string{"error": "database or migration not found"})
		return
	}
	slog.Error("migration operation failed", "error", err)
	jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "migration operation failed"})
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
