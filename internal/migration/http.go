package migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

type HTTPHandler struct {
	service     *Service
	routes      *http.ServeMux
	schemas     SchemaReader
	definitions DefinitionStore
}

// DefinitionStore menyediakan operasi definisi dan revisi pada database platform.
type DefinitionStore interface {
	DefinitionReader
	List(context.Context, string) ([]Migration, error)
	Create(context.Context, string, string, string, Revision) (Migration, Revision, error)
	Revise(context.Context, string, string, string, string, int64, Revision) (Migration, Revision, error)
}

type SchemaReader interface {
	Schemas(context.Context, string) ([]database.Schema, error)
	Tables(context.Context, string, string) ([]database.Table, error)
	Table(context.Context, string, string, string) (database.TableDetails, error)
}

func NewHTTPHandler(service *Service, auth *access.HTTPHandler, readers ...SchemaReader) *HTTPHandler {
	h := &HTTPHandler{service: service, routes: http.NewServeMux()}
	if service != nil {
		h.definitions, _ = service.definitions.(DefinitionStore)
	}
	if len(readers) > 0 {
		h.schemas = readers[0]
	}
	h.routes.Handle("GET /databases/{databaseID}/migrations", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.list)))
	h.routes.Handle("POST /databases/{databaseID}/migrations", auth.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.execute)))
	h.routes.Handle("POST /databases/{databaseID}/migrations/schema-diff", auth.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.schemaDiff)))
	h.routes.Handle("POST /databases/{databaseID}/migrations/explain", auth.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.explain)))
	h.routes.Handle("GET /databases/{databaseID}/migrations/{version}", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.detail)))
	h.routes.Handle("GET /databases/{databaseID}/migrations/definitions", auth.RequireDatabaseAccess(access.ViewOnly, h.withDefinitions(h.listDefinitions)))
	h.routes.Handle("POST /databases/{databaseID}/migrations/definitions", auth.RequireDatabaseAccess(access.ViewEdit, h.withDefinitions(h.createDefinition)))
	h.routes.Handle("GET /databases/{databaseID}/migrations/definitions/{migrationID}", auth.RequireDatabaseAccess(access.ViewOnly, h.withDefinitions(h.getDefinition)))
	h.routes.Handle("PUT /databases/{databaseID}/migrations/definitions/{migrationID}", auth.RequireDatabaseAccess(access.ViewEdit, h.withDefinitions(h.reviseDefinition)))
	h.routes.Handle("GET /databases/{databaseID}/migrations/definitions/{migrationID}/revisions/{revision}", auth.RequireDatabaseAccess(access.ViewOnly, h.withDefinitions(h.getDefinitionRevision)))
	h.routes.Handle("POST /databases/{databaseID}/migrations/definitions/{migrationID}/actions", auth.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.submitAction)))
	h.routes.Handle("GET /databases/{databaseID}/migrations/history", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.executionHistory)))
	h.routes.Handle("GET /databases/{databaseID}/migrations/history/{executionID}", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.executionDetail)))
	return h
}
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h.routes.ServeHTTP(w, r)
}

// withDefinitions memeriksa ketersediaan repository setelah autentikasi dan permission.
func (h *HTTPHandler) withDefinitions(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.definitions == nil {
			jsonResponse(w, http.StatusServiceUnavailable, map[string]string{"error": "migration definitions unavailable"})
			return
		}
		next(w, r)
	}
}

type definitionInput struct {
	Name               string `json:"name"`
	SQLUp              string `json:"sql_up"`
	SQLDown            string `json:"sql_down"`
	Irreversible       bool   `json:"irreversible"`
	IrreversibleReason string `json:"irreversible_reason"`
}

func (input definitionInput) normalized(number int64) (string, Revision, error) {
	name, err := NormalizeMigrationName(input.Name)
	if err != nil {
		return "", Revision{}, err
	}
	revision, err := NormalizeRevision(Revision{Number: number, SQLUp: input.SQLUp, SQLDown: input.SQLDown,
		Irreversible: input.Irreversible, IrreversibleReason: input.IrreversibleReason})
	return name, revision, err
}

func (h *HTTPHandler) listDefinitions(w http.ResponseWriter, r *http.Request) {
	definitions, err := h.definitions.List(r.Context(), r.PathValue("databaseID"))
	if err != nil {
		migrationError(w, err)
		return
	}
	if definitions == nil {
		definitions = []Migration{}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"migrations": definitions})
}

func (h *HTTPHandler) createDefinition(w http.ResponseWriter, r *http.Request) {
	var input definitionInput
	if !decodeDefinitionJSON(w, r, &input) {
		return
	}
	name, revision, err := input.normalized(1)
	if err != nil {
		migrationError(w, err)
		return
	}
	databaseID := r.PathValue("databaseID")
	definition, stored, err := h.definitions.Create(r.Context(), databaseID, access.CurrentUser(r.Context()).ID, name, revision)
	if err != nil {
		migrationError(w, err)
		return
	}
	w.Header().Set("Location", "/databases/"+databaseID+"/migrations/definitions/"+definition.ID)
	jsonResponse(w, http.StatusCreated, map[string]any{"migration": definition, "revision": stored})
}

func (h *HTTPHandler) getDefinition(w http.ResponseWriter, r *http.Request) {
	databaseID, migrationID := r.PathValue("databaseID"), r.PathValue("migrationID")
	definition, err := h.definitions.Get(r.Context(), databaseID, migrationID)
	if err != nil {
		migrationError(w, err)
		return
	}
	revision, err := h.definitions.GetRevision(r.Context(), databaseID, migrationID, definition.CurrentRevision)
	if err != nil {
		migrationError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"migration": definition, "revision": revision})
}

func (h *HTTPHandler) reviseDefinition(w http.ResponseWriter, r *http.Request) {
	var input struct {
		definitionInput
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !decodeDefinitionJSON(w, r, &input) {
		return
	}
	if input.ExpectedRevision < 1 {
		migrationError(w, ErrInvalidMigration)
		return
	}
	name, revision, err := input.normalized(input.ExpectedRevision + 1)
	if err != nil {
		migrationError(w, err)
		return
	}
	definition, stored, err := h.definitions.Revise(r.Context(), r.PathValue("databaseID"), r.PathValue("migrationID"),
		access.CurrentUser(r.Context()).ID, name, input.ExpectedRevision, revision)
	if err != nil {
		migrationError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"migration": definition, "revision": stored})
}

func (h *HTTPHandler) getDefinitionRevision(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
	if err != nil || number < 1 {
		migrationError(w, ErrInvalidMigration)
		return
	}
	revision, err := h.definitions.GetRevision(r.Context(), r.PathValue("databaseID"), r.PathValue("migrationID"), number)
	if err != nil {
		migrationError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"revision": revision})
}

func (h *HTTPHandler) executionHistory(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) > 1 || (len(query) == 1 && !query.Has("hide_rollback")) {
		migrationError(w, ErrInvalidMigration)
		return
	}
	hideRollback := false
	if values, exists := query["hide_rollback"]; exists {
		if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
			migrationError(w, ErrInvalidMigration)
			return
		}
		hideRollback = values[0] == "true"
	}
	if h.service == nil {
		migrationError(w, ErrStorage)
		return
	}
	history, err := h.service.ReadExecutionHistory(r.Context(), r.PathValue("databaseID"), hideRollback)
	if err != nil {
		migrationError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, history)
}

func (h *HTTPHandler) executionDetail(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		migrationError(w, ErrStorage)
		return
	}
	execution, err := h.service.GetExecution(r.Context(), r.PathValue("databaseID"), r.PathValue("executionID"))
	if err != nil {
		migrationError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"execution": execution})
}

func decodeDefinitionJSON(w http.ResponseWriter, r *http.Request, input any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		jsonResponse(w, http.StatusUnsupportedMediaType, map[string]string{"error": "content type must be application/json"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	return decodeRequestJSON(w, r, input)
}

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

// execute mempertahankan jalur pengajuan lama dengan payload tindakan berapproval.
func (h *HTTPHandler) execute(w http.ResponseWriter, r *http.Request) {
	var input struct {
		MigrationID string `json:"migration_id"`
		Action      Action `json:"action"`
		ApprovalID  string `json:"approval_id"`
	}
	if !decodeDefinitionJSON(w, r, &input) {
		return
	}
	h.enqueueAction(w, r, input.MigrationID, input.Action, input.ApprovalID)
}

func (h *HTTPHandler) submitAction(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Action     Action `json:"action"`
		ApprovalID string `json:"approval_id"`
	}
	if !decodeDefinitionJSON(w, r, &input) {
		return
	}
	h.enqueueAction(w, r, r.PathValue("migrationID"), input.Action, input.ApprovalID)
}

func (h *HTTPHandler) enqueueAction(w http.ResponseWriter, r *http.Request, migrationID string, action Action, approvalID string) {
	result, err := h.service.SubmitAction(r.Context(), access.CurrentUser(r.Context()).ID,
		r.PathValue("databaseID"), migrationID, action, approvalID)
	if err != nil {
		migrationError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, result)
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
		writeJSONDecodeError(w, err)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSONDecodeError(w, err)
		return false
	}
	return true
}

func writeJSONDecodeError(w http.ResponseWriter, err error) {
	var sizeError *http.MaxBytesError
	if errors.As(err, &sizeError) {
		jsonResponse(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return
	}
	jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "request must contain one valid JSON object"})
}

func migrationError(w http.ResponseWriter, err error) {
	status, message := http.StatusInternalServerError, "migration operation failed"
	switch {
	case errors.Is(err, ErrExecutionUnavailable):
		status, message = http.StatusServiceUnavailable, "migration execution unavailable"
	case errors.Is(err, ErrApprovalRequired):
		status, message = http.StatusConflict, "migration approval required or outdated"
	case errors.Is(err, ErrInvalidMigration), errors.Is(err, database.ErrInvalidInput):
		status, message = http.StatusBadRequest, "invalid migration request"
	case errors.Is(err, ErrNotFound), errors.Is(err, database.ErrNotFound), errors.Is(err, sql.ErrNoRows):
		status, message = http.StatusNotFound, "database or migration not found"
	case errors.Is(err, ErrImmutableMigration):
		status, message = http.StatusConflict, "executed migration definitions cannot be revised"
	case errors.Is(err, ErrConflict):
		status, message = http.StatusConflict, "migration revision or execution state has changed"
	case errors.Is(err, context.DeadlineExceeded):
		status, message = http.StatusGatewayTimeout, "migration operation timed out"
	case errors.Is(err, context.Canceled):
		status, message = http.StatusRequestTimeout, "request canceled"
	case errors.Is(err, targetdb.ErrPermissionDenied), errors.Is(err, access.ErrForbidden):
		status, message = http.StatusForbidden, "database access denied"
	case errors.Is(err, targetdb.ErrAuthentication), errors.Is(err, targetdb.ErrConnection):
		status, message = http.StatusBadGateway, "could not connect to the target database"
	case errors.Is(err, targetdb.ErrClosed):
		status, message = http.StatusServiceUnavailable, "target database unavailable"
	default:
		slog.Error("migration operation failed")
	}
	jsonResponse(w, status, map[string]string{"error": message})
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
