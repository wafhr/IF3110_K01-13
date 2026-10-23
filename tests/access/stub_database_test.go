package access_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"time"
)

const (
	testSessionToken = "test-session-token"
	testUserID       = "00000000-0000-4000-8000-000000000001"
)

type stubConnector struct {
	permission    string
	platformAdmin bool
}

func newStubConnector(permission string, platformAdmin bool) stubConnector {
	return stubConnector{permission: permission, platformAdmin: platformAdmin}
}

func (c stubConnector) Connect(context.Context) (driver.Conn, error) { return stubConn{config: c}, nil }
func (c stubConnector) Driver() driver.Driver                        { return stubDriver{config: c} }

type stubDriver struct{ config stubConnector }

func (d stubDriver) Open(string) (driver.Conn, error) { return stubConn{config: d.config}, nil }

type stubConn struct{ config stubConnector }

func (stubConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are unsupported")
}
func (stubConn) Close() error              { return nil }
func (stubConn) Begin() (driver.Tx, error) { return stubTx{}, nil }
func (stubConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return stubTx{}, nil
}

func (c stubConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "FROM access_sessions s JOIN access_users u"):
		if len(args) != 2 {
			return nil, errors.New("unexpected session query arguments")
		}
		providedHash, ok := args[0].Value.([]byte)
		wantHash := sha256.Sum256([]byte(testSessionToken))
		if !ok || !bytes.Equal(providedHash, wantHash[:]) {
			return nil, errors.New("unexpected session token hash")
		}
		now := time.Now().UTC()
		return &stubRows{
			columns: []string{"token_hash", "is_current", "grace_token", "grace_expiry", "csrf_token", "last_seen", "rotated_at", "absolute_expiry", "user_id", "username", "is_admin", "is_active", "created_at"},
			values:  [][]driver.Value{{providedHash, true, nil, nil, "test-csrf", now, now, now.Add(time.Hour), testUserID, "reader", c.config.platformAdmin, true, now}},
		}, nil
	case strings.Contains(query, "bool_or(p.permission = 'view_edit')"):
		rows := &stubRows{columns: []string{"permission"}}
		if c.config.permission != "" {
			rows.values = [][]driver.Value{{c.config.permission}}
		}
		return rows, nil
	default:
		return nil, errors.New("unexpected query in access-control test stub")
	}
}

func (stubConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

type stubTx struct{}

func (stubTx) Commit() error   { return nil }
func (stubTx) Rollback() error { return nil }

type stubRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *stubRows) Columns() []string { return r.columns }
func (r *stubRows) Close() error      { return nil }
func (r *stubRows) Next(destination []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(destination, r.values[r.index])
	r.index++
	return nil
}
