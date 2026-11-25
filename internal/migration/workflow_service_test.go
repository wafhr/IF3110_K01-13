package migration_test

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/testsql"
	"github.com/wafhr/IF3110_K01-13/internal/uuid"
)

const (
	workflowID       = "00000000-0000-4000-8000-000000000031"
	workflowGroupA   = "00000000-0000-4000-8000-000000000041"
	workflowGroupB   = "00000000-0000-4000-8000-000000000042"
	workflowLeaderB  = "00000000-0000-4000-8000-000000000052"
	workflowOldLead  = "00000000-0000-4000-8000-000000000053"
	workflowRevision = int64(2)
	workflowHash     = "0000000000000000000000000000000000000000000000000000000000000000"
)

// Statement fragments that identify each query of WorkflowService.
const (
	sqlWorkflowRow      = "plan_hash, submitted_by::text, submitted_at, policy_epoch"
	sqlPolicyEpoch      = "SELECT COALESCE((SELECT epoch FROM access_database_approval_epochs"
	sqlRequiredGroups   = "ORDER BY g.name, g.id"
	sqlLatestWorkflow   = "ORDER BY submitted_at DESC, id DESC LIMIT 1"
	sqlCurrentRevision  = "SELECT current_revision FROM migration_definitions WHERE id = $1 AND database_id = $2"
	sqlInitEpoch        = "INSERT INTO access_database_approval_epochs(database_id, epoch) VALUES ($1, 1)"
	sqlLockEpoch        = "WHERE database_id = $1 FOR UPDATE"
	sqlCountGroups      = "SELECT count(*) FROM access_group_database_permissions"
	sqlInsertWorkflow   = "INSERT INTO migration_workflows"
	sqlLockWorkflow     = "FOR UPDATE OF w"
	sqlShareEpoch       = "WHERE database_id = $1 FOR SHARE"
	sqlLeaderGroups     = "FOR UPDATE OF g"
	sqlInsertApproval   = "INSERT INTO migration_approvals"
	sqlUpdateStatus     = "UPDATE migration_workflows SET status"
	sqlApprovalSnapshot = "SELECT plan, plan_hash, action"
	sqlMissingApproval  = "m.user_id IS NULL OR a.group_id IS NULL"
)

func workflowPlan() migration.ActionPlan {
	return migration.ActionPlan{
		DatabaseID: repositoryDatabaseID, MigrationID: repositoryMigrationID, Revision: workflowRevision,
		Name: "ubah pelanggan", Action: migration.Apply, SQL: "UPDATE customers SET name = 'baru'",
		BaseExecutionID: executionID(7),
	}
}

// workflowSnapshot mirrors the stored approval snapshot: the plan as JSON and
// the SHA-256 of that JSON.
func workflowSnapshot(t *testing.T, plan migration.ActionPlan) (string, string) {
	t.Helper()
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return string(data), hex.EncodeToString(sum[:])
}

// approvalValues is one required group with its current leader and, when
// approvedBy is set, the leader who approved the workflow.
func approvalValues(group, name, permission, leader, approvedBy string) []driver.Value {
	var by, at driver.Value
	if approvedBy != "" {
		by, at = approvedBy, repositoryTime
	}
	return []driver.Value{group, name, permission, leader, by, at}
}

func workflowIDArgs(id func() string, rest ...any) func(*testing.T, []driver.NamedValue) {
	return func(t *testing.T, args []driver.NamedValue) {
		t.Helper()
		repositoryArguments(append([]any{id()}, rest...)...)(t, args)
	}
}

func fixedID(id string) func() string { return func() string { return id } }

// workflowReadSteps answers GetWorkflow: the stored workflow, the current
// access policy epoch, and the groups that must approve.
func workflowReadSteps(id func() string, storedEpoch, currentRevision, currentEpoch int64, approvals ...[]driver.Value) []repositoryStep {
	return []repositoryStep{
		{operation: "query", statement: sqlWorkflowRow, check: workflowIDArgs(id, repositoryDatabaseID), rows: [][]driver.Value{{
			workflowID, repositoryDatabaseID, repositoryMigrationID, workflowRevision, "apply", "pending_review",
			workflowHash, repositoryActorID, repositoryTime, storedEpoch, currentRevision,
		}}},
		{operation: "query", statement: sqlPolicyEpoch, rows: [][]driver.Value{{currentEpoch}}, check: repositoryArguments(repositoryDatabaseID)},
		{operation: "query", statement: sqlRequiredGroups, rows: approvals, check: workflowIDArgs(id, repositoryDatabaseID)},
	}
}

func testWorkflowService(t *testing.T, steps ...repositoryStep) *migration.WorkflowService {
	t.Helper()
	service, err := migration.NewWorkflowService(testMigrationDatabase(t, steps...))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// approvalStates lists whether each required group approved. approved_at is
// reported only for approved groups.
func approvalStates(t *testing.T, workflow migration.Workflow) []bool {
	t.Helper()
	states := make([]bool, len(workflow.Approvals))
	for index, approval := range workflow.Approvals {
		states[index] = approval.Approved
		if approval.Approved != (approval.ApprovedAt != nil) {
			t.Fatalf("approved_at grup %s tidak sesuai status approval", approval.GroupID)
		}
	}
	return states
}

func TestNewWorkflowServiceRequiresPlatformDatabase(t *testing.T) {
	if _, err := migration.NewWorkflowService(nil); err == nil {
		t.Fatal("workflow service tanpa database platform diterima")
	}
}

func TestGetWorkflowDerivesStatusFromCurrentLeadersAndPolicy(t *testing.T) {
	editors := func(approvedBy string) []driver.Value {
		return approvalValues(workflowGroupA, "editors", "view_edit", repositoryActorID, approvedBy)
	}
	readers := func(leader, approvedBy string) []driver.Value {
		return approvalValues(workflowGroupB, "readers", "view_only", leader, approvedBy)
	}
	for _, tc := range []struct {
		name            string
		storedEpoch     int64
		currentRevision int64
		currentEpoch    int64
		approvals       [][]driver.Value
		status          migration.WorkflowStatus
		approved        []bool
	}{
		{"every leader approved", 1, workflowRevision, 1, [][]driver.Value{editors(repositoryActorID), readers(workflowLeaderB, workflowLeaderB)},
			migration.WorkflowApproved, []bool{true, true}},
		{"view-only leader still pending", 1, workflowRevision, 1, [][]driver.Value{editors(repositoryActorID), readers(workflowLeaderB, "")},
			migration.WorkflowPendingReview, []bool{true, false}},
		{"approval by a replaced leader", 1, workflowRevision, 1, [][]driver.Value{editors(repositoryActorID), readers(workflowLeaderB, workflowOldLead)},
			migration.WorkflowPendingReview, []bool{true, false}},
		{"group without leader", 1, workflowRevision, 1, [][]driver.Value{editors(repositoryActorID), readers("", "")},
			migration.WorkflowPendingReview, []bool{true, false}},
		{"no group has access", 1, workflowRevision, 1, nil, migration.WorkflowPendingReview, []bool{}},
		{"access policy changed", 1, workflowRevision, 2, [][]driver.Value{editors(repositoryActorID), readers(workflowLeaderB, workflowLeaderB)},
			migration.WorkflowDraft, []bool{false, false}},
		{"definition revised", 1, workflowRevision + 1, 1, [][]driver.Value{editors(repositoryActorID), readers(workflowLeaderB, workflowLeaderB)},
			migration.WorkflowDraft, []bool{false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := testWorkflowService(t, workflowReadSteps(fixedID(workflowID), tc.storedEpoch, tc.currentRevision, tc.currentEpoch, tc.approvals...)...)
			workflow, err := service.GetWorkflow(context.Background(), repositoryActorID, repositoryDatabaseID, workflowID)
			if err != nil {
				t.Fatal(err)
			}
			if workflow.Status != tc.status || !reflect.DeepEqual(approvalStates(t, workflow), tc.approved) {
				t.Fatalf("status workflow = %s %v; ingin %s %v", workflow.Status, approvalStates(t, workflow), tc.status, tc.approved)
			}
			if workflow.ID != workflowID || workflow.Revision != workflowRevision || workflow.PlanHash != workflowHash ||
				workflow.SubmittedBy != repositoryActorID || workflow.SubmittedAt == nil {
				t.Fatalf("identitas workflow tidak sesuai: %+v", workflow)
			}
		})
	}
}

func TestGetWorkflowReportsMissingAndStorageErrors(t *testing.T) {
	failure := errors.New("platform unavailable")
	for _, tc := range []struct {
		name  string
		id    string
		steps []repositoryStep
		want  error
	}{
		{"invalid ID", "bukan-uuid", nil, migration.ErrNotFound},
		{"unknown workflow", workflowID, []repositoryStep{{operation: "query", statement: sqlWorkflowRow}}, migration.ErrNotFound},
		{"workflow read fails", workflowID, []repositoryStep{{operation: "query", statement: sqlWorkflowRow, err: failure}}, migration.ErrStorage},
		{"request canceled", workflowID, []repositoryStep{{operation: "query", statement: sqlWorkflowRow, err: context.Canceled}}, context.Canceled},
		{"policy read fails", workflowID, append(workflowReadSteps(fixedID(workflowID), 1, workflowRevision, 1)[:1],
			repositoryStep{operation: "query", statement: sqlPolicyEpoch, err: failure}), migration.ErrStorage},
		{"group read fails", workflowID, append(workflowReadSteps(fixedID(workflowID), 1, workflowRevision, 1)[:2],
			repositoryStep{operation: "query", statement: sqlRequiredGroups, err: failure}), migration.ErrStorage},
		{"group rows fail", workflowID, append(workflowReadSteps(fixedID(workflowID), 1, workflowRevision, 1)[:2],
			repositoryStep{operation: "query", statement: sqlRequiredGroups, nextErr: failure}), migration.ErrStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := testWorkflowService(t, tc.steps...)
			_, err := service.GetWorkflow(context.Background(), repositoryActorID, repositoryDatabaseID, tc.id)
			if !errors.Is(err, tc.want) || errors.Is(err, failure) {
				t.Fatalf("error = %v; ingin %v tanpa detail penyimpanan", err, tc.want)
			}
		})
	}
}

func TestSubmitReviewStoresSnapshotForCurrentPolicy(t *testing.T) {
	plan := workflowPlan()
	planJSON, hash := workflowSnapshot(t, plan)
	var inserted string
	insertedID := func() string { return inserted }
	steps := []repositoryStep{
		{operation: "begin"},
		{operation: "exec", statement: sqlInitEpoch, check: repositoryArguments(repositoryDatabaseID)},
		{operation: "query", statement: sqlLockEpoch, rows: [][]driver.Value{{int64(3)}}, check: repositoryArguments(repositoryDatabaseID)},
		{operation: "query", statement: sqlCountGroups, rows: [][]driver.Value{{int64(2)}}, check: repositoryArguments(repositoryDatabaseID)},
		{operation: "exec", statement: sqlInsertWorkflow, check: func(t *testing.T, args []driver.NamedValue) {
			values := testsql.Values(args)
			inserted, _ = values[0].(string)
			want := []driver.Value{repositoryDatabaseID, repositoryMigrationID, workflowRevision, "apply", planJSON, hash, int64(3), repositoryActorID}
			if !uuid.Valid(inserted) || !reflect.DeepEqual(values[1:], want) {
				t.Fatalf("snapshot review = %#v; ingin ID UUID lalu %#v", values, want)
			}
		}},
		{operation: "commit"},
	}
	steps = append(steps, workflowReadSteps(insertedID, 3, workflowRevision, 3,
		approvalValues(workflowGroupA, "editors", "view_edit", repositoryActorID, ""),
		approvalValues(workflowGroupB, "readers", "view_only", workflowLeaderB, ""))...)
	service := testWorkflowService(t, steps...)

	workflow, err := service.SubmitReview(context.Background(), repositoryActorID, plan)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Status != migration.WorkflowPendingReview || !reflect.DeepEqual(approvalStates(t, workflow), []bool{false, false}) {
		t.Fatalf("review baru harus menunggu semua leader: %+v", workflow)
	}
}

func TestSubmitReviewRejectsInvalidPlansWithoutStorage(t *testing.T) {
	for name, mutate := range map[string]func(*string, *migration.ActionPlan){
		"actor":     func(actor *string, _ *migration.ActionPlan) { *actor = "bukan-uuid" },
		"database":  func(_ *string, plan *migration.ActionPlan) { plan.DatabaseID = "" },
		"migration": func(_ *string, plan *migration.ActionPlan) { plan.MigrationID = "x" },
		"revision":  func(_ *string, plan *migration.ActionPlan) { plan.Revision = 0 },
		"action":    func(_ *string, plan *migration.ActionPlan) { plan.Action = "drop" },
	} {
		t.Run(name, func(t *testing.T) {
			actor, plan := repositoryActorID, workflowPlan()
			mutate(&actor, &plan)
			service := testWorkflowService(t)
			if _, err := service.SubmitReview(context.Background(), actor, plan); !errors.Is(err, migration.ErrInvalidMigration) {
				t.Fatalf("error = %v; ingin ErrInvalidMigration", err)
			}
		})
	}
}

func TestSubmitReviewFailsClosed(t *testing.T) {
	failure := errors.New("insert failed")
	prefix := []repositoryStep{
		{operation: "begin"},
		{operation: "exec", statement: sqlInitEpoch},
		{operation: "query", statement: sqlLockEpoch, rows: [][]driver.Value{{int64(1)}}},
	}
	for _, tc := range []struct {
		name  string
		steps []repositoryStep
		want  error
	}{
		{"no group has access", append(append([]repositoryStep{}, prefix...),
			repositoryStep{operation: "query", statement: sqlCountGroups, rows: [][]driver.Value{{int64(0)}}},
			repositoryStep{operation: "rollback"}), access.ErrForbidden},
		{"snapshot not stored", append(append([]repositoryStep{}, prefix...),
			repositoryStep{operation: "query", statement: sqlCountGroups, rows: [][]driver.Value{{int64(1)}}},
			repositoryStep{operation: "exec", statement: sqlInsertWorkflow, err: failure},
			repositoryStep{operation: "rollback"}), migration.ErrStorage},
		{"policy epoch not initialized", []repositoryStep{
			{operation: "begin"},
			{operation: "exec", statement: sqlInitEpoch, err: failure},
			{operation: "rollback"}}, migration.ErrStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := testWorkflowService(t, tc.steps...)
			if _, err := service.SubmitReview(context.Background(), repositoryActorID, workflowPlan()); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v; ingin %v", err, tc.want)
			}
		})
	}
}

func TestGetLatestWorkflowFallsBackToDraft(t *testing.T) {
	latest := repositoryStep{operation: "query", statement: sqlLatestWorkflow, rows: [][]driver.Value{{workflowID}},
		check: repositoryArguments(repositoryDatabaseID, repositoryMigrationID)}
	current := func(revision int64) repositoryStep {
		return repositoryStep{operation: "query", statement: sqlCurrentRevision, rows: [][]driver.Value{{revision}},
			check: repositoryArguments(repositoryMigrationID, repositoryDatabaseID)}
	}
	pending := approvalValues(workflowGroupA, "editors", "view_edit", repositoryActorID, "")
	for _, tc := range []struct {
		name     string
		steps    []repositoryStep
		id       string
		status   migration.WorkflowStatus
		revision int64
	}{
		{"active review", append([]repositoryStep{latest}, workflowReadSteps(fixedID(workflowID), 1, workflowRevision, 1, pending)...),
			workflowID, migration.WorkflowPendingReview, workflowRevision},
		{"review of an older revision", append(append([]repositoryStep{latest}, workflowReadSteps(fixedID(workflowID), 1, 3, 1, pending)...), current(3)),
			"", migration.WorkflowDraft, 3},
		{"never reviewed", []repositoryStep{{operation: "query", statement: sqlLatestWorkflow}, current(1)},
			"", migration.WorkflowDraft, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := testWorkflowService(t, tc.steps...)
			workflow, err := service.GetLatestWorkflow(context.Background(), repositoryActorID, repositoryDatabaseID, repositoryMigrationID)
			if err != nil {
				t.Fatal(err)
			}
			if workflow.ID != tc.id || workflow.Status != tc.status || workflow.Revision != tc.revision || workflow.Approvals == nil {
				t.Fatalf("workflow terbaru = %+v; ingin ID %q, %s, revisi %d", workflow, tc.id, tc.status, tc.revision)
			}
		})
	}
}

func TestGetLatestWorkflowReportsMissingAndStorageErrors(t *testing.T) {
	failure := errors.New("platform unavailable")
	for _, tc := range []struct {
		name        string
		migrationID string
		steps       []repositoryStep
		want        error
	}{
		{"invalid ID", "bukan-uuid", nil, migration.ErrNotFound},
		{"unknown definition", repositoryMigrationID, []repositoryStep{
			{operation: "query", statement: sqlLatestWorkflow}, {operation: "query", statement: sqlCurrentRevision}}, migration.ErrNotFound},
		{"review lookup fails", repositoryMigrationID, []repositoryStep{
			{operation: "query", statement: sqlLatestWorkflow, err: failure}}, migration.ErrStorage},
		{"revision lookup fails", repositoryMigrationID, []repositoryStep{
			{operation: "query", statement: sqlLatestWorkflow}, {operation: "query", statement: sqlCurrentRevision, err: failure}}, migration.ErrStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := testWorkflowService(t, tc.steps...)
			if _, err := service.GetLatestWorkflow(context.Background(), repositoryActorID, repositoryDatabaseID, tc.migrationID); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v; ingin %v", err, tc.want)
			}
		})
	}
}

// approveLockSteps lock the workflow and read the access policy epoch.
func approveLockSteps(storedEpoch, revision, currentRevision, currentEpoch int64) []repositoryStep {
	return []repositoryStep{
		{operation: "begin"},
		{operation: "query", statement: sqlLockWorkflow, rows: [][]driver.Value{{storedEpoch, revision, currentRevision}},
			check: repositoryArguments(workflowID, repositoryDatabaseID)},
		{operation: "query", statement: sqlShareEpoch, rows: [][]driver.Value{{currentEpoch}}, check: repositoryArguments(repositoryDatabaseID)},
	}
}

func TestApproveRecordsEveryGroupTheActorLeads(t *testing.T) {
	steps := append(approveLockSteps(1, workflowRevision, workflowRevision, 1),
		repositoryStep{operation: "query", statement: sqlLeaderGroups, rows: [][]driver.Value{{workflowGroupA}, {workflowGroupB}},
			check: repositoryArguments(repositoryDatabaseID, repositoryActorID)},
		repositoryStep{operation: "exec", statement: sqlInsertApproval, check: repositoryArguments(workflowID, workflowGroupA, repositoryActorID)},
		repositoryStep{operation: "exec", statement: sqlInsertApproval, check: repositoryArguments(workflowID, workflowGroupB, repositoryActorID)},
		repositoryStep{operation: "exec", statement: sqlUpdateStatus, check: repositoryArguments(workflowID, repositoryDatabaseID)},
		repositoryStep{operation: "commit"},
	)
	steps = append(steps, workflowReadSteps(fixedID(workflowID), 1, workflowRevision, 1,
		approvalValues(workflowGroupA, "editors", "view_edit", repositoryActorID, repositoryActorID),
		approvalValues(workflowGroupB, "readers", "view_only", repositoryActorID, repositoryActorID))...)
	service := testWorkflowService(t, steps...)

	workflow, err := service.Approve(context.Background(), repositoryActorID, repositoryDatabaseID, workflowID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Status != migration.WorkflowApproved || !reflect.DeepEqual(approvalStates(t, workflow), []bool{true, true}) {
		t.Fatalf("approval leader tidak lengkap: %+v", workflow)
	}
}

func TestApproveRejectsNonLeadersAndStaleReviews(t *testing.T) {
	failure := errors.New("insert failed")
	for _, tc := range []struct {
		name       string
		workflowID string
		steps      []repositoryStep
		want       error
	}{
		{"invalid ID", "bukan-uuid", nil, migration.ErrNotFound},
		{"unknown workflow", workflowID, []repositoryStep{
			{operation: "begin"}, {operation: "query", statement: sqlLockWorkflow}, {operation: "rollback"}}, migration.ErrNotFound},
		{"definition revised", workflowID, append(approveLockSteps(1, workflowRevision, workflowRevision+1, 1)[:2],
			repositoryStep{operation: "rollback"}), migration.ErrApprovalRequired},
		{"access policy changed", workflowID, append(approveLockSteps(1, workflowRevision, workflowRevision, 2),
			repositoryStep{operation: "rollback"}), migration.ErrApprovalRequired},
		{"actor leads no group with access", workflowID, append(approveLockSteps(1, workflowRevision, workflowRevision, 1),
			repositoryStep{operation: "query", statement: sqlLeaderGroups},
			repositoryStep{operation: "rollback"}), access.ErrForbidden},
		{"approval not stored", workflowID, append(approveLockSteps(1, workflowRevision, workflowRevision, 1),
			repositoryStep{operation: "query", statement: sqlLeaderGroups, rows: [][]driver.Value{{workflowGroupA}}},
			repositoryStep{operation: "exec", statement: sqlInsertApproval, err: failure},
			repositoryStep{operation: "rollback"}), migration.ErrStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := testWorkflowService(t, tc.steps...)
			if _, err := service.Approve(context.Background(), repositoryActorID, repositoryDatabaseID, tc.workflowID); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v; ingin %v", err, tc.want)
			}
		})
	}
}

// validationSteps answer ValidateApproval for a snapshot stored with
// storedEpoch, given the current policy epoch, definition revision, and
// whether a required leader approval is missing.
func validationSteps(t *testing.T, stored migration.ActionPlan, currentEpoch, currentRevision int64, missing bool) []repositoryStep {
	planJSON, hash := workflowSnapshot(t, stored)
	return []repositoryStep{
		{operation: "query", statement: sqlApprovalSnapshot, check: repositoryArguments(workflowID), rows: [][]driver.Value{{
			planJSON, hash, string(stored.Action), stored.DatabaseID, stored.MigrationID, stored.Revision, int64(1),
		}}},
		{operation: "query", statement: sqlPolicyEpoch, rows: [][]driver.Value{{currentEpoch}}, check: repositoryArguments(repositoryDatabaseID)},
		{operation: "query", statement: sqlCurrentRevision, rows: [][]driver.Value{{currentRevision}},
			check: repositoryArguments(repositoryMigrationID, repositoryDatabaseID)},
		{operation: "query", statement: sqlMissingApproval, rows: [][]driver.Value{{missing}}, check: repositoryArguments(workflowID, repositoryDatabaseID)},
	}
}

func workflowRequest(plan migration.ActionPlan) migration.ExecutionRequest {
	return migration.ExecutionRequest{ExecutionID: executionID(20), ApprovalID: workflowID, ActorID: repositoryActorID, Plan: plan}
}

func TestValidateApprovalAcceptsCompleteCurrentApproval(t *testing.T) {
	service := testWorkflowService(t, validationSteps(t, workflowPlan(), 1, workflowRevision, false)...)
	if err := service.ValidateApproval(context.Background(), workflowRequest(workflowPlan())); err != nil {
		t.Fatalf("approval lengkap ditolak: %v", err)
	}
}

func TestValidateApprovalRejectsOutdatedOrIncompleteApproval(t *testing.T) {
	approved := workflowPlan()
	advanced := approved
	advanced.BaseExecutionID = executionID(8)
	rewritten := approved
	rewritten.SQL = "DELETE FROM customers"
	for _, tc := range []struct {
		name    string
		request migration.ExecutionRequest
		steps   []repositoryStep
	}{
		{"invalid approval ID", migration.ExecutionRequest{ApprovalID: "bukan-uuid", Plan: approved}, nil},
		{"unknown approval", workflowRequest(approved), []repositoryStep{{operation: "query", statement: sqlApprovalSnapshot}}},
		{"history advanced after review", workflowRequest(advanced), validationSteps(t, approved, 1, workflowRevision, false)[:1]},
		{"SQL differs from review", workflowRequest(rewritten), validationSteps(t, approved, 1, workflowRevision, false)[:1]},
		{"corrupt snapshot", workflowRequest(approved), []repositoryStep{{operation: "query", statement: sqlApprovalSnapshot, rows: [][]driver.Value{{
			"{", workflowHash, "apply", repositoryDatabaseID, repositoryMigrationID, workflowRevision, int64(1),
		}}}}},
		{"access policy changed", workflowRequest(approved), validationSteps(t, approved, 2, workflowRevision, false)[:2]},
		{"definition revised", workflowRequest(approved), validationSteps(t, approved, 1, workflowRevision+1, false)[:3]},
		{"definition removed", workflowRequest(approved), append(validationSteps(t, approved, 1, workflowRevision, false)[:2],
			repositoryStep{operation: "query", statement: sqlCurrentRevision})},
		{"leader approval missing", workflowRequest(approved), validationSteps(t, approved, 1, workflowRevision, true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := testWorkflowService(t, tc.steps...)
			if err := service.ValidateApproval(context.Background(), tc.request); !errors.Is(err, migration.ErrApprovalRequired) {
				t.Fatalf("error = %v; ingin ErrApprovalRequired", err)
			}
		})
	}
}

func TestValidateApprovalReportsStorageFailures(t *testing.T) {
	failure := errors.New("platform unavailable")
	full := validationSteps(t, workflowPlan(), 1, workflowRevision, false)
	for _, tc := range []struct {
		name  string
		steps []repositoryStep
	}{
		{"snapshot read fails", []repositoryStep{{operation: "query", statement: sqlApprovalSnapshot, err: failure}}},
		{"policy read fails", append(full[:1:1], repositoryStep{operation: "query", statement: sqlPolicyEpoch, err: failure})},
		{"revision read fails", append(full[:2:2], repositoryStep{operation: "query", statement: sqlCurrentRevision, err: failure})},
		{"leader check fails", append(full[:3:3], repositoryStep{operation: "query", statement: sqlMissingApproval, err: failure})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := testWorkflowService(t, tc.steps...)
			err := service.ValidateApproval(context.Background(), workflowRequest(workflowPlan()))
			if !errors.Is(err, migration.ErrStorage) || errors.Is(err, migration.ErrApprovalRequired) {
				t.Fatalf("error = %v; ingin ErrStorage, bukan penolakan approval", err)
			}
		})
	}
}
