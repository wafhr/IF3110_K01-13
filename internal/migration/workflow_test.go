package migration

import (
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/uuid"
)

func TestWorkflowSnapshotIsStableAndBindsAction(t *testing.T) {
	plan := ActionPlan{DatabaseID: "8f000000-0000-4000-8000-000000000001", MigrationID: "8f000000-0000-4000-8000-000000000002", Revision: 3, Name: "add index", Action: Apply, SQL: "CREATE INDEX x ON t(id)"}
	_, first, err := planSnapshot(plan)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := planSnapshot(plan)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("same action plan produced a different approval hash")
	}
	plan.Action = Rollback
	_, changed, err := planSnapshot(plan)
	if err != nil {
		t.Fatal(err)
	}
	if first == changed {
		t.Fatal("approval hash did not bind the action")
	}
}

func TestWorkflowIDIsUUID(t *testing.T) {
	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	if !uuid.Valid(id) {
		t.Fatalf("uuid.New() = %q, not a UUID", id)
	}
}
