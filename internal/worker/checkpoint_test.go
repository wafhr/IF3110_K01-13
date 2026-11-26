package worker

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/job"
)

const (
	historySchemaLookup    = "to_regclass('platform_migration_executions')"
	checkpointTablesLookup = "to_regclass($1) IS NOT NULL"
)

func openTestCheckpoints(t *testing.T, target *stubTarget) (checkpoints, error) {
	t.Helper()
	conn, err := target.open(t).Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return openCheckpoints(context.Background(), conn)
}

func TestOpenCheckpointsUsesTheHistorySchema(t *testing.T) {
	target := newStubTarget().onQuery(historySchemaLookup, []driver.Value{`"Billing"`}).onQuery(checkpointTablesLookup, []driver.Value{false})
	store, err := openTestCheckpoints(t, target)
	if err != nil {
		t.Fatal(err)
	}
	if store.scopes != `"Billing".platform_migration_batch_scopes` || store.batches != `"Billing".platform_migration_batches` {
		t.Fatalf("checkpoint tables = %+v", store)
	}
	ddl := target.executed("CREATE TABLE IF NOT EXISTS")
	if len(ddl) != 1 || strings.Contains(ddl[0].query, "{{schema}}") ||
		!strings.Contains(ddl[0].query, `REFERENCES "Billing".platform_migration_batch_scopes(execution_id)`) {
		t.Fatalf("checkpoint DDL = %+v", ddl)
	}
}

func TestOpenCheckpointsRequiresExecutionHistory(t *testing.T) {
	for name, rows := range map[string][][]driver.Value{"missing table": {{nil}}, "no answer": nil} {
		target := newStubTarget().onQuery(historySchemaLookup, rows...)
		if _, err := openTestCheckpoints(t, target); !errors.Is(err, errHistoryMissing) {
			t.Errorf("%s: error = %v", name, err)
		}
		if target.ran("CREATE TABLE") {
			t.Errorf("%s: checkpoint tables were created without history", name)
		}
	}
}

func TestCheckpointReads(t *testing.T) {
	store := checkpoints{scopes: "public.platform_migration_batch_scopes", batches: "public.platform_migration_batches"}
	db := newStubTarget().
		onQuery("SELECT scope_end, total_rows, started_at", []driver.Value{"5000", int64(5000), testStart}).
		onQuery("ORDER BY batch_number DESC", []driver.Value{int64(2), "1001", "2000", int64(1000)}).
		onQuery("LEFT JOIN", []driver.Value{int64(2), int64(2000), int64(5000)}).
		open(t)
	ctx := context.Background()

	scope, found, err := store.scope(ctx, db, testExecutionID)
	if err != nil || !found || scope.End.String != "5000" || scope.Total != 5000 || !scope.StartedAt.Equal(testStart) {
		t.Fatalf("scope = %+v, %t, %v", scope, found, err)
	}
	last, found, err := store.lastBatch(ctx, db, testExecutionID)
	if err != nil || !found || last != (batchRange{Number: 2, First: "1001", Last: "2000", Rows: 1000}) {
		t.Fatalf("last batch = %+v, %t, %v", last, found, err)
	}
	progress, err := store.progress(ctx, db, testExecutionID)
	if err != nil || progress != (job.Progress{CommittedBatches: 2, CommittedRows: 2000, TotalRows: 5000}) {
		t.Fatalf("progress = %+v, %v", progress, err)
	}

	empty := newStubTarget().onQuery("SELECT scope_end").onQuery("ORDER BY batch_number DESC").open(t)
	if _, found, err := store.scope(ctx, empty, testExecutionID); found || err != nil {
		t.Fatalf("missing scope = %t, %v", found, err)
	}
	if _, found, err := store.lastBatch(ctx, empty, testExecutionID); found || err != nil {
		t.Fatalf("missing batch = %t, %v", found, err)
	}
}

func TestOpenCheckpointsSkipsDDLWhenTablesExist(t *testing.T) {
	target := newStubTarget().onQuery(historySchemaLookup, []driver.Value{"public"}).onQuery(checkpointTablesLookup, []driver.Value{true})
	if _, err := openTestCheckpoints(t, target); err != nil {
		t.Fatal(err)
	}
	lookup := target.executed(checkpointTablesLookup)
	if len(lookup) != 1 || lookup[0].args[0] != "public.platform_migration_batch_scopes" || lookup[0].args[1] != "public.platform_migration_batches" {
		t.Fatalf("checkpoint lookup = %+v", lookup)
	}
	if target.ran("CREATE TABLE") {
		t.Fatal("existing checkpoint tables were created again")
	}
}
