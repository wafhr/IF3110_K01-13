package worker

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

const (
	batchSQL       = "UPDATE accounts SET balance_cents = balance_cents + 100 WHERE id BETWEEN $1 AND $2"
	scopeLookup    = "SELECT scope_end, total_rows, started_at"
	scopeMeasure   = "AS bound FROM \"accounts\" WHERE"
	lastBatchQuery = "ORDER BY batch_number DESC"
	rangeQuery     = "WITH batch AS"
	progressQuery  = "LEFT JOIN"
	batchInsert    = "INSERT INTO public.platform_migration_batches"
	scopeInsert    = "INSERT INTO public.platform_migration_batch_scopes"
	scopeComplete  = "SET status = 'completed'"
)

func batchRequest(action migration.Action) migration.ExecutionRequest {
	request := testRequest()
	request.Plan.Action, request.Plan.SQL = action, batchSQL
	request.Plan.Batch = migration.BatchSpec{Table: "accounts", KeyColumn: "id", Size: 1000}
	return request
}

// batchTarget answers the checkpoint and key-range queries of one batch
// transaction. A nil last batch means nothing was committed yet.
func batchTarget(scope []driver.Value, last []driver.Value, next []driver.Value) *stubTarget {
	target := newStubTarget().onQuery(historySchemaLookup, []driver.Value{"public"}).onQuery(checkpointTablesLookup, []driver.Value{true})
	if scope == nil {
		target.onQuery(scopeLookup).onQuery(scopeMeasure, []driver.Value{int64(2500), "2500"})
	} else {
		target.onQuery(scopeLookup, scope)
	}
	if last == nil {
		target.onQuery(lastBatchQuery)
	} else {
		target.onQuery(lastBatchQuery, last)
	}
	return target.onQuery(rangeQuery, next).onQuery(progressQuery, []driver.Value{int64(1), int64(1000), int64(2500)})
}

func runOneBatch(t *testing.T, target *stubTarget, prepared *fakePrepared, request migration.ExecutionRequest) (migration.Execution, bool, []job.Progress, error) {
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
	execution, done, err := executor.runBatch(context.Background(), conn, request, store, newBatchQueries(request.Plan.Batch),
		func(progress job.Progress) { reports = append(reports, progress) })
	return execution, done, reports, err
}

func TestNewBatchQueriesQuoteIdentifiers(t *testing.T) {
	queries := newBatchQueries(migration.BatchSpec{Table: `billing.acc"ounts`, KeyColumn: "id%d", Size: 250})
	for name, query := range map[string]string{"scope": queries.scope, "first": queries.first, "next": queries.next} {
		if !strings.Contains(query, `"billing"."acc""ounts"`) || !strings.Contains(query, `"id%d"`) {
			t.Errorf("%s query does not quote identifiers: %s", name, query)
		}
		if !strings.Contains(query, "::text AS bound") {
			t.Errorf("%s query orders by the text form of the key: %s", name, query)
		}
	}
	if !strings.Contains(queries.scope, `count("id%d")`) || !strings.Contains(queries.scope, `WHERE "id%d" IS NOT NULL ORDER BY`) {
		t.Errorf("scope query counts or ends on NULL keys: %s", queries.scope)
	}
	if !strings.Contains(queries.first, "LIMIT 250") || !strings.Contains(queries.next, `"id%d" > $1 AND "id%d" <= $2`) {
		t.Fatalf("range queries = %s / %s", queries.first, queries.next)
	}
}

func TestRunBatchCommitsTheFirstRange(t *testing.T) {
	target := batchTarget(nil, nil, []driver.Value{int64(1000), "1", "1000"})
	_, done, reports, err := runOneBatch(t, target, &fakePrepared{plan: batchRequest(migration.Apply).Plan}, batchRequest(migration.Apply))
	if err != nil || done {
		t.Fatalf("runBatch = done %t, %v", done, err)
	}
	if scope := target.executed(scopeInsert); len(scope) != 1 || scope[0].args[7] != "2500" || scope[0].args[8] != int64(2500) {
		t.Fatalf("scope insert = %+v", scope)
	}
	if ranges := target.executed(rangeQuery); len(ranges) != 1 || len(ranges[0].args) != 1 || ranges[0].args[0] != "2500" {
		t.Fatalf("first range query = %+v", ranges)
	}
	if update := target.executed(batchSQL); len(update) != 1 || update[0].args[0] != "1" || update[0].args[1] != "1000" {
		t.Fatalf("batch SQL = %+v", update)
	}
	if saved := target.executed(batchInsert); len(saved) != 1 || saved[0].args[1] != int64(1) || saved[0].args[4] != int64(1000) {
		t.Fatalf("checkpoint = %+v", saved)
	}
	if target.commits != 1 || len(reports) != 1 || reports[0].CommittedRows != 1000 {
		t.Fatalf("commits = %d, reports = %+v", target.commits, reports)
	}
	order := target.order()
	if progress, commit := indexOf(order, progressQuery), indexOf(order, "COMMIT"); progress < 0 || progress > commit {
		t.Fatalf("progress was not read inside the batch transaction: %q", order)
	}
}

func TestRunBatchResumesAfterTheLastCheckpoint(t *testing.T) {
	scope := []driver.Value{"2500", int64(2500), testStart}
	target := batchTarget(scope, []driver.Value{int64(2), "1001", "2000", int64(1000)}, []driver.Value{int64(500), "2001", "2500"})
	if _, done, _, err := runOneBatch(t, target, &fakePrepared{plan: batchRequest(migration.Apply).Plan}, batchRequest(migration.Apply)); err != nil || done {
		t.Fatalf("runBatch = done %t, %v", done, err)
	}
	if target.ran(scopeInsert) || target.ran(scopeMeasure) {
		t.Fatal("a resumed execution measured its scope again")
	}
	if ranges := target.executed(rangeQuery); ranges[0].args[0] != "2000" || ranges[0].args[1] != "2500" {
		t.Fatalf("next range query arguments = %v", ranges[0].args)
	}
	if saved := target.executed(batchInsert); saved[0].args[1] != int64(3) || saved[0].args[2] != "2001" {
		t.Fatalf("checkpoint = %+v", saved)
	}
}

func TestRunBatchRecordsHistoryWhenNoRangeIsLeft(t *testing.T) {
	scope := []driver.Value{"2500", int64(2500), testStart}
	target := batchTarget(scope, []driver.Value{int64(3), "2001", "2500", int64(500)}, []driver.Value{int64(0), nil, nil})
	prepared := &fakePrepared{plan: batchRequest(migration.Apply).Plan}
	execution, done, reports, err := runOneBatch(t, target, prepared, batchRequest(migration.Apply))
	if err != nil || !done || execution.ID != testExecutionID {
		t.Fatalf("runBatch = %+v, done %t, %v", execution, done, err)
	}
	if prepared.recordedAt == nil || !prepared.recordedAt.Equal(testStart) {
		t.Fatalf("history started at %v, want the scope start", prepared.recordedAt)
	}
	if !target.ran(scopeComplete) || target.ran(batchSQL) || target.commits != 1 || len(reports) != 0 {
		t.Fatalf("finish statements = %q", target.order())
	}
}

func TestRunBatchStopsAtCommitEvidence(t *testing.T) {
	evidence := migration.Execution{ID: testExecutionID, Status: migration.Success}
	target := batchTarget(nil, nil, []driver.Value{int64(1000), "1", "1000"})
	execution, done, _, err := runOneBatch(t, target, &fakePrepared{evidence: &evidence}, batchRequest(migration.Apply))
	if err != nil || !done || execution != evidence || target.ran(rangeQuery) || target.commits != 0 {
		t.Fatalf("runBatch = %+v, done %t, %v", execution, done, err)
	}
}

func TestRunBatchRollsBackAFailedRange(t *testing.T) {
	target := batchTarget(nil, nil, []driver.Value{int64(1000), "1", "1000"})
	target.execErrors[batchSQL] = &pgconn.PgError{Code: "22012", Message: "division by zero"}
	_, done, reports, err := runOneBatch(t, target, &fakePrepared{plan: batchRequest(migration.Apply).Plan}, batchRequest(migration.Apply))
	var postgres *pgconn.PgError
	if !errors.As(err, &postgres) || done || len(reports) != 0 {
		t.Fatalf("runBatch = done %t, %v", done, err)
	}
	if target.ran(batchInsert) || target.commits != 0 || target.rollbacks != 1 {
		t.Fatalf("failed range statements = %q", target.order())
	}
}

func TestBatchRollbackWaitsForCompensation(t *testing.T) {
	target := newStubTarget()
	executor := newTestExecutor(t, target, database.PostgreSQL, &fakePreparer{})
	_, err := executor.Run(context.Background(), batchRequest(migration.Rollback), nil)
	if !errors.Is(err, ErrUnsupportedBatchAction) || target.ran("BEGIN") {
		t.Fatalf("batch rollback = %v", err)
	}
}

func indexOf(statements []string, fragment string) int {
	for i, statement := range statements {
		if strings.Contains(statement, fragment) {
			return i
		}
	}
	return -1
}
