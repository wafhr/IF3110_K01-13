package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

type PreparedExecution interface {
	Plan() migration.ActionPlan
	Committed() (migration.Execution, bool)
	Record(context.Context, time.Time) (migration.Execution, error)
}

// Preparer revalidates a queued request inside the locked target transaction.
type Preparer interface {
	PrepareExecution(context.Context, *sql.Tx, migration.ExecutionRequest) (PreparedExecution, error)
}

type migrationPreparer struct{ service *migration.Service }

func (p migrationPreparer) PrepareExecution(ctx context.Context, tx *sql.Tx, request migration.ExecutionRequest) (PreparedExecution, error) {
	return p.service.PrepareExecution(ctx, tx, request)
}

// MigrationPreparer adapts the migration service hooks to the executor.
func MigrationPreparer(service *migration.Service) Preparer {
	return migrationPreparer{service: service}
}

// Executor runs one attempt of a regular migration: lock the target, revalidate,
// execute the SQL, record history, and commit in a single target transaction.
type Executor struct {
	openTarget migration.TargetOpener
	preparer   Preparer
	now        func() time.Time
}

func NewExecutor(openTarget migration.TargetOpener, preparer Preparer) (*Executor, error) {
	if openTarget == nil || preparer == nil {
		return nil, errors.New("executor requires a target opener and a preparer")
	}
	return &Executor{openTarget: openTarget, preparer: preparer, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Run returns the committed execution. When the target already holds commit
// evidence for the execution ID, it returns that record without running SQL.
// Batch migrations report their committed progress through report, and the
// rollback of a batch migration reverts its batches in reverse order.
func (e *Executor) Run(ctx context.Context, request migration.ExecutionRequest, report ProgressFunc) (migration.Execution, error) {
	return e.locked(ctx, request, func(conn *sql.Conn) (migration.Execution, error) {
		plan := request.Plan
		switch {
		case plan.Batch.Enabled() && plan.Action == migration.Rollback:
			return e.revertBatches(ctx, conn, request, rollbackReversal(plan), report)
		case plan.Batch.Enabled():
			return e.runBatches(ctx, conn, request, report)
		default:
			return e.execute(ctx, conn, request)
		}
	})
}

// Compensate reverts the committed batches of a partial batch apply or
// forward with the down SQL approved in its plan, in reverse batch order.
func (e *Executor) Compensate(ctx context.Context, request migration.ExecutionRequest, report ProgressFunc) (migration.Execution, error) {
	plan := request.Plan
	if !plan.Batch.Enabled() || plan.Action == migration.Rollback || plan.CompensationSQL == "" {
		return migration.Execution{}, ErrCompensationUnavailable
	}
	return e.locked(ctx, request, func(conn *sql.Conn) (migration.Execution, error) {
		return e.revertBatches(ctx, conn, request, cancelReversal(request), report)
	})
}

// locked runs work on a dedicated target connection that holds the database
// migration lock for the whole call.
func (e *Executor) locked(ctx context.Context, request migration.ExecutionRequest, work func(*sql.Conn) (migration.Execution, error)) (migration.Execution, error) {
	db, dbms, err := e.openTarget(ctx, request.Plan.DatabaseID)
	if err != nil {
		return migration.Execution{}, err
	}
	defer db.Close()
	if dbms != database.PostgreSQL {
		return migration.Execution{}, ErrUnsupportedTarget
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return migration.Execution{}, err
	}
	defer conn.Close()
	unlock, err := lockDatabase(ctx, conn, request.Plan.DatabaseID)
	if err != nil {
		return migration.Execution{}, err
	}
	defer unlock()
	return work(conn)
}

func (e *Executor) execute(ctx context.Context, conn *sql.Conn, request migration.ExecutionRequest) (migration.Execution, error) {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return migration.Execution{}, err
	}
	defer tx.Rollback()
	prepared, err := e.preparer.PrepareExecution(ctx, tx, request)
	if err != nil {
		return migration.Execution{}, err
	}
	if committed, ok := prepared.Committed(); ok {
		return committed, nil
	}
	startedAt := e.now()
	if _, err := tx.ExecContext(ctx, prepared.Plan().SQL); err != nil {
		return migration.Execution{}, err
	}
	execution, err := prepared.Record(ctx, startedAt)
	if err != nil {
		return migration.Execution{}, err
	}
	if err := tx.Commit(); err != nil {
		return migration.Execution{}, commitError(err)
	}
	return execution, nil
}

// commitError separates a commit the server rejected, which rolled back, from
// a commit whose response was lost and may have been applied.
func commitError(err error) error {
	var postgres *pgconn.PgError
	if errors.As(err, &postgres) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrCommitUncertain, err)
}
