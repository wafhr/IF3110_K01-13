// Package testsql is a database/sql driver for unit tests. A Handler answers
// every statement, so a test controls results and failures without a real
// database. Prepared statements are not supported.
package testsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"testing"
)

// Handler answers the queries and statements a test database receives.
type Handler interface {
	Query(query string, args []driver.NamedValue) (driver.Rows, error)
	Exec(query string, args []driver.NamedValue) (driver.Result, error)
}

// TxHandler is implemented by handlers that observe transactions. Without
// it, Begin, Commit, and Rollback succeed silently.
type TxHandler interface {
	Begin(driver.TxOptions) error
	Commit() error
	Rollback() error
}

// Open returns a database backed by handler that is closed when the test ends.
func Open(t testing.TB, handler Handler) *sql.DB {
	t.Helper()
	db := sql.OpenDB(Connector(handler))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Connector returns a connector backed by handler, for tests that wrap it.
func Connector(handler Handler) driver.Connector { return connector{handler} }

// Values returns the argument values of a statement in order.
func Values(args []driver.NamedValue) []driver.Value {
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	return values
}

// Rows is a fixed result set. Without Names, columns are named column0,
// column1, ... after the width of the first row. Err, when set, is returned
// after the last row instead of io.EOF.
type Rows struct {
	Names  []string
	Values [][]driver.Value
	Err    error
	next   int
}

func (r *Rows) Columns() []string {
	if r.Names != nil || len(r.Values) == 0 {
		return r.Names
	}
	names := make([]string, len(r.Values[0]))
	for i := range names {
		names[i] = fmt.Sprintf("column%d", i)
	}
	return names
}

func (*Rows) Close() error { return nil }

func (r *Rows) Next(destination []driver.Value) error {
	if r.next >= len(r.Values) {
		if r.Err != nil {
			return r.Err
		}
		return io.EOF
	}
	copy(destination, r.Values[r.next])
	r.next++
	return nil
}

type connector struct{ handler Handler }

func (c connector) Connect(context.Context) (driver.Conn, error) { return conn(c), nil }
func (c connector) Driver() driver.Driver                        { return testDriver(c) }

type testDriver struct{ handler Handler }

func (d testDriver) Open(string) (driver.Conn, error) { return conn(d), nil }

type conn struct{ handler Handler }

func (conn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported")
}
func (conn) Close() error                { return nil }
func (c conn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }

func (c conn) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	if tx, ok := c.handler.(TxHandler); ok {
		if err := tx.Begin(options); err != nil {
			return nil, err
		}
		return tx, nil
	}
	return noTx{}, nil
}

func (c conn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.handler.Query(query, args)
}

func (c conn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.handler.Exec(query, args)
}

type noTx struct{}

func (noTx) Commit() error   { return nil }
func (noTx) Rollback() error { return nil }
