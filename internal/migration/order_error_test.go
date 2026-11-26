package migration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOrderErrorsExplainTheRule(t *testing.T) {
	state := MigrationState{DatabaseID: testOrderDatabaseID, Active: []Execution{{ID: testOrderExecutionID, MigrationID: testOrderLastMigrationID, Revision: 1}}}
	_, _, err := state.actionRevision(Migration{ID: testOrderOtherMigrationID, DatabaseID: testOrderDatabaseID, CurrentRevision: 1}, Rollback)
	if !errors.Is(err, ErrMigrationOrder) || !errors.Is(err, ErrConflict) {
		t.Fatalf("rollback of an earlier migration returned %v", err)
	}
	response := httptest.NewRecorder()
	migrationError(response, context.Background(), err)
	var body map[string]string
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusConflict || body["error"] != "only the last active migration can be rolled back" {
		t.Fatalf("status = %d, error = %q", response.Code, body["error"])
	}

	response = httptest.NewRecorder()
	migrationError(response, context.Background(), ErrApprovalRequired)
	if response.Code != http.StatusConflict {
		t.Fatalf("approval error status = %d", response.Code)
	}
}

const (
	testOrderDatabaseID       = "40000000-0000-4000-8000-000000000001"
	testOrderExecutionID      = "40000000-0000-4000-8000-000000000002"
	testOrderLastMigrationID  = "40000000-0000-4000-8000-000000000003"
	testOrderOtherMigrationID = "40000000-0000-4000-8000-000000000004"
)
