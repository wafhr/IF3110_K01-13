package migration_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
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

func (s *repositoryScript) Connect(context.Context) (driver.Conn, error) {
	return repositoryConn{s}, nil
}
func (s *repositoryScript) Driver() driver.Driver { return repositoryDriver{s} }

type repositoryDriver struct{ script *repositoryScript }

func (d repositoryDriver) Open(string) (driver.Conn, error) { return repositoryConn(d), nil }

type repositoryConn struct{ script *repositoryScript }

func (repositoryConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (repositoryConn) Close() error                        { return nil }
func (c repositoryConn) Begin() (driver.Tx, error) {
	_, err := c.script.take("begin", "", nil)
	return repositoryTx(c), err
}
func (c repositoryConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}
func (c repositoryConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	_, err := c.script.take("exec", query, args)
	return driver.RowsAffected(1), err
}
func (c repositoryConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	step, err := c.script.take("query", query, args)
	return &repositoryRows{step: step}, err
}

type repositoryTx struct{ script *repositoryScript }

func (tx repositoryTx) Commit() error {
	_, err := tx.script.take("commit", "", nil)
	return err
}
func (tx repositoryTx) Rollback() error {
	_, err := tx.script.take("rollback", "", nil)
	return err
}

type repositoryRows struct {
	step repositoryStep
	next int
}

func (r *repositoryRows) Columns() []string {
	if len(r.step.rows) == 0 {
		return nil
	}
	return make([]string, len(r.step.rows[0]))
}
func (*repositoryRows) Close() error { return nil }
func (r *repositoryRows) Next(values []driver.Value) error {
	if r.next == len(r.step.rows) {
		if r.step.nextErr != nil {
			return r.step.nextErr
		}
		return io.EOF
	}
	copy(values, r.step.rows[r.next])
	r.next++
	return nil
}

func testMigrationDatabase(t *testing.T, steps ...repositoryStep) *sql.DB {
	t.Helper()
	script := &repositoryScript{t: t, steps: steps}
	db := sql.OpenDB(script)
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
			false, "", repositoryActorID, repositoryTime}},
	}
}
