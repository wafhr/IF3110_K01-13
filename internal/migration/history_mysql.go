package migration

import (
	"context"
	"database/sql"
	"errors"

	"github.com/go-sql-driver/mysql"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

// MySQLMigrationLockName menghasilkan nama lock GET_LOCK per database fisik.
// Seluruh registrasi yang menunjuk database yang sama memakai lock ini, seperti
// advisory lock PostgreSQL. Panjangnya 51 karakter, di bawah batas 64 MySQL.
const MySQLMigrationLockName = "CONCAT('if3110_mig_', SHA1(DATABASE()))"

const mysqlHistoryInitLockName = "CONCAT('if3110_hist_', SHA1(DATABASE()))"

const mysqlExecutionColumns = "id, `sequence`, database_id, migration_id, revision, name, `action`, `sql`, status, " +
	"executed_by, started_at, finished_at, reverts_execution_id, legacy_version"

// History MySQL bersifat append-only melalui repository: aplikasi tidak pernah
// menjalankan UPDATE atau DELETE terhadap tabel ini. Trigger tidak dipakai
// karena pembuatannya membutuhkan privilege SUPER ketika binary log aktif.
const mysqlExecutionSchema = "CREATE TABLE IF NOT EXISTS platform_migration_executions (" +
	"id CHAR(36) NOT NULL PRIMARY KEY," +
	"`sequence` BIGINT NOT NULL AUTO_INCREMENT UNIQUE," +
	"database_id CHAR(36) NOT NULL," +
	"migration_id CHAR(36) NULL," +
	"revision BIGINT NULL," +
	"name VARCHAR(160) NOT NULL," +
	"`action` VARCHAR(16) NOT NULL," +
	"`sql` LONGTEXT NOT NULL," +
	"status VARCHAR(16) NOT NULL DEFAULT 'success'," +
	"executed_by CHAR(36) NOT NULL," +
	"started_at DATETIME(6) NOT NULL," +
	"finished_at DATETIME(6) NULL," +
	"reverts_execution_id CHAR(36) NULL UNIQUE," +
	"legacy_version BIGINT NULL," +
	"UNIQUE KEY platform_migration_executions_legacy (database_id, legacy_version)," +
	"KEY platform_migration_executions_database_idx (database_id, `sequence`)," +
	"CONSTRAINT platform_migration_executions_status CHECK (status = 'success')," +
	"CONSTRAINT platform_migration_executions_action CHECK (`action` IN ('apply', 'rollback', 'forward'))," +
	"CONSTRAINT platform_migration_executions_rollback CHECK ((`action` = 'rollback') = (reverts_execution_id IS NOT NULL))," +
	"CONSTRAINT platform_migration_executions_finished CHECK (finished_at IS NULL OR finished_at >= started_at)," +
	"CONSTRAINT platform_migration_executions_reverts FOREIGN KEY (reverts_execution_id) REFERENCES platform_migration_executions (id)" +
	") ENGINE=InnoDB"

func mysqlTableExists(ctx context.Context, connection historyConnection, table string) (bool, error) {
	var count int
	err := connection.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&count)
	return count > 0, err
}

// initializeMySQL menyiapkan schema di bawah lock bernama. DDL MySQL melakukan
// commit implisit, sehingga lock menggantikan transaksi pada PostgreSQL.
func (r *HistoryRepository) initializeMySQL(ctx context.Context, databaseID string) (initErr error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return historyError("open history initialization connection", err)
	}
	defer conn.Close()
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(`+mysqlHistoryInitLockName+`, 10)`).Scan(&acquired); err != nil {
		return historyError("lock history initialization", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return ErrConflict
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT RELEASE_LOCK(`+mysqlHistoryInitLockName+`)`)
	}()
	if _, err := conn.ExecContext(ctx, mysqlExecutionSchema); err != nil {
		return historyError("initialize execution history", err)
	}
	return importMySQLLegacyHistory(ctx, conn, databaseID)
}

func (r *HistoryRepository) ensureMySQLInitialized(ctx context.Context, databaseID string) error {
	exists, err := mysqlTableExists(ctx, r.db, "platform_migration_executions")
	if err != nil {
		return historyError("find execution history", err)
	}
	if !exists {
		return r.initializeMySQL(ctx, databaseID)
	}
	return importMySQLLegacyHistory(ctx, r.db, databaseID)
}

// importMySQLLegacyHistory menyalin migration sukses Milestone 1 dari tabel
// platform_migrations MySQL. Unique key mencegah impor ganda.
func importMySQLLegacyHistory(ctx context.Context, connection historyConnection, databaseID string) error {
	exists, err := mysqlTableExists(ctx, connection, "platform_migrations")
	if err != nil {
		return historyError("find legacy history", err)
	}
	if !exists {
		return nil
	}
	_, err = connection.ExecContext(ctx, "INSERT INTO platform_migration_executions "+
		"(id, database_id, name, `action`, `sql`, status, executed_by, started_at, finished_at, legacy_version) "+
		"SELECT UUID(), ?, legacy.name, 'apply', legacy.`sql`, 'success', legacy.executed_by, legacy.started_at, legacy.finished_at, legacy.version "+
		"FROM platform_migrations legacy WHERE legacy.status = 'success' AND NOT EXISTS ("+
		"SELECT 1 FROM platform_migration_executions history WHERE history.database_id = ? AND history.legacy_version = legacy.version"+
		") ORDER BY legacy.version", databaseID, databaseID)
	return historyError("import committed legacy history", err)
}

func appendMySQLExecution(ctx context.Context, tx *sql.Tx, execution Execution) (Execution, error) {
	var reverts any
	if execution.RevertsExecutionID != "" {
		reverts = execution.RevertsExecutionID
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO platform_migration_executions "+
		"(id, database_id, migration_id, revision, name, `action`, `sql`, status, executed_by, started_at, finished_at, reverts_execution_id) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, 'success', ?, ?, ?, ?)",
		execution.ID, execution.DatabaseID, execution.MigrationID, execution.Revision, execution.Name, string(execution.Action),
		execution.SQL, execution.ExecutedBy, execution.StartedAt, execution.FinishedAt, reverts)
	if err != nil {
		return Execution{}, historyError("append committed execution", err)
	}
	return getExecution(ctx, tx, database.MySQL, execution.DatabaseID, execution.ID)
}

func (r *HistoryRepository) mysqlLegacyAttempts(ctx context.Context) ([]Record, error) {
	attempts := make([]Record, 0)
	exists, err := mysqlTableExists(ctx, r.db, "platform_migrations")
	if err != nil {
		return nil, historyError("find legacy attempts", err)
	}
	if !exists {
		return attempts, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+historyColumns(database.MySQL)+`
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

// mysqlHistoryError memetakan error MySQL ke kategori aman tanpa pesan driver.
func mysqlHistoryError(err error) (error, bool) {
	var mysqlError *mysql.MySQLError
	if !errors.As(err, &mysqlError) {
		return nil, false
	}
	switch mysqlError.Number {
	case 1044, 1045, 1142, 1143, 1227:
		return targetdb.ErrPermissionDenied, true
	case 1062:
		return ErrConflict, true
	case 1452:
		return ErrNotFound, true
	case 3819:
		return ErrInvalidMigration, true
	}
	return nil, false
}
