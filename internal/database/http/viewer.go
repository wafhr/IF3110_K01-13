package databasehttp

import (
	"context"
	"errors"
	"net/http"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

type AccessibleRegistry interface {
	ListAccessible(context.Context, string) ([]database.AccessibleDatabase, error)
}

type SchemaReader interface {
	Schemas(context.Context, string) ([]database.Schema, error)
	Tables(context.Context, string, string) ([]database.Table, error)
	Table(context.Context, string, string, string) (database.TableDetails, error)
	Data(context.Context, string, string, string, database.PageOptions) (database.DataPage, error)
}

type ViewerHandler struct {
	registry AccessibleRegistry
	schemas  SchemaReader
	handler  http.Handler
}

func NewViewerHandler(registry AccessibleRegistry, schemas SchemaReader, auth *access.HTTPHandler) *ViewerHandler {
	h := &ViewerHandler{registry: registry, schemas: schemas}
	mux := http.NewServeMux()
	mux.Handle("GET /databases", auth.RequireAuthenticated(http.HandlerFunc(h.listAccessible)))
	mux.Handle("GET /databases/{databaseID}/schemas", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.listSchemas)))
	mux.Handle("GET /databases/{databaseID}/schemas/{schema}/tables", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.listTables)))
	mux.Handle("GET /databases/{databaseID}/schemas/{schema}/tables/{table}", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.tableDetails)))
	mux.Handle("GET /databases/{databaseID}/schemas/{schema}/tables/{table}/data", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.tableData)))
	h.handler = mux
	return h
}

func (h *ViewerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { serveAPI(w, r, h.handler) }

func (h *ViewerHandler) listAccessible(w http.ResponseWriter, r *http.Request) {
	user, ok := access.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	databases, err := h.registry.ListAccessible(r.Context(), user.ID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if databases == nil {
		databases = []database.AccessibleDatabase{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": databases})
}

func (h *ViewerHandler) listSchemas(w http.ResponseWriter, r *http.Request) {
	schemas, err := h.schemas.Schemas(r.Context(), r.PathValue("databaseID"))
	if err != nil {
		writeViewerError(w, err)
		return
	}
	if schemas == nil {
		schemas = []database.Schema{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"schemas": schemas})
}

func (h *ViewerHandler) listTables(w http.ResponseWriter, r *http.Request) {
	tables, err := h.schemas.Tables(r.Context(), r.PathValue("databaseID"), r.PathValue("schema"))
	if err != nil {
		writeViewerError(w, err)
		return
	}
	if tables == nil {
		tables = []database.Table{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tables": tables})
}

func (h *ViewerHandler) tableDetails(w http.ResponseWriter, r *http.Request) {
	table, err := h.schemas.Table(r.Context(), r.PathValue("databaseID"), r.PathValue("schema"), r.PathValue("table"))
	if err != nil {
		writeViewerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"table": table})
}

func writeViewerError(w http.ResponseWriter, err error) {
	if errors.Is(err, targetdb.ErrPermissionDenied) {
		writeError(w, http.StatusForbidden, "target database access denied")
		return
	}
	if errors.Is(err, database.ErrInvalidInput) {
		writeError(w, http.StatusBadRequest, "invalid schema or table name")
		return
	}
	writeServiceError(w, err)
}
