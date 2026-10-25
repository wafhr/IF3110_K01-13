package migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

type HTTPHandler struct {
	service *Service
	routes  *http.ServeMux
	schemas SchemaReader
}

type SchemaReader interface {
	Schemas(context.Context, string) ([]database.Schema, error)
	Tables(context.Context, string, string) ([]database.Table, error)
	Table(context.Context, string, string, string) (database.TableDetails, error)
}

func NewHTTPHandler(service *Service, auth *access.HTTPHandler, readers ...SchemaReader) *HTTPHandler {
	h := &HTTPHandler{service: service, routes: http.NewServeMux()}
	if len(readers) > 0 {
		h.schemas = readers[0]
	}
	h.routes.Handle("GET /databases/{databaseID}/migrations", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.list)))
	h.routes.Handle("POST /databases/{databaseID}/migrations", auth.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.execute)))
	h.routes.Handle("POST /databases/{databaseID}/migrations/schema-diff", auth.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.schemaDiff)))
	h.routes.Handle("POST /databases/{databaseID}/migrations/explain", auth.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.explain)))
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

func (h *HTTPHandler) schemaDiff(w http.ResponseWriter, r *http.Request) {
	if h.schemas == nil {
		jsonResponse(w, http.StatusNotImplemented, map[string]string{"error": "schema diff is unavailable"})
		return
	}
	var input struct {
		Desired json.RawMessage `json:"desired_schema"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if !decodeRequestJSON(w, r, &input) {
		return
	}
	desired, err := parseDesiredSchema(input.Desired)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid desired schema JSON"})
		return
	}
	databaseID := r.PathValue("databaseID")
	probe, dbms, err := h.service.target(r.Context(), databaseID)
	if err != nil {
		migrationError(w, err)
		return
	}
	_ = probe.Close()
	schemas := map[string]bool{}
	for _, table := range desired.Tables {
		schemas[table.Schema] = true
	}
	current := []database.TableDetails{}
	for schema := range schemas {
		tables, err := h.schemas.Tables(r.Context(), databaseID, schema)
		if err != nil {
			writeViewerSchemaError(w, err)
			return
		}
		for _, table := range tables {
			if table.Type != "table" && table.Type != "partitioned_table" {
				continue
			}
			detail, err := h.schemas.Table(r.Context(), databaseID, schema, table.Name)
			if err != nil {
				writeViewerSchemaError(w, err)
				return
			}
			current = append(current, detail)
		}
	}
	diff, err := BuildSchemaDiff(dbms, current, desired)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid desired schema"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"schema_diff": diff})
}

func writeViewerSchemaError(w http.ResponseWriter, err error) {
	if errors.Is(err, targetdb.ErrPermissionDenied) {
		jsonResponse(w, http.StatusForbidden, map[string]string{"error": "target database access denied"})
		return
	}
	if errors.Is(err, database.ErrInvalidInput) {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid desired schema"})
		return
	}
	migrationError(w, err)
}

func (h *HTTPHandler) explain(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SQL     string `json:"sql"`
		Analyze bool   `json:"analyze"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if !decodeRequestJSON(w, r, &input) {
		return
	}
	plan, err := h.service.Explain(r.Context(), r.PathValue("databaseID"), input.SQL, input.Analyze)
	if err != nil {
		status, message := http.StatusInternalServerError, "query plan preview failed"
		switch {
		case errors.Is(err, errInvalidPlanQuery):
			status, message = http.StatusBadRequest, "Cannot plan this SQL. Use one supported statement, such as SELECT, INSERT, UPDATE, or DELETE, and check syntax and object names."
		case errors.Is(err, context.DeadlineExceeded):
			status, message = http.StatusGatewayTimeout, "query plan preview timed out"
		case errors.Is(err, context.Canceled):
			status, message = http.StatusRequestTimeout, "query plan preview canceled"
		case errors.Is(err, database.ErrNotFound):
			status, message = http.StatusNotFound, "database not found"
		case errors.Is(err, targetdb.ErrPermissionDenied):
			status, message = http.StatusForbidden, "target database permission denied"
		case errors.Is(err, targetdb.ErrAuthentication), errors.Is(err, targetdb.ErrConnection):
			status, message = http.StatusBadGateway, "could not connect to the target database"
		}
		jsonResponse(w, status, map[string]string{"error": message})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"query_plan": plan})
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
