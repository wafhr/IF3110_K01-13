package worker_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
	"github.com/wafhr/IF3110_K01-13/internal/worker"
)

func setupWorkerTracing(t *testing.T) (*tracetest.SpanRecorder, *bytes.Buffer) {
	t.Helper()
	provider, err := telemetry.NewTracerProvider(io.Discard, "worker")
	if err != nil {
		t.Fatal(err)
	}
	recorder := tracetest.NewSpanRecorder()
	provider.RegisterSpanProcessor(recorder)
	previousProvider, previousLogger := otel.GetTracerProvider(), slog.Default()
	var logs bytes.Buffer
	otel.SetTracerProvider(provider)
	slog.SetDefault(telemetry.NewLogger(&logs, "worker"))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previousProvider)
		slog.SetDefault(previousLogger)
	})
	return recorder, &logs
}

type workerSQLConnector struct{}

func (workerSQLConnector) Connect(context.Context) (driver.Conn, error) { return workerSQLConn{}, nil }
func (workerSQLConnector) Driver() driver.Driver                        { return workerSQLDriver{} }

type workerSQLDriver struct{}

func (workerSQLDriver) Open(string) (driver.Conn, error) { return workerSQLConn{}, nil }

type workerSQLConn struct{}

func (workerSQLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unsupported prepare")
}
func (workerSQLConn) Begin() (driver.Tx, error) { return nil, errors.New("unsupported transaction") }
func (workerSQLConn) Close() error              { return nil }
func (workerSQLConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

func TestTraceAttemptPreservesProducerParentAcrossRetryAndSQLScopes(t *testing.T) {
	recorder, logs := setupWorkerTracing(t)
	source, cancelRequest := context.WithCancel(context.Background())
	source, parent := otel.Tracer("test-http").Start(source, "request")
	producer, enqueue := telemetry.StartProducerActivity(source, "enqueue")
	request := migration.ExecutionRequest{
		ExecutionID: "execution-1", ActorID: "actor-1",
		Plan:         migration.ActionPlan{DatabaseID: "database-1", MigrationID: "migration-1", Revision: 3, Action: migration.Apply, SQL: "UPDATE private_table SET private_value = 'fixture-private-secret'"},
		TraceContext: telemetry.CaptureTraceContext(producer),
	}
	producerID := trace.SpanContextFromContext(producer).SpanID()
	var enqueueErr error
	enqueue.End(&enqueueErr)
	parent.End()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var restored migration.ExecutionRequest
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	db := sql.OpenDB(telemetry.SQLConnector(workerSQLConnector{}, "postgresql", "target", "target-db"))
	defer db.Close()
	failure := errors.New("fixture-private-error")
	for attempt := 1; attempt <= 2; attempt++ {
		err := worker.TraceAttempt(context.Background(), restored, "job-1", attempt, func(ctx context.Context) (runErr error) {
			if ctx.Err() != nil {
				t.Fatal("worker mewarisi pembatalan request")
			}
			ctx, lock := telemetry.StartActivity(ctx, "worker_lock")
			defer lock.End(&runErr)
			ctx, batch := telemetry.StartActivity(ctx, "worker_batch", slog.Int("batch", 7))
			defer batch.End(&runErr)
			if attempt == 1 {
				return failure
			}
			_, runErr = db.ExecContext(ctx, restored.Plan.SQL)
			return runErr
		})
		if (attempt == 1 && err != failure) || (attempt == 2 && err != nil) {
			t.Fatal("hasil callback attempt berubah")
		}
	}
	consumers := 0
	batchIDs := make(map[trace.SpanID]bool)
	for _, span := range recorder.Ended() {
		if span.Name() == "worker_batch" {
			batchIDs[span.SpanContext().SpanID()] = true
		}
	}
	for _, span := range recorder.Ended() {
		if span.Name() == "worker_attempt" {
			consumers++
			if span.SpanKind() != trace.SpanKindConsumer || span.Parent().SpanID() != producerID || !span.Parent().IsRemote() || span.SpanContext().TraceID() != parent.SpanContext().TraceID() {
				t.Fatal("parent, kind, atau trace retry salah")
			}
		}
		if span.Name() == "db.exec" && !batchIDs[span.Parent().SpanID()] {
			t.Fatal("span SQL tidak mengikuti scope batch")
		}
	}
	if consumers != 2 {
		t.Fatal("jumlah span attempt tidak sesuai retry")
	}
	decoder := json.NewDecoder(strings.NewReader(logs.String()))
	completed, sqlSeen := 0, false
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if record["event"] == "worker_attempt_completed" {
			completed++
			if record["actor"] != restored.ActorID || record["execution_id"] != restored.ExecutionID || record["job_id"] != "job-1" || record["database_id"] != restored.Plan.DatabaseID {
				t.Fatal("metadata attempt hilang")
			}
			if (record["status"] == "failed") != (record["attempt"] == float64(1)) {
				t.Fatal("hasil retry tercatat salah")
			}
			if _, exists := record["batch"]; exists {
				t.Fatal("metadata batch bocor ke scope attempt")
			}
		}
		if record["event"] == "sql_exec_completed" {
			sqlSeen = true
			if record["attempt"] != float64(2) || record["batch"] != float64(7) || record["migration_id"] != restored.Plan.MigrationID || record["job_id"] != "job-1" || record["trace_id"] != parent.SpanContext().TraceID().String() {
				t.Fatal("korelasi SQL, batch, dan job hilang")
			}
		}
	}
	if completed != 2 || !sqlSeen {
		t.Fatal("log attempt atau SQL tidak lengkap")
	}
	if strings.Contains(logs.String(), "fixture-private-") || strings.Contains(logs.String(), "private_table") {
		t.Fatal("payload atau error mentah muncul pada log worker")
	}
}

func TestTraceAttemptSupportsLegacyPayloadAndRejectsCanceledWorker(t *testing.T) {
	recorder, _ := setupWorkerTracing(t)
	ctx, poller := otel.Tracer("poller").Start(context.Background(), "poller")
	defer poller.End()
	for _, carrier := range []telemetry.TraceContext{{}, {TraceParent: "invalid"}} {
		called := false
		err := worker.TraceAttempt(ctx, migration.ExecutionRequest{TraceContext: carrier}, "legacy-job", 1, func(ctx context.Context) error {
			called = true
			if trace.SpanContextFromContext(ctx).TraceID() == poller.SpanContext().TraceID() {
				t.Fatal("payload lama memakai trace poller")
			}
			return nil
		})
		if err != nil || !called {
			t.Fatal("payload tanpa trace valid tidak dapat diproses")
		}
	}
	if len(recorder.Ended()) != 2 {
		t.Fatal("span untuk payload lama tidak diakhiri")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err := worker.TraceAttempt(canceled, migration.ExecutionRequest{}, "job", 1, func(context.Context) error {
		t.Fatal("callback berjalan setelah worker dibatalkan")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("pembatalan worker tidak diteruskan")
	}
}

func TestTraceAttemptReportsPanicAndInvalidInputs(t *testing.T) {
	recorder, logs := setupWorkerTracing(t)
	for _, test := range []struct {
		job     string
		attempt int
		run     func(context.Context) error
	}{
		{"", 1, func(context.Context) error {
			t.Fatal("job kosong diproses")
			return nil
		}},
		{"job", 0, func(context.Context) error {
			t.Fatal("attempt nol diproses")
			return nil
		}},
		{"job", 1, nil},
	} {
		if err := worker.TraceAttempt(context.Background(), migration.ExecutionRequest{}, test.job, test.attempt, test.run); err == nil {
			t.Fatal("input attempt tidak valid diterima")
		}
	}
	func() {
		defer func() {
			if recover() != "fixture-private-panic" {
				t.Fatal("panic callback berubah atau ditelan")
			}
		}()
		_ = worker.TraceAttempt(context.Background(), migration.ExecutionRequest{}, "job", 1, func(context.Context) error { panic("fixture-private-panic") })
	}()
	if len(recorder.Ended()) != 4 {
		t.Fatal("span kegagalan attempt tidak diakhiri")
	}
	spans, _ := json.Marshal(tracetest.SpanStubsFromReadOnlySpans(recorder.Ended()))
	if strings.Contains(logs.String(), "fixture-private-panic") || strings.Contains(string(spans), "fixture-private-panic") {
		t.Fatal("nilai panic mentah muncul pada telemetry")
	}
}
