package migration

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/httpjson"
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
	h.routes.Handle("POST /databases/{databaseID}/migrations/schema-diff", auth.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.schemaDiff)))
	h.routes.Handle("POST /databases/{databaseID}/migrations/explain", auth.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.explain)))
	h.routes.Handle("GET /databases/{databaseID}/migrations/definitions", auth.RequireDatabaseAccess(access.ViewOnly, h.withDefinitions(h.listDefinitions)))
	h.routes.Handle("POST /databases/{databaseID}/migrations/definitions", auth.RequireDatabaseAccess(access.ViewEdit, h.withDefinitions(h.createDefinition)))
	h.routes.Handle("GET /databases/{databaseID}/migrations/definitions/{migrationID}", auth.RequireDatabaseAccess(access.ViewOnly, h.withDefinitions(h.getDefinition)))
	h.routes.Handle("PUT /databases/{databaseID}/migrations/definitions/{migrationID}", auth.RequireDatabaseAccess(access.ViewEdit, h.withDefinitions(h.reviseDefinition)))
	h.routes.Handle("GET /databases/{databaseID}/migrations/definitions/{migrationID}/revisions/{revision}", auth.RequireDatabaseAccess(access.ViewOnly, h.withDefinitions(h.getDefinitionRevision)))
	h.routes.Handle("POST /databases/{databaseID}/migrations/definitions/{migrationID}/actions", auth.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.submitAction)))
	h.routes.Handle("POST /databases/{databaseID}/migrations/definitions/{migrationID}/review", auth.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.submitReview)))
	h.routes.Handle("GET /databases/{databaseID}/migrations/definitions/{migrationID}/workflow", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.getLatestWorkflow)))
	h.routes.Handle("GET /databases/{databaseID}/migrations/workflows/{workflowID}", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.getWorkflow)))
	h.routes.Handle("POST /databases/{databaseID}/migrations/workflows/{workflowID}/approvals", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.approveWorkflow)))
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
			httpjson.Error(w, http.StatusServiceUnavailable, "migration definitions unavailable")
			return
		}
		next(w, r)
	}
}

type definitionInput struct {
	Name               string    `json:"name"`
	SQLUp              string    `json:"sql_up"`
	SQLDown            string    `json:"sql_down"`
	Irreversible       bool      `json:"irreversible"`
	IrreversibleReason string    `json:"irreversible_reason"`
	Batch              BatchSpec `json:"batch"`
}

func (input definitionInput) normalized(number int64) (string, Revision, error) {
	name, err := NormalizeMigrationName(input.Name)
	if err != nil {
		return "", Revision{}, err
	}
	revision, err := NormalizeRevision(Revision{Number: number, SQLUp: input.SQLUp, SQLDown: input.SQLDown,
		Irreversible: input.Irreversible, IrreversibleReason: input.IrreversibleReason, Batch: input.Batch})
	return name, revision, err
}

func (h *HTTPHandler) listDefinitions(w http.ResponseWriter, r *http.Request) {
	definitions, err := h.definitions.List(r.Context(), r.PathValue("databaseID"))
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	if definitions == nil {
		definitions = []Migration{}
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"migrations": definitions})
}

func (h *HTTPHandler) createDefinition(w http.ResponseWriter, r *http.Request) {
	var input definitionInput
	if !httpjson.Decode(w, r, &input) {
		return
	}
	name, revision, err := input.normalized(1)
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	databaseID := r.PathValue("databaseID")
	definition, stored, err := h.definitions.Create(r.Context(), databaseID, access.CurrentUser(r.Context()).ID, name, revision)
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	w.Header().Set("Location", "/databases/"+databaseID+"/migrations/definitions/"+definition.ID)
	httpjson.Write(w, http.StatusCreated, map[string]any{"migration": definition, "revision": stored})
}

func (h *HTTPHandler) getDefinition(w http.ResponseWriter, r *http.Request) {
	databaseID, migrationID := r.PathValue("databaseID"), r.PathValue("migrationID")
	definition, err := h.definitions.Get(r.Context(), databaseID, migrationID)
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	revision, err := h.definitions.GetRevision(r.Context(), databaseID, migrationID, definition.CurrentRevision)
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"migration": definition, "revision": revision})
}

func (h *HTTPHandler) reviseDefinition(w http.ResponseWriter, r *http.Request) {
	var input struct {
		definitionInput
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !httpjson.Decode(w, r, &input) {
		return
	}
	if input.ExpectedRevision < 1 {
		migrationError(w, r.Context(), ErrInvalidMigration)
		return
	}
	name, revision, err := input.normalized(input.ExpectedRevision + 1)
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	definition, stored, err := h.definitions.Revise(r.Context(), r.PathValue("databaseID"), r.PathValue("migrationID"),
		access.CurrentUser(r.Context()).ID, name, input.ExpectedRevision, revision)
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"migration": definition, "revision": stored})
}

func (h *HTTPHandler) getDefinitionRevision(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
	if err != nil || number < 1 {
		migrationError(w, r.Context(), ErrInvalidMigration)
		return
	}
	revision, err := h.definitions.GetRevision(r.Context(), r.PathValue("databaseID"), r.PathValue("migrationID"), number)
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"revision": revision})
}

func (h *HTTPHandler) executionHistory(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) > 1 || (len(query) == 1 && !query.Has("hide_rollback")) {
		migrationError(w, r.Context(), ErrInvalidMigration)
		return
	}
	hideRollback := false
	if values, exists := query["hide_rollback"]; exists {
		if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
			migrationError(w, r.Context(), ErrInvalidMigration)
			return
		}
		hideRollback = values[0] == "true"
	}
	if h.service == nil {
		migrationError(w, r.Context(), ErrStorage)
		return
	}
	history, err := h.service.ReadExecutionHistory(r.Context(), r.PathValue("databaseID"), hideRollback)
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusOK, history)
}

func (h *HTTPHandler) executionDetail(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		migrationError(w, r.Context(), ErrStorage)
		return
	}
	execution, err := h.service.GetExecution(r.Context(), r.PathValue("databaseID"), r.PathValue("executionID"))
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"execution": execution})
}

func (h *HTTPHandler) submitAction(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Action     Action `json:"action"`
		ApprovalID string `json:"approval_id"`
	}
	if !httpjson.Decode(w, r, &input) {
		return
	}
	h.enqueueAction(w, r, r.PathValue("migrationID"), input.Action, input.ApprovalID)
}

func (h *HTTPHandler) enqueueAction(w http.ResponseWriter, r *http.Request, migrationID string, action Action, approvalID string) {
	result, err := h.service.SubmitAction(r.Context(), access.CurrentUser(r.Context()).ID,
		r.PathValue("databaseID"), migrationID, action, approvalID)
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusAccepted, result)
}

func migrationError(w http.ResponseWriter, ctx context.Context, err error) {
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
	case errors.Is(err, ErrMigrationOrder):
		status, message = http.StatusConflict, strings.TrimPrefix(err.Error(), ErrMigrationOrder.Error()+": ")
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
		slog.ErrorContext(ctx, "migration_operation_failed", "status", "failed")
	}
	httpjson.Error(w, status, message)
}
