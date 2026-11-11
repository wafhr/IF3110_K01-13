package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

func decodeLog(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("log bukan satu objek JSON: %v", err)
	}
	return record
}

func TestNewLoggerWritesStructuredJSON(t *testing.T) {
	var output bytes.Buffer
	logger := telemetry.NewLogger(&output, "server")
	logger.Debug("debug_event")
	logger.Info("server_starting", "actor", "system", "database", "db-1", "status", "running", "correlation_id", "request-1")
	record := decodeLog(t, output.Bytes())
	for key, value := range map[string]string{"level": "INFO", "event": "server_starting", "service": "server", "actor": "system", "database": "db-1", "status": "running", "correlation_id": "request-1"} {
		if record[key] != value {
			t.Fatalf("atribut %s = %v; ingin %s", key, record[key], value)
		}
	}
	if _, exists := record["msg"]; exists {
		t.Fatal("field msg lama masih tersedia")
	}
	timestamp, err := time.Parse(time.RFC3339Nano, record["time"].(string))
	if err != nil || timestamp.Location() != time.UTC {
		t.Fatalf("waktu log tidak memakai UTC: %v, %v", timestamp, err)
	}
	if strings.Count(output.String(), "\n") != 1 {
		t.Fatal("satu peristiwa menghasilkan beberapa baris")
	}
}

func TestLoggerRedactsBoundNestedAndGroupedAttributes(t *testing.T) {
	var output bytes.Buffer
	metadata := map[string]any{
		"Password": "fixture-password", "count": int64(9223372036854775807),
		"items": []any{map[string]any{"token_hash": "fixture-token", "status": "valid"}},
	}
	logger := telemetry.NewLogger(&output, "server", "fixture-known-secret").With("metadata", metadata)
	logger.WithGroup("request").Error("request_failed",
		slog.Group("credentials", slog.String("id", "fixture-credential-id")),
		slog.Any("headers", map[string]string{"Set-Cookie": "fixture-cookie", "X-Request-ID": "request-1"}),
		slog.Any("error", errors.New("failed using fixture-known-secret")),
		slog.String("database", "db-1"),
	)
	raw := output.String()
	for _, secret := range []string{"fixture-password", "fixture-token", "fixture-credential-id", "fixture-cookie", "fixture-known-secret"} {
		if strings.Contains(raw, secret) {
			t.Fatal("nilai sensitif muncul pada log")
		}
	}
	record := decodeLog(t, output.Bytes())
	request := record["request"].(map[string]any)
	if request["database"] != "db-1" || record["event"] != "request_failed" {
		t.Fatal("atribut operasional ikut hilang")
	}
	if request["error"] != "failed using "+telemetry.Redacted {
		t.Fatal("pesan error tidak disanitasi")
	}
	if !strings.Contains(raw, "9223372036854775807") {
		t.Fatal("presisi angka berubah saat redaction")
	}
	if metadata["Password"] != "fixture-password" {
		t.Fatal("redaction mengubah data sumber")
	}
	item := metadata["items"].([]any)[0].(map[string]any)
	if item["token_hash"] != "fixture-token" {
		t.Fatal("redaction mengubah objek sumber bertingkat")
	}
}

func TestLoggerRedactsSensitiveWithGroup(t *testing.T) {
	var output bytes.Buffer
	telemetry.NewLogger(&output, "server").WithGroup("session").With("id", "fixture-session").Info("session_checked", "status", "valid")
	record := decodeLog(t, output.Bytes())
	session := record["session"].(map[string]any)
	if session["id"] != telemetry.Redacted || session["status"] != telemetry.Redacted || record["event"] != "session_checked" {
		t.Fatal("grup sensitif tidak disamarkan dengan benar")
	}
}

func TestLoggerSupportsConcurrentRecords(t *testing.T) {
	var output bytes.Buffer
	logger := telemetry.NewLogger(&output, "server")
	var workers sync.WaitGroup
	for index := range 40 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			logger.With("worker", index).Info("worker_event", "password", "fixture-password")
		}()
	}
	workers.Wait()
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 40 {
		t.Fatalf("jumlah log = %d", len(lines))
	}
	for _, line := range lines {
		record := decodeLog(t, []byte(line))
		if record["event"] != "worker_event" || record["password"] != telemetry.Redacted {
			t.Fatal("log paralel tidak lengkap")
		}
	}
}

type failedLogWriter struct{}

func (failedLogWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestLoggerHandlerPreservesWriterErrors(t *testing.T) {
	logger := telemetry.NewLogger(failedLogWriter{}, "server")
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "write_failed", 0)
	if err := logger.Handler().Handle(context.Background(), record); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("error writer = %v", err)
	}
}

func TestLoggerCorrelatesContextAfterWithAndWithGroup(t *testing.T) {
	span := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}})
	ctx, cancel := context.WithCancel(trace.ContextWithSpanContext(context.Background(), span))
	cancel()
	var output bytes.Buffer
	telemetry.NewLogger(&output, "server").With("actor", "user-1").WithGroup("request").With("database", "db-1").InfoContext(ctx, "query_completed")
	record := decodeLog(t, output.Bytes())
	request := record["request"].(map[string]any)
	if record["actor"] != "user-1" || request["database"] != "db-1" || request["trace_id"] != span.TraceID().String() || request["span_id"] != span.SpanID().String() {
		t.Fatal("logger turunan kehilangan atribut atau korelasi context yang dibatalkan")
	}
}

func TestLoggerOmitsIDsWithoutValidSpan(t *testing.T) {
	for _, ctx := range []context.Context{
		context.Background(),
		trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}})),
	} {
		var output bytes.Buffer
		telemetry.NewLogger(&output, "server").InfoContext(ctx, "system_event")
		record := decodeLog(t, output.Bytes())
		if _, exists := record["trace_id"]; exists {
			t.Fatal("trace_id palsu ditambahkan tanpa span valid")
		}
		if _, exists := record["span_id"]; exists {
			t.Fatal("span_id palsu ditambahkan tanpa span valid")
		}
	}
}

func logRecords(t *testing.T, data string) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(data))
	var records []map[string]any
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			return records
		}
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
}

func TestLogContextScopesRemainIndependent(t *testing.T) {
	_, logs := setupHTTPTracing(t)
	parent := telemetry.WithLogAttrs(context.Background(), slog.String("actor", "actor-1"), slog.String("database_id", "database-1"))
	child := telemetry.WithLogAttrs(parent, slog.String("database_id", "database-2"), slog.String("migration_id", "migration-1"))
	slog.InfoContext(child, "child")
	slog.InfoContext(parent, "parent")
	slog.InfoContext(context.Background(), "system")
	records := logRecords(t, logs.String())
	if records[0]["database_id"] != "database-2" || records[0]["actor"] != "actor-1" || records[1]["database_id"] != "database-1" || records[2]["actor"] != "system" {
		t.Fatal("metadata context bercampur")
	}
	if _, exists := records[1]["migration_id"]; exists {
		t.Fatal("metadata child mengubah parent")
	}
	var output bytes.Buffer
	telemetry.NewLogger(&output, "server").With("actor", "bound-actor").InfoContext(parent, "explicit", "database_id", "explicit-database")
	record := decodeLog(t, output.Bytes())
	if record["actor"] != "bound-actor" || record["database_id"] != "explicit-database" || strings.Count(output.String(), `"actor"`) != 1 {
		t.Fatal("metadata menduplikasi atau menimpa atribut eksplisit")
	}
}

func TestActivityRecordsOutcomeAndPreservesPanic(t *testing.T) {
	for _, scenario := range []string{"success", "failure", "panic"} {
		t.Run(scenario, func(t *testing.T) {
			recorder, logs := setupHTTPTracing(t)
			failure := errors.New("fixture-private-error")
			operation := func() (activityErr error) {
				_, activity := telemetry.StartActivity(context.Background(), "operation", slog.String("actor", "actor-1"))
				defer activity.End(&activityErr)
				activity.SetAttributes(slog.String("database_id", "created-database"))
				if scenario == "panic" {
					panic("fixture-private-panic")
				}
				if scenario == "failure" {
					return failure
				}
				return nil
			}
			var operationErr error
			var panicValue any
			func() {
				defer func() { panicValue = recover() }()
				operationErr = operation()
			}()
			if scenario == "failure" && operationErr != failure {
				t.Fatal("audit mengubah error operasi")
			}
			if scenario == "panic" && panicValue != "fixture-private-panic" {
				t.Fatal("audit menelan atau mengubah panic")
			}
			records := logRecords(t, logs.String())
			if len(records) != 2 || records[0]["event"] != "operation_started" || records[1]["event"] != "operation_completed" || records[1]["database_id"] != "created-database" {
				t.Fatal("log lifecycle tidak lengkap")
			}
			if (records[1]["status"] == "failed") != (scenario != "success") || records[1]["actor"] != "actor-1" {
				t.Fatal("hasil atau aktor audit tidak sesuai")
			}
			span := recorder.Ended()[0]
			if span.Name() != "operation" || (span.Status().Code == codes.Error) != (scenario != "success") {
				t.Fatal("hasil span berbeda dari hasil operasi")
			}
			encoded, _ := json.Marshal(tracetest.SpanStubsFromReadOnlySpans(recorder.Ended()))
			if strings.Contains(logs.String(), "fixture-private-") || strings.Contains(string(encoded), "fixture-private-") {
				t.Fatal("pesan error atau panic mentah tercatat")
			}
		})
	}
}

func TestActivityContextReachesSQLLogsAndSpans(t *testing.T) {
	recorder, logs := setupHTTPTracing(t)
	db := openSQLFixture(t, &sqlFixture{}, "postgresql", "target")
	ctx, parent := otel.Tracer("test").Start(context.Background(), "request")
	defer parent.End()
	var operationErr error
	ctx, activity := telemetry.StartActivity(ctx, "migration_test", slog.String("actor", "actor-1"), slog.String("database_id", "database-1"), slog.String("migration_id", "migration-1"))
	_, operationErr = db.ExecContext(ctx, "SELECT 1")
	activity.End(&operationErr)
	if operationErr != nil {
		t.Fatal(operationErr)
	}
	record := findSQLRecord(t, logRecords(t, logs.String()), "sql_exec_completed")
	if record["actor"] != "actor-1" || record["database_id"] != "database-1" || record["migration_id"] != "migration-1" || record["trace_id"] != parent.SpanContext().TraceID().String() {
		t.Fatal("SQL kehilangan metadata aktivitas")
	}
	for _, span := range recorder.Ended() {
		if span.Name() == "db.exec" {
			attrs := make(map[string]any)
			for _, attr := range span.Attributes() {
				attrs[string(attr.Key)] = attr.Value.AsInterface()
			}
			if attrs["actor"] != "actor-1" || attrs["database_id"] != "database-1" || attrs["migration_id"] != "migration-1" {
				t.Fatal("span SQL kehilangan metadata aktivitas")
			}
		}
	}
}
