package migration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/httpjson"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

func (h *HTTPHandler) schemaDiff(w http.ResponseWriter, r *http.Request) {
	if h.schemas == nil {
		httpjson.Error(w, http.StatusNotImplemented, "schema diff is unavailable")
		return
	}
	var input struct {
		Desired json.RawMessage `json:"desired_schema"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if !httpjson.Decode(w, r, &input) {
		return
	}
	desired, err := parseDesiredSchema(input.Desired)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid desired schema JSON")
		return
	}
	databaseID := r.PathValue("databaseID")
	probe, dbms, err := h.service.openTarget(r.Context(), databaseID)
	if err != nil {
		migrationError(w, r.Context(), err)
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
			writeViewerSchemaError(w, r.Context(), err)
			return
		}
		for _, table := range tables {
			if table.Type != "table" && table.Type != "partitioned_table" {
				continue
			}
			detail, err := h.schemas.Table(r.Context(), databaseID, schema, table.Name)
			if err != nil {
				writeViewerSchemaError(w, r.Context(), err)
				return
			}
			current = append(current, detail)
		}
	}
	diff, err := BuildSchemaDiff(dbms, current, desired)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid desired schema")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"schema_diff": diff})
}

func writeViewerSchemaError(w http.ResponseWriter, ctx context.Context, err error) {
	if errors.Is(err, targetdb.ErrPermissionDenied) {
		httpjson.Error(w, http.StatusForbidden, "target database access denied")
		return
	}
	if errors.Is(err, database.ErrInvalidInput) {
		httpjson.Error(w, http.StatusBadRequest, "invalid desired schema")
		return
	}
	migrationError(w, ctx, err)
}

func (h *HTTPHandler) explain(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SQL     string `json:"sql"`
		Analyze bool   `json:"analyze"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if !httpjson.Decode(w, r, &input) {
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
		httpjson.Error(w, status, message)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"query_plan": plan})
}
