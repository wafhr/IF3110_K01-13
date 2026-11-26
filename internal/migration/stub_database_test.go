package migration_test

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/testsql"
)

const (
	repositoryDatabaseID  = "00000000-0000-4000-8000-000000000001"
	repositoryMigrationID = "00000000-0000-4000-8000-000000000002"
	repositoryActorID     = "00000000-0000-4000-8000-000000000003"
)

var repositoryTime = time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)

type repositoryStep struct {
	operation string
	statement string
	rows      [][]driver.Value
	err       error
	nextErr   error
	check     func(*testing.T, []driver.NamedValue)
}

type repositoryScript struct {
	t     *testing.T
	steps []repositoryStep
	next  int
}

func (s *repositoryScript) take(operation, query string, args []driver.NamedValue) (repositoryStep, error) {
	s.t.Helper()
	if s.next >= len(s.steps) {
		s.t.Errorf("unexpected repository operation %s: %s", operation, query)
		return repositoryStep{}, errors.New("unexpected repository operation")
	}
	step := s.steps[s.next]
	s.next++
	if step.operation != operation || !strings.Contains(query, step.statement) {
		s.t.Errorf("operation %s: %s; want %s containing %q", operation, query, step.operation, step.statement)
		return repositoryStep{}, errors.New("unexpected repository operation")
	}
	if step.check != nil {
		step.check(s.t, args)
	}
	return step, step.err
}

func (s *repositoryScript) Query(query string, args []driver.NamedValue) (driver.Rows, error) {
	step, err := s.take("query", query, args)
	return &testsql.Rows{Values: step.rows, Err: step.nextErr}, err
}

func (s *repositoryScript) Exec(query string, args []driver.NamedValue) (driver.Result, error) {
	_, err := s.take("exec", query, args)
	return driver.RowsAffected(1), err
}

func (s *repositoryScript) Begin(driver.TxOptions) error {
	_, err := s.take("begin", "", nil)
	return err
}

func (s *repositoryScript) Commit() error {
	_, err := s.take("commit", "", nil)
	return err
}

func (s *repositoryScript) Rollback() error {
	_, err := s.take("rollback", "", nil)
	return err
}

func testMigrationDatabase(t *testing.T, steps ...repositoryStep) *sql.DB {
	t.Helper()
	script := &repositoryScript{t: t, steps: steps}
	db := sql.OpenDB(testsql.Connector(script))
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
		if script.next != len(script.steps) {
			t.Errorf("consumed %d of %d repository operations", script.next, len(script.steps))
		}
	})
	return db
}

func testMigrationRepository(t *testing.T, steps ...repositoryStep) *migration.Repository {
	t.Helper()
	db := testMigrationDatabase(t, steps...)
	repository, err := migration.NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func repositoryArguments(want ...any) func(*testing.T, []driver.NamedValue) {
	return func(t *testing.T, args []driver.NamedValue) {
		t.Helper()
		values := make([]any, len(args))
		for index, arg := range args {
			values[index] = arg.Value
		}
		if !reflect.DeepEqual(values, want) {
			t.Fatalf("repository arguments = %#v; want %#v", values, want)
		}
	}
}

func definitionStep(statement string, number int64, frozen bool) repositoryStep {
	var frozenAt driver.Value
	if frozen {
		frozenAt = repositoryTime
	}
	return repositoryStep{
		operation: "query",
		statement: statement,
		rows: [][]driver.Value{{repositoryMigrationID, repositoryDatabaseID, "create customers", number,
			repositoryActorID, repositoryTime, repositoryTime, frozenAt}},
	}
}

func revisionStep(statement string, number int64) repositoryStep {
	revision := validRevision()
	return repositoryStep{
		operation: "query",
		statement: statement,
		rows: [][]driver.Value{{repositoryMigrationID, number, revision.SQLUp, revision.SQLDown,
			false, "", "", "", int64(0), repositoryActorID, repositoryTime}},
	}
}
