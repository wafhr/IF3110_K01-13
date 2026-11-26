package migration

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
	"github.com/wafhr/IF3110_K01-13/internal/uuid"
)

// PostgreSQL membatasi advisory lock pada database fisik tempat koneksi dibuka.
// Semua registrasi target yang sama menggunakan kunci ini agar eksekusi berseri.
const targetMigrationLockKey int64 = 311004

// MigrationLockKey dipakai worker untuk pg_advisory_lock(bigint) pada koneksi
// target sebelum membuka transaksi READ COMMITTED. Lock dipertahankan sampai
// commit atau rollback selesai, termasuk selama pembacaan bukti commit.
func MigrationLockKey(databaseID string) (int64, error) {
	if !uuid.Valid(databaseID) {
		return 0, ErrInvalidMigration
	}
	return targetMigrationLockKey, nil
}

// PreparedExecution menyimpan hasil validasi dan transaksi milik worker.
// Worker menjalankan Plan().SQL, memanggil Record, lalu commit transaksi yang
// sama. Jika salah satu langkah gagal, worker wajib melakukan rollback.
type PreparedExecution struct {
	tx                *sql.Tx
	dbms              database.DBMS
	request           ExecutionRequest
	committed         *Execution
	historySearchPath string
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
func (p PreparedExecution) Record(ctx context.Context, startedAt time.Time) (recorded Execution, activityErr error) {
	ctx = p.request.ExecutionContext(ctx)
	ctx, activity := telemetry.StartActivity(ctx, "migration_history_stage", slog.Bool("commit_pending", true))
	defer activity.End(&activityErr)

	if p.tx == nil || p.committed != nil {
		return Execution{}, ErrConflict
	}
	finishedAt := time.Now().UTC()
	plan := p.request.Plan
	execution := Execution{ID: p.request.ExecutionID, DatabaseID: plan.DatabaseID,
		MigrationID: plan.MigrationID, Revision: plan.Revision, Name: plan.Name, Action: plan.Action,
		SQL: plan.SQL, Status: Success, ExecutedBy: p.request.ActorID, StartedAt: startedAt,
		FinishedAt: &finishedAt, RevertsExecutionID: plan.RevertsExecutionID}
	if p.dbms == database.MySQL {
		return appendExecution(ctx, p.tx, database.MySQL, execution)
	}
	// SQL migration dapat mengganti search_path. Metadata dan trigger history
	// memakai schema asalnya, lalu pengaturan SQL migration dipulihkan kembali.
	var migrationSearchPath string
	if err := p.tx.QueryRowContext(ctx, `SELECT pg_catalog.current_setting('search_path')`).Scan(&migrationSearchPath); err != nil {
		return Execution{}, historyError("read migration search path", err)
	}
	if _, err := p.tx.ExecContext(ctx, `SELECT pg_catalog.set_config('search_path', $1, true)`, p.historySearchPath); err != nil {
		return Execution{}, historyError("set execution history search path", err)
	}
	stored, appendErr := AppendExecution(ctx, p.tx, execution)
	_, restoreErr := p.tx.ExecContext(ctx, `SELECT pg_catalog.set_config('search_path', $1, true)`, migrationSearchPath)
	if appendErr != nil {
		return Execution{}, appendErr
	}
	if restoreErr != nil {
		return Execution{}, historyError("restore migration search path", restoreErr)
	}
	return stored, nil
}

// PrepareExecution dipanggil worker sebelum SQL. Schema history disiapkan oleh
// jalur pengajuan. Pemeriksaan ulang menggunakan history lengkap di transaksi
// target, permission terkini, dan approval yang masih sesuai snapshot.
func (s *Service) PrepareExecution(ctx context.Context, tx *sql.Tx, request ExecutionRequest) (PreparedExecution, error) {
	return s.PrepareExecutionOn(ctx, tx, database.PostgreSQL, request)
}

// PrepareExecutionOn menjalankan validasi yang sama untuk DBMS target tertentu.
// Pada MySQL, lock GET_LOCK menggantikan advisory lock PostgreSQL.
func (s *Service) PrepareExecutionOn(ctx context.Context, tx *sql.Tx, dbms database.DBMS, request ExecutionRequest) (prepared PreparedExecution, activityErr error) {
	ctx = request.ExecutionContext(ctx)
	ctx, activity := telemetry.StartActivity(ctx, "migration_execution_prepare")
	defer activity.End(&activityErr)

	plan := request.Plan
	if tx == nil || !uuid.Valid(request.ExecutionID) || !uuid.Valid(request.ApprovalID) || !uuid.Valid(request.ActorID) ||
		!uuid.Valid(plan.DatabaseID) || !uuid.Valid(plan.MigrationID) || plan.Revision < 1 ||
		(plan.Action != Apply && plan.Action != Rollback && plan.Action != Forward) ||
		(dbms != database.PostgreSQL && dbms != database.MySQL) ||
		(dbms == database.MySQL && plan.Batch.Enabled()) {
		return PreparedExecution{}, ErrInvalidMigration
	}
	locked, err := holdsMigrationLock(ctx, tx, dbms, plan.DatabaseID)
	if err != nil {
		return PreparedExecution{}, historyError("check migration lock", err)
	}
	if !locked {
		return PreparedExecution{}, ErrConflict
	}
	existing, err := getExecutionInTransaction(ctx, tx, dbms, plan.DatabaseID, request.ExecutionID)
	if err == nil {
		if existing.MigrationID != plan.MigrationID || existing.Revision != plan.Revision || existing.Name != plan.Name ||
			existing.Action != plan.Action || existing.SQL != plan.SQL || existing.ExecutedBy != request.ActorID ||
			existing.RevertsExecutionID != plan.RevertsExecutionID {
			return PreparedExecution{}, ErrConflict
		}
		activity.SetAttributes(slog.Bool("commit_proof", true))
		return PreparedExecution{tx: tx, dbms: dbms, request: request, committed: &existing}, nil
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
	history, err := listExecutionsInTransaction(ctx, tx, dbms, plan.DatabaseID)
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
	if transactionControlFor(dbms, fresh.SQL) {
		return PreparedExecution{}, ErrInvalidMigration
	}
	if err := s.execution.Approvals.ValidateApproval(ctx, request); err != nil {
		return PreparedExecution{}, err
	}
	if dbms == database.MySQL {
		if err := definitions.FreezeRevision(ctx, plan.DatabaseID, plan.MigrationID, plan.Revision); err != nil {
			return PreparedExecution{}, err
		}
		return PreparedExecution{tx: tx, dbms: dbms, request: request}, nil
	}
	// Lokasi metadata dicatat sebelum SQL mengubah pengaturan koneksi. Schema
	// lama tetap digunakan, termasuk ketika tabel history berada di luar public.
	var historySchema string
	if err := tx.QueryRowContext(ctx, `SELECT pg_catalog.quote_ident(n.nspname)
        FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
        WHERE c.oid = pg_catalog.to_regclass('platform_migration_executions')`).Scan(&historySchema); err != nil {
		return PreparedExecution{}, historyError("find execution history schema", err)
	}
	if err := definitions.FreezeRevision(ctx, plan.DatabaseID, plan.MigrationID, plan.Revision); err != nil {
		return PreparedExecution{}, err
	}
	return PreparedExecution{tx: tx, dbms: dbms, request: request, historySearchPath: "pg_catalog, " + historySchema}, nil
}

// holdsMigrationLock memastikan transaksi berjalan pada koneksi yang memegang
// lock migration database fisik. PostgreSQL juga harus READ COMMITTED dan
// read-write; MySQL tidak menyediakan level transaksi aktif untuk diperiksa.
func holdsMigrationLock(ctx context.Context, tx *sql.Tx, dbms database.DBMS, databaseID string) (bool, error) {
	if dbms == database.MySQL {
		var holder sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT IS_USED_LOCK(`+MySQLMigrationLockName+`) = CONNECTION_ID()`).Scan(&holder)
		return holder.Valid && holder.Int64 == 1, err
	}
	key, _ := MigrationLockKey(databaseID)
	var locked bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
        SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()
        AND mode = 'ExclusiveLock' AND granted AND classid::bigint = 0 AND objid::bigint = $1 AND objsubid = 1
    ) AND current_setting('transaction_isolation') = 'read committed'
      AND current_setting('transaction_read_only') = 'off'`, key).Scan(&locked)
	return locked, err
}
