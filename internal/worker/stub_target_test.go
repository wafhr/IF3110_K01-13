package worker

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
)

// stubTarget is a target database that records statements and transaction
// outcomes, with injectable failures.
type stubTarget struct {
	mu         sync.Mutex
	statements []string
	isolation  driver.IsolationLevel
	execErrors map[string]error
	commitErr  error
	committed  bool
	rolledBack bool
}

func newStubTarget() *stubTarget { return &stubTarget{execErrors: map[string]error{}} }

func (s *stubTarget) open(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(stubTargetConnector{target: s})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (s *stubTarget) ran(fragment string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, statement := range s.statements {
		if strings.Contains(statement, fragment) {
			return true
		}
	}
	return false
}

func (s *stubTarget) order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.statements...)
}

func (s *stubTarget) exec(query string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statements = append(s.statements, query)
	for fragment, err := range s.execErrors {
		if strings.Contains(query, fragment) {
			return err
		}
	}
	return nil
}

func (s *stubTarget) finish(statement string, commit bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statements = append(s.statements, statement)
	if commit {
		s.committed = s.commitErr == nil
		return s.commitErr
	}
	s.rolledBack = true
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
	c.target.statements = append(c.target.statements, "BEGIN")
	c.target.mu.Unlock()
	return stubTargetTx(c), nil
}

func (c stubTargetConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if err := c.target.exec(query); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

type stubTargetTx struct{ target *stubTarget }

func (t stubTargetTx) Commit() error   { return t.target.finish("COMMIT", true) }
func (t stubTargetTx) Rollback() error { return t.target.finish("ROLLBACK", false) }
