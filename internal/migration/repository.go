package migration

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed schema.sql
var definitionSchema string

const definitionColumns = `id::text, database_id::text, name, current_revision, created_by::text, created_at, updated_at, frozen_at`
const revisionColumns = `migration_id::text, number, sql_up, sql_down, irreversible, irreversible_reason, created_by::text, created_at`

// Repository menyimpan definisi pada database platform. Pemanggil menerapkan
// kontrol akses sebelum menyediakan operasi ini melalui service atau HTTP.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) (*Repository, error) {
	if db == nil {
		return nil, errors.New("migration repository requires a platform database")
	}
	return &Repository{db: db}, nil
}

// Initialize mengantrekan inisialisasi bersamaan dan menerapkan schema secara atomik.
// Registry database dan kontrol akses diinisialisasi sebelum operasi ini.
func (r *Repository) Initialize(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return repositoryError("begin schema initialization", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(311002)`); err != nil {
		return repositoryError("lock schema initialization", err)
	}
	if _, err := tx.ExecContext(ctx, definitionSchema); err != nil {
		return repositoryError("initialize migration definitions", err)
	}
	return repositoryError("commit schema initialization", tx.Commit())
}

// Create menetapkan identitas, pembuat, dan revisi pertama di sisi server.
func (r *Repository) Create(ctx context.Context, databaseID, actorID, name string, revision Revision) (Migration, Revision, error) {
	if !validUUID(databaseID) {
		return Migration{}, Revision{}, ErrNotFound
	}
	if !validUUID(actorID) {
		return Migration{}, Revision{}, ErrInvalidMigration
	}
	name, err := NormalizeMigrationName(name)
	if err != nil {
		return Migration{}, Revision{}, err
	}
	revision.Number = 1
	revision.CreatedBy = actorID
	revision, err = NormalizeRevision(revision)
	if err != nil {
		return Migration{}, Revision{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Migration{}, Revision{}, repositoryError("begin migration creation", err)
	}
	defer tx.Rollback()
	var registeredID string
	if err := tx.QueryRowContext(ctx, `SELECT id::text FROM managed_databases WHERE id = $1 FOR KEY SHARE`, databaseID).Scan(&registeredID); err != nil {
		return Migration{}, Revision{}, repositoryError("find registered database", err)
	}
	definition, err := scanDefinition(tx.QueryRowContext(ctx, `
		INSERT INTO migration_definitions (database_id, name, current_revision, created_by)
		VALUES ($1, $2, 1, $3) RETURNING `+definitionColumns, databaseID, name, actorID))
	if err != nil {
		return Migration{}, Revision{}, repositoryError("create migration definition", err)
	}
	revision.MigrationID = definition.ID
	revision, err = insertRevision(ctx, tx, revision)
	if err != nil {
		return Migration{}, Revision{}, repositoryError("create first revision", err)
	}
	if err := tx.Commit(); err != nil {
		return Migration{}, Revision{}, repositoryError("commit migration creation", err)
	}
	return definition, revision, nil
}

func (r *Repository) Get(ctx context.Context, databaseID, migrationID string) (Migration, error) {
	if !validUUID(databaseID) || !validUUID(migrationID) {
		return Migration{}, ErrNotFound
	}
	definition, err := scanDefinition(r.db.QueryRowContext(ctx, `SELECT `+definitionColumns+`
		FROM migration_definitions WHERE database_id = $1 AND id = $2`, databaseID, migrationID))
	return definition, repositoryError("get migration definition", err)
}

func (r *Repository) List(ctx context.Context, databaseID string) ([]Migration, error) {
	if !validUUID(databaseID) {
		return nil, ErrNotFound
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+definitionColumns+`
		FROM migration_definitions WHERE database_id = $1 ORDER BY created_at, id`, databaseID)
	if err != nil {
		return nil, repositoryError("list migration definitions", err)
	}
	defer rows.Close()
	definitions := make([]Migration, 0)
	for rows.Next() {
		definition, err := scanDefinition(rows)
		if err != nil {
			return nil, repositoryError("read migration definition", err)
		}
		definitions = append(definitions, definition)
	}
	if err := rows.Err(); err != nil {
		return nil, repositoryError("list migration definitions", err)
	}
	return definitions, nil
}

func (r *Repository) GetRevision(ctx context.Context, databaseID, migrationID string, number int64) (Revision, error) {
	if !validUUID(databaseID) || !validUUID(migrationID) {
		return Revision{}, ErrNotFound
	}
	if number < 1 {
		return Revision{}, ErrInvalidMigration
	}
	revision, err := scanRevision(r.db.QueryRowContext(ctx, `SELECT `+revisionColumns+`
		FROM migration_revisions WHERE migration_id = $2 AND number = $3
		AND EXISTS (SELECT 1 FROM migration_definitions WHERE database_id = $1 AND id = $2)`, databaseID, migrationID, number))
	return revision, repositoryError("get migration revision", err)
}

// Revise menambahkan revisi selama definisi masih dapat diedit. expectedRevision
// mencegah perubahan dari editor dengan revisi usang menimpa definisi terbaru.
// Perubahan pada definisi yang sudah dieksekusi atau dibekukan memerlukan migration baru.
func (r *Repository) Revise(ctx context.Context, databaseID, migrationID, actorID, name string, expectedRevision int64, revision Revision) (Migration, Revision, error) {
	if !validUUID(databaseID) || !validUUID(migrationID) {
		return Migration{}, Revision{}, ErrNotFound
	}
	if !validUUID(actorID) || expectedRevision < 1 {
		return Migration{}, Revision{}, ErrInvalidMigration
	}
	name, err := NormalizeMigrationName(name)
	if err != nil {
		return Migration{}, Revision{}, err
	}
	revision.MigrationID, revision.Number, revision.CreatedBy = migrationID, expectedRevision+1, actorID
	revision, err = NormalizeRevision(revision)
	if err != nil {
		return Migration{}, Revision{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Migration{}, Revision{}, repositoryError("begin migration revision", err)
	}
	defer tx.Rollback()
	definition, err := scanDefinition(tx.QueryRowContext(ctx, `SELECT `+definitionColumns+`
		FROM migration_definitions WHERE database_id = $1 AND id = $2 FOR UPDATE`, databaseID, migrationID))
	if err != nil {
		return Migration{}, Revision{}, repositoryError("lock migration definition", err)
	}
	if definition.FrozenAt != nil {
		return Migration{}, Revision{}, ErrImmutableMigration
	}
	if definition.CurrentRevision != expectedRevision {
		return Migration{}, Revision{}, ErrConflict
	}
	revision, err = insertRevision(ctx, tx, revision)
	if err != nil {
		return Migration{}, Revision{}, repositoryError("append migration revision", err)
	}
	definition, err = scanDefinition(tx.QueryRowContext(ctx, `
		UPDATE migration_definitions SET name = $3, current_revision = $4, updated_at = CURRENT_TIMESTAMP
		WHERE database_id = $1 AND id = $2 RETURNING `+definitionColumns, databaseID, migrationID, name, revision.Number))
	if err != nil {
		return Migration{}, Revision{}, repositoryError("update current revision", err)
	}
	if err := tx.Commit(); err != nil {
		return Migration{}, Revision{}, repositoryError("commit migration revision", err)
	}
	return definition, revision, nil
}

// FreezeRevision dipanggil setelah validasi ulang permission dan approval, sebelum
// mencoba menjalankan SQL target. Lock definisinya digunakan bersama Revise untuk
// menolak revisi usang dan menjaga immutability meskipun eksekusi kemudian gagal.
func (r *Repository) FreezeRevision(ctx context.Context, databaseID, migrationID string, number int64) error {
	if !validUUID(databaseID) || !validUUID(migrationID) {
		return ErrNotFound
	}
	if number < 1 {
		return ErrInvalidMigration
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return repositoryError("begin revision freeze", err)
	}
	defer tx.Rollback()
	definition, err := scanDefinition(tx.QueryRowContext(ctx, `SELECT `+definitionColumns+`
		FROM migration_definitions WHERE database_id = $1 AND id = $2 FOR UPDATE`, databaseID, migrationID))
	if err != nil {
		return repositoryError("lock revision for execution", err)
	}
	if definition.CurrentRevision != number {
		return ErrConflict
	}
	if definition.FrozenAt == nil {
		var frozenID string
		if err := tx.QueryRowContext(ctx, `UPDATE migration_definitions
			SET frozen_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
			WHERE database_id = $1 AND id = $2 RETURNING id::text`, databaseID, migrationID).Scan(&frozenID); err != nil {
			return repositoryError("freeze migration revision", err)
		}
	}
	return repositoryError("commit revision freeze", tx.Commit())
}

func insertRevision(ctx context.Context, tx *sql.Tx, revision Revision) (Revision, error) {
	return scanRevision(tx.QueryRowContext(ctx, `
		INSERT INTO migration_revisions (migration_id, number, sql_up, sql_down, irreversible, irreversible_reason, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+revisionColumns,
		revision.MigrationID, revision.Number, revision.SQLUp, revision.SQLDown, revision.Irreversible, revision.IrreversibleReason, revision.CreatedBy))
}

func scanDefinition(row interface{ Scan(...any) error }) (Migration, error) {
	var definition Migration
	err := row.Scan(&definition.ID, &definition.DatabaseID, &definition.Name, &definition.CurrentRevision,
		&definition.CreatedBy, &definition.CreatedAt, &definition.UpdatedAt, &definition.FrozenAt)
	return definition, err
}

func scanRevision(row interface{ Scan(...any) error }) (Revision, error) {
	var revision Revision
	err := row.Scan(&revision.MigrationID, &revision.Number, &revision.SQLUp, &revision.SQLDown,
		&revision.Irreversible, &revision.IrreversibleReason, &revision.CreatedBy, &revision.CreatedAt)
	return revision, err
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil && len(decoded) == 16
}

func repositoryError(operation string, err error) error {
	if err == nil {
		return nil
	}
	for _, cancellation := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cancellation) {
			return cancellation
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23505":
			return ErrConflict
		case "23503":
			return ErrNotFound
		case "23514":
			if postgresError.ConstraintName == "migration_revisions_immutable" || postgresError.ConstraintName == "migration_definitions_frozen" {
				return ErrImmutableMigration
			}
			return ErrInvalidMigration
		}
	}
	return fmt.Errorf("%w: %s", ErrStorage, operation)
}
