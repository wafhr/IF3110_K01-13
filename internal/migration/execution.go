package migration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
)

// ExecutionRequest mengikat job pada aktor, approval, revisi, SQL, dan posisi
// history yang ditinjau. Antrean menyimpan seluruh payload secara persisten.
type ExecutionRequest struct {
	ExecutionID string     `json:"execution_id"`
	ApprovalID  string     `json:"approval_id"`
	ActorID     string     `json:"actor_id"`
	Plan        ActionPlan `json:"plan"`
}

type QueuedAction struct {
	JobID       string `json:"job_id"`
	ExecutionID string `json:"execution_id"`
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

// SubmitAction memvalidasi pengajuan tanpa menjalankan SQL. Approval mengikat
// snapshot hasil PrepareAction yang juga tersedia bagi modul workflow.
func (s *Service) SubmitAction(ctx context.Context, actorID, databaseID, migrationID string, action Action, approvalID string) (QueuedAction, error) {
	if !validUUID(actorID) || !validUUID(databaseID) || !validUUID(migrationID) || !validUUID(approvalID) ||
		(action != Apply && action != Rollback && action != Forward) {
		return QueuedAction{}, ErrInvalidMigration
	}
	if s.execution.Authorizer == nil || s.execution.Approvals == nil || s.execution.Queue == nil {
		return QueuedAction{}, ErrExecutionUnavailable
	}
	if err := s.execution.Authorizer.AuthorizeDatabase(ctx, actorID, databaseID, access.ViewEdit); err != nil {
		return QueuedAction{}, err
	}
	plan, err := s.PrepareAction(ctx, databaseID, migrationID, action)
	if err != nil {
		return QueuedAction{}, err
	}
	if transactionControl(plan.SQL, database.PostgreSQL) {
		return QueuedAction{}, ErrInvalidMigration
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return QueuedAction{}, ErrStorage
	}
	id[6], id[8] = (id[6]&0x0f)|0x40, (id[8]&0x3f)|0x80
	executionID := fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	request := ExecutionRequest{ExecutionID: executionID, ApprovalID: approvalID, ActorID: actorID, Plan: plan}
	if err := s.execution.Approvals.ValidateApproval(ctx, request); err != nil {
		return QueuedAction{}, err
	}
	jobID, err := s.execution.Queue.Enqueue(ctx, request)
	if err != nil {
		return QueuedAction{}, err
	}
	if jobID == "" {
		return QueuedAction{}, ErrStorage
	}
	return QueuedAction{JobID: jobID, ExecutionID: executionID}, nil
}

// MigrationLockKey dipakai worker untuk pg_advisory_lock(bigint) pada koneksi
// target yang sama sebelum membuka transaksi READ COMMITTED. Huruf UUID
// dinormalisasi agar satu database menggunakan kunci yang sama. Lock dipertahankan
// sampai commit atau rollback selesai, termasuk selama pembacaan bukti commit.
func MigrationLockKey(databaseID string) (int64, error) {
	if !validUUID(databaseID) {
		return 0, ErrInvalidMigration
	}
	return int64(databaseLock(databaseID)), nil
}

// PreparedExecution menyimpan hasil validasi dan transaksi milik worker.
// Worker menjalankan Plan().SQL, memanggil Record, lalu commit transaksi yang
// sama. Jika salah satu langkah gagal, worker wajib melakukan rollback.
type PreparedExecution struct {
	tx        *sql.Tx
	request   ExecutionRequest
	committed *Execution
}

func (p PreparedExecution) Plan() ActionPlan { return p.request.Plan }

// Committed menyediakan bukti commit untuk job duplikat atau commit ambigu.
// Worker menyelesaikan job tanpa menjalankan SQL kembali jika bukti tersedia.
func (p PreparedExecution) Committed() (Execution, bool) {
	if p.committed == nil {
		return Execution{}, false
	}
	return *p.committed, true
}

// Record menyimpan history pada transaksi perubahan target. Nilai kembali baru
// menjadi bukti sukses setelah worker menerima hasil commit yang pasti.
func (p PreparedExecution) Record(ctx context.Context, startedAt time.Time) (Execution, error) {
	if p.tx == nil || p.committed != nil {
		return Execution{}, ErrConflict
	}
	finishedAt := time.Now().UTC()
	plan := p.request.Plan
	return AppendExecution(ctx, p.tx, Execution{ID: p.request.ExecutionID, DatabaseID: plan.DatabaseID,
		MigrationID: plan.MigrationID, Revision: plan.Revision, Name: plan.Name, Action: plan.Action,
		SQL: plan.SQL, Status: Success, ExecutedBy: p.request.ActorID, StartedAt: startedAt,
		FinishedAt: &finishedAt, RevertsExecutionID: plan.RevertsExecutionID})
}

// PrepareExecution dipanggil worker sebelum SQL. Schema history disiapkan oleh
// jalur pengajuan. Pemeriksaan ulang menggunakan history lengkap di transaksi
// target, permission terkini, dan approval yang masih sesuai snapshot.
func (s *Service) PrepareExecution(ctx context.Context, tx *sql.Tx, request ExecutionRequest) (PreparedExecution, error) {
	plan := request.Plan
	if tx == nil || !validUUID(request.ExecutionID) || !validUUID(request.ApprovalID) || !validUUID(request.ActorID) ||
		!validUUID(plan.DatabaseID) || !validUUID(plan.MigrationID) || plan.Revision < 1 ||
		(plan.Action != Apply && plan.Action != Rollback && plan.Action != Forward) {
		return PreparedExecution{}, ErrInvalidMigration
	}
	key, _ := MigrationLockKey(plan.DatabaseID)
	var locked bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
        SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()
        AND mode = 'ExclusiveLock' AND granted AND classid::bigint = 0 AND objid::bigint = $1 AND objsubid = 1
    ) AND current_setting('transaction_isolation') = 'read committed'
      AND current_setting('transaction_read_only') = 'off'`, key).Scan(&locked)
	if err != nil {
		return PreparedExecution{}, historyError("check migration lock", err)
	}
	if !locked {
		return PreparedExecution{}, ErrConflict
	}
	existing, err := GetExecutionInTransaction(ctx, tx, plan.DatabaseID, request.ExecutionID)
	if err == nil {
		if existing.MigrationID != plan.MigrationID || existing.Revision != plan.Revision || existing.Name != plan.Name ||
			existing.Action != plan.Action || existing.SQL != plan.SQL || existing.ExecutedBy != request.ActorID ||
			existing.RevertsExecutionID != plan.RevertsExecutionID {
			return PreparedExecution{}, ErrConflict
		}
		return PreparedExecution{tx: tx, request: request, committed: &existing}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return PreparedExecution{}, err
	}
	definitions, ok := s.definitions.(ExecutionDefinitionStore)
	if !ok || s.execution.Authorizer == nil || s.execution.Approvals == nil {
		return PreparedExecution{}, ErrExecutionUnavailable
	}
	if err := s.execution.Authorizer.AuthorizeDatabase(ctx, request.ActorID, plan.DatabaseID, access.ViewEdit); err != nil {
		return PreparedExecution{}, err
	}
	history, err := ListExecutionsInTransaction(ctx, tx, plan.DatabaseID)
	if err != nil {
		return PreparedExecution{}, err
	}
	definition, err := definitions.Get(ctx, plan.DatabaseID, plan.MigrationID)
	if err != nil {
		return PreparedExecution{}, err
	}
	revision, err := definitions.GetRevision(ctx, plan.DatabaseID, plan.MigrationID, plan.Revision)
	if err != nil {
		return PreparedExecution{}, err
	}
	fresh, err := PlanAction(plan.DatabaseID, history, definition, revision, plan.Action)
	if err != nil {
		return PreparedExecution{}, err
	}
	if fresh != plan {
		return PreparedExecution{}, ErrConflict
	}
	if transactionControl(fresh.SQL, database.PostgreSQL) {
		return PreparedExecution{}, ErrInvalidMigration
	}
	if err := s.execution.Approvals.ValidateApproval(ctx, request); err != nil {
		return PreparedExecution{}, err
	}
	if err := definitions.FreezeRevision(ctx, plan.DatabaseID, plan.MigrationID, plan.Revision); err != nil {
		return PreparedExecution{}, err
	}
	return PreparedExecution{tx: tx, request: request}, nil
}
