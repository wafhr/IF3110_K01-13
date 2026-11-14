package worker

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

const unlockTimeout = 5 * time.Second

// lockDatabase holds the session-level advisory lock that serializes every
// migration of one physical target database across all workers. The lock
// belongs to conn, so it is released by unlock or, if the worker dies, when
// PostgreSQL ends the session.
func lockDatabase(ctx context.Context, conn *sql.Conn, databaseID string) (unlock func(), lockErr error) {
	key, err := migration.MigrationLockKey(databaseID)
	if err != nil {
		return nil, err
	}
	lockCtx, activity := telemetry.StartActivity(ctx, "migration_lock_acquire", slog.String("database_id", databaseID))
	defer activity.End(&lockErr)
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
