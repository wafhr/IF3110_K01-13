package telemetry_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

type sqlFixture struct {
	execErr, queryErr, beginErr, prepareErr, commitErr, rollbackErr error
	connectErr, pingErr                                             error
	result                                                          driver.Result
	rows                                                            func() driver.Rows
	query                                                           string
	args                                                            []driver.NamedValue
	options                                                         driver.TxOptions
	commits, rollbacks, preparedExecs, calls                        int
	skipExec                                                        bool
	badConnections                                                  int
}

func (f *sqlFixture) Connect(context.Context) (driver.Conn, error) {
	return sqlFixtureConn{f}, f.connectErr
}
func (f *sqlFixture) Driver() driver.Driver { return sqlFixtureDriver{f} }

type sqlFixtureDriver struct{ fixture *sqlFixture }

func (d sqlFixtureDriver) Open(string) (driver.Conn, error) {
	return d.fixture.Connect(context.Background())
}

type sqlFixtureConn struct{ fixture *sqlFixture }

func (c sqlFixtureConn) Close() error { return nil }
func (c sqlFixtureConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}
func (c sqlFixtureConn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	return sqlFixtureStmt{fixture: c.fixture, query: query}, c.fixture.prepareErr
}
func (c sqlFixtureConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c sqlFixtureConn) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.fixture.options = options
	return sqlFixtureTx(c), c.fixture.beginErr
}
func (c sqlFixtureConn) Ping(context.Context) error { return c.fixture.pingErr }
func (c sqlFixtureConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	f := c.fixture
	f.calls++
	f.query, f.args = query, append([]driver.NamedValue(nil), args...)
	if f.badConnections > 0 {
		f.badConnections--
		return nil, driver.ErrBadConn
	}
	if f.skipExec {
		return nil, driver.ErrSkip
	}
	if f.result != nil {
		return f.result, f.execErr
	}
	return driver.RowsAffected(3), f.execErr
}
func (c sqlFixtureConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.fixture.query, c.fixture.args = query, append([]driver.NamedValue(nil), args...)
	if c.fixture.queryErr != nil {
		return nil, c.fixture.queryErr
	}
	if c.fixture.rows != nil {
		return c.fixture.rows(), nil
	}
	return &sqlFixtureRows{values: []int64{11, 22}}, nil
}

type sqlFixtureStmt struct {
	fixture *sqlFixture
	query   string
}

func (s sqlFixtureStmt) Close() error  { return nil }
func (s sqlFixtureStmt) NumInput() int { return -1 }
func (s sqlFixtureStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("use ExecContext")
}
func (s sqlFixtureStmt) Query([]driver.Value) (driver.Rows, error) {
	return nil, errors.New("use QueryContext")
}
func (s sqlFixtureStmt) ExecContext(_ context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.fixture.preparedExecs++
	s.fixture.query, s.fixture.args = s.query, append([]driver.NamedValue(nil), args...)
	return driver.RowsAffected(4), s.fixture.execErr
}
func (s sqlFixtureStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return (sqlFixtureConn{s.fixture}).QueryContext(ctx, s.query, args)
}

type sqlFixtureTx struct{ fixture *sqlFixture }

func (tx sqlFixtureTx) Commit() error   { tx.fixture.commits++; return tx.fixture.commitErr }
func (tx sqlFixtureTx) Rollback() error { tx.fixture.rollbacks++; return tx.fixture.rollbackErr }

type sqlFixtureRows struct {
	values                        []int64
	index                         int
	nextErr, closeErr, nextSetErr error
	nextSet                       bool
}

func (r *sqlFixtureRows) Columns() []string { return []string{"id"} }
func (r *sqlFixtureRows) Close() error      { return r.closeErr }
func (r *sqlFixtureRows) Next(values []driver.Value) error {
	if r.index < len(r.values) {
		values[0] = r.values[r.index]
		r.index++
		return nil
	}
	if r.nextErr != nil {
		return r.nextErr
	}
	return io.EOF
}
func (r *sqlFixtureRows) HasNextResultSet() bool { return r.nextSet }
func (r *sqlFixtureRows) NextResultSet() error {
	r.nextSet = false
	if r.nextSetErr != nil {
		return r.nextSetErr
	}
	r.values, r.index = []int64{33}, 0
	return nil
}
func (*sqlFixtureRows) ColumnTypeDatabaseTypeName(int) string             { return "INT8" }
func (*sqlFixtureRows) ColumnTypeScanType(int) reflect.Type               { return reflect.TypeOf(int64(0)) }
func (*sqlFixtureRows) ColumnTypeLength(int) (int64, bool)                { return 0, false }
func (*sqlFixtureRows) ColumnTypeNullable(int) (bool, bool)               { return false, true }
func (*sqlFixtureRows) ColumnTypePrecisionScale(int) (int64, int64, bool) { return 0, 0, false }

type unavailableRowCount struct{}

func (unavailableRowCount) LastInsertId() (int64, error) { return 77, nil }
func (unavailableRowCount) RowsAffected() (int64, error) {
	return 0, errors.New("unsupported row count")
}

func findSQLRecord(t *testing.T, records []map[string]any, event string) map[string]any {
	t.Helper()
	for _, record := range records {
		if record["event"] == event {
			return record
		}
	}
	t.Fatalf("event %s tidak tercatat", event)
	return nil
}

func openSQLFixture(t *testing.T, fixture *sqlFixture, system, role string) *sql.DB {
	t.Helper()
	db := sql.OpenDB(telemetry.SQLConnector(fixture, system, role, "application"))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLExecPreservesInputsAndCorrelatesLogs(t *testing.T) {
	recorder, logs := setupHTTPTracing(t)
	fixture := &sqlFixture{}
	db := openSQLFixture(t, fixture, "postgresql", "platform")
	ctx, parent := otel.Tracer("test").Start(context.Background(), "request")
	defer parent.End()
	query := "UPDATE private_table SET private_column = $1 WHERE id = $2 /* fixture-comment-secret */"
	result, err := db.ExecContext(ctx, query, "fixture-parameter-secret", int64(42))
	if err != nil {
		t.Fatal(err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 3 {
		t.Fatal("hasil driver berubah")
	}
	if fixture.query != query || fixture.args[0].Value != "fixture-parameter-secret" || fixture.args[1].Value != int64(42) {
		t.Fatal("SQL atau parameter driver berubah")
	}
	records := logRecords(t, logs.String())
	started := findSQLRecord(t, records, "sql_exec_started")
	completed := findSQLRecord(t, records, "sql_exec_completed")
	if completed["rows_affected"] != float64(3) || completed["parameter_count"] != float64(2) || completed["status"] != "success" || completed["database_role"] != "platform" || completed["database"] != "application" || completed["parameters"] != telemetry.Redacted {
		t.Fatal("atribut SQL tidak sesuai")
	}
	if started["trace_id"] != parent.SpanContext().TraceID().String() || started["trace_id"] != completed["trace_id"] || started["span_id"] != completed["span_id"] {
		t.Fatal("log SQL kehilangan korelasi")
	}
	encoded, _ := json.Marshal(tracetest.SpanStubsFromReadOnlySpans(recorder.Ended()))
	for _, secret := range []string{"private_table", "private_column", "fixture-comment-secret", "fixture-parameter-secret"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(string(encoded), secret) {
			t.Fatal("SQL sensitif muncul pada log atau span")
		}
	}
	found := false
	for _, span := range recorder.Ended() {
		if span.Name() == "db.exec" {
			found = true
			if span.SpanKind() != trace.SpanKindClient || span.Parent().SpanID() != parent.SpanContext().SpanID() {
				t.Fatal("span SQL tidak menjadi child request")
			}
		}
	}
	if !found {
		t.Fatal("span exec tidak tercatat")
	}
}

func TestSQLQueriesCountRowsAndPreserveMetadata(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			recorder, logs := setupHTTPTracing(t)
			fixture := &sqlFixture{}
			db := openSQLFixture(t, fixture, "mysql", "target")
			rows, err := db.QueryContext(context.Background(), "SELECT secret_column FROM secret_table")
			if err != nil {
				t.Fatal(err)
			}
			columns, err := rows.ColumnTypes()
			if err != nil || len(columns) != 1 || columns[0].DatabaseTypeName() != "INT8" || columns[0].ScanType() != reflect.TypeOf(int64(0)) {
				t.Fatal("metadata kolom berubah")
			}
			count := 0
			for rows.Next() {
				var value int64
				if err := rows.Scan(&value); err != nil {
					t.Fatal(err)
				}
				count++
				if partial {
					break
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			completed := findSQLRecord(t, logRecords(t, logs.String()), "sql_query_completed")
			if completed["rows_read"] != float64(count) || completed["rows_complete"] != !partial || completed["status"] != "success" {
				t.Fatal("jumlah row atau kelengkapan hasil tidak sesuai")
			}
			for _, span := range recorder.Ended() {
				if span.Name() == "db.query" && span.Status().Code != codes.Unset {
					t.Fatal("query sukses ditandai gagal")
				}
			}
		})
	}
}

func TestSQLReportsSafeErrorsAndPreservesOriginalError(t *testing.T) {
	for _, test := range []struct {
		name            string
		err             error
		state, category string
	}{
		{"postgres", &pgconn.PgError{Code: "23505", Message: "fixture-private-detail"}, "23505", "sql_error"},
		{"mysql", &drivermysql.MySQLError{SQLState: [5]byte{'4', '2', '0', '0', '0'}, Message: "fixture-private-detail"}, "42000", "sql_error"},
		{"wrapped", fmt.Errorf("fixture-private-detail: %w", &pgconn.PgError{Code: "42501"}), "42501", "sql_error"},
		{"canceled", context.Canceled, "", "canceled"},
		{"timeout", context.DeadlineExceeded, "", "timeout"},
		{"invalid state", &pgconn.PgError{Code: "fixture-private-detail", Message: "fixture-private-detail"}, "", "driver_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder, logs := setupHTTPTracing(t)
			db := openSQLFixture(t, &sqlFixture{execErr: test.err}, "postgresql", "target")
			_, err := db.ExecContext(context.Background(), "SELECT 'fixture-literal-secret'")
			if err != test.err {
				t.Fatal("error driver diubah")
			}
			completed := findSQLRecord(t, logRecords(t, logs.String()), "sql_exec_completed")
			if completed["status"] != "failed" || completed["error"] != test.category || completed["level"] != "ERROR" {
				t.Fatal("kategori error tidak sesuai")
			}
			if test.state != "" && completed["sqlstate"] != test.state {
				t.Fatal("SQLSTATE tidak tercatat")
			}
			if test.state == "" {
				if _, exists := completed["sqlstate"]; exists {
					t.Fatal("SQLSTATE tidak valid dicatat")
				}
			}
			encoded, _ := json.Marshal(tracetest.SpanStubsFromReadOnlySpans(recorder.Ended()))
			if strings.Contains(logs.String(), "fixture-private-detail") || strings.Contains(string(encoded), "fixture-private-detail") {
				t.Fatal("pesan mentah driver bocor")
			}
		})
	}
}

func TestSQLPreparedStatementsFallbackAndRetry(t *testing.T) {
	for _, test := range []struct {
		name string
		skip bool
		bad  int
	}{{"prepared fallback", true, 0}, {"retry", false, 1}} {
		t.Run(test.name, func(t *testing.T) {
			_, logs := setupHTTPTracing(t)
			fixture := &sqlFixture{skipExec: test.skip, badConnections: test.bad}
			db := openSQLFixture(t, fixture, "postgresql", "target")
			result, err := db.ExecContext(context.Background(), "UPDATE table_name SET value = $1", "fixture-argument")
			if err != nil {
				t.Fatal(err)
			}
			count, err := result.RowsAffected()
			if err != nil || (test.skip && count != 4) || (!test.skip && count != 3) {
				t.Fatal("fallback atau retry mengubah hasil")
			}
			if test.skip && fixture.preparedExecs != 1 {
				t.Fatal("prepared fallback tidak dijalankan")
			}
			if test.bad > 0 && fixture.calls != 2 {
				t.Fatal("retry ErrBadConn berubah")
			}
			if test.skip && strings.Contains(logs.String(), `"status":"failed"`) {
				t.Fatal("ErrSkip ditandai sebagai kegagalan")
			}
			stmt, err := db.PrepareContext(context.Background(), "SELECT value FROM table_name WHERE id = $1")
			if err != nil {
				t.Fatal(err)
			}
			defer stmt.Close()
			rows, err := stmt.QueryContext(context.Background(), int64(1))
			if err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSQLTransactionsRecordFinalOutcomeSeparately(t *testing.T) {
	for _, test := range []struct {
		name    string
		commit  bool
		failure error
	}{
		{"commit", true, nil}, {"rollback", false, nil},
		{"failed commit", true, &pgconn.PgError{Code: "40001", Message: "fixture-private-detail"}},
		{"failed rollback", false, errors.New("fixture-private-detail")},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, logs := setupHTTPTracing(t)
			fixture := &sqlFixture{commitErr: test.failure, rollbackErr: test.failure}
			db := openSQLFixture(t, fixture, "postgresql", "platform")
			tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
			if err != nil {
				t.Fatal(err)
			}
			if !fixture.options.ReadOnly || fixture.options.Isolation != driver.IsolationLevel(sql.LevelRepeatableRead) {
				t.Fatal("opsi transaksi berubah")
			}
			if _, err := tx.ExecContext(context.Background(), "SELECT 1"); err != nil {
				t.Fatal(err)
			}
			operation := "rollback"
			if test.commit {
				operation, err = "commit", tx.Commit()
			} else {
				err = tx.Rollback()
			}
			if err != test.failure {
				t.Fatal("hasil transaksi berubah")
			}
			_ = tx.Rollback()
			records := logRecords(t, logs.String())
			findSQLRecord(t, records, "sql_begin_completed")
			findSQLRecord(t, records, "sql_exec_completed")
			completed := findSQLRecord(t, records, "sql_"+operation+"_completed")
			if (completed["status"] == "failed") != (test.failure != nil) || fixture.commits+fixture.rollbacks != 1 {
				t.Fatal("hasil akhir atau jumlah operasi transaksi tidak sesuai")
			}
		})
	}
}

func TestSQLStreamingAndMultipleResultErrors(t *testing.T) {
	failure := &pgconn.PgError{Code: "22000", Message: "fixture-private-detail"}
	for _, test := range []struct {
		name          string
		rows          *sqlFixtureRows
		expectedCount int
		failed        bool
	}{
		{"next error", &sqlFixtureRows{values: []int64{1}, nextErr: failure}, 1, true},
		{"close error", &sqlFixtureRows{values: []int64{1}, closeErr: failure}, 1, true},
		{"result set error", &sqlFixtureRows{values: []int64{1}, nextSet: true, nextSetErr: failure}, 1, true},
		{"multiple results", &sqlFixtureRows{values: []int64{1, 2}, nextSet: true}, 3, false},
		{"empty last result", &sqlFixtureRows{values: []int64{1}, nextSet: true, nextSetErr: io.EOF}, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, logs := setupHTTPTracing(t)
			db := openSQLFixture(t, &sqlFixture{rows: func() driver.Rows { return test.rows }}, "mysql", "target")
			rows, err := db.QueryContext(context.Background(), "SELECT 1; SELECT 2")
			if err != nil {
				t.Fatal(err)
			}
			for {
				for rows.Next() {
					var value int64
					if err := rows.Scan(&value); err != nil {
						t.Fatal(err)
					}
				}
				if !rows.NextResultSet() {
					break
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if test.failed && err != failure {
				t.Fatal("error streaming tidak diteruskan")
			}
			completed := findSQLRecord(t, logRecords(t, logs.String()), "sql_query_completed")
			if completed["rows_read"] != float64(test.expectedCount) || (completed["status"] == "failed") != test.failed || (!test.failed && completed["rows_complete"] != true) {
				t.Fatal("hasil streaming tidak tercatat dengan benar")
			}
		})
	}
}

func TestSQLUnavailableRowCountKeepsResultUsable(t *testing.T) {
	_, logs := setupHTTPTracing(t)
	db := openSQLFixture(t, &sqlFixture{result: unavailableRowCount{}}, "mysql", "target")
	result, err := db.ExecContext(context.Background(), "INSERT INTO table_name VALUES (1)")
	if err != nil {
		t.Fatal(err)
	}
	if id, err := result.LastInsertId(); err != nil || id != 77 {
		t.Fatal("LastInsertId berubah")
	}
	completed := findSQLRecord(t, logRecords(t, logs.String()), "sql_exec_completed")
	if _, exists := completed["rows_affected"]; exists {
		t.Fatal("row count dibuat saat tidak tersedia")
	}
	if completed["status"] != "success" {
		t.Fatal("row count yang tidak tersedia membuat query gagal")
	}
}

func TestSQLSetupAndInitialQueryFailures(t *testing.T) {
	failure := &pgconn.PgError{Code: "42501", Message: "fixture-private-detail"}
	for _, operation := range []string{"connect", "ping", "begin", "prepare", "query"} {
		t.Run(operation, func(t *testing.T) {
			recorder, logs := setupHTTPTracing(t)
			fixture := &sqlFixture{}
			db := openSQLFixture(t, fixture, "postgresql", "platform")
			var err error
			switch operation {
			case "connect":
				fixture.connectErr = failure
				err = db.PingContext(context.Background())
			case "ping":
				fixture.pingErr = failure
				err = db.PingContext(context.Background())
			case "begin":
				fixture.beginErr = failure
				_, err = db.BeginTx(context.Background(), nil)
			case "prepare":
				fixture.prepareErr = failure
				_, err = db.PrepareContext(context.Background(), "SELECT 1")
			case "query":
				fixture.queryErr = failure
				_, err = db.QueryContext(context.Background(), "SELECT 1")
			}
			if err != failure {
				t.Fatal("error driver awal tidak diteruskan")
			}
			record := findSQLRecord(t, logRecords(t, logs.String()), "sql_"+operation+"_completed")
			if record["status"] != "failed" || record["sqlstate"] != "42501" {
				t.Fatal("kegagalan awal tidak tercatat")
			}
			found := false
			for _, span := range recorder.Ended() {
				if span.Name() == "db."+operation && span.Status().Code == codes.Error {
					found = true
				}
			}
			if !found {
				t.Fatal("span kegagalan tidak diakhiri")
			}
		})
	}
}

func TestSQLEmptyStatementIncludesQueryMetadata(t *testing.T) {
	_, logs := setupHTTPTracing(t)
	db := openSQLFixture(t, &sqlFixture{}, "postgresql", "target")
	if _, err := db.ExecContext(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	record := findSQLRecord(t, logRecords(t, logs.String()), "sql_exec_completed")
	if record["query_template"] != "" || record["parameter_count"] != float64(0) || record["parameters"] != telemetry.Redacted {
		t.Fatal("metadata SQL kosong tidak lengkap")
	}
}

func TestSQLCanceledReaderRecordsFailureWhenClosed(t *testing.T) {
	_, logs := setupHTTPTracing(t)
	db := openSQLFixture(t, &sqlFixture{}, "postgresql", "target")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, err := db.QueryContext(ctx, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = rows.Close()
	record := findSQLRecord(t, logRecords(t, logs.String()), "sql_query_completed")
	if record["status"] != "failed" || record["error"] != "canceled" || record["rows_complete"] != false {
		t.Fatal("reader yang dibatalkan tercatat sukses")
	}
}
