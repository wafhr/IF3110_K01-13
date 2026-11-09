package migration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
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
	if !validUUID(databaseID) || !validUUID(migrationID) {
		return ActionPlan{}, ErrInvalidMigration
	}
	if action != Apply && action != Rollback && action != Forward {
		return ActionPlan{}, ErrInvalidMigration
	}
	if s.definitions == nil || s.openTarget == nil {
		return ActionPlan{}, fmt.Errorf("%w: migration definition dependencies are unavailable", ErrStorage)
	}
	definition, err := s.definitions.Get(ctx, databaseID, migrationID)
	if err != nil {
		return ActionPlan{}, err
	}
	db, history, err := s.openHistory(ctx, databaseID)
	if err != nil {
		return ActionPlan{}, err
	}
	defer db.Close()
	executions, err := history.List(ctx, databaseID, false)
	if err != nil {
		return ActionPlan{}, err
	}
	state, err := BuildMigrationState(databaseID, executions)
	if err != nil {
		return ActionPlan{}, err
	}
	number, _, err := state.actionRevision(definition, action)
	if err != nil {
		return ActionPlan{}, err
	}
	revision, err := s.definitions.GetRevision(ctx, databaseID, migrationID, number)
	if err != nil {
		return ActionPlan{}, err
	}
	return state.planAction(definition, revision, action)
}

// SubmitAction memvalidasi pengajuan tanpa menjalankan SQL. Approval mengikat
// snapshot hasil PrepareAction yang juga tersedia bagi modul workflow.
func (s *Service) SubmitAction(ctx context.Context, actorID, databaseID, migrationID string, action Action, approvalID string) (queuedAction QueuedAction, activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "migration_submit", slog.String("actor", actorID), slog.String("database_id", databaseID), slog.String("migration_id", migrationID))
	defer activity.End(&activityErr)

	if !validUUID(actorID) || !validUUID(databaseID) || !validUUID(migrationID) || !validUUID(approvalID) ||
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
	activity.SetAttributes(slog.String("execution_id", executionID), slog.Int64("revision", plan.Revision))
	ctx = telemetry.WithLogAttrs(ctx, slog.String("execution_id", executionID), slog.Int64("revision", plan.Revision))
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
	activity.SetAttributes(slog.String("job_id", jobID))
	return QueuedAction{JobID: jobID, ExecutionID: executionID}, nil
}

// PostgreSQL membatasi advisory lock pada database fisik tempat koneksi dibuka.
// Semua registrasi target yang sama menggunakan kunci ini agar eksekusi berseri.
const targetMigrationLockKey int64 = 311004

// MigrationLockKey dipakai worker untuk pg_advisory_lock(bigint) pada koneksi
// target sebelum membuka transaksi READ COMMITTED. Lock dipertahankan sampai
// commit atau rollback selesai, termasuk selama pembacaan bukti commit.
func MigrationLockKey(databaseID string) (int64, error) {
	if !validUUID(databaseID) {
		return 0, ErrInvalidMigration
	}
	return targetMigrationLockKey, nil
}

// PreparedExecution menyimpan hasil validasi dan transaksi milik worker.
// Worker menjalankan Plan().SQL, memanggil Record, lalu commit transaksi yang
// sama. Jika salah satu langkah gagal, worker wajib melakukan rollback.
type PreparedExecution struct {
	tx                *sql.Tx
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
func (p PreparedExecution) Record(ctx context.Context, startedAt time.Time) (Execution, error) {
	if p.tx == nil || p.committed != nil {
		return Execution{}, ErrConflict
	}
	finishedAt := time.Now().UTC()
	plan := p.request.Plan
	execution := Execution{ID: p.request.ExecutionID, DatabaseID: plan.DatabaseID,
		MigrationID: plan.MigrationID, Revision: plan.Revision, Name: plan.Name, Action: plan.Action,
		SQL: plan.SQL, Status: Success, ExecutedBy: p.request.ActorID, StartedAt: startedAt,
		FinishedAt: &finishedAt, RevertsExecutionID: plan.RevertsExecutionID}
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
	return PreparedExecution{tx: tx, request: request, historySearchPath: "pg_catalog, " + historySchema}, nil
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

	if !validUUID(executionID) {
		return Execution{}, ErrNotFound
	}
	db, history, err := s.openHistory(ctx, databaseID)
	if err != nil {
		return Execution{}, err
	}
	defer db.Close()
	return history.Get(ctx, databaseID, executionID)
}

// openHistory membuka pool milik operasi untuk menyiapkan history PostgreSQL.
// Pemanggil menutup pool yang dikembalikan setelah pembacaan selesai.
func (s *Service) openHistory(ctx context.Context, databaseID string) (*sql.DB, *HistoryRepository, error) {
	if !validUUID(databaseID) {
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
	if dbms != database.PostgreSQL {
		_ = db.Close()
		return nil, nil, fmt.Errorf("%w: execution history requires PostgreSQL", ErrInvalidMigration)
	}
	history, err := NewHistoryRepository(db)
	if err == nil {
		err = history.ensureInitialized(ctx, databaseID)
	}
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return db, history, nil
}

func (s *Service) List(ctx context.Context, databaseID string) ([]Record, error) {
	db, dbms, err := s.openTarget(ctx, databaseID)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureHistory(ctx, db, dbms); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT `+historyColumns(dbms)+` FROM platform_migrations ORDER BY version DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Record, 0)
	for rows.Next() {
		var row Record
		if err := rows.Scan(&row.Version, &row.Name, &row.SQL, &row.Status, &row.ExecutedBy, &row.StartedAt, &row.FinishedAt); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (s *Service) Get(ctx context.Context, databaseID string, version int64) (Record, error) {
	db, dbms, err := s.openTarget(ctx, databaseID)
	if err != nil {
		return Record{}, err
	}
	defer db.Close()
	if err := ensureHistory(ctx, db, dbms); err != nil {
		return Record{}, err
	}
	var row Record
	err = db.QueryRowContext(ctx, `SELECT `+historyColumns(dbms)+` FROM platform_migrations WHERE version=`+placeholder(dbms, 1), version).Scan(&row.Version, &row.Name, &row.SQL, &row.Status, &row.ExecutedBy, &row.StartedAt, &row.FinishedAt)
	return row, err
}

func (s *Service) Execute(ctx context.Context, databaseID, userID, name, statement string) (Record, error) {
	name, err := NormalizeMigrationName(name)
	if err != nil {
		return Record{}, err
	}
	statement = strings.TrimSpace(statement)
	if err := validateSQL("sql", statement); err != nil {
		return Record{}, err
	}
	db, dbms, err := s.openTarget(ctx, databaseID)
	if err != nil {
		return Record{}, err
	}
	defer db.Close()
	if transactionControl(statement, dbms) {
		return Record{}, fmt.Errorf("%w: transaction control statements are managed by the migration runner", ErrInvalidMigration)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return Record{}, err
	}
	defer conn.Close()
	unlock, err := acquireLock(ctx, conn, databaseID, dbms)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	if err := ensureHistory(ctx, conn, dbms); err != nil {
		return Record{}, err
	}
	row, err := insertRunning(ctx, conn, dbms, name, statement, userID)
	if err != nil {
		return Record{}, fmt.Errorf("record migration: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		s.markFailed(ctx, conn, dbms, row.Version)
		return row, err
	}
	if _, err = tx.ExecContext(ctx, statement); err != nil {
		_ = tx.Rollback()
		s.markFailed(ctx, conn, dbms, row.Version)
		row.Status = Failed
		finished := time.Now().UTC()
		row.FinishedAt = &finished
		return row, fmt.Errorf("migration execution failed (version %d): %w", row.Version, err)
	}
	finished := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `UPDATE platform_migrations SET status='success',finished_at=`+placeholder(dbms, 1)+` WHERE version=`+placeholder(dbms, 2), finished, row.Version); err != nil {
		_ = tx.Rollback()
		s.markFailed(ctx, conn, dbms, row.Version)
		return row, err
	}
	if err = tx.Commit(); err != nil {
		s.markFailed(ctx, conn, dbms, row.Version)
		return row, err
	}
	row.Status = Success
	row.FinishedAt = &finished
	return row, nil
}

func transactionControl(statement string, dbms database.DBMS) bool {
	return transactionControlWithBackslashMode(statement, dbms, false) || transactionControlWithBackslashMode(statement, dbms, true)
}

func transactionControlWithBackslashMode(statement string, dbms database.DBMS, backslashEscapes bool) bool {
	if dbms == database.MySQL && mysqlExecutableControl(statement, backslashEscapes) {
		return true
	}
	return sqlStatementsContainTransactionControl(statement, dbms, backslashEscapes)
}

func sqlStatementsContainTransactionControl(statement string, dbms database.DBMS, backslashEscapes bool) bool {
	for position := 0; position < len(statement); {
		position = skipSQLTrivia(statement, position, dbms)
		if position >= len(statement) {
			return false
		}
		first, end := sqlWord(statement, position)
		next := skipSQLTrivia(statement, end, dbms)
		second, secondEnd := sqlWord(statement, next)
		if forbiddenTransactionStatement(first, second, statement, end, secondEnd, dbms, backslashEscapes) {
			return true
		}
		position = nextSQLStatement(statement, end, dbms, backslashEscapes)
	}
	return false
}

func forbiddenTransactionStatement(first, second, statement string, firstEnd, secondEnd int, dbms database.DBMS, backslashEscapes bool) bool {
	switch first {
	case "BEGIN", "START", "COMMIT", "END", "ROLLBACK", "ABORT", "SAVEPOINT", "RELEASE", "PREPARE", "XA":
		return true
	case "LOCK", "UNLOCK":
		return dbms == database.MySQL && second == "TABLES"
	case "SET":
		if second == "TRANSACTION" || strings.Contains(second, "AUTOCOMMIT") ||
			(dbms == database.MySQL && sqlContainsWord(statement, firstEnd, dbms, backslashEscapes, "AUTOCOMMIT")) {
			return true
		}
		if second == "LOCAL" || second == "SESSION" || second == "GLOBAL" {
			next := skipSQLTrivia(statement, secondEnd, dbms)
			third, _ := sqlWord(statement, next)
			return third == "TRANSACTION" || strings.Contains(third, "AUTOCOMMIT")
		}
	}
	return false
}

func sqlWord(statement string, position int) (string, int) {
	start := position
	for position < len(statement) {
		b := statement[position]
		if (b < 'A' || b > 'Z') && (b < 'a' || b > 'z') && (b < '0' || b > '9') &&
			b != '_' && b != '$' && b != '@' && b != '.' {
			break
		}
		position++
	}
	return strings.ToUpper(statement[start:position]), position
}

func skipSQLTrivia(statement string, position int, dbms database.DBMS) int {
	for position < len(statement) {
		if strings.ContainsRune(" \t\r\n\f\v", rune(statement[position])) {
			position++
			continue
		}
		if position+1 < len(statement) && statement[position:position+2] == "--" &&
			(dbms != database.MySQL || position+2 == len(statement) || statement[position+2] <= ' ') {
			position = skipSQLLine(statement, position+2)
			continue
		}
		if dbms == database.MySQL && statement[position] == '#' {
			position = skipSQLLine(statement, position+1)
			continue
		}
		if position+1 < len(statement) && statement[position:position+2] == "/*" {
			position = skipSQLBlockComment(statement, position)
			continue
		}
		break
	}
	return position
}

func skipSQLLine(statement string, position int) int {
	for position < len(statement) && statement[position] != '\n' && statement[position] != '\r' {
		position++
	}
	return position
}

func skipSQLBlockComment(statement string, position int) int {
	depth := 1
	position += 2
	for position < len(statement) && depth > 0 {
		if position+1 < len(statement) && statement[position:position+2] == "/*" {
			depth++
			position += 2
		} else if position+1 < len(statement) && statement[position:position+2] == "*/" {
			depth--
			position += 2
		} else {
			position++
		}
	}
	return position
}

func nextSQLStatement(statement string, position int, dbms database.DBMS, backslashEscapes bool) int {
	for position < len(statement) {
		if position+1 < len(statement) && statement[position:position+2] == "--" &&
			(dbms != database.MySQL || position+2 == len(statement) || statement[position+2] <= ' ') {
			position = skipSQLLine(statement, position+2)
			continue
		}
		if dbms == database.MySQL && statement[position] == '#' {
			position = skipSQLLine(statement, position+1)
			continue
		}
		if position+1 < len(statement) && statement[position:position+2] == "/*" {
			position = skipSQLBlockComment(statement, position)
			continue
		}
		if statement[position] == ';' {
			return position + 1
		}
		quote := statement[position]
		if quote == '\'' || quote == '"' || (quote == '`' && dbms == database.MySQL) {
			position = skipSQLQuoted(statement, position, quote, backslashEscapes)
			continue
		}
		if dbms == database.PostgreSQL && statement[position] == '$' &&
			(position == 0 || !isSQLWordByte(statement[position-1])) {
			if end := dollarQuoteEnd(statement, position); end > position {
				position = end
				continue
			}
		}
		position++
	}
	return len(statement)
}

func skipSQLQuoted(statement string, position int, quote byte, backslashEscapes bool) int {
	position++
	for position < len(statement) {
		if statement[position] == quote {
			if position+1 < len(statement) && statement[position+1] == quote {
				position += 2
				continue
			}
			return position + 1
		}
		if statement[position] == '\\' && backslashEscapes && position+1 < len(statement) {
			position += 2
			continue
		}
		position++
	}
	return position
}

func sqlContainsWord(statement string, position int, dbms database.DBMS, backslashEscapes bool, target string) bool {
	for position < len(statement) {
		position = skipSQLTrivia(statement, position, dbms)
		if position >= len(statement) || statement[position] == ';' {
			return false
		}
		quote := statement[position]
		if quote == '\'' || quote == '"' || (quote == '`' && dbms == database.MySQL) {
			position = skipSQLQuoted(statement, position, quote, backslashEscapes)
			continue
		}
		if dbms == database.PostgreSQL && quote == '$' &&
			(position == 0 || !isSQLWordByte(statement[position-1])) {
			if end := dollarQuoteEnd(statement, position); end > position {
				position = end
				continue
			}
		}
		word, end := sqlWord(statement, position)
		if strings.Contains(word, target) {
			return true
		}
		if end == position {
			position++
		} else {
			position = end
		}
	}
	return false
}

func isSQLWordByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

func dollarQuoteEnd(statement string, position int) int {
	end := position + 1
	for end < len(statement) && isSQLWordByte(statement[end]) {
		end++
	}
	if end >= len(statement) || statement[end] != '$' || (end > position+1 && statement[position+1] >= '0' && statement[position+1] <= '9') {
		return position
	}
	delimiter := statement[position : end+1]
	closing := strings.Index(statement[end+1:], delimiter)
	if closing < 0 {
		return len(statement)
	}
	return end + 1 + closing + len(delimiter)
}

func mysqlExecutableControl(statement string, backslashEscapes bool) bool {
	for position := 0; ; {
		start := strings.Index(statement[position:], "/*!")
		if start < 0 {
			return false
		}
		start += position + 3
		end := strings.Index(statement[start:], "*/")
		if end < 0 {
			return false
		}
		end += start
		for start < end && (statement[start] >= '0' && statement[start] <= '9' || statement[start] <= ' ') {
			start++
		}
		commentBody := statement[start:end]
		if strings.Contains(commentBody, "/*!") || strings.Contains(strings.ToUpper(commentBody), "AUTOCOMMIT") ||
			sqlStatementsContainTransactionControl(commentBody, database.MySQL, backslashEscapes) {
			return true
		}
		position = end + 2
	}
}

func ensureHistory(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, dbms database.DBMS) error {
	statement := `CREATE TABLE IF NOT EXISTS platform_migrations (version BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,name TEXT NOT NULL,sql TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('running','success','failed')),executed_by UUID NOT NULL,started_at TIMESTAMPTZ NOT NULL DEFAULT now(),finished_at TIMESTAMPTZ)`
	if dbms == database.MySQL {
		statement = "CREATE TABLE IF NOT EXISTS platform_migrations (version BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,name VARCHAR(160) NOT NULL,`sql` LONGTEXT NOT NULL,status VARCHAR(16) NOT NULL CHECK(status IN ('running','success','failed')),executed_by CHAR(36) NOT NULL,started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),finished_at DATETIME(6) NULL) ENGINE=InnoDB"
	}
	_, err := db.ExecContext(ctx, statement)
	return err
}

func (s *Service) markFailed(ctx context.Context, conn *sql.Conn, dbms database.DBMS, version int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = conn.ExecContext(ctx, `UPDATE platform_migrations SET status='failed',finished_at=CURRENT_TIMESTAMP WHERE version=`+placeholder(dbms, 1)+` AND status='running'`, version)
}

func historyColumns(dbms database.DBMS) string {
	if dbms == database.MySQL {
		return "version,name,`sql`,status,executed_by,started_at,finished_at"
	}
	return "version,name,sql,status,executed_by::text,started_at,finished_at"
}

func placeholder(dbms database.DBMS, position int) string {
	if dbms == database.MySQL {
		return "?"
	}
	return fmt.Sprintf("$%d", position)
}

func acquireLock(ctx context.Context, conn *sql.Conn, id string, dbms database.DBMS) (func(), error) {
	if dbms == database.MySQL {
		var acquired sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 5)`, "if3110_migration_"+id).Scan(&acquired); err != nil {
			return nil, err
		}
		if !acquired.Valid || acquired.Int64 != 1 {
			return nil, errors.New("could not acquire migration lock")
		}
		return func() {
			_, _ = conn.ExecContext(context.Background(), `SELECT RELEASE_LOCK(?)`, "if3110_migration_"+id)
		}, nil
	}
	if dbms != database.PostgreSQL {
		return nil, database.ErrInvalidInput
	}
	lock := targetMigrationLockKey
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lock); err != nil {
		return nil, err
	}
	return func() { _, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lock) }, nil
}

func insertRunning(ctx context.Context, conn *sql.Conn, dbms database.DBMS, name, statement, userID string) (Record, error) {
	var row Record
	if dbms == database.PostgreSQL {
		err := conn.QueryRowContext(ctx, `INSERT INTO platform_migrations(name,sql,status,executed_by) VALUES($1,$2,'running',$3) RETURNING version,name,sql,status,executed_by::text,started_at`, name, statement, userID).
			Scan(&row.Version, &row.Name, &row.SQL, &row.Status, &row.ExecutedBy, &row.StartedAt)
		return row, err
	}
	if dbms != database.MySQL {
		return row, database.ErrInvalidInput
	}
	result, err := conn.ExecContext(ctx, "INSERT INTO platform_migrations(name,`sql`,status,executed_by) VALUES(?,?,'running',?)", name, statement, userID)
	if err != nil {
		return row, err
	}
	row.Version, err = result.LastInsertId()
	if err != nil {
		return row, err
	}
	err = conn.QueryRowContext(ctx, `SELECT `+historyColumns(dbms)+` FROM platform_migrations WHERE version=?`, row.Version).
		Scan(&row.Version, &row.Name, &row.SQL, &row.Status, &row.ExecutedBy, &row.StartedAt, &row.FinishedAt)
	return row, err
}
