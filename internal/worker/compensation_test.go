package worker

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

const (
	compensationSQL  = "UPDATE accounts SET balance_cents = balance_cents - 100 WHERE id BETWEEN $1 AND $2"
	revertedID       = "20000000-0000-4000-8000-000000000009"
	lastAppliedQuery = "reverted_at IS NULL ORDER BY batch_number DESC"
	batchRevert      = "SET reverted_at = $3"
	scopeCopy        = "SELECT $1, database_id"
)

func cancelRequest() migration.ExecutionRequest {
	request := batchRequest(migration.Apply)
	request.Plan.CompensationSQL = compensationSQL
	return request
}

func rollbackRequest() migration.ExecutionRequest {
	request := batchRequest(migration.Rollback)
	request.Plan.SQL, request.Plan.RevertsExecutionID = compensationSQL, revertedID
	return request
}

// reversalTarget answers one compensation transaction; a nil applied batch
// means every batch is already reverted.
func reversalTarget(applied []driver.Value) *stubTarget {
	target := newStubTarget().
		onQuery(historySchemaLookup, []driver.Value{"public"}).
		onQuery(checkpointTablesLookup, []driver.Value{true}).
		onQuery(scopeLookup, []driver.Value{"2500", int64(2500), testStart}).
		onQuery(progressQuery, []driver.Value{int64(2), int64(2000), int64(2500)})
	if applied == nil {
		return target.onQuery(lastAppliedQuery)
	}
	return target.onQuery(lastAppliedQuery, applied)
}

func runOneReversal(t *testing.T, target *stubTarget, prepared *fakePrepared, request migration.ExecutionRequest, plan reversal) (migration.Execution, bool, []job.Progress, error) {
	t.Helper()
	executor := newTestExecutor(t, target, database.PostgreSQL, &fakePreparer{prepared: prepared})
	conn, err := target.open(t).Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	store, err := openCheckpoints(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	var reports []job.Progress
	execution, done, err := executor.revertBatch(context.Background(), conn, request, store, plan,
		func(progress job.Progress) { reports = append(reports, progress) })
	return execution, done, reports, err
}

func TestCompensateRequiresApprovedDownSQL(t *testing.T) {
	irreversible := cancelRequest()
	irreversible.Plan.CompensationSQL = ""
	for name, request := range map[string]migration.ExecutionRequest{
		"regular migration": testRequest(),
		"batch rollback":    rollbackRequest(),
		"irreversible":      irreversible,
	} {
		target := newStubTarget()
		_, err := newTestExecutor(t, target, database.PostgreSQL, &fakePreparer{}).Compensate(context.Background(), request, nil)
		if !errors.Is(err, ErrCompensationUnavailable) || target.ran("pg_advisory_lock") {
			t.Errorf("%s: compensate = %v", name, err)
		}
	}
	if classify(ErrCompensationUnavailable) != permanentFailure {
		t.Fatal("missing down SQL must not be retried")
	}
}

func TestCancellationRevertsTheLastAppliedBatch(t *testing.T) {
	target := reversalTarget([]driver.Value{int64(3), "2001", "2500", int64(500)})
	request := cancelRequest()
	_, done, reports, err := runOneReversal(t, target, &fakePrepared{plan: request.Plan}, request, cancelReversal(request))
	if err != nil || done {
		t.Fatalf("revertBatch = done %t, %v", done, err)
	}
	if lookup := target.executed(lastAppliedQuery); lookup[0].args[0] != testExecutionID {
		t.Fatalf("cancellation reverted another execution: %v", lookup[0].args)
	}
	if down := target.executed(compensationSQL); len(down) != 1 || down[0].args[0] != "2001" || down[0].args[1] != "2500" {
		t.Fatalf("down SQL = %+v", down)
	}
	if marked := target.executed(batchRevert); len(marked) != 1 || marked[0].args[0] != testExecutionID || marked[0].args[1] != int64(3) {
		t.Fatalf("reverted checkpoint = %+v", marked)
	}
	if target.commits != 1 || len(reports) != 1 || reports[0].CommittedBatches != 2 {
		t.Fatalf("commits = %d, reports = %+v", target.commits, reports)
	}
}

func TestCancellationFinishesWithoutHistory(t *testing.T) {
	target := reversalTarget(nil)
	request := cancelRequest()
	prepared := &fakePrepared{plan: request.Plan}
	execution, done, _, err := runOneReversal(t, target, prepared, request, cancelReversal(request))
	if err != nil || !done || execution.ID != "" || prepared.recordedAt != nil {
		t.Fatalf("cancellation = %+v, done %t, %v", execution, done, err)
	}
	finished := target.executed(scopeFinish)
	if len(finished) != 1 || finished[0].args[0] != testExecutionID || finished[0].args[1] != scopeReverted || target.commits != 1 {
		t.Fatalf("finish statements = %+v", finished)
	}
}

func TestRollbackFinishesWithHistory(t *testing.T) {
	target := reversalTarget(nil)
	request := rollbackRequest()
	prepared := &fakePrepared{plan: request.Plan}
	execution, done, _, err := runOneReversal(t, target, prepared, request, rollbackReversal(request.Plan))
	if err != nil || !done || execution.ID != testExecutionID {
		t.Fatalf("rollback = %+v, done %t, %v", execution, done, err)
	}
	if prepared.recordedAt == nil || !prepared.recordedAt.Equal(testStart) {
		t.Fatalf("rollback history started at %v", prepared.recordedAt)
	}
	finished := target.executed(scopeFinish)
	if len(finished) != 2 || finished[0].args[0] != testExecutionID || finished[0].args[1] != scopeCompleted ||
		finished[1].args[0] != revertedID || finished[1].args[1] != scopeReverted {
		t.Fatalf("finish statements = %+v", finished)
	}
}

func TestRollbackStartsItsOwnScope(t *testing.T) {
	target := newStubTarget().
		onQuery(historySchemaLookup, []driver.Value{"public"}).
		onQuery(checkpointTablesLookup, []driver.Value{true}).
		onQueryOnce(scopeLookup).
		onQuery(scopeLookup, []driver.Value{"2500", int64(2500), testStart}).
		onQuery(lastAppliedQuery, []driver.Value{int64(3), "2001", "2500", int64(500)}).
		onQuery(progressQuery, []driver.Value{int64(2), int64(2000), int64(2500)})
	request := rollbackRequest()
	if _, _, _, err := runOneReversal(t, target, &fakePrepared{plan: request.Plan}, request, rollbackReversal(request.Plan)); err != nil {
		t.Fatal(err)
	}
	copied := target.executed(scopeCopy)
	if len(copied) != 1 || copied[0].args[0] != testExecutionID || copied[0].args[2] != revertedID {
		t.Fatalf("scope copy = %+v", copied)
	}
	if marked := target.executed(batchRevert); marked[0].args[0] != revertedID {
		t.Fatalf("rollback reverted batches of %v", marked[0].args[0])
	}
}

func TestCancellationAfterCompletionIsRejected(t *testing.T) {
	evidence := migration.Execution{ID: testExecutionID, Status: migration.Success}
	request := cancelRequest()
	target := reversalTarget([]driver.Value{int64(3), "2001", "2500", int64(500)})
	_, done, _, err := runOneReversal(t, target, &fakePrepared{evidence: &evidence}, request, cancelReversal(request))
	if !errors.Is(err, errExecutionFinished) || done || target.ran(compensationSQL) {
		t.Fatalf("cancellation of a finished execution = done %t, %v", done, err)
	}
	if classify(err) != permanentFailure {
		t.Fatal("a finished execution must not be cancelled on retry")
	}
	rollback := rollbackRequest()
	execution, done, _, err := runOneReversal(t, reversalTarget(nil), &fakePrepared{evidence: &evidence}, rollback, rollbackReversal(rollback.Plan))
	if err != nil || !done || execution != evidence {
		t.Fatalf("recorded rollback = %+v, done %t, %v", execution, done, err)
	}
}

func TestFailedCompensationKeepsTheBatchApplied(t *testing.T) {
	target := reversalTarget([]driver.Value{int64(3), "2001", "2500", int64(500)})
	target.execErrors[compensationSQL] = &pgconn.PgError{Code: "23514", Message: "check constraint violated"}
	request := cancelRequest()
	_, done, reports, err := runOneReversal(t, target, &fakePrepared{plan: request.Plan}, request, cancelReversal(request))
	var postgres *pgconn.PgError
	if !errors.As(err, &postgres) || done || len(reports) != 0 {
		t.Fatalf("failed compensation = done %t, %v", done, err)
	}
	if target.ran(batchRevert) || target.commits != 0 || target.rollbacks != 1 {
		t.Fatalf("statements = %q", target.order())
	}
}
