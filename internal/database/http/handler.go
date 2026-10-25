// Package databasehttp exposes the database-management HTTP API.
package databasehttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

type Registry interface {
	Create(context.Context, string, database.ConnectionConfig) (database.Database, error)
	Get(context.Context, string) (database.Database, error)
	List(context.Context) ([]database.Database, error)
	Update(context.Context, string, string, database.ConnectionConfig) (database.Database, error)
	DeleteWithAccessCleanup(context.Context, string) error
}

type Connections interface {
	Check(context.Context, string) error
	Test(context.Context, database.ConnectionConfig) error
	Invalidate(string) error
}

type Handler struct {
	registry    Registry
	connections Connections
	handler     http.Handler
}

// NewHandler always wraps its routes with the access module's admin and CSRF
// middleware. Never mount an unprotected copy of the management mux.
func NewHandler(registry Registry, connections Connections, auth *access.HTTPHandler) *Handler {
	h := &Handler{registry: registry, connections: connections}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/databases", h.list)
	mux.HandleFunc("POST /admin/databases", h.create)
	mux.HandleFunc("POST /admin/databases/test-connection", h.testConnection)
	mux.HandleFunc("GET /admin/databases/{databaseID}", h.get)
	mux.HandleFunc("PUT /admin/databases/{databaseID}", h.update)
	mux.HandleFunc("DELETE /admin/databases/{databaseID}", h.delete)
	mux.HandleFunc("POST /admin/databases/{databaseID}/test-connection", h.checkConnection)
	h.handler = auth.RequirePlatformAdmin(true, mux)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	serveAPI(w, r, h.handler)
}

func serveAPI(w http.ResponseWriter, r *http.Request, next http.Handler) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	next.ServeHTTP(w, r.WithContext(ctx))
}

type connectionInput struct {
	Host         string  `json:"host"`
	Port         *int    `json:"port"`
	DatabaseName string  `json:"database_name"`
	Username     string  `json:"username"`
	Password     *string `json:"password"`
	SSLMode      string  `json:"ssl_mode"`
}

type databaseInput struct {
	Name string `json:"name"`
	connectionInput
}

func (input connectionInput) config() (database.ConnectionConfig, error) {
	// Explicit presence prevents accidentally clearing a password during PUT.
	// An explicitly empty string remains valid for passwordless authentication.
	if input.Password == nil || (input.Port != nil && (*input.Port < 1 || *input.Port > 65535)) {
		return database.ConnectionConfig{}, database.ErrInvalidInput
	}
	config := database.ConnectionConfig{Host: input.Host, DatabaseName: input.DatabaseName,
		Username: input.Username, Password: *input.Password, SSLMode: input.SSLMode}
	if input.Port != nil {
		config.Port = *input.Port
	}
	return database.NormalizeConnectionConfig(config)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	databases, err := h.registry.List(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if databases == nil {
		databases = []database.Database{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": databases})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	metadata, err := h.registry.Get(r.Context(), r.PathValue("databaseID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"database": metadata})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var input databaseInput
	if !decodeJSON(w, r, &input) {
		return
	}
	config, err := input.config()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	metadata, err := h.registry.Create(r.Context(), input.Name, config)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	w.Header().Set("Location", "/admin/databases/"+metadata.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"database": metadata})
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	var input databaseInput
	if !decodeJSON(w, r, &input) {
		return
	}
	config, err := input.config()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	id := r.PathValue("databaseID")
	metadata, err := h.registry.Update(r.Context(), id, input.Name, config)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	h.invalidate(id)
	writeJSON(w, http.StatusOK, map[string]any{"database": metadata})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("databaseID")
	if err := h.registry.DeleteWithAccessCleanup(r.Context(), id); err != nil {
		writeServiceError(w, err)
		return
	}
	h.invalidate(id)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) invalidate(id string) {
	if err := h.connections.Invalidate(id); err != nil {
		// The write has already committed. Invalidate removes the cache entry
		// before closing its pool, so a close error must not imply a failed write.
		slog.Error("database pool cleanup failed")
	}
}

func (h *Handler) checkConnection(w http.ResponseWriter, r *http.Request) {
	if err := h.connections.Check(r.Context(), r.PathValue("databaseID")); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) testConnection(w http.ResponseWriter, r *http.Request) {
	var input connectionInput
	if !decodeJSON(w, r, &input) {
		return
	}
	config, err := input.config()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if err := h.connections.Test(r.Context(), config); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON request")
		}
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "request must contain one JSON value")
		}
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, database.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid database configuration")
	case errors.Is(err, database.ErrNotFound):
		writeError(w, http.StatusNotFound, "database not found")
	case errors.Is(err, database.ErrObjectNotFound):
		writeError(w, http.StatusNotFound, "schema or table not found")
	case errors.Is(err, database.ErrConflict):
		writeError(w, http.StatusConflict, "database name already exists")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "database operation timed out")
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusRequestTimeout, "request canceled")
	case errors.Is(err, targetdb.ErrAuthentication):
		writeError(w, http.StatusBadGateway, "target database authentication failed")
	case errors.Is(err, targetdb.ErrPermissionDenied):
		writeError(w, http.StatusBadGateway, "target database access denied")
	case errors.Is(err, targetdb.ErrConnection):
		writeError(w, http.StatusBadGateway, "target database connection failed")
	case errors.Is(err, targetdb.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "database connections unavailable")
	default:
		slog.Error("database management operation failed")
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
