//go:build integration

package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/app"
	databasehttp "github.com/wafhr/IF3110_K01-13/internal/database/http"
	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

func TestMigrationWorkflowRequiresEveryCurrentGroupLeaderIntegration(t *testing.T) {
	f := newMigrationFixture(t)
	ctx := context.Background()
	var viewGroupID, viewLeaderID string
	if err := f.platform.QueryRowContext(ctx, `SELECT g.id::text, m.user_id::text FROM access_groups g
		JOIN access_group_database_permissions p ON p.group_id = g.id
		JOIN access_group_members m ON m.group_id = g.id
		WHERE p.database_id = $1 AND p.permission = 'view_only' LIMIT 1`, f.databaseID).Scan(&viewGroupID, &viewLeaderID); err != nil {
		t.Fatal(err)
	}
	accessService := access.NewService(f.platform)
	f.platformAdmin.do(t, "PUT", "/admin/groups/"+f.editorGroupID+"/leader", map[string]string{"user_id": f.actorID}, 204)
	f.platformAdmin.do(t, "PUT", "/admin/groups/"+viewGroupID+"/leader", map[string]string{"user_id": f.actorID}, 400)
	f.platformAdmin.do(t, "PUT", "/admin/groups/"+viewGroupID+"/leader", map[string]string{"user_id": viewLeaderID}, 204)
	workflowService, err := migration.NewWorkflowService(f.platform)
	if err != nil {
		t.Fatal(err)
	}
	queueService, err := job.NewService(f.jobs)
	if err != nil {
		t.Fatal(err)
	}
	auth := access.NewHTTPHandler(accessService, false)
	f.server.Close()
	f.migrations = migration.NewServiceWithExecution(f.openTarget, f.definitions, migration.ExecutionDependencies{
		Authorizer: accessService, Approvals: workflowService, Queue: queueService,
	})
	f.server = httptest.NewServer(telemetry.HTTPHandler(app.RoutesWith(app.Modules{
		Access: auth, Database: databasehttp.NewHandler(f.registry, f.pools, auth),
		Viewer:    databasehttp.NewViewerHandler(f.registry, f.inspector, auth),
		Migration: migration.NewHTTPHandler(f.migrations, auth), Job: job.NewHTTPHandler(queueService, auth),
	})))
	f.editor.baseURL, f.reader.baseURL = f.server.URL, f.server.URL
	definition := f.createMigration(t, "workflow test", "SELECT 1", "SELECT 2")
	plan, err := f.migrations.PrepareAction(ctx, f.databaseID, definition.ID, migration.Apply)
	if err != nil {
		t.Fatal(err)
	}
	var review struct {
		Workflow migration.Workflow `json:"workflow"`
	}
	path := f.migrationPath() + "/definitions/" + definition.ID
	decode(t, f.editor.do(t, "POST", path+"/review", map[string]string{"action": "apply"}, 201), &review)
	workflow := review.Workflow
	if workflow.Status != migration.WorkflowPendingReview || len(workflow.Approvals) != 2 {
		t.Fatalf("review snapshot did not include both edit and view-only groups: %+v", workflow)
	}
	var pending struct {
		Workflow migration.Workflow `json:"workflow"`
	}
	decode(t, f.reader.do(t, "GET", path+"/workflow", nil, 200), &pending)
	if pending.Workflow.Status != migration.WorkflowPendingReview {
		t.Fatalf("latest workflow = %s, want pending_review", pending.Workflow.Status)
	}
	approvePath := f.migrationPath() + "/workflows/" + workflow.ID + "/approvals"
	var partial struct { Workflow migration.Workflow `json:"workflow"` }
	decode(t, f.reader.do(t, "POST", approvePath, map[string]string{}, 200), &partial)
	if partial.Workflow.Status != migration.WorkflowPendingReview || len(partial.Workflow.Approvals) != 2 {
		t.Fatalf("one group approval prematurely completed the workflow: %+v", partial.Workflow)
	}
	for _, item := range partial.Workflow.Approvals {
		if item.GroupID == viewGroupID && !item.Approved { t.Fatal("view-only group's leader approval was not recorded") }
		if item.GroupID == f.editorGroupID && item.Approved { t.Fatal("editor group's approval was recorded before its leader approved") }
	}
	var approved struct {
		Workflow migration.Workflow `json:"workflow"`
	}
	decode(t, f.editor.do(t, "POST", approvePath, map[string]string{}, 200), &approved)
	if approved.Workflow.Status != migration.WorkflowApproved {
		t.Fatalf("all current group leaders approved but state is %s", approved.Workflow.Status)
	}
	var queued map[string]any
	decode(t, f.editor.do(t, "POST", path+"/actions", map[string]string{"action": "apply", "approval_id": workflow.ID}, 202), &queued)
	request := migration.ExecutionRequest{ExecutionID: "8f000000-0000-4000-8000-000000000003", ApprovalID: workflow.ID, ActorID: f.actorID, Plan: plan}
	if err := workflowService.ValidateApproval(ctx, request); err != nil {
		t.Fatalf("complete approvals rejected: %v", err)
	}
	if err := accessService.DeleteDatabasePermission(ctx, viewGroupID, f.databaseID); err != nil {
		t.Fatal(err)
	}
	if err := workflowService.ValidateApproval(ctx, request); !errors.Is(err, migration.ErrApprovalRequired) {
		t.Fatalf("grant change did not invalidate the approval snapshot: %v", err)
	}
	currentPlan, err := f.migrations.PrepareAction(ctx, f.databaseID, definition.ID, migration.Apply)
	if err != nil {
		t.Fatal(err)
	}
	newReview, err := workflowService.SubmitReview(ctx, f.actorID, currentPlan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workflowService.Approve(ctx, f.actorID, f.databaseID, newReview.ID); err != nil {
		t.Fatal(err)
	}
	currentRequest := migration.ExecutionRequest{ExecutionID: "8f000000-0000-4000-8000-000000000004", ApprovalID: newReview.ID, ActorID: f.actorID, Plan: currentPlan}
	if err := workflowService.ValidateApproval(ctx, currentRequest); err != nil {
		t.Fatalf("fresh approval rejected: %v", err)
	}
	if _, _, err := f.definitions.Revise(ctx, f.databaseID, definition.ID, f.actorID, "workflow revised", 1,
		migration.Revision{SQLUp: "SELECT 3", SQLDown: "SELECT 2"}); err != nil {
		t.Fatal(err)
	}
	if err := workflowService.ValidateApproval(ctx, currentRequest); !errors.Is(err, migration.ErrApprovalRequired) {
		t.Fatalf("definition revision did not invalidate approval: %v", err)
	}
	latest, err := workflowService.GetLatestWorkflow(ctx, f.actorID, f.databaseID, definition.ID)
	if err != nil || latest.Status != migration.WorkflowDraft {
		t.Fatalf("revised definition workflow = %+v, %v; want draft", latest, err)
	}
}
