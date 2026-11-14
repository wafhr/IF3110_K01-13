package job

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

type statement struct {
	query string
	args  []driver.Value
}

type queryRule struct {
	fragment string
	rows     [][]driver.Value
	err      error
}

type execRule struct {
	fragment string
	affected int64
	err      error
}

// stubDatabase answers SQL by statement fragment and records what was executed.
type stubDatabase struct {
	mu         sync.Mutex
	queries    []queryRule
	execs      []execRule
	statements []statement
}

func (s *stubDatabase) onQuery(fragment string, rows ...[]driver.Value) *stubDatabase {
	s.queries = append(s.queries, queryRule{fragment: fragment, rows: rows})
	return s
}

func (s *stubDatabase) failQuery(fragment string, err error) *stubDatabase {
	s.queries = append(s.queries, queryRule{fragment: fragment, err: err})
	return s
}

func (s *stubDatabase) onExec(fragment string, affected int64) *stubDatabase {
	s.execs = append(s.execs, execRule{fragment: fragment, affected: affected})
	return s
}

func (s *stubDatabase) executed(fragment string) []statement {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matches []statement
	for _, st := range s.statements {
		if strings.Contains(st.query, fragment) {
			matches = append(matches, st)
		}
	}
	return matches
}

func (s *stubDatabase) repository(t *testing.T) *Repository {
	t.Helper()
	db := sql.OpenDB(stubConnector{database: s})
	t.Cleanup(func() { _ = db.Close() })
	repository, err := NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func (s *stubDatabase) record(query string, args []driver.NamedValue) {
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statements = append(s.statements, statement{query: query, args: values})
}

func (s *stubDatabase) query(query string, args []driver.NamedValue) (driver.Rows, error) {
	s.record(query, args)
	for _, rule := range s.queries {
		if strings.Contains(query, rule.fragment) {
			if rule.err != nil {
				return nil, rule.err
			}
			return &stubRows{rows: rule.rows}, nil
		}
	}
	return nil, fmt.Errorf("unexpected query: %s", query)
}

func (s *stubDatabase) exec(query string, args []driver.NamedValue) (driver.Result, error) {
	s.record(query, args)
	for _, rule := range s.execs {
		if strings.Contains(query, rule.fragment) {
			if rule.err != nil {
				return nil, rule.err
			}
			return driver.RowsAffected(rule.affected), nil
		}
	}
	return driver.RowsAffected(1), nil
}

var jobColumnNames = []string{
	"id", "payload", "status", "attempts", "committed_batches", "committed_rows",
	"total_rows", "last_error", "available_at", "created_at", "started_at", "finished_at",
}

type stubConnector struct{ database *stubDatabase }

func (c stubConnector) Connect(context.Context) (driver.Conn, error) { return stubConn(c), nil }
func (c stubConnector) Driver() driver.Driver                        { return stubDriver(c) }

type stubDriver struct{ database *stubDatabase }

func (d stubDriver) Open(string) (driver.Conn, error) { return stubConn(d), nil }

type stubConn struct{ database *stubDatabase }

func (stubConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported")
}
func (stubConn) Close() error              { return nil }
func (stubConn) Begin() (driver.Tx, error) { return stubTx{}, nil }
func (stubConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return stubTx{}, nil
}

func (c stubConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.database.query(query, args)
}

func (c stubConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.database.exec(query, args)
}

type stubTx struct{}

func (stubTx) Commit() error   { return nil }
func (stubTx) Rollback() error { return nil }

type stubRows struct {
	rows  [][]driver.Value
	index int
}

func (r *stubRows) Columns() []string { return jobColumnNames }
func (r *stubRows) Close() error      { return nil }
func (r *stubRows) Next(destination []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(destination, r.rows[r.index])
	r.index++
	return nil
}
