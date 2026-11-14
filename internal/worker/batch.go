package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

var ErrUnsupportedBatchAction = errors.New("rollback of batch migrations runs as batch compensation")

// ProgressFunc receives the committed progress after every batch commit.
type ProgressFunc func(job.Progress)

// batchQueries holds the statements that read key ranges of the batch table.
// Only keys are read, never whole rows, so memory use stays bounded by the
// batch size regardless of the table size. Keys are returned as text but
// always ordered by their own type; the text output is aliased so ORDER BY
// never resolves to it. Rows whose key is NULL are outside every range.
type batchQueries struct {
	scope string
	first string
	next  string
}

func newBatchQueries(spec migration.BatchSpec) batchQueries {
	table := pgx.Identifier(spec.TableIdentifier()).Sanitize()
	key := pgx.Identifier{spec.KeyColumn}.Sanitize()
	keyRange := func(filter string) string {
		return `WITH batch AS (SELECT ` + key + ` AS k FROM ` + table + ` WHERE ` + filter + ` ORDER BY ` + key +
			` LIMIT ` + strconv.Itoa(spec.Size) + `)
			SELECT count(*), (SELECT k::text AS bound FROM batch ORDER BY k LIMIT 1), (SELECT k::text AS bound FROM batch ORDER BY k DESC LIMIT 1) FROM batch`
	}
	return batchQueries{
		scope: `SELECT count(` + key + `), (SELECT ` + key + `::text AS bound FROM ` + table + ` WHERE ` + key + ` IS NOT NULL ORDER BY ` +
			key + ` DESC LIMIT 1) FROM ` + table,
		first: keyRange(key + ` <= $1`),
		next:  keyRange(key + ` > $1 AND ` + key + ` <= $2`),
	}
}

func (q batchQueries) measure(ctx context.Context, tx *sql.Tx) (batchScope, error) {
	var scope batchScope
	if err := tx.QueryRowContext(ctx, q.scope).Scan(&scope.Total, &scope.End); err != nil {
		return batchScope{}, fmt.Errorf("measure batch scope: %w", err)
	}
	return scope, nil
}

// following returns the next range after previous, bounded by the scope end.
func (q batchQueries) following(ctx context.Context, tx *sql.Tx, scope batchScope, previous *batchRange) (batchRange, error) {
	if !scope.End.Valid {
		return batchRange{}, nil
	}
	var row *sql.Row
	if previous == nil {
		row = tx.QueryRowContext(ctx, q.first, scope.End.String)
	} else {
		row = tx.QueryRowContext(ctx, q.next, previous.Last, scope.End.String)
	}
	var next batchRange
	var first, last sql.NullString
	if err := row.Scan(&next.Rows, &first, &last); err != nil {
		return batchRange{}, fmt.Errorf("select next batch: %w", err)
	}
	next.First, next.Last = first.String, last.String
	if previous != nil {
		next.Number = previous.Number + 1
	} else {
		next.Number = 1
	}
	return next, nil
}

// runBatches commits one key range per transaction while the session lock is
// held, so no other migration of the database runs between batch commits. An
// attempt resumes after the last committed batch, and the execution history is
// written in a final transaction once no range is left.
func (e *Executor) runBatches(ctx context.Context, conn *sql.Conn, request migration.ExecutionRequest, report ProgressFunc) (migration.Execution, error) {
	if request.Plan.Action == migration.Rollback {
		return migration.Execution{}, ErrUnsupportedBatchAction
	}
	store, err := openCheckpoints(ctx, conn)
	if err != nil {
		return migration.Execution{}, err
	}
	queries := newBatchQueries(request.Plan.Batch)
	for {
		execution, done, err := e.runBatch(ctx, conn, request, store, queries, report)
		if err != nil || done {
			return execution, err
		}
	}
}

func (e *Executor) runBatch(ctx context.Context, conn *sql.Conn, request migration.ExecutionRequest, store checkpoints,
	queries batchQueries, report ProgressFunc) (migration.Execution, bool, error) {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return migration.Execution{}, false, err
	}
	defer tx.Rollback()
	prepared, err := e.preparer.PrepareExecution(ctx, tx, request)
	if err != nil {
		return migration.Execution{}, false, err
	}
	if committed, ok := prepared.Committed(); ok {
		return committed, true, nil
	}
	scope, err := e.batchScope(ctx, tx, request, store, queries)
	if err != nil {
		return migration.Execution{}, false, err
	}
	last, resumed, err := store.lastBatch(ctx, tx, request.ExecutionID)
	if err != nil {
		return migration.Execution{}, false, err
	}
	var previous *batchRange
	if resumed {
		previous = &last
	}
	next, err := queries.following(ctx, tx, scope, previous)
	if err != nil {
		return migration.Execution{}, false, err
	}
	if next.Rows == 0 {
		execution, err := e.finishBatches(ctx, tx, prepared, request.ExecutionID, store, scope)
		return execution, err == nil, err
	}
	progress, err := e.commitBatch(ctx, tx, prepared.Plan().SQL, request.ExecutionID, store, next)
	if err != nil {
		return migration.Execution{}, false, err
	}
	if report != nil {
		report(progress)
	}
	return migration.Execution{}, false, nil
}

func (e *Executor) batchScope(ctx context.Context, tx *sql.Tx, request migration.ExecutionRequest, store checkpoints, queries batchQueries) (batchScope, error) {
	scope, found, err := store.scope(ctx, tx, request.ExecutionID)
	if err != nil || found {
		return scope, err
	}
	if scope, err = queries.measure(ctx, tx); err != nil {
		return batchScope{}, err
	}
	scope.StartedAt = e.now()
	return scope, store.createScope(ctx, tx, request, scope)
}

// commitBatch returns the progress read inside the batch transaction, so every
// commit is reported even if the platform or target is unreachable afterwards.
func (e *Executor) commitBatch(ctx context.Context, tx *sql.Tx, statement, executionID string, store checkpoints, batch batchRange) (progress job.Progress, batchErr error) {
	ctx = telemetry.WithLogAttrs(ctx, slog.Int64("batch", batch.Number))
	ctx, activity := telemetry.StartActivity(ctx, "migration_batch", slog.Int64("batch", batch.Number), slog.Int64("batch_rows", batch.Rows))
	defer activity.End(&batchErr)
	if _, err := tx.ExecContext(ctx, statement, batch.First, batch.Last); err != nil {
		return job.Progress{}, err
	}
	if err := store.saveBatch(ctx, tx, executionID, batch, e.now()); err != nil {
		return job.Progress{}, err
	}
	progress, err := store.progress(ctx, tx, executionID)
	if err != nil {
		return job.Progress{}, err
	}
	if err := tx.Commit(); err != nil {
		return job.Progress{}, commitError(err)
	}
	return progress, nil
}

func (e *Executor) finishBatches(ctx context.Context, tx *sql.Tx, prepared PreparedExecution, executionID string, store checkpoints, scope batchScope) (migration.Execution, error) {
	execution, err := prepared.Record(ctx, scope.StartedAt)
	if err != nil {
		return migration.Execution{}, err
	}
	if err := store.complete(ctx, tx, executionID, e.now()); err != nil {
		return migration.Execution{}, err
	}
	if err := tx.Commit(); err != nil {
		return migration.Execution{}, commitError(err)
	}
	return execution, nil
}
