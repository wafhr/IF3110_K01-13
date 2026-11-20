package worker

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

// fakeDialectPreparer records the DBMS the executor passes for non-PostgreSQL targets.
type fakeDialectPreparer struct {
	fakePreparer
	dbms database.DBMS
}

func (p *fakeDialectPreparer) PrepareExecutionOn(ctx context.Context, tx *sql.Tx, dbms database.DBMS, request migration.ExecutionRequest) (PreparedExecution, error) {
	p.dbms = dbms
	return p.PrepareExecution(ctx, tx, request)
}

func mysqlLockedTarget() *stubTarget {
	return newStubTarget().onQuery("GET_LOCK", []driver.Value{int64(1)})
}

func TestExecutorRunsMySQLWithNamedLock(t *testing.T) {
	target := mysqlLockedTarget()
	prepared := &fakePrepared{plan: testRequest().Plan}
	preparer := &fakeDialectPreparer{fakePreparer: fakePreparer{prepared: prepared}}
	execution, err := newTestExecutor(t, target, database.MySQL, preparer).Run(context.Background(), testRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if execution.ID != testExecutionID || preparer.dbms != database.MySQL || prepared.recordedAt == nil {
		t.Fatalf("execution = %+v, dbms = %q", execution, preparer.dbms)
	}
	want := []string{"SELECT GET_LOCK(" + migration.MySQLMigrationLockName + ", -1)", "BEGIN", migrationSQL, "COMMIT",
		"SELECT RELEASE_LOCK(" + migration.MySQLMigrationLockName + ")"}
	if got := target.order(); !slices.Equal(got, want) {
		t.Fatalf("statements = %q, want %q", got, want)
	}
	if target.ran("pg_advisory") {
		t.Fatal("MySQL target used a PostgreSQL lock")
	}
}

func TestExecutorRollsBackFailedMySQLAttempt(t *testing.T) {
	target := mysqlLockedTarget()
	target.execErrors[migrationSQL] = &mysql.MySQLError{Number: 1146, Message: "Table 'shop.missing' doesn't exist"}
	prepared := &fakePrepared{plan: testRequest().Plan}
	_, err := newTestExecutor(t, target, database.MySQL, &fakeDialectPreparer{fakePreparer: fakePreparer{prepared: prepared}}).
		Run(context.Background(), testRequest(), nil)
	if err == nil || classify(err) != permanentFailure {
		t.Fatalf("failed MySQL attempt returned %v", err)
	}
	if target.commits > 0 || prepared.recordedAt != nil || !target.ran("ROLLBACK") || !target.ran("RELEASE_LOCK") {
		t.Fatalf("statements = %q", target.order())
	}
}

func TestExecutorRejectsUnsupportedMySQLJobs(t *testing.T) {
	batch := testRequest()
	batch.Plan.Batch = migration.BatchSpec{Table: "accounts", KeyColumn: "id", Size: 100}
	tests := map[string]struct {
		preparer Preparer
		request  migration.ExecutionRequest
	}{
		"preparer without MySQL support": {preparer: &fakePreparer{}, request: testRequest()},
		"batch migration":                {preparer: &fakeDialectPreparer{}, request: batch},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			target := mysqlLockedTarget()
			_, err := newTestExecutor(t, target, database.MySQL, test.preparer).Run(context.Background(), test.request, nil)
			if !errors.Is(err, ErrUnsupportedTarget) || target.ran("GET_LOCK") {
				t.Fatalf("returned %v, statements = %q", err, target.order())
			}
		})
	}
	target := mysqlLockedTarget()
	if _, err := newTestExecutor(t, target, database.DBMS("oracle"), &fakeDialectPreparer{}).Run(context.Background(), testRequest(), nil); !errors.Is(err, ErrUnsupportedTarget) {
		t.Fatalf("unknown DBMS returned %v", err)
	}
}

func TestLockDatabaseMySQL(t *testing.T) {
	target := mysqlLockedTarget()
	conn, err := target.open(t).Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	unlock, err := lockDatabase(context.Background(), conn, database.MySQL, testDatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if !target.ran("GET_LOCK") || !target.ran("RELEASE_LOCK") {
		t.Fatalf("statements = %q", target.order())
	}

	busy := newStubTarget().onQuery("GET_LOCK", []driver.Value{int64(0)})
	busyConn, err := busy.open(t).Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer busyConn.Close()
	if _, err := lockDatabase(context.Background(), busyConn, database.MySQL, testDatabaseID); !errors.Is(err, migration.ErrConflict) {
		t.Fatalf("unavailable lock returned %v", err)
	}
}

func TestMySQLErrorClassificationAndReason(t *testing.T) {
	tests := []struct {
		number uint16
		want   failureKind
	}{
		{number: 1064, want: permanentFailure},
		{number: 1146, want: permanentFailure},
		{number: 1205, want: transientFailure},
		{number: 1213, want: transientFailure},
		{number: 1040, want: transientFailure},
	}
	for _, test := range tests {
		err := fmt.Errorf("execute: %w", &mysql.MySQLError{Number: test.number, Message: "server says no"})
		if got := classify(err); got != test.want {
			t.Errorf("classify(%d) = %d, want %d", test.number, got, test.want)
		}
	}
	reason := failureReason(&mysql.MySQLError{Number: 1146, Message: "Table 'shop.missing' doesn't exist"})
	if reason != "MySQL error 1146: Table 'shop.missing' doesn't exist" {
		t.Fatalf("reason = %q", reason)
	}
	if err := commitError(&mysql.MySQLError{Number: 1180, Message: "commit failed"}); errors.Is(err, ErrCommitUncertain) {
		t.Fatal("a rejected MySQL commit was treated as uncertain")
	}
}
