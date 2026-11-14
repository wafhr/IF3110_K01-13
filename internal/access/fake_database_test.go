package access

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
	"time"
)

const (
	testSessionToken = "test-session-token"
	testCSRFToken    = "test-csrf-token"
	testUserID       = "00000000-0000-4000-8000-000000000001"
	testDatabaseID   = "00000000-0000-4000-8000-000000000002"
	testGroupID      = "00000000-0000-4000-8000-000000000003"
	testUsername     = "reader"

	sessionLookup    = "FROM access_sessions s JOIN access_users u"
	permissionLookup = "bool_or(p.permission = 'view_edit')"
)

var sessionColumns = []string{
	"token_hash", "is_current", "grace_token", "grace_expiry", "csrf_token", "last_seen_at", "rotated_at",
	"rotation_required", "absolute_expires_at", "user_id", "username", "is_platform_admin", "is_active", "created_at",
}

type statement struct {
	query string
	args  []driver.Value
}

type queryRule struct {
	fragment string
	columns  []string
	rows     [][]driver.Value
	err      error
}

type execRule struct {
	fragment string
	affected int64
	err      error
}

// fakeDatabase answers SQL by matching statement fragments, records every
// executed statement, and treats unmatched writes as affecting one row.
type fakeDatabase struct {
	mu         sync.Mutex
	queries    []queryRule
	execs      []execRule
	statements []statement
}

func newFakeDatabase() *fakeDatabase { return &fakeDatabase{} }

func (f *fakeDatabase) onQuery(fragment string, columns []string, rows ...[]driver.Value) *fakeDatabase {
	f.queries = append(f.queries, queryRule{fragment: fragment, columns: columns, rows: rows})
	return f
}

func (f *fakeDatabase) failQuery(fragment string, err error) *fakeDatabase {
	f.queries = append(f.queries, queryRule{fragment: fragment, err: err})
	return f
}

func (f *fakeDatabase) onExec(fragment string, affected int64) *fakeDatabase {
	f.execs = append(f.execs, execRule{fragment: fragment, affected: affected})
	return f
}

func (f *fakeDatabase) failExec(fragment string, err error) *fakeDatabase {
	f.execs = append(f.execs, execRule{fragment: fragment, err: err})
	return f
}

func (f *fakeDatabase) withSession(row sessionRow) *fakeDatabase {
	return f.onQuery(sessionLookup, sessionColumns, row.values())
}

func (f *fakeDatabase) withoutSession() *fakeDatabase {
	return f.onQuery(sessionLookup, sessionColumns)
}

func (f *fakeDatabase) withPermission(permission Permission) *fakeDatabase {
	if permission == "" {
		return f.onQuery(permissionLookup, []string{"permission"})
	}
	return f.onQuery(permissionLookup, []string{"permission"}, []driver.Value{string(permission)})
}

func (f *fakeDatabase) executed(fragment string) []statement {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matches []statement
	for _, s := range f.statements {
		if strings.Contains(s.query, fragment) {
			matches = append(matches, s)
		}
	}
	return matches
}

func (f *fakeDatabase) ran(fragment string) bool { return len(f.executed(fragment)) > 0 }

func (f *fakeDatabase) open(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(fakeConnector{database: f})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (f *fakeDatabase) record(query string, args []driver.NamedValue) {
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statements = append(f.statements, statement{query: query, args: values})
}

func (f *fakeDatabase) query(query string, args []driver.NamedValue) (driver.Rows, error) {
	f.record(query, args)
	for _, rule := range f.queries {
		if strings.Contains(query, rule.fragment) {
			if rule.err != nil {
				return nil, rule.err
			}
			return &fakeRows{columns: rule.columns, rows: rule.rows}, nil
		}
	}
	return nil, fmt.Errorf("unexpected query: %s", query)
}

func (f *fakeDatabase) exec(query string, args []driver.NamedValue) (driver.Result, error) {
	f.record(query, args)
	for _, rule := range f.execs {
		if strings.Contains(query, rule.fragment) {
			if rule.err != nil {
				return nil, rule.err
			}
			return driver.RowsAffected(rule.affected), nil
		}
	}
	return driver.RowsAffected(1), nil
}

type sessionRow struct {
	current          bool
	graceToken       string
	graceExpiry      time.Time
	lastSeen         time.Time
	rotatedAt        time.Time
	rotationRequired bool
	absoluteExpiry   time.Time
	platformAdmin    bool
	inactive         bool
}

func activeSession(now time.Time) sessionRow {
	return sessionRow{current: true, lastSeen: now.Add(-time.Minute), rotatedAt: now.Add(-time.Minute), absoluteExpiry: now.Add(time.Hour)}
}

func (r sessionRow) values() []driver.Value {
	var graceToken, graceExpiry driver.Value
	if r.graceToken != "" {
		graceToken, graceExpiry = r.graceToken, r.graceExpiry
	}
	return []driver.Value{
		tokenHash(testSessionToken), r.current, graceToken, graceExpiry, testCSRFToken, r.lastSeen, r.rotatedAt,
		r.rotationRequired, r.absoluteExpiry, testUserID, testUsername, r.platformAdmin, !r.inactive, r.lastSeen,
	}
}

type sqlStateError string

func (e sqlStateError) Error() string    { return "sql state " + string(e) }
func (e sqlStateError) SQLState() string { return string(e) }

var errDatabaseDown = errors.New("connection refused by db.internal:5432")

type fakeConnector struct{ database *fakeDatabase }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) { return fakeConn(c), nil }
func (c fakeConnector) Driver() driver.Driver                        { return fakeDriver(c) }

type fakeDriver struct{ database *fakeDatabase }

func (d fakeDriver) Open(string) (driver.Conn, error) { return fakeConn(d), nil }

type fakeConn struct{ database *fakeDatabase }

func (fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported")
}
func (fakeConn) Close() error              { return nil }
func (fakeConn) Begin() (driver.Tx, error) { return fakeTx{}, nil }
func (fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return fakeTx{}, nil
}

func (c fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.database.query(query, args)
}

func (c fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.database.exec(query, args)
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeRows struct {
	columns []string
	rows    [][]driver.Value
	index   int
}

func (r *fakeRows) Columns() []string { return r.columns }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(destination []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(destination, r.rows[r.index])
	r.index++
	return nil
}

func newTestService(t *testing.T, database *fakeDatabase, now time.Time) *Service {
	t.Helper()
	return &Service{db: database.open(t), now: func() time.Time { return now }}
}
