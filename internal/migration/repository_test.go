package migration_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

func TestNewMigrationRepositoryRejectsNilDatabase(t *testing.T) {
	if _, err := migration.NewRepository(nil); err == nil {
		t.Fatal("expected missing platform database error")
	}
}

func TestMigrationRepositoryInitialize(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure int
	}{
		{"success", -1},
		{"begin failure", 0},
		{"schema lock failure", 1},
		{"DDL failure", 2},
		{"commit failure", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := tc.failure
			steps := []repositoryStep{
				{operation: "begin"},
				{operation: "exec", statement: "pg_advisory_xact_lock(311002)"},
				{operation: "exec", statement: "CREATE TABLE IF NOT EXISTS migration_definitions"},
				{operation: "commit"},
			}
			if failure >= 0 {
				steps[failure].err = errors.New("private database detail")
				steps = steps[:failure+1]
				if failure > 0 && failure < 3 {
					steps = append(steps, repositoryStep{operation: "rollback"})
				}
			}
			err := testMigrationRepository(t, steps...).Initialize(context.Background())
			if failure == -1 {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, migration.ErrStorage) || strings.Contains(err.Error(), "private database detail") {
				t.Fatalf("Initialize() error = %v; want sanitized storage error", err)
			}
		})
	}
}

func createRepositorySteps() []repositoryStep {
	definition := definitionStep("INSERT INTO migration_definitions", 1, false)
	definition.check = repositoryArguments(repositoryDatabaseID, "create customers", repositoryActorID)
	revision := revisionStep("INSERT INTO migration_revisions", 1)
	revision.check = repositoryArguments(repositoryMigrationID, int64(1), validRevision().SQLUp, validRevision().SQLDown, false, "", repositoryActorID)
	return []repositoryStep{
		{operation: "begin"},
		{operation: "query", statement: "FOR KEY SHARE", rows: [][]driver.Value{{repositoryDatabaseID}}, check: repositoryArguments(repositoryDatabaseID)},
		definition, revision, {operation: "commit"},
	}
}

func TestMigrationRepositoryCreate(t *testing.T) {
	repository := testMigrationRepository(t, createRepositorySteps()...)
	input := validRevision()
	input.Number, input.MigrationID, input.CreatedBy = 999, "spoofed-id", "spoofed-author"
	definition, revision, err := repository.Create(context.Background(), repositoryDatabaseID, repositoryActorID, " create customers ", input)
	if err != nil {
		t.Fatal(err)
	}
	if definition.ID != repositoryMigrationID || definition.CurrentRevision != 1 || definition.FrozenAt != nil ||
		revision.MigrationID != definition.ID || revision.Number != 1 || revision.CreatedBy != repositoryActorID ||
		revision.SQLUp != input.SQLUp || revision.SQLDown != input.SQLDown {
		t.Fatalf("unexpected stored migration %+v or revision %+v", definition, revision)
	}
}

func TestMigrationRepositoryCreateRollsBackOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		step int
		err  error
		want error
	}{
		{"begin", 0, errors.New("private detail"), migration.ErrStorage},
		{"missing registration", 1, sql.ErrNoRows, migration.ErrNotFound},
		{"definition", 2, errors.New("private detail"), migration.ErrStorage},
		{"revision", 3, errors.New("private detail"), migration.ErrStorage},
		{"duplicate", 3, &pgconn.PgError{Code: "23505"}, migration.ErrConflict},
		{"commit", 4, errors.New("private detail"), migration.ErrStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := createRepositorySteps()
			steps[tc.step].err = tc.err
			steps = steps[:tc.step+1]
			if tc.step > 0 && tc.step < 4 {
				steps = append(steps, repositoryStep{operation: "rollback"})
			}
			definition, revision, err := testMigrationRepository(t, steps...).Create(context.Background(), repositoryDatabaseID, repositoryActorID, "create customers", validRevision())
			if !errors.Is(err, tc.want) || definition != (migration.Migration{}) || revision != (migration.Revision{}) {
				t.Fatalf("Create() = %+v, %+v, %v; want %v and empty results", definition, revision, err, tc.want)
			}
		})
	}
}

func TestMigrationRepositoryReadsAreDatabaseScoped(t *testing.T) {
	get := definitionStep("WHERE database_id = $1 AND id = $2", 1, true)
	get.check = repositoryArguments(repositoryDatabaseID, repositoryMigrationID)
	getRevision := revisionStep("WHERE database_id = $1 AND id = $2", 1)
	getRevision.check = repositoryArguments(repositoryDatabaseID, repositoryMigrationID, int64(1))
	list := definitionStep("WHERE database_id = $1 ORDER BY created_at, id", 1, false)
	list.check = repositoryArguments(repositoryDatabaseID)
	repository := testMigrationRepository(t, get, getRevision, list)
	definition, err := repository.Get(context.Background(), repositoryDatabaseID, repositoryMigrationID)
	if err != nil || definition.FrozenAt == nil || !definition.FrozenAt.Equal(repositoryTime) {
		t.Fatalf("Get() = %+v, %v", definition, err)
	}
	revision, err := repository.GetRevision(context.Background(), repositoryDatabaseID, repositoryMigrationID, 1)
	if err != nil || revision.Number != 1 || revision.SQLUp != validRevision().SQLUp {
		t.Fatalf("GetRevision() = %+v, %v", revision, err)
	}
	definitions, err := repository.List(context.Background(), repositoryDatabaseID)
	if err != nil || len(definitions) != 1 {
		t.Fatalf("List() = %+v, %v", definitions, err)
	}
}

func TestMigrationRepositoryListEmptyAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		step repositoryStep
		want error
	}{
		{"empty", repositoryStep{operation: "query", statement: "FROM migration_definitions"}, nil},
		{"query", repositoryStep{operation: "query", err: errors.New("private detail")}, migration.ErrStorage},
		{"row iteration", repositoryStep{operation: "query", nextErr: errors.New("private detail")}, migration.ErrStorage},
		{"scan", repositoryStep{operation: "query", rows: [][]driver.Value{{"wrong column count"}}}, migration.ErrStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definitions, err := testMigrationRepository(t, tc.step).List(context.Background(), repositoryDatabaseID)
			if !errors.Is(err, tc.want) || (tc.want == nil && (definitions == nil || len(definitions) != 0)) {
				t.Fatalf("List() = %+v, %v; want %v", definitions, err, tc.want)
			}
		})
	}
}

func reviseRepositorySteps() []repositoryStep {
	lock := definitionStep("FOR UPDATE", 1, false)
	lock.check = repositoryArguments(repositoryDatabaseID, repositoryMigrationID)
	revision := revisionStep("INSERT INTO migration_revisions", 2)
	revision.check = repositoryArguments(repositoryMigrationID, int64(2), validRevision().SQLUp, validRevision().SQLDown, false, "", repositoryActorID)
	update := definitionStep("UPDATE migration_definitions", 2, false)
	update.check = repositoryArguments(repositoryDatabaseID, repositoryMigrationID, "create customers", int64(2))
	return []repositoryStep{{operation: "begin"}, lock, revision, update, {operation: "commit"}}
}

func TestMigrationRepositoryRevise(t *testing.T) {
	definition, revision, err := testMigrationRepository(t, reviseRepositorySteps()...).Revise(context.Background(), repositoryDatabaseID, repositoryMigrationID, repositoryActorID, "create customers", 1, validRevision())
	if err != nil || definition.CurrentRevision != 2 || revision.Number != 2 || revision.CreatedBy != repositoryActorID {
		t.Fatalf("Revise() = %+v, %+v, %v", definition, revision, err)
	}
}

func TestMigrationRepositoryReviseRejectsFrozenAndStaleDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		number int64
		frozen bool
		want   error
	}{
		{"frozen", 1, true, migration.ErrImmutableMigration},
		{"stale", 2, false, migration.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository := testMigrationRepository(t, repositoryStep{operation: "begin"}, definitionStep("FOR UPDATE", tc.number, tc.frozen), repositoryStep{operation: "rollback"})
			_, _, err := repository.Revise(context.Background(), repositoryDatabaseID, repositoryMigrationID, repositoryActorID, "create customers", 1, validRevision())
			if !errors.Is(err, tc.want) {
				t.Fatalf("Revise() error = %v; want %v", err, tc.want)
			}
		})
	}
}

func TestMigrationRepositoryReviseRollsBackOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure int
	}{
		{"begin", 0},
		{"lock", 1},
		{"insert revision", 2},
		{"update definition", 3},
		{"commit", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := tc.failure
			steps := reviseRepositorySteps()
			steps[failure].err = errors.New("private detail")
			steps = steps[:failure+1]
			if failure > 0 && failure < 4 {
				steps = append(steps, repositoryStep{operation: "rollback"})
			}
			definition, revision, err := testMigrationRepository(t, steps...).Revise(context.Background(), repositoryDatabaseID, repositoryMigrationID, repositoryActorID, "create customers", 1, validRevision())
			if !errors.Is(err, migration.ErrStorage) || definition != (migration.Migration{}) || revision != (migration.Revision{}) {
				t.Fatalf("Revise() = %+v, %+v, %v; want storage error and empty results", definition, revision, err)
			}
		})
	}
}

func TestMigrationRepositoryFreezeRevision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frozen bool
	}{
		{"first execution", false},
		{"repeat execution", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []repositoryStep{{operation: "begin"}, definitionStep("FOR UPDATE", 1, tc.frozen)}
			if !tc.frozen {
				steps = append(steps, repositoryStep{operation: "query", statement: "SET frozen_at = CURRENT_TIMESTAMP", rows: [][]driver.Value{{repositoryMigrationID}}, check: repositoryArguments(repositoryDatabaseID, repositoryMigrationID)})
			}
			steps = append(steps, repositoryStep{operation: "commit"})
			if err := testMigrationRepository(t, steps...).FreezeRevision(context.Background(), repositoryDatabaseID, repositoryMigrationID, 1); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("stale revision", func(t *testing.T) {
		repository := testMigrationRepository(t, repositoryStep{operation: "begin"}, definitionStep("FOR UPDATE", 2, false), repositoryStep{operation: "rollback"})
		if err := repository.FreezeRevision(context.Background(), repositoryDatabaseID, repositoryMigrationID, 1); !errors.Is(err, migration.ErrConflict) {
			t.Fatalf("FreezeRevision() error = %v; want conflict", err)
		}
	})
}

func TestMigrationRepositoryRejectsInvalidInputBeforeDatabaseAccess(t *testing.T) {
	repository := testMigrationRepository(t)
	ctx := context.Background()
	if _, _, err := repository.Create(ctx, "bad", repositoryActorID, "name", validRevision()); !errors.Is(err, migration.ErrNotFound) {
		t.Fatal("invalid database ID was accepted:", err)
	}
	if _, _, err := repository.Create(ctx, repositoryDatabaseID, "bad", "name", validRevision()); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal("invalid actor ID was accepted:", err)
	}
	if _, _, err := repository.Create(ctx, repositoryDatabaseID, repositoryActorID, " ", validRevision()); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal("invalid name was accepted:", err)
	}
	if _, _, err := repository.Create(ctx, repositoryDatabaseID, repositoryActorID, "name", migration.Revision{}); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal("invalid SQL was accepted:", err)
	}
	if _, err := repository.Get(ctx, repositoryDatabaseID, "bad"); !errors.Is(err, migration.ErrNotFound) {
		t.Fatal("invalid migration ID was accepted:", err)
	}
	if _, err := repository.List(ctx, "bad"); !errors.Is(err, migration.ErrNotFound) {
		t.Fatal("invalid list database ID was accepted:", err)
	}
	if _, err := repository.GetRevision(ctx, repositoryDatabaseID, repositoryMigrationID, 0); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal("invalid revision number was accepted:", err)
	}
	if _, _, err := repository.Revise(ctx, repositoryDatabaseID, repositoryMigrationID, repositoryActorID, "name", math.MaxInt64, validRevision()); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal("revision overflow was accepted:", err)
	}
	if err := repository.FreezeRevision(ctx, repositoryDatabaseID, repositoryMigrationID, 0); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal("invalid execution revision was accepted:", err)
	}
}

func TestMigrationRepositorySanitizesStorageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"missing", sql.ErrNoRows, migration.ErrNotFound},
		{"canceled", context.Canceled, context.Canceled},
		{"deadline", context.DeadlineExceeded, context.DeadlineExceeded},
		{"unique", &pgconn.PgError{Code: "23505", Message: "private detail"}, migration.ErrConflict},
		{"foreign key", &pgconn.PgError{Code: "23503", Message: "private detail"}, migration.ErrNotFound},
		{"revision immutable", &pgconn.PgError{Code: "23514", ConstraintName: "migration_revisions_immutable", Message: "private detail"}, migration.ErrImmutableMigration},
		{"definition frozen", &pgconn.PgError{Code: "23514", ConstraintName: "migration_definitions_frozen", Message: "private detail"}, migration.ErrImmutableMigration},
		{"check", &pgconn.PgError{Code: "23514", Message: "private detail"}, migration.ErrInvalidMigration},
		{"other", errors.New("private detail"), migration.ErrStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository := testMigrationRepository(t, repositoryStep{operation: "query", err: tc.err})
			_, err := repository.Get(context.Background(), repositoryDatabaseID, repositoryMigrationID)
			if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "private detail") {
				t.Fatalf("Get() error = %v; want sanitized %v", err, tc.want)
			}
		})
	}
}

func TestMigrationRepositoryFreezeRollsBackOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure int
	}{
		{"begin", 0},
		{"lock", 1},
		{"freeze update", 2},
		{"commit", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []repositoryStep{
				{operation: "begin"},
				definitionStep("FOR UPDATE", 1, false),
				{operation: "query", statement: "SET frozen_at = CURRENT_TIMESTAMP", rows: [][]driver.Value{{repositoryMigrationID}}},
				{operation: "commit"},
			}
			steps[tc.failure].err = errors.New("private detail")
			steps = steps[:tc.failure+1]
			if tc.failure > 0 && tc.failure < 3 {
				steps = append(steps, repositoryStep{operation: "rollback"})
			}
			err := testMigrationRepository(t, steps...).FreezeRevision(context.Background(), repositoryDatabaseID, repositoryMigrationID, 1)
			if !errors.Is(err, migration.ErrStorage) {
				t.Fatalf("FreezeRevision() error = %v; want storage error", err)
			}
		})
	}
}
