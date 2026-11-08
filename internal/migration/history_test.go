package migration_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

func executionID(number int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", number)
}

func committedExecution(number int, action migration.Action) migration.Execution {
	finished := repositoryTime.Add(time.Second)
	return migration.Execution{
		ID: executionID(number), Sequence: int64(number), Name: "ubah pelanggan",
		DatabaseID: repositoryDatabaseID, MigrationID: repositoryMigrationID,
		Revision: 1, Action: action, SQL: "\n-- Ubah pelanggan\nUPDATE customers SET name = 'baru';\n",
		Status: migration.Success, ExecutedBy: repositoryActorID,
		StartedAt: repositoryTime, FinishedAt: &finished,
	}
}

func executionValues(execution migration.Execution) []driver.Value {
	var migrationID, revision, revertsID, legacyVersion, finished driver.Value
	if execution.MigrationID != "" {
		migrationID, revision = execution.MigrationID, execution.Revision
	}
	if execution.RevertsExecutionID != "" {
		revertsID = execution.RevertsExecutionID
	}
	if execution.LegacyVersion != nil {
		legacyVersion = *execution.LegacyVersion
	}
	if execution.FinishedAt != nil {
		finished = *execution.FinishedAt
	}
	return []driver.Value{execution.ID, execution.Sequence, execution.DatabaseID, migrationID, revision,
		execution.Name, string(execution.Action), execution.SQL, string(execution.Status), execution.ExecutedBy,
		execution.StartedAt, finished, revertsID, legacyVersion}
}

func testHistoryRepository(t *testing.T, steps ...repositoryStep) *migration.HistoryRepository {
	t.Helper()
	repository, err := migration.NewHistoryRepository(testMigrationDatabase(t, steps...))
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func testHistoryTransaction(t *testing.T, steps ...repositoryStep) (*migration.HistoryRepository, *sql.Tx) {
	t.Helper()
	db := testMigrationDatabase(t, steps...)
	repository, err := migration.NewHistoryRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return repository, tx
}

func TestHistoryRepositoryRequiresDatabase(t *testing.T) {
	if _, err := migration.NewHistoryRepository(nil); err == nil {
		t.Fatal("database target wajib tersedia")
	}
}

func historyInitializationSteps(legacy bool) []repositoryStep {
	steps := []repositoryStep{
		{operation: "begin"},
		{operation: "exec", statement: "pg_advisory_xact_lock(311003)"},
		{operation: "exec", statement: "CREATE TABLE IF NOT EXISTS platform_migration_executions"},
		{operation: "query", statement: "to_regclass('platform_migrations')", rows: [][]driver.Value{{legacy}}},
	}
	if legacy {
		steps = append(steps, repositoryStep{operation: "query", statement: "SELECT EXISTS", rows: [][]driver.Value{{true}}, check: repositoryArguments(repositoryDatabaseID)})
		steps = append(steps, repositoryStep{operation: "exec", statement: "WHERE status = 'success' ORDER BY version", check: repositoryArguments(repositoryDatabaseID)})
	}
	return append(steps, repositoryStep{operation: "commit"})
}

func TestHistoryRepositoryInitialize(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			if err := testHistoryRepository(t, historyInitializationSteps(legacy)...).Initialize(context.Background(), repositoryDatabaseID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHistoryRepositoryInitializeRollsBackOnFailure(t *testing.T) {
	for failure, name := range []string{"begin", "schema lock", "DDL", "legacy lookup", "legacy pending lookup", "legacy import", "commit"} {
		t.Run(name, func(t *testing.T) {
			steps := historyInitializationSteps(true)
			steps[failure].err = errors.New("private database detail")
			steps = steps[:failure+1]
			if failure > 0 && failure < 6 {
				steps = append(steps, repositoryStep{operation: "rollback"})
			}
			err := testHistoryRepository(t, steps...).Initialize(context.Background(), repositoryDatabaseID)
			if !errors.Is(err, migration.ErrStorage) || strings.Contains(err.Error(), "private database detail") {
				t.Fatalf("Initialize() = %v; diharapkan error penyimpanan yang aman", err)
			}
		})
	}
}

func TestHistoryAppendUsesCallerTransaction(t *testing.T) {
	execution := committedExecution(1, migration.Apply)
	insert := repositoryStep{
		operation: "query", statement: "INSERT INTO platform_migration_executions",
		rows: [][]driver.Value{executionValues(execution)},
		check: repositoryArguments(execution.ID, execution.DatabaseID, execution.MigrationID, execution.Revision,
			execution.Name, string(execution.Action), execution.SQL, execution.ExecutedBy, execution.StartedAt, *execution.FinishedAt, ""),
	}
	repository, tx := testHistoryTransaction(t, repositoryStep{operation: "begin"},
		repositoryStep{operation: "exec", statement: execution.SQL}, insert, repositoryStep{operation: "rollback"})
	if _, err := tx.ExecContext(context.Background(), execution.SQL); err != nil {
		t.Fatal(err)
	}
	input := execution
	input.Sequence, input.Name = 999, " ubah pelanggan "
	stored, err := repository.Append(context.Background(), tx, input)
	if err != nil || !reflect.DeepEqual(stored, execution) {
		t.Fatalf("Append() = %+v, %v", stored, err)
	}
}

func TestHistoryAppendRejectsInvalidExecutions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*migration.Execution)
	}{
		{"execution ID", func(e *migration.Execution) { e.ID = "bad" }},
		{"database ID", func(e *migration.Execution) { e.DatabaseID = "bad" }},
		{"migration ID", func(e *migration.Execution) { e.MigrationID = "bad" }},
		{"actor ID", func(e *migration.Execution) { e.ExecutedBy = "bad" }},
		{"revision", func(e *migration.Execution) { e.Revision = 0 }},
		{"failed", func(e *migration.Execution) { e.Status = migration.Failed }},
		{"running", func(e *migration.Execution) { e.Status = migration.Running }},
		{"forged legacy", func(e *migration.Execution) { version := int64(1); e.LegacyVersion = &version }},
		{"start time", func(e *migration.Execution) { e.StartedAt = time.Time{} }},
		{"finish time", func(e *migration.Execution) { e.FinishedAt = nil }},
		{"time ordering", func(e *migration.Execution) { e.StartedAt = e.FinishedAt.Add(time.Second) }},
		{"name", func(e *migration.Execution) { e.Name = " " }},
		{"SQL", func(e *migration.Execution) { e.SQL = " " }},
		{"action", func(e *migration.Execution) { e.Action = "retry" }},
		{"apply reference", func(e *migration.Execution) { e.RevertsExecutionID = executionID(2) }},
		{"rollback without reference", func(e *migration.Execution) { e.Action = migration.Rollback }},
		{"self rollback", func(e *migration.Execution) { e.Action, e.RevertsExecutionID = migration.Rollback, e.ID }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository, tx := testHistoryTransaction(t, repositoryStep{operation: "begin"}, repositoryStep{operation: "rollback"})
			execution := committedExecution(1, migration.Apply)
			tc.change(&execution)
			stored, err := repository.Append(context.Background(), tx, execution)
			if !errors.Is(err, migration.ErrInvalidMigration) || !reflect.DeepEqual(stored, migration.Execution{}) {
				t.Fatalf("Append() = %+v, %v; diharapkan input tidak valid", stored, err)
			}
		})
	}
	t.Run("nil transaction", func(t *testing.T) {
		_, err := testHistoryRepository(t).Append(context.Background(), nil, committedExecution(1, migration.Apply))
		if !errors.Is(err, migration.ErrInvalidMigration) {
			t.Fatal(err)
		}
	})
}

func TestHistoryAppendMapsStorageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"duplicate ID", &pgconn.PgError{Code: "23505"}, migration.ErrConflict},
		{"missing origin", &pgconn.PgError{Code: "23503"}, migration.ErrNotFound},
		{"invalid origin", &pgconn.PgError{Code: "23514"}, migration.ErrInvalidMigration},
		{"target permission", &pgconn.PgError{Code: "42501"}, targetdb.ErrPermissionDenied},
		{"target shutdown", &pgconn.PgError{Code: "57P01"}, targetdb.ErrConnection},
		{"connection lost", &pgconn.PgError{Code: "08006"}, targetdb.ErrConnection},
		{"canceled", context.Canceled, context.Canceled},
		{"other", errors.New("private SQL detail"), migration.ErrStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository, tx := testHistoryTransaction(t, repositoryStep{operation: "begin"},
				repositoryStep{operation: "query", err: tc.err}, repositoryStep{operation: "rollback"})
			stored, err := repository.Append(context.Background(), tx, committedExecution(1, migration.Apply))
			if !errors.Is(err, tc.want) || !reflect.DeepEqual(stored, migration.Execution{}) || strings.Contains(err.Error(), "private SQL detail") {
				t.Fatalf("Append() = %+v, %v; diharapkan %v", stored, err, tc.want)
			}
		})
	}
}

func TestHistoryReadsScopeAndLegacyMetadata(t *testing.T) {
	execution := committedExecution(1, migration.Apply)
	legacyVersion := int64(7)
	execution.MigrationID, execution.Revision, execution.LegacyVersion, execution.FinishedAt = "", 0, &legacyVersion, nil
	get := repositoryStep{operation: "query", statement: "WHERE database_id = $1 AND id = $2",
		rows: [][]driver.Value{executionValues(execution)}, check: repositoryArguments(repositoryDatabaseID, execution.ID)}
	list := repositoryStep{operation: "query", statement: "WHERE database_id = $1 ORDER BY sequence DESC",
		rows: [][]driver.Value{executionValues(execution)}, check: repositoryArguments(repositoryDatabaseID)}
	repository := testHistoryRepository(t, get, list)
	stored, err := repository.Get(context.Background(), repositoryDatabaseID, execution.ID)
	if err != nil || !reflect.DeepEqual(stored, execution) {
		t.Fatalf("Get() = %+v, %v", stored, err)
	}
	history, err := repository.List(context.Background(), repositoryDatabaseID, false)
	if err != nil || !reflect.DeepEqual(history, []migration.Execution{execution}) {
		t.Fatalf("List() = %+v, %v", history, err)
	}
}

func TestHistoryListErrorsAndEmptyResults(t *testing.T) {
	for _, tc := range []struct {
		name string
		step repositoryStep
		want error
	}{
		{"empty", repositoryStep{operation: "query"}, nil},
		{"query", repositoryStep{operation: "query", err: errors.New("private detail")}, migration.ErrStorage},
		{"iteration", repositoryStep{operation: "query", nextErr: errors.New("private detail")}, migration.ErrStorage},
		{"scan", repositoryStep{operation: "query", rows: [][]driver.Value{{"invalid row"}}}, migration.ErrStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history, err := testHistoryRepository(t, tc.step).List(context.Background(), repositoryDatabaseID, true)
			if !errors.Is(err, tc.want) || (tc.want == nil && (history == nil || len(history) != 0)) {
				t.Fatalf("List() = %+v, %v", history, err)
			}
		})
	}
}

func TestLegacyAttemptsPreserveFailedAndRunningRecords(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", exists), func(t *testing.T) {
			steps := []repositoryStep{{operation: "query", rows: [][]driver.Value{{exists}}}}
			if exists {
				steps = append(steps, repositoryStep{operation: "query", statement: "WHERE status <> 'success' ORDER BY version DESC",
					rows: [][]driver.Value{
						{int64(2), "sedang berjalan", "SELECT 2", "running", repositoryActorID, repositoryTime, nil},
						{int64(1), "gagal", "SELECT 1/0", "failed", repositoryActorID, repositoryTime, repositoryTime},
					}})
			}
			attempts, err := testHistoryRepository(t, steps...).LegacyAttempts(context.Background())
			if err != nil || attempts == nil {
				t.Fatalf("LegacyAttempts() = %+v, %v", attempts, err)
			}
			if exists && (len(attempts) != 2 || attempts[0].Status != migration.Running || attempts[1].Status != migration.Failed) {
				t.Fatalf("attempt lama berubah: %+v", attempts)
			}
		})
	}
}

func TestHideRollbackUsesExecutionIDsAndPreservesForward(t *testing.T) {
	a := committedExecution(1, migration.Apply)
	b := committedExecution(2, migration.Apply)
	rollback := committedExecution(3, migration.Rollback)
	rollback.RevertsExecutionID = b.ID
	forward := committedExecution(4, migration.Forward)
	c := committedExecution(5, migration.Apply)
	history := []migration.Execution{c, forward, rollback, b, a}
	original := append([]migration.Execution(nil), history...)
	want := []migration.Execution{c, forward, a}
	if got := migration.HideRollback(history); !reflect.DeepEqual(got, want) {
		t.Fatalf("HideRollback() = %+v; diharapkan %+v", got, want)
	}
	if !reflect.DeepEqual(history, original) {
		t.Fatal("filter mengubah input history")
	}
	repository := testHistoryRepository(t, repositoryStep{operation: "query", rows: [][]driver.Value{
		executionValues(c), executionValues(forward), executionValues(rollback), executionValues(b), executionValues(a),
	}})
	got, err := repository.List(context.Background(), repositoryDatabaseID, true)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("List(hideRollback) = %+v, %v", got, err)
	}
}

func TestHideRollbackKeepsFailedOrUnmatchedAttempts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*migration.Execution, *migration.Execution)
	}{
		{"failed rollback", func(_ *migration.Execution, r *migration.Execution) { r.Status = migration.Failed }},
		{"running rollback", func(_ *migration.Execution, r *migration.Execution) { r.Status = migration.Running }},
		{"failed original", func(e *migration.Execution, _ *migration.Execution) { e.Status = migration.Failed }},
		{"missing original", func(_ *migration.Execution, r *migration.Execution) { r.RevertsExecutionID = executionID(9) }},
		{"different database", func(_ *migration.Execution, r *migration.Execution) { r.DatabaseID = executionID(9) }},
		{"different migration", func(_ *migration.Execution, r *migration.Execution) { r.MigrationID = executionID(9) }},
		{"different revision", func(_ *migration.Execution, r *migration.Execution) { r.Revision = 2 }},
		{"rollback origin", func(e *migration.Execution, _ *migration.Execution) { e.Action = migration.Rollback }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := committedExecution(1, migration.Apply)
			rollback := committedExecution(2, migration.Rollback)
			rollback.RevertsExecutionID = original.ID
			tc.change(&original, &rollback)
			history := []migration.Execution{rollback, original}
			if got := migration.HideRollback(history); !reflect.DeepEqual(got, history) {
				t.Fatalf("attempt tidak boleh disembunyikan: %+v", got)
			}
		})
	}
}

func TestHistoryRejectsInvalidReadIdentifiers(t *testing.T) {
	repository := testHistoryRepository(t)
	if err := repository.Initialize(context.Background(), "bad"); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal(err)
	}
	if _, err := repository.Get(context.Background(), repositoryDatabaseID, "bad"); !errors.Is(err, migration.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := repository.List(context.Background(), "bad", false); !errors.Is(err, migration.ErrNotFound) {
		t.Fatal(err)
	}
}
