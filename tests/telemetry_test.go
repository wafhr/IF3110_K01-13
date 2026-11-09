//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
	"github.com/wafhr/IF3110_K01-13/internal/worker"
)

// Pembacaan output disinkronkan dengan handler HTTP dan exporter batch.
type telemetryOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (o *telemetryOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.Write(data)
}
func (o *telemetryOutput) snapshot() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.String()
}

type telemetryFixture struct {
	logs, spans telemetryOutput
	recorder    *tracetest.SpanRecorder
	providers   []*sdktrace.TracerProvider
}

func newTelemetryFixture(t *testing.T) *telemetryFixture {
	t.Helper()
	previousProvider, previousPropagator, previousLogger := otel.GetTracerProvider(), otel.GetTextMapPropagator(), slog.Default()
	f := &telemetryFixture{recorder: tracetest.NewSpanRecorder()}
	t.Cleanup(func() {
		for _, provider := range f.providers {
			_ = provider.Shutdown(context.Background())
		}
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		slog.SetDefault(previousLogger)
	})
	otel.SetTextMapPropagator(propagation.TraceContext{})
	f.useService(t, "server")
	return f
}

func (f *telemetryFixture) useService(t *testing.T, service string) {
	t.Helper()
	provider, err := telemetry.NewTracerProvider(&f.spans, service)
	if err != nil {
		t.Fatal(err)
	}
	provider.RegisterSpanProcessor(f.recorder)
	f.providers = append(f.providers, provider)
	otel.SetTracerProvider(provider)
	slog.SetDefault(telemetry.NewLogger(&f.logs, service))
}

func (f *telemetryFixture) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, provider := range f.providers {
		if err := provider.ForceFlush(ctx); err != nil {
			t.Fatal("flush telemetry gagal", err)
		}
	}
}

func telemetryRecords(t *testing.T, output string) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output))
	var records []map[string]any
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			return records
		}
		if err != nil {
			t.Fatal("output telemetry bukan JSON", err)
		}
		records = append(records, record)
	}
}

func telemetryRecord(t *testing.T, records []map[string]any, event, traceID string) map[string]any {
	t.Helper()
	for _, record := range records {
		if record["event"] == event && record["trace_id"] == traceID {
			return record
		}
	}
	t.Fatalf("event %s tidak ditemukan untuk trace %s", event, traceID)
	return nil
}

func withTelemetryParent(traceID string) func(*http.Request) {
	return func(request *http.Request) { request.Header.Set("traceparent", "00-"+traceID+"-0123456789abcdef-01") }
}

func assertTelemetryPrivacy(t *testing.T, capture *telemetryFixture, secrets ...string) {
	t.Helper()
	output := capture.logs.snapshot() + capture.spans.snapshot()
	for _, secret := range append([]string{adminPassword, memberPassword, targetPassword}, secrets...) {
		if secret != "" && strings.Contains(output, secret) {
			t.Fatal("nilai sensitif ditemukan pada log atau span")
		}
	}
}

func TestTelemetryHTTPIntegration(t *testing.T) {
	capture := newTelemetryFixture(t)
	f := newMigrationFixture(t)
	readTrace := "11111111111111111111111111111111"
	deniedTrace := "22222222222222222222222222222222"
	invalidTrace := "33333333333333333333333333333333"
	f.editor.do(t, "GET", f.migrationPath()+"/definitions", nil, 200, withTelemetryParent(readTrace))
	f.reader.do(t, "POST", f.migrationPath()+"/definitions", map[string]string{"name": "fixture-private-body"}, 403, withTelemetryParent(deniedTrace))
	f.anonymous().do(t, "POST", "/auth/login", map[string]string{"username": "fixture-private-username", "password": ""}, 400, withTelemetryParent(invalidTrace))
	address, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	var cookies []string
	for _, cookie := range f.editor.client.Jar.Cookies(address) {
		cookies = append(cookies, cookie.Value)
	}
	f.server.Close()
	capture.flush(t)
	records := telemetryRecords(t, capture.logs.snapshot())
	read := telemetryRecord(t, records, "http_request_completed", readTrace)
	domain := telemetryRecord(t, records, "migration_definitions_list_completed", readTrace)
	var sql map[string]any
	for _, span := range capture.recorder.Ended() {
		if span.Name() == "db.query" && span.Parent().SpanID().String() == domain["span_id"] {
			for _, record := range records {
				if record["event"] == "sql_query_completed" && record["span_id"] == span.SpanContext().SpanID().String() {
					sql = record
				}
			}
		}
	}
	if sql == nil {
		t.Fatal("query domain tidak terhubung ke log dan span SQL")
	}
	denied := telemetryRecord(t, records, "access_denied", deniedTrace)
	invalid := telemetryRecord(t, records, "auth_login_rejected", invalidTrace)
	if read["http_status_code"] != float64(200) || read["actor"] != f.actorID || domain["actor"] != f.actorID || domain["database_id"] != f.databaseID || domain["status"] != "success" {
		t.Fatal("audit HTTP/domain kehilangan identitas atau hasil")
	}
	if sql["database_role"] != "platform" || sql["actor"] != f.actorID || sql["rows_read"] == nil {
		t.Fatal("SQL read-only platform tidak diinstrumentasi")
	}
	if denied["actor"] == "anonymous" || denied["status"] != "failed" || denied["reason"] != "database_permission_required" || invalid["actor"] != "anonymous" {
		t.Fatal("identitas penolakan akses atau login tidak sesuai")
	}
	foundServer := false
	for _, span := range capture.recorder.Ended() {
		if span.SpanKind() == trace.SpanKindServer && span.SpanContext().TraceID().String() == readTrace {
			foundServer = true
			if !span.Parent().IsRemote() || span.Parent().SpanID().String() != "0123456789abcdef" {
				t.Fatal("parent HTTP tidak dipulihkan")
			}
		}
	}
	if !foundServer {
		t.Fatal("span HTTP tidak diakhiri")
	}
	assertTelemetryPrivacy(t, capture, append(cookies, f.editor.csrf, "fixture-private-body", "fixture-private-username")...)
}

func TestTelemetryQueueIntegration(t *testing.T) {
	capture := newTelemetryFixture(t)
	f := newMigrationFixture(t)
	definition := f.createMigration(t, "telemetry success", "CREATE TABLE telemetry_rows (id int PRIMARY KEY); INSERT INTO telemetry_rows VALUES (1);", "DROP TABLE telemetry_rows")
	failed := f.createMigration(t, "telemetry failure", "SELECT CAST('fixture-private-literal' AS integer)", "SELECT 1")
	applyTrace := "44444444444444444444444444444444"
	failTrace := "55555555555555555555555555555555"
	request := f.queueAction(t, definition, migration.Apply, withTelemetryParent(applyTrace))
	rejected := f.queueAction(t, failed, migration.Apply, withTelemetryParent(failTrace))
	f.server.Close()
	capture.flush(t)
	capture.useService(t, "worker")
	run := func(request migration.ExecutionRequest, attempt int) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return worker.TraceAttempt(ctx, request, request.ExecutionID, attempt, func(ctx context.Context) (batchErr error) {
			ctx, batch := telemetry.StartActivity(ctx, "worker_batch", slog.Int("batch", 1))
			defer batch.End(&batchErr)
			_, batchErr = f.executeJobContext(ctx, request)
			return batchErr
		})
	}
	if err := run(rejected, 1); err == nil {
		t.Fatal("SQL yang gagal dianggap sukses")
	}
	if err := run(request, 1); err != nil {
		t.Fatal(err)
	}
	if err := run(request, 2); err != nil {
		t.Fatal("pengiriman ulang gagal", err)
	}
	var count int
	if err := f.target.QueryRow("SELECT COUNT(*) FROM telemetry_rows").Scan(&count); err != nil || count != 1 {
		t.Fatal("pengiriman ulang menjalankan SQL kembali", err)
	}
	capture.flush(t)
	records := telemetryRecords(t, capture.logs.snapshot())
	producer := telemetryRecord(t, records, "migration_enqueue_completed", applyTrace)
	consumer := telemetryRecord(t, records, "worker_attempt_completed", applyTrace)
	var targetRecords []map[string]any
	for _, record := range records {
		if record["service"] == "worker" && record["job_id"] != nil && record["database_role"] == "target" {
			targetRecords = append(targetRecords, record)
		}
	}
	rollback := telemetryRecord(t, targetRecords, "sql_rollback_completed", failTrace)
	commit := telemetryRecord(t, targetRecords, "sql_commit_completed", applyTrace)
	failure := telemetryRecord(t, targetRecords, "sql_exec_completed", failTrace)
	// Error eksekusi dipilih terpisah karena query persiapan juga memakai Exec.
	for _, record := range targetRecords {
		if record["event"] == "sql_exec_completed" && record["trace_id"] == failTrace && record["status"] == "failed" {
			failure = record
		}
	}
	if producer["service"] != "server" || consumer["service"] != "worker" || consumer["job_id"] != request.ExecutionID || consumer["actor"] != f.actorID || consumer["status"] != "success" {
		t.Fatal("korelasi producer/consumer tidak sesuai payload persisten")
	}
	if commit["job_id"] != request.ExecutionID || commit["attempt"] != float64(1) || commit["batch"] != float64(1) || rollback["job_id"] != rejected.ExecutionID || failure["sqlstate"] != "22P02" {
		t.Fatal("SQL, commit, atau rollback kehilangan scope/error")
	}
	parent := trace.SpanContextFromContext(telemetry.RestoreTraceContext(context.Background(), request.TraceContext))
	attempts, locks, proof := 0, false, false
	for _, span := range capture.recorder.Ended() {
		if span.Name() == "worker_attempt" && span.SpanContext().TraceID().String() == applyTrace {
			attempts++
			if span.SpanKind() != trace.SpanKindConsumer || span.Parent().SpanID() != parent.SpanID() || !span.Parent().IsRemote() {
				t.Fatal("retry tidak memakai parent producer yang tersimpan")
			}
		}
		if span.Name() == "migration_lock_acquire" && span.SpanContext().TraceID().String() == applyTrace {
			locks = true
		}
	}
	for _, record := range records {
		if record["event"] == "migration_execution_prepare_completed" && record["trace_id"] == applyTrace && record["commit_proof"] == true && record["attempt"] == float64(2) {
			proof = true
		}
	}
	if attempts != 2 || !locks || !proof {
		t.Fatal("span lock, retry, atau bukti commit tidak tercatat")
	}
	assertTelemetryPrivacy(t, capture, "fixture-private-literal", definition.Name, rejected.Plan.SQL)
}

func TestTelemetryHTTPPanicPrivacyIntegration(t *testing.T) {
	capture := newTelemetryFixture(t)
	secret := "fixture-private-session-panic"
	server := httptest.NewServer(telemetry.HTTPHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(secret) })))
	t.Cleanup(server.Close)
	response, err := server.Client().Get(server.URL)
	if response != nil {
		response.Body.Close()
	}
	server.Close()
	capture.flush(t)
	if err == nil {
		t.Fatal("panic tidak menghentikan respons HTTP")
	}
	assertTelemetryPrivacy(t, capture, secret)
	records := telemetryRecords(t, capture.logs.snapshot())
	if len(records) != 1 || records[0]["error_type"] != "panic" || records[0]["status"] != "failed" {
		t.Fatal("kegagalan HTTP tidak tercatat aman")
	}
}
