package migration

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

//go:embed target_schema.sql
var executionSchema string

const executionColumns = `id::text, sequence, database_id::text, migration_id::text, revision, name, action, sql, status, executed_by::text, started_at, finished_at, reverts_execution_id::text, legacy_version`

// HistoryRepository menyimpan history commit pada database target PostgreSQL.
// Pemanggil memeriksa permission dan menggunakan koneksi target yang sesuai.
type HistoryRepository struct {
	db *sql.DB
}

func NewHistoryRepository(db *sql.DB) (*HistoryRepository, error) {
	if db == nil {
		return nil, errors.New("migration history requires a target database")
	}
	return &HistoryRepository{db: db}, nil
}

// Initialize menyiapkan schema dan mengimpor history sukses Milestone 1 secara
// atomik. Penanda legacy menyatakan bahwa definisi SQL down belum tersedia.
func (r *HistoryRepository) Initialize(ctx context.Context, databaseID string) error {
	if !validUUID(databaseID) {
		return ErrInvalidMigration
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return historyError("begin history initialization", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(311003)`); err != nil {
		return historyError("lock history initialization", err)
	}
	if _, err := tx.ExecContext(ctx, executionSchema); err != nil {
		return historyError("initialize execution history", err)
	}
	if err := importLegacyHistory(ctx, tx, databaseID); err != nil {
		return err
	}
	return historyError("commit history initialization", tx.Commit())
}

type historyConnection interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// ensureInitialized memakai schema yang sudah tersedia pada jalur pembacaan.
// Schema baru disiapkan saat tabel history belum ada.
func (r *HistoryRepository) ensureInitialized(ctx context.Context, databaseID string) error {
	var exists bool
	if err := r.db.QueryRowContext(ctx, `SELECT to_regclass('platform_migration_executions') IS NOT NULL`).Scan(&exists); err != nil {
		return historyError("find execution history", err)
	}
	if !exists {
		return r.Initialize(ctx, databaseID)
	}
	return importLegacyHistory(ctx, r.db, databaseID)
}

func importLegacyHistory(ctx context.Context, connection historyConnection, databaseID string) error {
	var exists bool
	if err := connection.QueryRowContext(ctx, `SELECT to_regclass('platform_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return historyError("find legacy history", err)
	}
	if !exists {
		return nil
	}
	var pending bool
	if err := connection.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM platform_migrations legacy WHERE status = 'success' AND NOT EXISTS (
			SELECT 1 FROM platform_migration_executions history WHERE database_id = $1 AND legacy_version = legacy.version
		))`, databaseID).Scan(&pending); err != nil {
		return historyError("find unimported legacy history", err)
	}
	if !pending {
		return nil
	}
	_, err := connection.ExecContext(ctx, `
		INSERT INTO platform_migration_executions
		(database_id, name, action, sql, status, executed_by, started_at, finished_at, legacy_version)
		SELECT $1, name, 'apply', sql, 'success', executed_by, started_at, finished_at, version
		FROM platform_migrations WHERE status = 'success' ORDER BY version
		ON CONFLICT (database_id, legacy_version) DO NOTHING`, databaseID)
	return historyError("import committed legacy history", err)
}

// Append mencatat SQL yang berhasil melalui transaksi target milik executor.
// Executor melakukan commit setelah SQL dan history berhasil disimpan bersama.
// ID execution yang sama menimbulkan konflik agar pengulangan SQL di-rollback.
func (r *HistoryRepository) Append(ctx context.Context, tx *sql.Tx, execution Execution) (Execution, error) {
	return AppendExecution(ctx, tx, execution)
}

// AppendExecution mencatat history melalui transaksi target milik worker.
func AppendExecution(ctx context.Context, tx *sql.Tx, execution Execution) (Execution, error) {
	if tx == nil {
		return Execution{}, fmt.Errorf("%w: target transaction is required", ErrInvalidMigration)
	}
	var err error
	execution, err = normalizeCommittedExecution(execution)
	if err != nil {
		return Execution{}, err
	}
	stored, err := scanExecution(tx.QueryRowContext(ctx, `
		INSERT INTO platform_migration_executions
		(id, database_id, migration_id, revision, name, action, sql, status, executed_by, started_at, finished_at, reverts_execution_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'success', $8, $9, $10, NULLIF($11, '')::uuid)
		RETURNING `+executionColumns, execution.ID, execution.DatabaseID, execution.MigrationID, execution.Revision,
		execution.Name, execution.Action, execution.SQL, execution.ExecutedBy, execution.StartedAt, execution.FinishedAt, execution.RevertsExecutionID))
	if err != nil {
		return Execution{}, historyError("append committed execution", err)
	}
	return stored, nil
}

func (r *HistoryRepository) Get(ctx context.Context, databaseID, executionID string) (Execution, error) {
	if !validUUID(databaseID) || !validUUID(executionID) {
		return Execution{}, ErrNotFound
	}
	return getExecution(ctx, r.db, databaseID, executionID)
}

// List mengurutkan history dari pencatatan terbaru. Filter hanya memengaruhi
// hasil pembacaan dan mempertahankan seluruh catatan yang sudah tersimpan.
func (r *HistoryRepository) List(ctx context.Context, databaseID string, hideRollback bool) ([]Execution, error) {
	return listExecutions(ctx, r.db, databaseID, hideRollback)
}

type executionQueries interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ListExecutionsInTransaction membaca history lengkap untuk validasi worker.
func ListExecutionsInTransaction(ctx context.Context, tx *sql.Tx, databaseID string) ([]Execution, error) {
	if tx == nil {
		return nil, ErrInvalidMigration
	}
	return listExecutions(ctx, tx, databaseID, false)
}

// GetExecutionInTransaction mencari bukti commit pada koneksi target yang dikunci.
func GetExecutionInTransaction(ctx context.Context, tx *sql.Tx, databaseID, executionID string) (Execution, error) {
	if tx == nil || !validUUID(databaseID) || !validUUID(executionID) {
		return Execution{}, ErrInvalidMigration
	}
	return getExecution(ctx, tx, databaseID, executionID)
}

func getExecution(ctx context.Context, connection executionQueries, databaseID, executionID string) (Execution, error) {
	execution, err := scanExecution(connection.QueryRowContext(ctx, `SELECT `+executionColumns+`
        FROM platform_migration_executions WHERE database_id = $1 AND id = $2`, databaseID, executionID))
	if err != nil {
		return Execution{}, historyError("get committed execution", err)
	}
	return execution, nil
}

func listExecutions(ctx context.Context, connection executionQueries, databaseID string, hideRollback bool) ([]Execution, error) {
	if !validUUID(databaseID) {
		return nil, ErrNotFound
	}
	rows, err := connection.QueryContext(ctx, `SELECT `+executionColumns+`
		FROM platform_migration_executions WHERE database_id = $1 ORDER BY sequence DESC`, databaseID)
	if err != nil {
		return nil, historyError("list committed executions", err)
	}
	defer rows.Close()
	executions := make([]Execution, 0)
	for rows.Next() {
		execution, err := scanExecution(rows)
		if err != nil {
			return nil, historyError("read committed execution", err)
		}
		executions = append(executions, execution)
	}
	if err := rows.Err(); err != nil {
		return nil, historyError("list committed executions", err)
	}
	if hideRollback {
		return HideRollback(executions), nil
	}
	return executions, nil
}

// LegacyAttempts menyediakan attempt gagal/running Milestone 1 untuk tampilan
// transisi. Catatan ini tetap terpisah dari history commit Milestone 2.
func (r *HistoryRepository) LegacyAttempts(ctx context.Context) ([]Record, error) {
	var exists bool
	if err := r.db.QueryRowContext(ctx, `SELECT to_regclass('platform_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, historyError("find legacy attempts", err)
	}
	attempts := make([]Record, 0)
	if !exists {
		return attempts, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+historyColumns(database.PostgreSQL)+`
		FROM platform_migrations WHERE status <> 'success' ORDER BY version DESC`)
	if err != nil {
		return nil, historyError("list legacy attempts", err)
	}
	defer rows.Close()
	for rows.Next() {
		var attempt Record
		if err := rows.Scan(&attempt.Version, &attempt.Name, &attempt.SQL, &attempt.Status,
			&attempt.ExecutedBy, &attempt.StartedAt, &attempt.FinishedAt); err != nil {
			return nil, historyError("read legacy attempt", err)
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, historyError("list legacy attempts", err)
	}
	return attempts, nil
}

// HideRollback menyembunyikan pasangan eksekusi asal dan rollback sukses yang
// merujuk ID-nya. Urutan serta isi slice masukan dipertahankan.
func HideRollback(executions []Execution) []Execution {
	byID := make(map[string]Execution, len(executions))
	for _, execution := range executions {
		byID[strings.ToLower(execution.ID)] = execution
	}
	hidden := make(map[string]bool)
	for _, execution := range executions {
		original, exists := byID[strings.ToLower(execution.RevertsExecutionID)]
		if execution.Action == Rollback && execution.Status == Success && exists && original.Status == Success &&
			(original.Action == Apply || original.Action == Forward) && original.ID != execution.ID &&
			strings.EqualFold(original.DatabaseID, execution.DatabaseID) && strings.EqualFold(original.MigrationID, execution.MigrationID) &&
			original.Revision == execution.Revision {
			hidden[strings.ToLower(original.ID)] = true
			hidden[strings.ToLower(execution.ID)] = true
		}
	}
	visible := make([]Execution, 0, len(executions))
	for _, execution := range executions {
		if !hidden[strings.ToLower(execution.ID)] {
			visible = append(visible, execution)
		}
	}
	return visible
}

func normalizeCommittedExecution(execution Execution) (Execution, error) {
	if !validUUID(execution.ID) || !validUUID(execution.DatabaseID) || !validUUID(execution.MigrationID) || !validUUID(execution.ExecutedBy) || execution.Revision < 1 {
		return Execution{}, fmt.Errorf("%w: execution identities and revision are required", ErrInvalidMigration)
	}
	if execution.Status != Success || execution.LegacyVersion != nil {
		return Execution{}, fmt.Errorf("%w: only successful new executions can be appended", ErrInvalidMigration)
	}
	if execution.StartedAt.IsZero() || execution.FinishedAt == nil || execution.FinishedAt.Before(execution.StartedAt) {
		return Execution{}, fmt.Errorf("%w: valid execution timestamps are required", ErrInvalidMigration)
	}
	var err error
	execution.Name, err = NormalizeMigrationName(execution.Name)
	if err != nil {
		return Execution{}, err
	}
	if err := validateSQL("sql", execution.SQL); err != nil {
		return Execution{}, err
	}
	switch execution.Action {
	case Apply, Forward:
		if execution.RevertsExecutionID != "" {
			return Execution{}, fmt.Errorf("%w: only rollback can reference a reverted execution", ErrInvalidMigration)
		}
	case Rollback:
		if !validUUID(execution.RevertsExecutionID) || strings.EqualFold(execution.ID, execution.RevertsExecutionID) {
			return Execution{}, fmt.Errorf("%w: rollback must reference another execution", ErrInvalidMigration)
		}
	default:
		return Execution{}, fmt.Errorf("%w: unsupported migration action", ErrInvalidMigration)
	}
	return execution, nil
}

func scanExecution(row interface{ Scan(...any) error }) (Execution, error) {
	var execution Execution
	var migrationID, revertsID sql.NullString
	var revision, legacyVersion sql.NullInt64
	if err := row.Scan(&execution.ID, &execution.Sequence, &execution.DatabaseID, &migrationID, &revision,
		&execution.Name, &execution.Action, &execution.SQL, &execution.Status, &execution.ExecutedBy,
		&execution.StartedAt, &execution.FinishedAt, &revertsID, &legacyVersion); err != nil {
		return Execution{}, err
	}
	execution.MigrationID, execution.Revision, execution.RevertsExecutionID = migrationID.String, revision.Int64, revertsID.String
	if legacyVersion.Valid {
		execution.LegacyVersion = &legacyVersion.Int64
	}
	return execution, nil
}

// historyError mengelompokkan kegagalan target tanpa membocorkan detail driver.
func historyError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		if postgresError.Code == "42501" {
			return targetdb.ErrPermissionDenied
		}
		if strings.HasPrefix(postgresError.Code, "08") || postgresError.Code == "57P01" || postgresError.Code == "57P02" || postgresError.Code == "57P03" {
			return targetdb.ErrConnection
		}
	}
	return repositoryError(operation, err)
}
