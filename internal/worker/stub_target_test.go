package worker

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

type stubCall struct {
	query string
	args  []driver.Value
}

type stubQuery struct {
	fragment  string
	rows      [][]driver.Value
	remaining int
}

// stubTarget is a target database that records statements, arguments, and
// transaction outcomes, answers queries by statement fragment, and injects
// failures.
type stubTarget struct {
	mu         sync.Mutex
	calls      []stubCall
	queries    []stubQuery
	isolation  driver.IsolationLevel
	execErrors map[string]error
	commitErr  error
	commits    int
	rollbacks  int
}

func newStubTarget() *stubTarget { return &stubTarget{execErrors: map[string]error{}} }

func (s *stubTarget) onQuery(fragment string, rows ...[]driver.Value) *stubTarget {
	s.queries = append(s.queries, stubQuery{fragment: fragment, rows: rows})
	return s
}

// onQueryOnce answers the first matching query only; later ones fall through
// to the next matching rule.
func (s *stubTarget) onQueryOnce(fragment string, rows ...[]driver.Value) *stubTarget {
	s.queries = append(s.queries, stubQuery{fragment: fragment, rows: rows, remaining: 1})
	return s
}

func (s *stubTarget) open(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(stubTargetConnector{target: s})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (s *stubTarget) ran(fragment string) bool { return len(s.executed(fragment)) > 0 }

func (s *stubTarget) executed(fragment string) []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matches []stubCall
	for _, call := range s.calls {
		if strings.Contains(call.query, fragment) {
			matches = append(matches, call)
		}
	}
	return matches
}

func (s *stubTarget) order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	statements := make([]string, len(s.calls))
	for i, call := range s.calls {
		statements[i] = call.query
	}
	return statements
}

func (s *stubTarget) record(query string, args []driver.NamedValue) {
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubCall{query: query, args: values})
}

func (s *stubTarget) exec(query string, args []driver.NamedValue) error {
	s.record(query, args)
	s.mu.Lock()
	defer s.mu.Unlock()
	for fragment, err := range s.execErrors {
		if strings.Contains(query, fragment) {
			return err
		}
	}
	return nil
}

func (s *stubTarget) query(query string, args []driver.NamedValue) (driver.Rows, error) {
	s.record(query, args)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, rule := range s.queries {
		if rule.remaining < 0 || !strings.Contains(query, rule.fragment) {
			continue
		}
		if rule.remaining == 1 {
			s.queries[i].remaining = -1
		}
		return &stubTargetRows{rows: rule.rows}, nil
	}
	return nil, fmt.Errorf("unexpected query: %s", query)
}

func (s *stubTarget) finish(statement string, commit bool) error {
	s.record(statement, nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !commit {
		s.rollbacks++
		return nil
	}
	if s.commitErr != nil {
		return s.commitErr
	}
	s.commits++
	return nil
}

type stubTargetConnector struct{ target *stubTarget }

func (c stubTargetConnector) Connect(context.Context) (driver.Conn, error) {
	return stubTargetConn(c), nil
}
func (c stubTargetConnector) Driver() driver.Driver { return stubTargetDriver(c) }

type stubTargetDriver struct{ target *stubTarget }

func (d stubTargetDriver) Open(string) (driver.Conn, error) { return stubTargetConn(d), nil }

type stubTargetConn struct{ target *stubTarget }

func (stubTargetConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported")
}
func (stubTargetConn) Close() error              { return nil }
func (stubTargetConn) Begin() (driver.Tx, error) { return nil, errors.New("use BeginTx") }

func (c stubTargetConn) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.target.mu.Lock()
	c.target.isolation = options.Isolation
	c.target.mu.Unlock()
	c.target.record("BEGIN", nil)
	return stubTargetTx(c), nil
}

func (c stubTargetConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.target.exec(query, args); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

func (c stubTargetConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.target.query(query, args)
}

type stubTargetTx struct{ target *stubTarget }

func (t stubTargetTx) Commit() error   { return t.target.finish("COMMIT", true) }
func (t stubTargetTx) Rollback() error { return t.target.finish("ROLLBACK", false) }

type stubTargetRows struct {
	rows  [][]driver.Value
	index int
}

func (r *stubTargetRows) Columns() []string {
	if len(r.rows) == 0 {
		return []string{"value"}
	}
	columns := make([]string, len(r.rows[0]))
	for i := range columns {
		columns[i] = fmt.Sprintf("column%d", i)
	}
	return columns
}
func (r *stubTargetRows) Close() error { return nil }
func (r *stubTargetRows) Next(destination []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(destination, r.rows[r.index])
	r.index++
	return nil
}
