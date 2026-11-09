package telemetry

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/ngrok/sqlmw"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// SQLConnector menginstrumentasi koneksi tanpa registrasi nama driver global.
// System berisi postgresql/mysql, role berisi platform/target, dan database hanya nama.
func SQLConnector(parent driver.Connector, system, role, database string) driver.Connector {
	wrapped := sqlmw.Driver(sqlDriver{Connector: parent}, &sqlInterceptor{
		system: system, role: role, database: database,
	})
	// Adapter selalu mengembalikan connector asal tanpa melakukan parsing DSN.
	connector, _ := wrapped.(driver.DriverContext).OpenConnector("")
	return connector
}

type sqlDriver struct{ driver.Connector }

func (d sqlDriver) Open(string) (driver.Conn, error)               { return d.Connect(context.Background()) }
func (d sqlDriver) OpenConnector(string) (driver.Connector, error) { return d.Connector, nil }

type sqlInterceptor struct {
	sqlmw.NullInterceptor
	system, role, database string
}

type sqlActivity struct {
	ctx      context.Context
	span     trace.Span
	started  time.Time
	attrs    []slog.Attr
	rows     int64
	complete bool
	readErr  error
}

type sqlActivityKey struct{}

func (i *sqlInterceptor) start(ctx context.Context, operation, query string, parameterCount int) *sqlActivity {
	attrs := []slog.Attr{
		slog.String("database", i.database), slog.String("database_role", i.role),
		slog.String("db_system", i.system), slog.String("operation", operation),
	}
	spanAttrs := []attribute.KeyValue{
		attribute.String("db.system.name", i.system), attribute.String("db.namespace", i.database),
		attribute.String("db.operation.name", operation), attribute.String("database_role", i.role),
	}
	if operation == "exec" || operation == "query" || operation == "prepare" {
		template := SanitizeSQL(query, i.system)
		attrs = append(attrs, slog.String("query_template", template), slog.String("parameters", Redacted), slog.Int("parameter_count", parameterCount))
		spanAttrs = append(spanAttrs, attribute.String("db.query.summary", template), attribute.String("db.query.parameters", Redacted), attribute.Int("db.query.parameter_count", parameterCount))
	}
	ctx, span := otel.Tracer("github.com/wafhr/IF3110_K01-13/internal/telemetry/sql").Start(ctx, "db."+operation,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(spanAttrs...))
	activity := &sqlActivity{ctx: ctx, span: span, started: time.Now(), attrs: attrs}
	startedAttrs := append(append([]slog.Attr(nil), attrs...), slog.String("status", "running"))
	slog.LogAttrs(ctx, slog.LevelInfo, "sql_"+operation+"_started", startedAttrs...)
	return activity
}

func (a *sqlActivity) finish(operation string, err error, extra ...slog.Attr) {
	defer a.span.End()
	status, level := "success", slog.LevelInfo
	attrs := append(append([]slog.Attr(nil), a.attrs...), extra...)
	if errors.Is(err, driver.ErrSkip) {
		status = "skipped"
	} else if err != nil {
		status, level = "failed", slog.LevelError
		category, state := sqlError(err)
		attrs = append(attrs, slog.String("error", category))
		a.span.SetStatus(codes.Error, category)
		a.span.SetAttributes(attribute.String("error.type", category))
		a.span.RecordError(errors.New(category))
		if state != "" {
			attrs = append(attrs, slog.String("sqlstate", state))
			a.span.SetAttributes(attribute.String("db.response.status_code", state))
		}
	}
	attrs = append(attrs, slog.String("status", status), slog.Float64("duration_ms", float64(time.Since(a.started))/float64(time.Millisecond)))
	slog.LogAttrs(a.ctx, level, "sql_"+operation+"_completed", attrs...)
}

func (i *sqlInterceptor) ConnectorConnect(ctx context.Context, connector driver.Connector) (driver.Conn, error) {
	activity := i.start(ctx, "connect", "", 0)
	conn, err := connector.Connect(activity.ctx)
	activity.finish("connect", err)
	return conn, err
}

func (i *sqlInterceptor) ConnPing(ctx context.Context, conn driver.Pinger) error {
	activity := i.start(ctx, "ping", "", 0)
	err := conn.Ping(activity.ctx)
	activity.finish("ping", err)
	return err
}

func (i *sqlInterceptor) ConnBeginTx(ctx context.Context, conn driver.ConnBeginTx, options driver.TxOptions) (context.Context, driver.Tx, error) {
	activity := i.start(ctx, "begin", "", 0)
	tx, err := conn.BeginTx(activity.ctx, options)
	activity.finish("begin", err)
	return ctx, tx, err
}

func (i *sqlInterceptor) TxCommit(ctx context.Context, tx driver.Tx) error {
	activity := i.start(ctx, "commit", "", 0)
	err := tx.Commit()
	activity.finish("commit", err)
	return err
}

func (i *sqlInterceptor) TxRollback(ctx context.Context, tx driver.Tx) error {
	activity := i.start(ctx, "rollback", "", 0)
	err := tx.Rollback()
	activity.finish("rollback", err)
	return err
}

func (i *sqlInterceptor) ConnPrepareContext(ctx context.Context, conn driver.ConnPrepareContext, query string) (context.Context, driver.Stmt, error) {
	activity := i.start(ctx, "prepare", query, 0)
	stmt, err := conn.PrepareContext(activity.ctx, query)
	activity.finish("prepare", err)
	return ctx, stmt, err
}

func (i *sqlInterceptor) ConnExecContext(ctx context.Context, conn driver.ExecerContext, query string, args []driver.NamedValue) (driver.Result, error) {
	return i.exec(ctx, query, args, func(ctx context.Context) (driver.Result, error) { return conn.ExecContext(ctx, query, args) })
}

func (i *sqlInterceptor) StmtExecContext(ctx context.Context, stmt driver.StmtExecContext, query string, args []driver.NamedValue) (driver.Result, error) {
	return i.exec(ctx, query, args, func(ctx context.Context) (driver.Result, error) { return stmt.ExecContext(ctx, args) })
}

func (i *sqlInterceptor) exec(ctx context.Context, query string, args []driver.NamedValue, next func(context.Context) (driver.Result, error)) (driver.Result, error) {
	activity := i.start(ctx, "exec", query, len(args))
	result, err := next(activity.ctx)
	var attrs []slog.Attr
	if err == nil && result != nil {
		if count, countErr := result.RowsAffected(); countErr == nil {
			attrs = append(attrs, slog.Int64("rows_affected", count))
			activity.span.SetAttributes(attribute.Int64("db.response.affected_rows", count))
		}
	}
	activity.finish("exec", err, attrs...)
	return result, err
}

func (i *sqlInterceptor) ConnQueryContext(ctx context.Context, conn driver.QueryerContext, query string, args []driver.NamedValue) (context.Context, driver.Rows, error) {
	return i.query(ctx, query, args, func(ctx context.Context) (driver.Rows, error) { return conn.QueryContext(ctx, query, args) })
}

func (i *sqlInterceptor) StmtQueryContext(ctx context.Context, stmt driver.StmtQueryContext, query string, args []driver.NamedValue) (context.Context, driver.Rows, error) {
	return i.query(ctx, query, args, func(ctx context.Context) (driver.Rows, error) { return stmt.QueryContext(ctx, args) })
}

func (i *sqlInterceptor) query(ctx context.Context, query string, args []driver.NamedValue, next func(context.Context) (driver.Rows, error)) (context.Context, driver.Rows, error) {
	activity := i.start(ctx, "query", query, len(args))
	rows, err := next(activity.ctx)
	if err != nil {
		activity.finish("query", err)
		return ctx, nil, err
	}
	// Span query tetap terbuka selama pembacaan agar error streaming ikut tercatat.
	return context.WithValue(activity.ctx, sqlActivityKey{}, activity), sqlRows{Rows: rows, activity: activity}, nil
}

func (i *sqlInterceptor) RowsNext(ctx context.Context, rows driver.Rows, destination []driver.Value) error {
	err := rows.Next(destination)
	if activity, ok := ctx.Value(sqlActivityKey{}).(*sqlActivity); ok {
		switch {
		case err == nil:
			activity.rows++
		case errors.Is(err, io.EOF):
			next, multiple := rows.(driver.RowsNextResultSet)
			activity.complete = !multiple || !next.HasNextResultSet()
		default:
			activity.readErr = err
		}
	}
	return err
}

func (i *sqlInterceptor) RowsClose(ctx context.Context, rows driver.Rows) error {
	err := rows.Close()
	if activity, ok := ctx.Value(sqlActivityKey{}).(*sqlActivity); ok {
		failure := activity.readErr
		if failure == nil {
			failure = err
		}
		if failure == nil {
			failure = activity.ctx.Err()
		}
		activity.span.SetAttributes(attribute.Int64("db.response.returned_rows", activity.rows), attribute.Bool("db.response.rows_complete", activity.complete))
		activity.finish("query", failure, slog.Int64("rows_read", activity.rows), slog.Bool("rows_complete", activity.complete))
	}
	return err
}

// sqlRows menyimpan error antar-result-set dan meneruskan metadata kolom.
// sqlmw memilih interface opsional berdasarkan Rows asli dari Unwrap.
type sqlRows struct {
	driver.Rows
	activity *sqlActivity
}

func (r sqlRows) Unwrap() driver.Rows { return r.Rows }
func (r sqlRows) HasNextResultSet() bool {
	next, ok := r.Rows.(driver.RowsNextResultSet)
	return ok && next.HasNextResultSet()
}
func (r sqlRows) NextResultSet() error {
	err := r.Rows.(driver.RowsNextResultSet).NextResultSet()
	if errors.Is(err, io.EOF) {
		r.activity.complete = true
	} else if err != nil {
		r.activity.readErr = err
	}
	return err
}
func (r sqlRows) ColumnTypeDatabaseTypeName(index int) string {
	return r.Rows.(driver.RowsColumnTypeDatabaseTypeName).ColumnTypeDatabaseTypeName(index)
}
func (r sqlRows) ColumnTypeLength(index int) (int64, bool) {
	return r.Rows.(driver.RowsColumnTypeLength).ColumnTypeLength(index)
}
func (r sqlRows) ColumnTypeNullable(index int) (bool, bool) {
	return r.Rows.(driver.RowsColumnTypeNullable).ColumnTypeNullable(index)
}
func (r sqlRows) ColumnTypePrecisionScale(index int) (int64, int64, bool) {
	return r.Rows.(driver.RowsColumnTypePrecisionScale).ColumnTypePrecisionScale(index)
}
func (r sqlRows) ColumnTypeScanType(index int) reflect.Type {
	return r.Rows.(driver.RowsColumnTypeScanType).ColumnTypeScanType(index)
}

// Pesan driver tidak diteruskan karena dapat memuat SQL, parameter, dan credential.
func sqlError(err error) (category, state string) {
	var postgres interface{ SQLState() string }
	var mysql *drivermysql.MySQLError
	switch {
	case errors.As(err, &postgres):
		state = postgres.SQLState()
	case errors.As(err, &mysql):
		state = string(mysql.SQLState[:])
	}
	if len(state) != 5 {
		state = ""
	}
	for _, ch := range state {
		if (ch < '0' || ch > '9') && (ch < 'A' || ch > 'Z') {
			state = ""
			break
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled", state
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", state
	case errors.Is(err, driver.ErrBadConn):
		return "connection_error", state
	case state != "":
		return "sql_error", state
	default:
		return "driver_error", state
	}
}
