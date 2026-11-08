package migration_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

func TestExecuteValidatesBeforeOpeningTarget(t *testing.T) {
	for _, tc := range []struct {
		name, migrationName, statement string
		wantError                      error
		wantOpens                      int
	}{
		{"empty name", "", "SELECT 1", migration.ErrInvalidMigration, 0},
		{"oversized name", strings.Repeat("a", migration.MaxMigrationNameBytes+1), "SELECT 1", migration.ErrInvalidMigration, 0},
		{"NUL name", "private\x00name", "SELECT 1", migration.ErrInvalidMigration, 0},
		{"invalid UTF-8 name", string([]byte{0xff}), "SELECT 1", migration.ErrInvalidMigration, 0},
		{"empty SQL", "preview", " \t\n", migration.ErrInvalidMigration, 0},
		{"oversized SQL", "preview", strings.Repeat("a", migration.MaxSQLBytes+1), migration.ErrInvalidMigration, 0},
		{"NUL SQL", "preview", "SELECT 1\x00", migration.ErrInvalidMigration, 0},
		{"invalid UTF-8 SQL", "preview", string([]byte{0xff}), migration.ErrInvalidMigration, 0},
		{"legacy SQL without down", " preview ", " SELECT 1 ", context.Canceled, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opens := 0
			service := migration.NewService(func(_ context.Context, databaseID string) (*sql.DB, database.DBMS, error) {
				opens++
				if databaseID != "target-id" {
					t.Errorf("target database ID = %q", databaseID)
				}
				return nil, database.PostgreSQL, context.Canceled
			})
			record, err := service.Execute(context.Background(), "target-id", "user-id", tc.migrationName, tc.statement)
			if !errors.Is(err, tc.wantError) || opens != tc.wantOpens || record != (migration.Record{}) {
				t.Fatalf("Execute() = %+v, %v; target opens = %d; want %v and %d opens", record, err, opens, tc.wantError, tc.wantOpens)
			}
		})
	}
}

type actionDefinitionReader struct {
	t                *testing.T
	definition       migration.Migration
	revision         migration.Revision
	expectedRevision int64
	getCalls         int
	revisionCalls    int
	definitionError  error
	revisionError    error
}

func (r *actionDefinitionReader) Get(_ context.Context, databaseID, migrationID string) (migration.Migration, error) {
	r.t.Helper()
	r.getCalls++
	if databaseID != r.definition.DatabaseID || migrationID != r.definition.ID {
		r.t.Fatalf("scope definisi tidak sesuai: %q, %q", databaseID, migrationID)
	}
	return r.definition, r.definitionError
}

func (r *actionDefinitionReader) GetRevision(_ context.Context, databaseID, migrationID string, number int64) (migration.Revision, error) {
	r.t.Helper()
	r.revisionCalls++
	if databaseID != r.definition.DatabaseID || migrationID != r.definition.ID || number != r.expectedRevision {
		r.t.Fatalf("revisi yang diminta tidak sesuai: %q, %q, %d", databaseID, migrationID, number)
	}
	return r.revision, r.revisionError
}

func actionHistorySteps(history []migration.Execution) []repositoryStep {
	rows := make([][]driver.Value, 0, len(history))
	for index := len(history) - 1; index >= 0; index-- {
		rows = append(rows, executionValues(history[index]))
	}
	return append(openedHistorySteps(), repositoryStep{operation: "query",
		statement: "WHERE database_id = $1 ORDER BY sequence DESC", rows: rows, check: repositoryArguments(repositoryDatabaseID)})
}

func TestPrepareActionLoadsOriginalRevision(t *testing.T) {
	for _, tc := range []struct {
		name            string
		action          migration.Action
		migrationNumber int
		history         []migration.Execution
	}{
		{"apply current revision", migration.Apply, 4, rolledBackTimeline()},
		{"rollback latest forward execution", migration.Rollback, 2, append(rolledBackTimeline(), timelineExecution(6, 2, migration.Forward))},
		{"forward original revision", migration.Forward, 2, rolledBackTimeline()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execution := timelineExecution(1, tc.migrationNumber, migration.Apply)
			definition := timelineDefinition(execution)
			definition.CurrentRevision = 9
			revision := timelineRevision(execution)
			if tc.action == migration.Apply {
				revision.Number = definition.CurrentRevision
			}
			reader := &actionDefinitionReader{t: t, definition: definition, revision: revision, expectedRevision: revision.Number}
			target := testMigrationDatabase(t, actionHistorySteps(tc.history)...)
			service := migration.NewServiceWithRepository(func(_ context.Context, databaseID string) (*sql.DB, database.DBMS, error) {
				if databaseID != repositoryDatabaseID {
					t.Fatal("database target tidak sesuai")
				}
				return target, database.PostgreSQL, nil
			}, reader)
			plan, err := service.PrepareAction(context.Background(), repositoryDatabaseID, definition.ID, tc.action)
			if err != nil {
				t.Fatal(err)
			}
			wantSQL, err := revision.SQLFor(tc.action)
			if err != nil {
				t.Fatal(err)
			}
			last := tc.history[len(tc.history)-1]
			if plan.SQL != wantSQL || plan.Revision != revision.Number || plan.BaseExecutionID != last.ID || reader.getCalls != 1 || reader.revisionCalls != 1 {
				t.Fatalf("rencana memakai revisi atau history yang salah: %+v", plan)
			}
			if tc.action == migration.Rollback && plan.RevertsExecutionID != last.ID {
				t.Fatalf("rollback harus merujuk execution forward terakhir: %+v", plan)
			}
		})
	}
}

func TestPrepareActionRejectsInvalidRequestsBeforeDependencies(t *testing.T) {
	reader := &actionDefinitionReader{t: t}
	service := migration.NewServiceWithRepository(func(context.Context, string) (*sql.DB, database.DBMS, error) {
		t.Fatal("target dibuka untuk request tidak valid")
		return nil, "", nil
	}, reader)
	for _, tc := range []struct {
		databaseID, migrationID string
		action                  migration.Action
	}{
		{"bad", repositoryMigrationID, migration.Apply},
		{repositoryDatabaseID, "bad", migration.Apply},
		{repositoryDatabaseID, repositoryMigrationID, "bad"},
	} {
		if _, err := service.PrepareAction(context.Background(), tc.databaseID, tc.migrationID, tc.action); !errors.Is(err, migration.ErrInvalidMigration) {
			t.Fatal("request tidak valid diterima:", err)
		}
	}
	if reader.getCalls != 0 {
		t.Fatal("definisi dibaca untuk request tidak valid")
	}
	if _, err := migration.NewService(nil).PrepareAction(context.Background(), repositoryDatabaseID, repositoryMigrationID, migration.Apply); !errors.Is(err, migration.ErrStorage) {
		t.Fatal("repository yang belum tersedia tidak ditangani:", err)
	}
}

func TestPrepareActionPropagatesReadFailures(t *testing.T) {
	for _, failure := range []string{"definition", "target", "history initialization", "history read", "revision"} {
		t.Run(failure, func(t *testing.T) {
			execution := timelineExecution(1, 1, migration.Apply)
			reader := &actionDefinitionReader{t: t, definition: timelineDefinition(execution), revision: timelineRevision(execution), expectedRevision: 1}
			steps := actionHistorySteps(nil)
			want := migration.ErrStorage
			var targetError error
			switch failure {
			case "definition":
				reader.definitionError, want, steps = migration.ErrNotFound, migration.ErrNotFound, nil
			case "target":
				targetError, want, steps = context.DeadlineExceeded, context.DeadlineExceeded, nil
			case "history initialization":
				steps = []repositoryStep{{operation: "query", err: errors.New("private detail")}}
			case "history read":
				steps[len(steps)-1].err = errors.New("private detail")
			case "revision":
				reader.revisionError, want = migration.ErrNotFound, migration.ErrNotFound
			}
			target := testMigrationDatabase(t, steps...)
			service := migration.NewServiceWithRepository(func(context.Context, string) (*sql.DB, database.DBMS, error) {
				if targetError != nil {
					return nil, "", targetError
				}
				return target, database.PostgreSQL, nil
			}, reader)
			_, err := service.PrepareAction(context.Background(), repositoryDatabaseID, reader.definition.ID, migration.Apply)
			if !errors.Is(err, want) || strings.Contains(err.Error(), "private detail") {
				t.Fatalf("PrepareAction() = %v; diharapkan %v", err, want)
			}
		})
	}
}

func TestPrepareActionRejectsUnsupportedTarget(t *testing.T) {
	execution := timelineExecution(1, 1, migration.Apply)
	reader := &actionDefinitionReader{t: t, definition: timelineDefinition(execution)}
	target := testMigrationDatabase(t)
	service := migration.NewServiceWithRepository(func(context.Context, string) (*sql.DB, database.DBMS, error) {
		return target, database.MySQL, nil
	}, reader)
	if _, err := service.PrepareAction(context.Background(), repositoryDatabaseID, reader.definition.ID, migration.Apply); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal("target di luar cakupan tindakan revisi diterima:", err)
	}
}

func TestPrepareActionRejectsMissingTargetOpener(t *testing.T) {
	reader := &actionDefinitionReader{t: t}
	service := migration.NewServiceWithRepository(nil, reader)
	if _, err := service.PrepareAction(context.Background(), repositoryDatabaseID, repositoryMigrationID, migration.Apply); !errors.Is(err, migration.ErrStorage) {
		t.Fatal("konektor target yang belum tersedia tidak ditangani:", err)
	}
	if reader.getCalls != 0 {
		t.Fatal("dependency yang belum lengkap digunakan untuk membaca definisi")
	}
}

func openedHistorySteps() []repositoryStep {
	steps := []repositoryStep{{operation: "query", statement: "to_regclass('platform_migration_executions')", rows: [][]driver.Value{{false}}}}
	return append(steps, historyInitializationSteps(false)...)
}

func TestExecutionHistoryReadsExistingSchemaWithReadOnlyQueries(t *testing.T) {
	steps := []repositoryStep{
		{operation: "query", statement: "to_regclass('platform_migration_executions')", rows: [][]driver.Value{{true}}},
		{operation: "query", statement: "to_regclass('platform_migrations')", rows: [][]driver.Value{{true}}},
		{operation: "query", statement: "SELECT EXISTS", rows: [][]driver.Value{{false}}},
		{operation: "query", statement: "ORDER BY sequence DESC"},
		{operation: "query", statement: "to_regclass('platform_migrations')", rows: [][]driver.Value{{true}}},
		{operation: "query", statement: "WHERE status <> 'success'"},
	}
	target := testMigrationDatabase(t, steps...)
	service := migration.NewService(func(context.Context, string) (*sql.DB, database.DBMS, error) {
		return target, database.PostgreSQL, nil
	})
	history, err := service.ReadExecutionHistory(context.Background(), repositoryDatabaseID, false)
	if err != nil || history.Executions == nil || history.LegacyAttempts == nil {
		t.Fatalf("pembacaan history harus memakai query read-only pada schema siap pakai: %+v, %v", history, err)
	}
}
