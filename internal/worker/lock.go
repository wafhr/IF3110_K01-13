package worker

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

const unlockTimeout = 5 * time.Second

// lockDatabase holds the session-level advisory lock that serializes every
// migration of one physical target database across all workers. The lock
// belongs to conn, so it is released by unlock or, if the worker dies, when
// PostgreSQL ends the session. MySQL uses a named GET_LOCK with the same
// session lifetime.
func lockDatabase(ctx context.Context, conn *sql.Conn, dbms database.DBMS, databaseID string) (unlock func(), lockErr error) {
	key, err := migration.MigrationLockKey(databaseID)
	if err != nil {
		return nil, err
	}
	lockCtx, activity := telemetry.StartActivity(ctx, "migration_lock_acquire", slog.String("database_id", databaseID))
	defer activity.End(&lockErr)
	if dbms == database.MySQL {
		return lockMySQL(ctx, lockCtx, conn, databaseID)
	}
	if _, err := conn.ExecContext(lockCtx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		return nil, err
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		var releaseErr error
		releaseCtx, activity := telemetry.StartActivity(releaseCtx, "migration_lock_release", slog.String("database_id", databaseID))
		defer activity.End(&releaseErr)
		_, releaseErr = conn.ExecContext(releaseCtx, `SELECT pg_advisory_unlock($1)`, key)
	}, nil
}

// lockMySQL waits without a server timeout, like pg_advisory_lock. Canceling
// ctx closes the connection, which also releases a lock the session holds.
func lockMySQL(ctx, lockCtx context.Context, conn *sql.Conn, databaseID string) (func(), error) {
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(lockCtx, `SELECT GET_LOCK(`+migration.MySQLMigrationLockName+`, -1)`).Scan(&acquired); err != nil {
		return nil, err
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return nil, migration.ErrConflict
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		var releaseErr error
		releaseCtx, activity := telemetry.StartActivity(releaseCtx, "migration_lock_release", slog.String("database_id", databaseID))
		defer activity.End(&releaseErr)
		_, releaseErr = conn.ExecContext(releaseCtx, `SELECT RELEASE_LOCK(`+migration.MySQLMigrationLockName+`)`)
	}, nil
}
