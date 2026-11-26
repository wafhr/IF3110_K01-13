package migration

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
	"github.com/wafhr/IF3110_K01-13/internal/uuid"
)

// TargetOpener mengembalikan pool baru yang dimiliki operasi migration.
// Gunakan pool khusus karena runner menutupnya setelah setiap operasi selesai.
type TargetOpener func(context.Context, string) (*sql.DB, database.DBMS, error)

type Service struct {
	openTarget  TargetOpener
	definitions DefinitionReader
	execution   ExecutionDependencies
}

// NewService menggunakan registry database untuk membuka setiap koneksi target.
func NewService(openTarget TargetOpener) *Service { return &Service{openTarget: openTarget} }

// DefinitionReader menyediakan definisi dan revisi asli dari database platform.
type DefinitionReader interface {
	Get(context.Context, string, string) (Migration, error)
	GetRevision(context.Context, string, string, int64) (Revision, error)
}

func NewServiceWithRepository(openTarget TargetOpener, definitions DefinitionReader) *Service {
	service := NewService(openTarget)
	service.definitions = definitions
	return service
}

type ActionAuthorizer interface {
	AuthorizeDatabase(context.Context, string, string, access.Permission) error
}

// ApprovalChecker memeriksa approval semua leader grup yang memiliki akses.
// Implementasi memverifikasi kesesuaian aktor dan seluruh snapshot Plan serta
// status approval terbaru, termasuk perubahan leader dan akses grup.
type ApprovalChecker interface {
	ValidateApproval(context.Context, ExecutionRequest) error
}

// ActionQueue mengembalikan ID setelah job dan payload tersimpan persisten.
// Pengiriman ulang job yang sama wajib mempertahankan ExecutionID aslinya.
type ActionQueue interface {
	Enqueue(context.Context, ExecutionRequest) (string, error)
}

type ExecutionDependencies struct {
	Authorizer ActionAuthorizer
	Approvals  ApprovalChecker
	Queue      ActionQueue
}

type ExecutionDefinitionStore interface {
	DefinitionReader
	FreezeRevision(context.Context, string, string, int64) error
}

func NewServiceWithExecution(openTarget TargetOpener, definitions ExecutionDefinitionStore, dependencies ExecutionDependencies) *Service {
	service := NewServiceWithRepository(openTarget, definitions)
	service.execution = dependencies
	return service
}

// PrepareAction menyusun rencana tanpa menjalankan SQL migration. Rencana
// permintaan harus diperiksa kembali setelah lock dan sebelum eksekusi worker.
func (s *Service) PrepareAction(ctx context.Context, databaseID, migrationID string, action Action) (ActionPlan, error) {
	plan, _, err := s.prepareAction(ctx, databaseID, migrationID, action)
	return plan, err
}

// prepareAction juga mengembalikan DBMS target agar validasi SQL memakai
// aturan dialek yang sama dengan runner.
func (s *Service) prepareAction(ctx context.Context, databaseID, migrationID string, action Action) (ActionPlan, database.DBMS, error) {
	if !uuid.Valid(databaseID) || !uuid.Valid(migrationID) {
		return ActionPlan{}, "", ErrInvalidMigration
	}
	if action != Apply && action != Rollback && action != Forward {
		return ActionPlan{}, "", ErrInvalidMigration
	}
	if s.definitions == nil || s.openTarget == nil {
		return ActionPlan{}, "", fmt.Errorf("%w: migration definition dependencies are unavailable", ErrStorage)
	}
	definition, err := s.definitions.Get(ctx, databaseID, migrationID)
	if err != nil {
		return ActionPlan{}, "", err
	}
	db, history, err := s.openHistory(ctx, databaseID)
	if err != nil {
		return ActionPlan{}, "", err
	}
	defer db.Close()
	executions, err := history.listAll(ctx)
	if err != nil {
		return ActionPlan{}, "", err
	}
	state, err := BuildMigrationState(databaseID, executions)
	if err != nil {
		return ActionPlan{}, "", err
	}
	number, _, err := state.actionRevision(definition, action)
	if err != nil {
		return ActionPlan{}, "", err
	}
	revision, err := s.definitions.GetRevision(ctx, databaseID, migrationID, number)
	if err != nil {
		return ActionPlan{}, "", err
	}
	plan, err := state.planAction(definition, revision, action)
	if err != nil {
		return ActionPlan{}, "", err
	}
	if history.dbms == database.MySQL && plan.Batch.Enabled() {
		return ActionPlan{}, "", fmt.Errorf("%w: batch migrations require a PostgreSQL target", ErrInvalidMigration)
	}
	return plan, history.dbms, nil
}

// SubmitAction memvalidasi pengajuan tanpa menjalankan SQL. Approval mengikat
// snapshot hasil PrepareAction yang juga tersedia bagi modul workflow.
func (s *Service) SubmitAction(ctx context.Context, actorID, databaseID, migrationID string, action Action, approvalID string) (queuedAction QueuedAction, activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "migration_submit", slog.String("actor", actorID), slog.String("database_id", databaseID), slog.String("migration_id", migrationID))
	defer activity.End(&activityErr)

	if !uuid.Valid(actorID) || !uuid.Valid(databaseID) || !uuid.Valid(migrationID) || !uuid.Valid(approvalID) ||
		(action != Apply && action != Rollback && action != Forward) {
		return QueuedAction{}, ErrInvalidMigration
	}
	activity.SetAttributes(slog.String("action", string(action)))
	if s.execution.Authorizer == nil || s.execution.Approvals == nil || s.execution.Queue == nil {
		return QueuedAction{}, ErrExecutionUnavailable
	}
	if err := s.execution.Authorizer.AuthorizeDatabase(ctx, actorID, databaseID, access.ViewEdit); err != nil {
		return QueuedAction{}, err
	}
	plan, dbms, err := s.prepareAction(ctx, databaseID, migrationID, action)
	if err != nil {
		return QueuedAction{}, err
	}
	if transactionControlFor(dbms, plan.SQL) {
		return QueuedAction{}, ErrInvalidMigration
	}
	executionID, err := uuid.New()
	if err != nil {
		return QueuedAction{}, ErrStorage
	}
	activity.SetAttributes(slog.String("execution_id", executionID), slog.Int64("revision", plan.Revision))
	ctx = telemetry.WithLogAttrs(ctx, slog.String("execution_id", executionID), slog.Int64("revision", plan.Revision))
	request := ExecutionRequest{ExecutionID: executionID, ApprovalID: approvalID, ActorID: actorID, Plan: plan}
	if err := s.execution.Approvals.ValidateApproval(ctx, request); err != nil {
		return QueuedAction{}, err
	}
	jobID, err := s.enqueueAction(ctx, request)
	if err != nil {
		return QueuedAction{}, err
	}
	activity.SetAttributes(slog.String("job_id", jobID))
	return QueuedAction{JobID: jobID, ExecutionID: executionID}, nil
}

// Trace parent disalin dari span producer setelah approval selesai diperiksa.
// Antrean menyimpan seluruh ExecutionRequest agar retry memakai parent yang sama.
func (s *Service) enqueueAction(ctx context.Context, request ExecutionRequest) (jobID string, activityErr error) {
	ctx, activity := telemetry.StartProducerActivity(ctx, "migration_enqueue")
	defer activity.End(&activityErr)
	request.TraceContext = telemetry.CaptureTraceContext(ctx)
	jobID, err := s.execution.Queue.Enqueue(ctx, request)
	if err != nil {
		return "", err
	}
	if jobID == "" {
		return "", ErrStorage
	}
	activity.SetAttributes(slog.String("job_id", jobID))
	return jobID, nil
}

// ReadExecutionHistory menyajikan commit dan attempt legacy sebagai dua koleksi.
func (s *Service) ReadExecutionHistory(ctx context.Context, databaseID string, hideRollback bool) (result ExecutionHistory, activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "migration_history_read", slog.String("database_id", databaseID))
	defer activity.End(&activityErr)

	db, history, err := s.openHistory(ctx, databaseID)
	if err != nil {
		return ExecutionHistory{}, err
	}
	defer db.Close()
	executions, err := history.List(ctx, databaseID, hideRollback)
	if err != nil {
		return ExecutionHistory{}, err
	}
	attempts, err := history.LegacyAttempts(ctx)
	if err != nil {
		return ExecutionHistory{}, err
	}
	return ExecutionHistory{Executions: executions, LegacyAttempts: attempts}, nil
}

func (s *Service) GetExecution(ctx context.Context, databaseID, executionID string) (result Execution, activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "migration_execution_read", slog.String("database_id", databaseID), slog.String("execution_id", executionID))
	defer activity.End(&activityErr)

	if !uuid.Valid(executionID) {
		return Execution{}, ErrNotFound
	}
	db, history, err := s.openHistory(ctx, databaseID)
	if err != nil {
		return Execution{}, err
	}
	defer db.Close()
	return history.Get(ctx, databaseID, executionID)
}

// openHistory membuka pool milik operasi untuk menyiapkan history target.
// Pemanggil menutup pool yang dikembalikan setelah pembacaan selesai.
func (s *Service) openHistory(ctx context.Context, databaseID string) (*sql.DB, *HistoryRepository, error) {
	if !uuid.Valid(databaseID) {
		return nil, nil, ErrInvalidMigration
	}
	if s.openTarget == nil {
		return nil, nil, ErrStorage
	}
	db, dbms, err := s.openTarget(ctx, databaseID)
	if err != nil {
		return nil, nil, err
	}
	if db == nil {
		return nil, nil, ErrStorage
	}
	history, err := newHistoryRepository(db, dbms)
	if err == nil {
		err = history.ensureInitialized(ctx, databaseID)
	}
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return db, history, nil
}

func historyColumns(dbms database.DBMS) string {
	if dbms == database.MySQL {
		return "version,name,`sql`,status,executed_by,started_at,finished_at"
	}
	return "version,name,sql,status,executed_by::text,started_at,finished_at"
}
