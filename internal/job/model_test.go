package job

import (
	"errors"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

const (
	testJobID       = "10000000-0000-4000-8000-000000000001"
	testExecutionID = "10000000-0000-4000-8000-000000000002"
	testApprovalID  = "10000000-0000-4000-8000-000000000003"
	testActorID     = "10000000-0000-4000-8000-000000000004"
	testDatabaseID  = "10000000-0000-4000-8000-000000000005"
	testMigrationID = "10000000-0000-4000-8000-000000000006"
	testWorkerID    = "worker-a"
)

func testRequest() migration.ExecutionRequest {
	return migration.ExecutionRequest{
		ExecutionID: testExecutionID, ApprovalID: testApprovalID, ActorID: testActorID,
		Plan: migration.ActionPlan{
			DatabaseID: testDatabaseID, MigrationID: testMigrationID, Revision: 2, Name: "add index",
			Action: migration.Apply, SQL: "CREATE INDEX orders_customer_idx ON orders (customer_id)",
		},
		TraceContext: telemetry.TraceContext{TraceParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
	}
}

func TestValidateRequest(t *testing.T) {
	tests := []struct {
		name   string
		change func(*migration.ExecutionRequest)
		valid  bool
	}{
		{name: "complete request", change: func(*migration.ExecutionRequest) {}, valid: true},
		{name: "rollback", change: func(r *migration.ExecutionRequest) { r.Plan.Action = migration.Rollback }, valid: true},
		{name: "forward", change: func(r *migration.ExecutionRequest) { r.Plan.Action = migration.Forward }, valid: true},
		{name: "missing execution ID", change: func(r *migration.ExecutionRequest) { r.ExecutionID = "" }},
		{name: "malformed approval ID", change: func(r *migration.ExecutionRequest) { r.ApprovalID = "approval" }},
		{name: "malformed actor ID", change: func(r *migration.ExecutionRequest) { r.ActorID = "actor" }},
		{name: "malformed database ID", change: func(r *migration.ExecutionRequest) { r.Plan.DatabaseID = "../db" }},
		{name: "malformed migration ID", change: func(r *migration.ExecutionRequest) { r.Plan.MigrationID = "x" }},
		{name: "unknown action", change: func(r *migration.ExecutionRequest) { r.Plan.Action = "drop" }},
		{name: "zero revision", change: func(r *migration.ExecutionRequest) { r.Plan.Revision = 0 }},
		{name: "blank SQL", change: func(r *migration.ExecutionRequest) { r.Plan.SQL = " \n\t" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := testRequest()
			test.change(&request)
			err := validateRequest(request)
			if test.valid != (err == nil) {
				t.Fatalf("validateRequest error = %v, want valid %t", err, test.valid)
			}
			if err != nil && !errors.Is(err, ErrInvalidJob) {
				t.Fatalf("error %v is not ErrInvalidJob", err)
			}
		})
	}
}

func TestStatusTerminal(t *testing.T) {
	tests := map[Status]bool{Pending: false, Running: false, Partial: false, Succeeded: true, Failed: true}
	for status, want := range tests {
		if got := status.Terminal(); got != want {
			t.Errorf("%s terminal = %t, want %t", status, got, want)
		}
	}
}

func TestSameExecutionIgnoresOnlyTraceContext(t *testing.T) {
	stored := testRequest()
	redelivered := testRequest()
	redelivered.TraceContext = telemetry.TraceContext{}
	if !sameExecution(stored, redelivered) {
		t.Fatal("a redelivery with another trace context was treated as a different execution")
	}
	changed := testRequest()
	changed.Plan.SQL = "DROP TABLE orders"
	if sameExecution(stored, changed) {
		t.Fatal("a different SQL snapshot was treated as the same execution")
	}
}

func TestDescribeCopiesRequestIdentity(t *testing.T) {
	job := Job{Request: testRequest()}
	job.describe()
	if job.ExecutionID != testExecutionID || job.DatabaseID != testDatabaseID || job.MigrationID != testMigrationID ||
		job.ActorID != testActorID || job.Action != migration.Apply || job.Revision != 2 {
		t.Fatalf("job identity = %+v", job)
	}
}
