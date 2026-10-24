package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

const testDatabaseID = "00000000-0000-4000-8000-000000000001"

var testCreatedAt = time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)

type databaseStep struct {
	statement string
	exec      bool
	columns   []string
	rows      [][]driver.Value
	err       error
	nextErr   error
	result    driver.Result
	check     func(*testing.T, []driver.NamedValue)
}

type databaseScript struct {
	t     *testing.T
	steps []databaseStep
	next  int
}

func (s *databaseScript) take(query string, exec bool, args []driver.NamedValue) (databaseStep, error) {
	s.t.Helper()
	if s.next >= len(s.steps) {
		s.t.Error("unexpected database operation")
		return databaseStep{}, errors.New("unexpected database operation")
	}
	step := s.steps[s.next]
	s.next++
	if step.exec != exec || !strings.Contains(query, step.statement) {
		s.t.Errorf("unexpected database operation: %s", query)
		return databaseStep{}, errors.New("unexpected database operation")
	}
	if step.check != nil {
		step.check(s.t, args)
	}
	return step, step.err
}

func (s *databaseScript) Connect(context.Context) (driver.Conn, error) { return scriptConn{s}, nil }
func (s *databaseScript) Driver() driver.Driver                        { return scriptDriver{s} }

type scriptDriver struct{ script *databaseScript }

func (d scriptDriver) Open(string) (driver.Conn, error) { return scriptConn{d.script}, nil }

type scriptConn struct{ script *databaseScript }

func (scriptConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (scriptConn) Close() error                        { return nil }
func (scriptConn) Begin() (driver.Tx, error)           { return nil, errors.New("unsupported") }

func (c scriptConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	step, err := c.script.take(query, false, args)
	if err != nil {
		return nil, err
	}
	return &scriptRows{columns: step.columns, values: step.rows, nextErr: step.nextErr}, nil
}

func (c scriptConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	step, err := c.script.take(query, true, args)
	if err != nil {
		return nil, err
	}
	if step.result == nil {
		return driver.RowsAffected(1), nil
	}
	return step.result, nil
}

type scriptRows struct {
	columns []string
	values  [][]driver.Value
	next    int
	nextErr error
}

func (r *scriptRows) Columns() []string { return r.columns }
func (*scriptRows) Close() error        { return nil }
func (r *scriptRows) Next(values []driver.Value) error {
	if r.next == len(r.values) {
		if r.nextErr != nil {
			return r.nextErr
		}
		return io.EOF
	}
	copy(values, r.values[r.next])
	r.next++
	return nil
}

func testRepository(t *testing.T, steps ...databaseStep) *Repository {
	t.Helper()
	script := &databaseScript{t: t, steps: steps}
	db := sql.OpenDB(script)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
		if script.next != len(script.steps) {
			t.Errorf("consumed %d of %d database operations", script.next, len(script.steps))
		}
	})
	repository, err := NewRepository(db, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func metadataStep(statement, name string) databaseStep {
	return databaseStep{
		statement: statement,
		columns:   []string{"id", "name", "host", "port", "database_name", "username", "ssl_mode", "created_at", "updated_at"},
		rows:      [][]driver.Value{{testDatabaseID, name, "localhost", int64(5432), "app", "reader", "require", testCreatedAt, testCreatedAt}},
	}
}

type stateError string

func (e stateError) Error() string    { return "driver detail with private-password" }
func (e stateError) SQLState() string { return string(e) }

type badResult struct{}

func (badResult) LastInsertId() (int64, error) { return 0, errors.New("unsupported") }
func (badResult) RowsAffected() (int64, error) { return 0, errors.New("private-password") }
