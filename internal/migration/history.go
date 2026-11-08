package migration

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/wafhr/IF3110_K01-13/internal/database"
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
		return repositoryError("begin history initialization", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(311003)`); err != nil {
		return repositoryError("lock history initialization", err)
	}
	if _, err := tx.ExecContext(ctx, executionSchema); err != nil {
		return repositoryError("initialize execution history", err)
	}
	var legacyExists bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('platform_migrations') IS NOT NULL`).Scan(&legacyExists); err != nil {
		return repositoryError("find legacy history", err)
	}
	if legacyExists {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO platform_migration_executions
			(database_id, name, action, sql, status, executed_by, started_at, finished_at, legacy_version)
			SELECT $1, name, 'apply', sql, 'success', executed_by, started_at, finished_at, version
			FROM platform_migrations WHERE status = 'success' ORDER BY version
			ON CONFLICT (database_id, legacy_version) DO NOTHING`, databaseID); err != nil {
			return repositoryError("import committed legacy history", err)
		}
	}
	return repositoryError("commit history initialization", tx.Commit())
}

// Append mencatat SQL yang berhasil melalui transaksi target milik executor.
// Executor melakukan commit setelah SQL dan history berhasil disimpan bersama.
// ID execution yang sama menimbulkan konflik agar pengulangan SQL di-rollback.
func (r *HistoryRepository) Append(ctx context.Context, tx *sql.Tx, execution Execution) (Execution, error) {
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
		return Execution{}, repositoryError("append committed execution", err)
	}
	return stored, nil
}

func (r *HistoryRepository) Get(ctx context.Context, databaseID, executionID string) (Execution, error) {
	if !validUUID(databaseID) || !validUUID(executionID) {
		return Execution{}, ErrNotFound
	}
	execution, err := scanExecution(r.db.QueryRowContext(ctx, `SELECT `+executionColumns+`
		FROM platform_migration_executions WHERE database_id = $1 AND id = $2`, databaseID, executionID))
	if err != nil {
		return Execution{}, repositoryError("get committed execution", err)
	}
	return execution, nil
}

// List mengurutkan history dari pencatatan terbaru. Filter hanya memengaruhi
// hasil pembacaan dan mempertahankan seluruh catatan yang sudah tersimpan.
func (r *HistoryRepository) List(ctx context.Context, databaseID string, hideRollback bool) ([]Execution, error) {
	if !validUUID(databaseID) {
		return nil, ErrNotFound
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+executionColumns+`
		FROM platform_migration_executions WHERE database_id = $1 ORDER BY sequence DESC`, databaseID)
	if err != nil {
		return nil, repositoryError("list committed executions", err)
	}
	defer rows.Close()
	executions := make([]Execution, 0)
	for rows.Next() {
		execution, err := scanExecution(rows)
		if err != nil {
			return nil, repositoryError("read committed execution", err)
		}
		executions = append(executions, execution)
	}
	if err := rows.Err(); err != nil {
		return nil, repositoryError("list committed executions", err)
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
		return nil, repositoryError("find legacy attempts", err)
	}
	attempts := make([]Record, 0)
	if !exists {
		return attempts, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+historyColumns(database.PostgreSQL)+`
		FROM platform_migrations WHERE status <> 'success' ORDER BY version DESC`)
	if err != nil {
		return nil, repositoryError("list legacy attempts", err)
	}
	defer rows.Close()
	for rows.Next() {
		var attempt Record
		if err := rows.Scan(&attempt.Version, &attempt.Name, &attempt.SQL, &attempt.Status,
			&attempt.ExecutedBy, &attempt.StartedAt, &attempt.FinishedAt); err != nil {
			return nil, repositoryError("read legacy attempt", err)
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, repositoryError("list legacy attempts", err)
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
