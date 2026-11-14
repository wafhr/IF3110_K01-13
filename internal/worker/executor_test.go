package worker

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

const (
	testDatabaseID  = "20000000-0000-4000-8000-000000000001"
	testExecutionID = "20000000-0000-4000-8000-000000000002"
	migrationSQL    = "ALTER TABLE accounts ADD COLUMN nickname TEXT"
)

var testStart = time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

func testRequest() migration.ExecutionRequest {
	return migration.ExecutionRequest{
		ExecutionID: testExecutionID,
		Plan:        migration.ActionPlan{DatabaseID: testDatabaseID, Action: migration.Apply, SQL: migrationSQL},
	}
}

type fakePrepared struct {
	plan       migration.ActionPlan
	evidence   *migration.Execution
	recordErr  error
	recordedAt *time.Time
}

func (p *fakePrepared) Plan() migration.ActionPlan { return p.plan }

func (p *fakePrepared) Committed() (migration.Execution, bool) {
	if p.evidence == nil {
		return migration.Execution{}, false
	}
	return *p.evidence, true
}

func (p *fakePrepared) Record(_ context.Context, startedAt time.Time) (migration.Execution, error) {
	p.recordedAt = &startedAt
	if p.recordErr != nil {
		return migration.Execution{}, p.recordErr
	}
	return migration.Execution{ID: testExecutionID, Status: migration.Success, StartedAt: startedAt}, nil
}

type fakePreparer struct {
	prepared *fakePrepared
	err      error
	tx       *sql.Tx
}

func (p *fakePreparer) PrepareExecution(_ context.Context, tx *sql.Tx, _ migration.ExecutionRequest) (PreparedExecution, error) {
	p.tx = tx
	if p.err != nil {
		return nil, p.err
	}
	return p.prepared, nil
}

func newTestExecutor(t *testing.T, target *stubTarget, dbms database.DBMS, preparer Preparer) *Executor {
	t.Helper()
	executor, err := NewExecutor(func(context.Context, string) (*sql.DB, database.DBMS, error) {
		return target.open(t), dbms, nil
	}, preparer)
	if err != nil {
		t.Fatal(err)
	}
	executor.now = func() time.Time { return testStart }
	return executor
}

func TestNewExecutorRequiresDependencies(t *testing.T) {
	if _, err := NewExecutor(nil, &fakePreparer{}); err == nil {
		t.Fatal("executor accepted a nil target opener")
	}
	if _, err := NewExecutor(func(context.Context, string) (*sql.DB, database.DBMS, error) { return nil, "", nil }, nil); err == nil {
		t.Fatal("executor accepted a nil preparer")
	}
}

func TestExecutorCommitsSQLAndHistoryTogether(t *testing.T) {
	target := newStubTarget()
	prepared := &fakePrepared{plan: testRequest().Plan}
	execution, err := newTestExecutor(t, target, database.PostgreSQL, &fakePreparer{prepared: prepared}).Run(context.Background(), testRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if execution.ID != testExecutionID || prepared.recordedAt == nil || !prepared.recordedAt.Equal(testStart) {
		t.Fatalf("execution = %+v, recorded at %v", execution, prepared.recordedAt)
	}
	if target.isolation != driver.IsolationLevel(sql.LevelReadCommitted) || target.commits == 0 {
		t.Fatalf("isolation = %d, commits = %d", target.isolation, target.commits)
	}
	want := []string{"SELECT pg_advisory_lock($1)", "BEGIN", migrationSQL, "COMMIT", "SELECT pg_advisory_unlock($1)"}
	if got := target.order(); !slices.Equal(got, want) {
		t.Fatalf("statements = %q, want %q", got, want)
	}
}

func TestExecutorReturnsCommitEvidenceWithoutRunningSQL(t *testing.T) {
	target := newStubTarget()
	evidence := migration.Execution{ID: testExecutionID, Status: migration.Success}
	prepared := &fakePrepared{plan: testRequest().Plan, evidence: &evidence}
	execution, err := newTestExecutor(t, target, database.PostgreSQL, &fakePreparer{prepared: prepared}).Run(context.Background(), testRequest(), nil)
	if err != nil || execution != evidence {
		t.Fatalf("execution = %+v, %v", execution, err)
	}
	if target.ran(migrationSQL) || prepared.recordedAt != nil || target.commits > 0 {
		t.Fatal("a committed execution ran again")
	}
	if !target.ran("pg_advisory_unlock") {
		t.Fatal("lock was not released")
	}
}

func TestExecutorRollsBackFailedAttempts(t *testing.T) {
	syntaxError := &pgconn.PgError{Code: "42601", Message: "syntax error"}
	tests := []struct {
		name        string
		prepareErr  error
		sqlErr      error
		recordErr   error
		wantErr     error
		wantRecord  bool
		wantSQLRuns bool
	}{
		{name: "revalidation rejects the plan", prepareErr: migration.ErrConflict, wantErr: migration.ErrConflict},
		{name: "migration SQL fails", sqlErr: syntaxError, wantErr: syntaxError, wantSQLRuns: true},
		{name: "history cannot be recorded", recordErr: migration.ErrStorage, wantErr: migration.ErrStorage, wantSQLRuns: true, wantRecord: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := newStubTarget()
			if test.sqlErr != nil {
				target.execErrors[migrationSQL] = test.sqlErr
			}
			prepared := &fakePrepared{plan: testRequest().Plan, recordErr: test.recordErr}
			_, err := newTestExecutor(t, target, database.PostgreSQL, &fakePreparer{prepared: prepared, err: test.prepareErr}).Run(context.Background(), testRequest(), nil)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if target.commits > 0 || target.rollbacks == 0 || !target.ran("pg_advisory_unlock") {
				t.Fatalf("commits = %d, rollbacks = %d", target.commits, target.rollbacks)
			}
			if target.ran(migrationSQL) != test.wantSQLRuns || (prepared.recordedAt != nil) != test.wantRecord {
				t.Fatal("attempt continued after a failed step")
			}
		})
	}
}

func TestExecutorClassifiesCommitFailures(t *testing.T) {
	tests := []struct {
		name          string
		commitErr     error
		wantUncertain bool
	}{
		{name: "server rejected the commit", commitErr: &pgconn.PgError{Code: "40001", Message: "could not serialize access"}},
		{name: "commit response was lost", commitErr: errors.New("unexpected EOF"), wantUncertain: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := newStubTarget()
			target.commitErr = test.commitErr
			_, err := newTestExecutor(t, target, database.PostgreSQL, &fakePreparer{prepared: &fakePrepared{plan: testRequest().Plan}}).Run(context.Background(), testRequest(), nil)
			if err == nil || errors.Is(err, ErrCommitUncertain) != test.wantUncertain {
				t.Fatalf("commit error = %v, want uncertain %t", err, test.wantUncertain)
			}
		})
	}
}

func TestExecutorRejectsUnsupportedOrUnreachableTargets(t *testing.T) {
	target := newStubTarget()
	_, err := newTestExecutor(t, target, database.MySQL, &fakePreparer{}).Run(context.Background(), testRequest(), nil)
	if !errors.Is(err, ErrUnsupportedTarget) || target.ran("pg_advisory_lock") {
		t.Fatalf("MySQL target returned %v", err)
	}
	unreachable, err := NewExecutor(func(context.Context, string) (*sql.DB, database.DBMS, error) {
		return nil, "", targetdb.ErrConnection
	}, &fakePreparer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unreachable.Run(context.Background(), testRequest(), nil); !errors.Is(err, targetdb.ErrConnection) {
		t.Fatalf("unreachable target returned %v", err)
	}
}
