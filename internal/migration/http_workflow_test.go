package migration_test

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/testsql"
	"github.com/wafhr/IF3110_K01-13/internal/uuid"
)

const (
	reviewPath   = migrationAPIBase + "/definitions/" + repositoryMigrationID + "/review"
	latestPath   = migrationAPIBase + "/definitions/" + repositoryMigrationID + "/workflow"
	workflowPath = migrationAPIBase + "/workflows/" + workflowID
	approvePath  = workflowPath + "/approvals"
)

// workflowAPIHandler serves the migration API with a WorkflowService backed by
// workflowSteps as the approval provider. Reviews read the target history
// from targetSteps.
func workflowAPIHandler(t *testing.T, authSteps, targetSteps, workflowSteps []repositoryStep) http.Handler {
	t.Helper()
	store, _, dependencies := executionFixture(t)
	dependencies.Approvals = testWorkflowService(t, workflowSteps...)
	target := testMigrationDatabase(t, targetSteps...)
	service := migration.NewServiceWithExecution(executionOpener(target), store, dependencies)
	return migrationAPIHandler(t, service, authSteps...)
}

func serveWorkflow(t *testing.T, handler http.Handler, method, path, body string) (*httptest.ResponseRecorder, migration.Workflow) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, migrationRequest(method, path, body))
	var payload struct {
		Workflow migration.Workflow `json:"workflow"`
	}
	if response.Code < 300 {
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatalf("respons workflow bukan JSON: %v %s", err, response.Body.String())
		}
	}
	return response, payload.Workflow
}

// reviewedPlan is the plan the review endpoint prepares for an apply of the
// executionFixture definition on an empty history.
func reviewedPlan(t *testing.T) migration.ActionPlan {
	t.Helper()
	_, request, _ := executionFixture(t)
	return request.Plan
}

func TestReviewAPIStoresPreparedPlanForEditors(t *testing.T) {
	plan := reviewedPlan(t)
	planJSON, hash := workflowSnapshot(t, plan)
	var inserted string
	workflowSteps := []repositoryStep{
		{operation: "begin"},
		{operation: "exec", statement: sqlInitEpoch},
		{operation: "query", statement: sqlLockEpoch, rows: [][]driver.Value{{int64(1)}}},
		{operation: "query", statement: sqlCountGroups, rows: [][]driver.Value{{int64(1)}}},
		{operation: "exec", statement: sqlInsertWorkflow, check: func(t *testing.T, args []driver.NamedValue) {
			values := testsql.Values(args)
			inserted, _ = values[0].(string)
			want := []driver.Value{repositoryDatabaseID, repositoryMigrationID, plan.Revision, "apply", planJSON, hash, int64(1), repositoryActorID}
			if !uuid.Valid(inserted) || !reflect.DeepEqual(values[1:], want) {
				t.Fatalf("review menyimpan snapshot %#v; ingin %#v", values[1:], want)
			}
		}},
		{operation: "commit"},
	}
	workflowSteps = append(workflowSteps, workflowReadSteps(func() string { return inserted }, 1, workflowRevision, 1,
		approvalValues(workflowGroupA, "editors", "view_edit", repositoryActorID, ""))...)
	handler := workflowAPIHandler(t, migrationAuthorizationSteps(access.ViewEdit, false), actionHistorySteps(nil), workflowSteps)

	response, workflow := serveWorkflow(t, handler, http.MethodPost, reviewPath, `{"action":"apply"}`)
	if response.Code != http.StatusCreated || workflow.Status != migration.WorkflowPendingReview || len(workflow.Approvals) != 1 {
		t.Fatalf("review API = %d %s", response.Code, response.Body.String())
	}
}

func TestReviewAPIRejectsInvalidAndUnauthorizedRequests(t *testing.T) {
	editor := migrationAuthorizationSteps(access.ViewEdit, false)
	for _, tc := range []struct {
		name          string
		authSteps     []repositoryStep
		targetSteps   []repositoryStep
		workflowSteps []repositoryStep
		body          string
		dropCSRF      bool
		want          int
	}{
		{name: "view-only member", authSteps: migrationAuthorizationSteps(access.ViewOnly, false), body: `{"action":"apply"}`, want: http.StatusForbidden},
		{name: "missing CSRF token", authSteps: editor[:len(editor)-1], body: `{"action":"apply"}`, dropCSRF: true, want: http.StatusForbidden},
		{name: "unknown action", authSteps: editor, body: `{"action":"drop"}`, want: http.StatusBadRequest},
		{name: "unknown field", authSteps: editor, body: `{"action":"apply","approved":true}`, want: http.StatusBadRequest},
		{name: "no group has access", authSteps: editor, targetSteps: actionHistorySteps(nil), body: `{"action":"apply"}`, want: http.StatusForbidden,
			workflowSteps: []repositoryStep{
				{operation: "begin"},
				{operation: "exec", statement: sqlInitEpoch},
				{operation: "query", statement: sqlLockEpoch, rows: [][]driver.Value{{int64(1)}}},
				{operation: "query", statement: sqlCountGroups, rows: [][]driver.Value{{int64(0)}}},
				{operation: "rollback"},
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := workflowAPIHandler(t, tc.authSteps, tc.targetSteps, tc.workflowSteps)
			request := migrationRequest(http.MethodPost, reviewPath, tc.body)
			if tc.dropCSRF {
				request.Header.Del("X-CSRF-Token")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("review API = %d %s; ingin %d", response.Code, response.Body.String(), tc.want)
			}
		})
	}
}

func TestWorkflowReadAPIsServeViewOnlyMembers(t *testing.T) {
	approved := workflowReadSteps(fixedID(workflowID), 1, workflowRevision, 1,
		approvalValues(workflowGroupA, "editors", "view_edit", repositoryActorID, repositoryActorID))
	for _, tc := range []struct {
		name   string
		path   string
		steps  []repositoryStep
		status migration.WorkflowStatus
	}{
		{"workflow by ID", workflowPath, approved, migration.WorkflowApproved},
		{"latest workflow of a definition", latestPath, append([]repositoryStep{{operation: "query", statement: sqlLatestWorkflow,
			rows: [][]driver.Value{{workflowID}}}}, approved...), migration.WorkflowApproved},
		{"definition never reviewed", latestPath, []repositoryStep{{operation: "query", statement: sqlLatestWorkflow},
			{operation: "query", statement: sqlCurrentRevision, rows: [][]driver.Value{{int64(1)}}}}, migration.WorkflowDraft},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := workflowAPIHandler(t, migrationAuthorizationSteps(access.ViewOnly, false), nil, tc.steps)
			response, workflow := serveWorkflow(t, handler, http.MethodGet, tc.path, "")
			if response.Code != http.StatusOK || workflow.Status != tc.status {
				t.Fatalf("API workflow = %d %s; ingin status %s", response.Code, response.Body.String(), tc.status)
			}
		})
	}
}

func TestWorkflowReadAPIReportsUnknownWorkflow(t *testing.T) {
	handler := workflowAPIHandler(t, migrationAuthorizationSteps(access.ViewOnly, false), nil,
		[]repositoryStep{{operation: "query", statement: sqlWorkflowRow}})
	response, _ := serveWorkflow(t, handler, http.MethodGet, workflowPath, "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("workflow tidak dikenal = %d %s", response.Code, response.Body.String())
	}
}

func TestApprovalAPIAcceptsViewOnlyLeader(t *testing.T) {
	steps := append(approveLockSteps(1, workflowRevision, workflowRevision, 1),
		repositoryStep{operation: "query", statement: sqlLeaderGroups, rows: [][]driver.Value{{workflowGroupB}},
			check: repositoryArguments(repositoryDatabaseID, repositoryActorID)},
		repositoryStep{operation: "exec", statement: sqlInsertApproval, check: repositoryArguments(workflowID, workflowGroupB, repositoryActorID)},
		repositoryStep{operation: "exec", statement: sqlUpdateStatus},
		repositoryStep{operation: "commit"},
	)
	steps = append(steps, workflowReadSteps(fixedID(workflowID), 1, workflowRevision, 1,
		approvalValues(workflowGroupA, "editors", "view_edit", workflowLeaderB, workflowLeaderB),
		approvalValues(workflowGroupB, "readers", "view_only", repositoryActorID, repositoryActorID))...)
	handler := workflowAPIHandler(t, migrationAuthorizationSteps(access.ViewOnly, false), nil, steps)

	response, workflow := serveWorkflow(t, handler, http.MethodPost, approvePath, `{}`)
	if response.Code != http.StatusOK || workflow.Status != migration.WorkflowApproved {
		t.Fatalf("approval leader View Only = %d %s", response.Code, response.Body.String())
	}
}

func TestApprovalAPIRejectsNonLeadersAndStaleReviews(t *testing.T) {
	viewer := migrationAuthorizationSteps(access.ViewOnly, false)
	for _, tc := range []struct {
		name      string
		authSteps []repositoryStep
		steps     []repositoryStep
		dropCSRF  bool
		want      int
	}{
		{name: "member who leads no group", authSteps: viewer, want: http.StatusForbidden, steps: append(approveLockSteps(1, workflowRevision, workflowRevision, 1),
			repositoryStep{operation: "query", statement: sqlLeaderGroups}, repositoryStep{operation: "rollback"})},
		{name: "definition revised after review", authSteps: viewer, want: http.StatusConflict, steps: append(
			approveLockSteps(1, workflowRevision, workflowRevision+1, 1)[:2], repositoryStep{operation: "rollback"})},
		{name: "access policy changed after review", authSteps: viewer, want: http.StatusConflict, steps: append(
			approveLockSteps(1, workflowRevision, workflowRevision, 2), repositoryStep{operation: "rollback"})},
		{name: "unknown workflow", authSteps: viewer, want: http.StatusNotFound, steps: []repositoryStep{
			{operation: "begin"}, {operation: "query", statement: sqlLockWorkflow}, {operation: "rollback"}}},
		{name: "user without database access", authSteps: migrationAuthorizationSteps("", false), want: http.StatusForbidden},
		{name: "missing CSRF token", authSteps: viewer[:len(viewer)-1], dropCSRF: true, want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := workflowAPIHandler(t, tc.authSteps, nil, tc.steps)
			request := migrationRequest(http.MethodPost, approvePath, `{}`)
			if tc.dropCSRF {
				request.Header.Del("X-CSRF-Token")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("approval API = %d %s; ingin %d", response.Code, response.Body.String(), tc.want)
			}
		})
	}
}

func TestWorkflowAPIsRequireWorkflowProvider(t *testing.T) {
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, reviewPath, `{"action":"apply"}`},
		{http.MethodGet, latestPath, ""},
		{http.MethodGet, workflowPath, ""},
		{http.MethodPost, approvePath, `{}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			store, _, dependencies := executionFixture(t)
			dependencies.Approvals = approvalCheckerFunc(func(context.Context, migration.ExecutionRequest) error { return nil })
			service := migration.NewServiceWithExecution(executionOpener(testMigrationDatabase(t)), store, dependencies)
			handler := migrationAPIHandler(t, service, migrationAuthorizationSteps(access.ViewEdit, false)...)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, migrationRequest(tc.method, tc.path, tc.body))
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("workflow tanpa provider = %d %s", response.Code, response.Body.String())
			}
		})
	}
}
