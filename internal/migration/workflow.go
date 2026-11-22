package migration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/uuid"
)

type WorkflowStatus string

const (
	WorkflowDraft         WorkflowStatus = "draft"
	WorkflowPendingReview WorkflowStatus = "pending_review"
	WorkflowApproved      WorkflowStatus = "approved"
)

type RequiredApproval struct {
	GroupID    string     `json:"group_id"`
	GroupName  string     `json:"group_name"`
	Permission string     `json:"permission"`
	LeaderID   string     `json:"leader_id,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
	Approved   bool       `json:"approved"`
}

type Workflow struct {
	ID          string             `json:"id,omitempty"`
	DatabaseID  string             `json:"database_id"`
	MigrationID string             `json:"migration_id"`
	Revision    int64              `json:"revision"`
	Action      Action             `json:"action"`
	Status      WorkflowStatus     `json:"status"`
	PlanHash    string             `json:"plan_hash,omitempty"`
	SubmittedBy string             `json:"submitted_by,omitempty"`
	SubmittedAt *time.Time         `json:"submitted_at,omitempty"`
	Approvals   []RequiredApproval `json:"approvals"`
}

// WorkflowService persists approval requests and is also the production
// ApprovalChecker used by both API submission and worker revalidation.
type WorkflowService struct{ db *sql.DB }

func NewWorkflowService(db *sql.DB) (*WorkflowService, error) {
	if db == nil {
		return nil, errors.New("workflow service requires a platform database")
	}
	return &WorkflowService{db: db}, nil
}

func planSnapshot(plan ActionPlan) ([]byte, string, error) {
	data, err := json.Marshal(plan)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256(data)
	return data, hex.EncodeToString(hash[:]), nil
}

func (s *WorkflowService) SubmitReview(ctx context.Context, actorID string, plan ActionPlan) (Workflow, error) {
	if !uuid.Valid(actorID) || !uuid.Valid(plan.DatabaseID) || !uuid.Valid(plan.MigrationID) || plan.Revision < 1 ||
		(plan.Action != Apply && plan.Action != Rollback && plan.Action != Forward) {
		return Workflow{}, ErrInvalidMigration
	}
	data, hash, err := planSnapshot(plan)
	if err != nil {
		return Workflow{}, fmt.Errorf("encode approval snapshot: %w", err)
	}
	id, err := uuid.New()
	if err != nil {
		return Workflow{}, fmt.Errorf("create workflow ID: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workflow{}, fmt.Errorf("begin review submission: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO access_database_approval_epochs(database_id, epoch) VALUES ($1, 1) ON CONFLICT DO NOTHING`, plan.DatabaseID); err != nil {
		return Workflow{}, workflowStorage("initialize approval policy epoch", err)
	}
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT epoch FROM access_database_approval_epochs WHERE database_id = $1 FOR UPDATE`, plan.DatabaseID).Scan(&epoch); err != nil {
		return Workflow{}, fmt.Errorf("read approval policy epoch: %w", err)
	}
	var groups int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM access_group_database_permissions WHERE database_id = $1`, plan.DatabaseID).Scan(&groups); err != nil {
		return Workflow{}, fmt.Errorf("count database approval groups: %w", err)
	}
	if groups == 0 {
		return Workflow{}, access.ErrForbidden
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO migration_workflows
		(id, database_id, migration_id, revision, action, plan, plan_hash, policy_epoch, status, submitted_by)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, 'pending_review', $9)`,
		id, plan.DatabaseID, plan.MigrationID, plan.Revision, string(plan.Action), string(data), hash, epoch, actorID)
	if err != nil {
		return Workflow{}, workflowStorage("save review request", err)
	}
	if err := tx.Commit(); err != nil {
		return Workflow{}, workflowStorage("commit review request", err)
	}
	return s.GetWorkflow(ctx, actorID, plan.DatabaseID, id)
}

func (s *WorkflowService) GetWorkflow(ctx context.Context, actorID, databaseID, id string) (Workflow, error) {
	if !uuid.Valid(actorID) || !uuid.Valid(databaseID) || !uuid.Valid(id) {
		return Workflow{}, ErrNotFound
	}
	var result Workflow
	var submitted time.Time
	var storedHash string
	var epoch int64
	var currentRevision int64
	err := s.db.QueryRowContext(ctx, `SELECT id::text, database_id::text, migration_id::text, revision, action, status,
		plan_hash, submitted_by::text, submitted_at, policy_epoch,
		(SELECT current_revision FROM migration_definitions d WHERE d.id = migration_workflows.migration_id)
		FROM migration_workflows WHERE id = $1 AND database_id = $2`, id, databaseID).
		Scan(&result.ID, &result.DatabaseID, &result.MigrationID, &result.Revision, &result.Action, &result.Status,
			&storedHash, &result.SubmittedBy, &submitted, &epoch, &currentRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, ErrNotFound
	}
	if err != nil {
		return Workflow{}, workflowStorage("read workflow", err)
	}
	result.SubmittedAt, result.PlanHash = &submitted, storedHash
	currentEpoch, err := s.policyEpoch(ctx, databaseID)
	if err != nil {
		return Workflow{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT g.id::text, g.name, p.permission, COALESCE(g.leader_user_id::text, ''),
		a.leader_user_id::text, a.approved_at
		FROM access_group_database_permissions p JOIN access_groups g ON g.id = p.group_id
		LEFT JOIN migration_approvals a ON a.workflow_id = $1 AND a.group_id = g.id
		WHERE p.database_id = $2 ORDER BY g.name, g.id`, id, databaseID)
	if err != nil {
		return Workflow{}, workflowStorage("read required approvals", err)
	}
	result.Approvals = make([]RequiredApproval, 0)
	validSet := currentEpoch == epoch && currentRevision == result.Revision
	allApproved := validSet
	for rows.Next() {
		var item RequiredApproval
		var currentLeader, approvedLeader sql.NullString
		var approvedAt sql.NullTime
		if err := rows.Scan(&item.GroupID, &item.GroupName, &item.Permission, &currentLeader, &approvedLeader, &approvedAt); err != nil {
			rows.Close()
			return Workflow{}, workflowStorage("scan required approval", err)
		}
		if currentLeader.Valid {
			item.LeaderID = currentLeader.String
		}
		item.Approved = validSet && currentLeader.Valid && approvedLeader.Valid && currentLeader.String == approvedLeader.String && approvedAt.Valid
		if item.Approved {
			t := approvedAt.Time
			item.ApprovedAt = &t
		}
		if !item.Approved {
			allApproved = false
		}
		result.Approvals = append(result.Approvals, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Workflow{}, workflowStorage("read required approvals", err)
	}
	if err := rows.Close(); err != nil {
		return Workflow{}, workflowStorage("close required approvals", err)
	}
	if len(result.Approvals) == 0 {
		allApproved = false
	}
	if currentRevision != result.Revision || currentEpoch != epoch {
		result.Status = WorkflowDraft
	} else if allApproved {
		result.Status = WorkflowApproved
	} else {
		result.Status = WorkflowPendingReview
	}
	return result, nil
}

// GetLatestWorkflow reports the newest review request, or the current
// definition as a draft when no review exists or the definition has advanced.
func (s *WorkflowService) GetLatestWorkflow(ctx context.Context, actorID, databaseID, migrationID string) (Workflow, error) {
	if !uuid.Valid(actorID) || !uuid.Valid(databaseID) || !uuid.Valid(migrationID) {
		return Workflow{}, ErrNotFound
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id::text FROM migration_workflows
		WHERE database_id = $1 AND migration_id = $2 ORDER BY submitted_at DESC, id DESC LIMIT 1`, databaseID, migrationID).Scan(&id)
	if err == nil {
		result, getErr := s.GetWorkflow(ctx, actorID, databaseID, id)
		if getErr != nil {
			return Workflow{}, getErr
		}
		if result.Status != WorkflowDraft {
			return result, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, workflowStorage("find latest workflow", err)
	}
	var revision int64
	if err := s.db.QueryRowContext(ctx, `SELECT current_revision FROM migration_definitions WHERE id = $1 AND database_id = $2`, migrationID, databaseID).Scan(&revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Workflow{}, ErrNotFound
		}
		return Workflow{}, workflowStorage("read draft migration revision", err)
	}
	return Workflow{DatabaseID: databaseID, MigrationID: migrationID, Revision: revision, Status: WorkflowDraft, Approvals: []RequiredApproval{}}, nil
}

func (s *WorkflowService) Approve(ctx context.Context, actorID, databaseID, workflowID string) (Workflow, error) {
	if !uuid.Valid(actorID) || !uuid.Valid(databaseID) || !uuid.Valid(workflowID) {
		return Workflow{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workflow{}, workflowStorage("begin approval", err)
	}
	defer tx.Rollback()
	var epoch int64
	var storedEpoch int64
	var workflowRevision, currentRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT w.policy_epoch, w.revision, d.current_revision
		FROM migration_workflows w JOIN migration_definitions d ON d.id = w.migration_id
		WHERE w.id = $1 AND w.database_id = $2 FOR UPDATE OF w`, workflowID, databaseID).Scan(&storedEpoch, &workflowRevision, &currentRevision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Workflow{}, ErrNotFound
		}
		return Workflow{}, workflowStorage("lock workflow for approval", err)
	}
	if workflowRevision != currentRevision {
		return Workflow{}, ErrApprovalRequired
	}
	if err := tx.QueryRowContext(ctx, `SELECT epoch FROM access_database_approval_epochs WHERE database_id = $1 FOR SHARE`, databaseID).Scan(&epoch); err != nil {
		return Workflow{}, workflowStorage("read approval policy epoch", err)
	}
	if epoch != storedEpoch {
		return Workflow{}, ErrApprovalRequired
	}
	rows, err := tx.QueryContext(ctx, `SELECT g.id::text FROM access_group_database_permissions p JOIN access_groups g ON g.id = p.group_id
		JOIN access_users u ON u.id = g.leader_user_id AND u.is_active
		JOIN access_group_members m ON m.group_id = g.id AND m.user_id = u.id
		WHERE p.database_id = $1 AND u.id = $2 FOR UPDATE OF g`, databaseID, actorID)
	if err != nil {
		return Workflow{}, workflowStorage("find leader approvals", err)
	}
	var groupIDs []string
	for rows.Next() {
		var groupID string
		if err := rows.Scan(&groupID); err != nil {
			rows.Close()
			return Workflow{}, workflowStorage("scan leader group", err)
		}
		groupIDs = append(groupIDs, groupID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Workflow{}, workflowStorage("read leader groups", err)
	}
	if err := rows.Close(); err != nil {
		return Workflow{}, workflowStorage("close leader groups", err)
	}
	if len(groupIDs) == 0 {
		return Workflow{}, access.ErrForbidden
	}
	for _, groupID := range groupIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO migration_approvals(workflow_id, group_id, leader_user_id)
			VALUES ($1, $2, $3) ON CONFLICT(workflow_id, group_id) DO UPDATE
			SET leader_user_id = EXCLUDED.leader_user_id, approved_at = CURRENT_TIMESTAMP`, workflowID, groupID, actorID); err != nil {
			return Workflow{}, workflowStorage("save leader approval", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migration_workflows SET status = CASE WHEN NOT EXISTS (
		SELECT 1 FROM access_group_database_permissions p JOIN access_groups g ON g.id = p.group_id
		LEFT JOIN migration_approvals a ON a.workflow_id = $1 AND a.group_id = g.id AND a.leader_user_id = g.leader_user_id
		LEFT JOIN access_users u ON u.id = g.leader_user_id AND u.is_active
		WHERE p.database_id = $2 AND (g.leader_user_id IS NULL OR u.id IS NULL OR a.group_id IS NULL)
	) THEN 'approved' ELSE 'pending_review' END, updated_at = CURRENT_TIMESTAMP WHERE id = $1`, workflowID, databaseID); err != nil {
		return Workflow{}, workflowStorage("update workflow status", err)
	}
	if err := tx.Commit(); err != nil {
		return Workflow{}, workflowStorage("commit leader approval", err)
	}
	return s.GetWorkflow(ctx, actorID, databaseID, workflowID)
}

func (s *WorkflowService) ValidateApproval(ctx context.Context, request ExecutionRequest) error {
	if !uuid.Valid(request.ApprovalID) {
		return ErrApprovalRequired
	}
	var data []byte
	var hash, action, dbID, migrationID string
	var revision, epoch int64
	var storedPlan ActionPlan
	err := s.db.QueryRowContext(ctx, `SELECT plan, plan_hash, action, database_id::text, migration_id::text, revision, policy_epoch
		FROM migration_workflows WHERE id = $1`, request.ApprovalID).Scan(&data, &hash, &action, &dbID, &migrationID, &revision, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrApprovalRequired
	}
	if err != nil {
		return workflowStorage("load approval snapshot", err)
	}
	if err := json.Unmarshal(data, &storedPlan); err != nil {
		return ErrApprovalRequired
	}
	_, currentHash, err := planSnapshot(request.Plan)
	if err != nil {
		return workflowStorage("hash requested approval snapshot", err)
	}
	if hash != currentHash || storedPlan != request.Plan || dbID != request.Plan.DatabaseID || migrationID != request.Plan.MigrationID || revision != request.Plan.Revision || action != string(request.Plan.Action) {
		return ErrApprovalRequired
	}
	currentEpoch, err := s.policyEpoch(ctx, dbID)
	if err != nil {
		return err
	}
	if currentEpoch != epoch {
		return ErrApprovalRequired
	}
	var currentRevision int64
	if err := s.db.QueryRowContext(ctx, `SELECT current_revision FROM migration_definitions WHERE id = $1 AND database_id = $2`, migrationID, dbID).Scan(&currentRevision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrApprovalRequired
		}
		return workflowStorage("validate current migration revision", err)
	}
	if currentRevision != revision {
		return ErrApprovalRequired
	}
	var missing bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM access_group_database_permissions p JOIN access_groups g ON g.id = p.group_id
		LEFT JOIN access_users u ON u.id = g.leader_user_id AND u.is_active
		LEFT JOIN access_group_members m ON m.group_id = g.id AND m.user_id = g.leader_user_id
		LEFT JOIN migration_approvals a ON a.workflow_id = $1 AND a.group_id = g.id AND a.leader_user_id = g.leader_user_id
		WHERE p.database_id = $2 AND (g.leader_user_id IS NULL OR u.id IS NULL OR m.user_id IS NULL OR a.group_id IS NULL)
	)`, request.ApprovalID, dbID).Scan(&missing)
	if err != nil {
		return workflowStorage("validate required leader approvals", err)
	}
	if missing {
		return ErrApprovalRequired
	}
	return nil
}

func (s *WorkflowService) policyEpoch(ctx context.Context, databaseID string) (int64, error) {
	var epoch int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT epoch FROM access_database_approval_epochs WHERE database_id = $1), 1)`, databaseID).Scan(&epoch)
	if err != nil {
		return 0, workflowStorage("read approval policy epoch", err)
	}
	return epoch, nil
}

func workflowStorage(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %s", ErrStorage, operation)
}
