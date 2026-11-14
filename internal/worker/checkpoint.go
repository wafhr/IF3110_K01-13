package worker

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

//go:embed checkpoint.sql
var checkpointSchema string

var errHistoryMissing = errors.New("execution history is not initialized on the target")

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// batchScope is fixed when a batch execution starts: keys above End, inserted
// later, are never processed, and Total is the row count at that moment.
type batchScope struct {
	End       sql.NullString
	Total     int64
	StartedAt time.Time
}

type batchRange struct {
	Number int64
	First  string
	Last   string
	Rows   int64
}

// checkpoints stores batch scopes and committed batches in the schema of the
// target's execution history. Each batch row is written in the transaction of
// the batch it describes, so it is the commit evidence of that batch.
type checkpoints struct {
	scopes  string
	batches string
}

func openCheckpoints(ctx context.Context, conn *sql.Conn) (checkpoints, error) {
	var schema sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT pg_catalog.quote_ident(n.nspname)
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = pg_catalog.to_regclass('platform_migration_executions')`).Scan(&schema); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return checkpoints{}, fmt.Errorf("find execution history schema: %w", err)
	}
	if !schema.Valid {
		return checkpoints{}, errHistoryMissing
	}
	store := checkpoints{
		scopes:  schema.String + ".platform_migration_batch_scopes",
		batches: schema.String + ".platform_migration_batches",
	}
	// Creating the tables needs CREATE on the schema even when they exist, so
	// they are created only once and later attempts need only data privileges.
	var ready bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_catalog.to_regclass($1) IS NOT NULL AND pg_catalog.to_regclass($2) IS NOT NULL`,
		store.scopes, store.batches).Scan(&ready); err != nil {
		return checkpoints{}, fmt.Errorf("find batch checkpoints: %w", err)
	}
	if ready {
		return store, nil
	}
	if _, err := conn.ExecContext(ctx, strings.ReplaceAll(checkpointSchema, "{{schema}}", schema.String)); err != nil {
		return checkpoints{}, fmt.Errorf("initialize batch checkpoints: %w", err)
	}
	return store, nil
}

func (c checkpoints) scope(ctx context.Context, q querier, executionID string) (batchScope, bool, error) {
	var scope batchScope
	err := q.QueryRowContext(ctx, `SELECT scope_end, total_rows, started_at FROM `+c.scopes+` WHERE execution_id = $1`, executionID).
		Scan(&scope.End, &scope.Total, &scope.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return batchScope{}, false, nil
	}
	if err != nil {
		return batchScope{}, false, fmt.Errorf("load batch scope: %w", err)
	}
	return scope, true, nil
}

func (c checkpoints) createScope(ctx context.Context, q querier, request migration.ExecutionRequest, scope batchScope) error {
	plan := request.Plan
	_, err := q.ExecContext(ctx, `INSERT INTO `+c.scopes+` (execution_id, database_id, migration_id, revision, batch_table,
		batch_key, batch_size, scope_end, total_rows, started_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		request.ExecutionID, plan.DatabaseID, plan.MigrationID, plan.Revision, plan.Batch.Table, plan.Batch.KeyColumn,
		plan.Batch.Size, scope.End, scope.Total, scope.StartedAt)
	if err != nil {
		return fmt.Errorf("save batch scope: %w", err)
	}
	return nil
}

func (c checkpoints) lastBatch(ctx context.Context, q querier, executionID string) (batchRange, bool, error) {
	var last batchRange
	err := q.QueryRowContext(ctx, `SELECT batch_number, first_key, last_key, row_count FROM `+c.batches+`
		WHERE execution_id = $1 ORDER BY batch_number DESC LIMIT 1`, executionID).Scan(&last.Number, &last.First, &last.Last, &last.Rows)
	if errors.Is(err, sql.ErrNoRows) {
		return batchRange{}, false, nil
	}
	if err != nil {
		return batchRange{}, false, fmt.Errorf("load last batch: %w", err)
	}
	return last, true, nil
}

func (c checkpoints) saveBatch(ctx context.Context, q querier, executionID string, batch batchRange, committedAt time.Time) error {
	if _, err := q.ExecContext(ctx, `INSERT INTO `+c.batches+` (execution_id, batch_number, first_key, last_key, row_count, committed_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, executionID, batch.Number, batch.First, batch.Last, batch.Rows, committedAt); err != nil {
		return fmt.Errorf("save batch checkpoint: %w", err)
	}
	return nil
}

func (c checkpoints) complete(ctx context.Context, q querier, executionID string, completedAt time.Time) error {
	if _, err := q.ExecContext(ctx, `UPDATE `+c.scopes+` SET status = 'completed', completed_at = $2 WHERE execution_id = $1`, executionID, completedAt); err != nil {
		return fmt.Errorf("complete batch scope: %w", err)
	}
	return nil
}

func (c checkpoints) progress(ctx context.Context, q querier, executionID string) (job.Progress, error) {
	var progress job.Progress
	err := q.QueryRowContext(ctx, `SELECT count(b.batch_number), COALESCE(sum(b.row_count), 0), s.total_rows
		FROM `+c.scopes+` s LEFT JOIN `+c.batches+` b ON b.execution_id = s.execution_id
		WHERE s.execution_id = $1 GROUP BY s.total_rows`, executionID).
		Scan(&progress.CommittedBatches, &progress.CommittedRows, &progress.TotalRows)
	if err != nil {
		return job.Progress{}, fmt.Errorf("load batch progress: %w", err)
	}
	return progress, nil
}
