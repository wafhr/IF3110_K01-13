package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

const (
	scopeCompleted = "completed"
	scopeReverted  = "reverted"
)

var (
	ErrCompensationUnavailable = errors.New("the migration has no approved down SQL to revert its batches")
	errExecutionFinished       = fmt.Errorf("%w: the execution finished before it could be cancelled", migration.ErrConflict)
)

// reversal describes which committed batches are reverted and with which
// approved down SQL. A rollback reverts another execution and records itself
// in the history; a cancellation reverts its own partial execution, which never
// reached the history.
type reversal struct {
	revertedID string
	statement  string
	rollback   bool
}

func rollbackReversal(plan migration.ActionPlan) reversal {
	return reversal{revertedID: plan.RevertsExecutionID, statement: plan.SQL, rollback: true}
}

func cancelReversal(request migration.ExecutionRequest) reversal {
	return reversal{revertedID: request.ExecutionID, statement: request.Plan.CompensationSQL}
}

// revertBatches runs the down SQL for one committed batch per transaction,
// from the last batch to the first, while the session lock is held. Each
// reverted batch is marked in the transaction that reverts it, so an
// interrupted compensation continues with the batches still applied.
func (e *Executor) revertBatches(ctx context.Context, conn *sql.Conn, request migration.ExecutionRequest, plan reversal, report ProgressFunc) (migration.Execution, error) {
	store, err := openCheckpoints(ctx, conn)
	if err != nil {
		return migration.Execution{}, err
	}
	for {
		execution, done, err := e.revertBatch(ctx, conn, request, store, plan, report)
		if err != nil || done {
			return execution, err
		}
	}
}

func (e *Executor) revertBatch(ctx context.Context, conn *sql.Conn, request migration.ExecutionRequest, store checkpoints,
	plan reversal, report ProgressFunc) (migration.Execution, bool, error) {
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
		if !plan.rollback {
			return migration.Execution{}, false, errExecutionFinished
		}
		return committed, true, nil
	}
	scope, err := e.reversalScope(ctx, tx, request, store, plan)
	if err != nil {
		return migration.Execution{}, false, err
	}
	batch, applied, err := store.lastApplied(ctx, tx, plan.revertedID)
	if err != nil {
		return migration.Execution{}, false, err
	}
	if !applied {
		execution, err := e.finishReversal(ctx, tx, prepared, request, store, plan, scope)
		return execution, err == nil, err
	}
	progress, err := e.revertCommittedBatch(ctx, tx, plan, store, batch)
	if err != nil {
		return migration.Execution{}, false, err
	}
	if report != nil {
		report(progress)
	}
	return migration.Execution{}, false, nil
}

// reversalScope returns the scope that dates the reversal. A rollback starts
// its own scope from the reverted execution's; a cancellation reuses its own.
func (e *Executor) reversalScope(ctx context.Context, tx *sql.Tx, request migration.ExecutionRequest, store checkpoints, plan reversal) (batchScope, error) {
	scope, found, err := store.scope(ctx, tx, request.ExecutionID)
	if err != nil || found {
		return scope, err
	}
	if !plan.rollback {
		return batchScope{}, errScopeMissing
	}
	if err := store.copyScope(ctx, tx, request.ExecutionID, plan.revertedID, e.now()); err != nil {
		return batchScope{}, err
	}
	scope, _, err = store.scope(ctx, tx, request.ExecutionID)
	return scope, err
}

// revertCommittedBatch returns the batches still applied, read inside the
// transaction that reverts this one.
func (e *Executor) revertCommittedBatch(ctx context.Context, tx *sql.Tx, plan reversal, store checkpoints, batch batchRange) (progress job.Progress, batchErr error) {
	ctx = telemetry.WithLogAttrs(ctx, slog.Int64("batch", batch.Number))
	ctx, activity := telemetry.StartActivity(ctx, "migration_batch_compensation", slog.Int64("batch", batch.Number),
		slog.Int64("batch_rows", batch.Rows), slog.String("reverted_execution_id", plan.revertedID))
	defer activity.End(&batchErr)
	if _, err := tx.ExecContext(ctx, plan.statement, batch.First, batch.Last); err != nil {
		return job.Progress{}, err
	}
	if err := store.markReverted(ctx, tx, plan.revertedID, batch.Number, e.now()); err != nil {
		return job.Progress{}, err
	}
	progress, err := store.progress(ctx, tx, plan.revertedID)
	if err != nil {
		return job.Progress{}, err
	}
	if err := tx.Commit(); err != nil {
		return job.Progress{}, commitError(err)
	}
	return progress, nil
}

// finishReversal runs once every batch is reverted. A rollback records its
// execution in the history in this transaction; a cancellation only closes
// its scope.
func (e *Executor) finishReversal(ctx context.Context, tx *sql.Tx, prepared PreparedExecution, request migration.ExecutionRequest,
	store checkpoints, plan reversal, scope batchScope) (migration.Execution, error) {
	var execution migration.Execution
	if plan.rollback {
		recorded, err := prepared.Record(ctx, scope.StartedAt)
		if err != nil {
			return migration.Execution{}, err
		}
		if err := store.finish(ctx, tx, request.ExecutionID, scopeCompleted, e.now()); err != nil {
			return migration.Execution{}, err
		}
		execution = recorded
	}
	if err := store.finish(ctx, tx, plan.revertedID, scopeReverted, e.now()); err != nil {
		return migration.Execution{}, err
	}
	if err := tx.Commit(); err != nil {
		return migration.Execution{}, commitError(err)
	}
	return execution, nil
}
