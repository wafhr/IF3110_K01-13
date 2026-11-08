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
