package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

func TestTracerProviderFlushesLinkedSpansOnShutdown(t *testing.T) {
	for _, service := range []string{"server", "worker"} {
		t.Run(service, func(t *testing.T) {
			var output bytes.Buffer
			provider, err := telemetry.NewTracerProvider(&output, service)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			tracer := provider.Tracer("migration")
			parentCtx, parent := tracer.Start(context.Background(), "migration.execute")
			childCtx, child := tracer.Start(parentCtx, "migration.query")
			var logs bytes.Buffer
			telemetry.NewLogger(&logs, service).InfoContext(childCtx, "query_completed")
			child.End()
			parent.End()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := provider.Shutdown(shutdownCtx); err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(&output)
			var spans []map[string]any
			for {
				var span map[string]any
				err := decoder.Decode(&span)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				spans = append(spans, span)
			}
			if len(spans) != 2 {
				t.Fatalf("jumlah span setelah shutdown = %d", len(spans))
			}
			childSpan, parentSpan := spans[0], spans[1]
			childIDs := childSpan["SpanContext"].(map[string]any)
			parentIDs := parentSpan["SpanContext"].(map[string]any)
			recordedParent := childSpan["Parent"].(map[string]any)
			log := decodeLog(t, logs.Bytes())
			if log["trace_id"] != childIDs["TraceID"] || log["span_id"] != childIDs["SpanID"] {
				t.Fatal("ID log berbeda dari span yang diekspor")
			}
			if childIDs["TraceID"] != parentIDs["TraceID"] || recordedParent["SpanID"] != parentIDs["SpanID"] {
				t.Fatal("hubungan parent-child span terputus")
			}
			if childSpan["Name"] != "migration.query" || childSpan["InstrumentationScope"].(map[string]any)["Name"] != "migration" {
				t.Fatal("identitas span atau instrumentation scope berubah")
			}
			attrs := childSpan["Resource"].([]any)
			if len(attrs) != 1 || attrs[0].(map[string]any)["Key"] != "service.name" || attrs[0].(map[string]any)["Value"].(map[string]any)["Value"] != service {
				t.Fatal("service.name tidak sesuai identitas proses")
			}
			if err := provider.Shutdown(shutdownCtx); err != nil {
				t.Fatal("shutdown kedua gagal", err)
			}
		})
	}
}

func TestTracerProviderRedactsWithoutChangingOriginalSpan(t *testing.T) {
	var output bytes.Buffer
	provider, err := telemetry.NewTracerProvider(&output, "server", "fixture-known-secret")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	recorder := tracetest.NewSpanRecorder()
	provider.RegisterSpanProcessor(recorder)
	tracer := provider.Tracer("scope fixture-known-secret", trace.WithInstrumentationAttributes(attribute.String("api_key", "fixture-scope-key")))
	ctx, parent := tracer.Start(context.Background(), "parent")
	_, span := tracer.Start(ctx, "operation fixture-known-secret",
		trace.WithAttributes(
			attribute.String("password", "fixture-password"),
			attribute.StringSlice("notes", []string{"safe", "fixture-known-secret"}),
			attribute.Int64("row_count", 9223372036854775807),
			attribute.Map("details", attribute.String("session_id", "fixture-nested-session"), attribute.Bool("valid", true)),
			attribute.Slice("items", attribute.StringValue("fixture-known-secret"), attribute.MapValue(attribute.String("token", "fixture-nested-token"))),
			attribute.ByteSlice("binary", []byte("fixture-binary-secret")),
		),
		trace.WithLinks(trace.Link{SpanContext: parent.SpanContext(), Attributes: []attribute.KeyValue{attribute.String("session_id", "fixture-link-session")}}),
	)
	span.AddEvent("event fixture-known-secret", trace.WithAttributes(attribute.String("authorization", "fixture-event-token")))
	span.RecordError(errors.New("password=fixture-exception"))
	span.SetStatus(codes.Error, "failed using fixture-known-secret")
	span.End()
	parent.End()
	flushCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := provider.ForceFlush(flushCtx); err != nil {
		t.Fatal(err)
	}
	raw := output.String()
	for _, secret := range []string{"fixture-known-secret", "fixture-password", "fixture-scope-key", "fixture-nested-session", "fixture-nested-token", "fixture-link-session", "fixture-event-token", "fixture-exception", "fixture-binary-secret", "Zml4dHVyZS1iaW5hcnktc2VjcmV0"} {
		if strings.Contains(raw, secret) {
			t.Fatal("nilai sensitif muncul pada trace")
		}
	}
	if !strings.Contains(raw, "9223372036854775807") || !strings.Contains(raw, "safe") || !strings.Contains(raw, telemetry.Redacted) {
		t.Fatal("redaction menghilangkan atribut operasional")
	}
	original := recorder.Ended()[0]
	if original.Name() != "operation fixture-known-secret" || original.Status().Description != "failed using fixture-known-secret" {
		t.Fatal("redaction mengubah snapshot span asli")
	}
	if original.Attributes()[0].Value.AsString() != "fixture-password" || original.Attributes()[1].Value.AsStringSlice()[1] != "fixture-known-secret" || original.Events()[0].Attributes[0].Value.AsString() != "fixture-event-token" || original.Links()[0].Attributes[0].Value.AsString() != "fixture-link-session" {
		t.Fatal("redaction mengubah atribut asli")
	}
}

func TestTracerProviderHonorsParentSampling(t *testing.T) {
	var output bytes.Buffer
	provider, err := telemetry.NewTracerProvider(&output, "server")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	remote := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, Remote: true})
	ctx, span := provider.Tracer("test").Start(trace.ContextWithRemoteSpanContext(context.Background(), remote), "unsampled")
	if span.IsRecording() || span.SpanContext().TraceID() != remote.TraceID() {
		t.Fatal("keputusan sampling parent tidak dihormati")
	}
	var logs bytes.Buffer
	telemetry.NewLogger(&logs, "server").InfoContext(ctx, "unsampled_event")
	if decodeLog(t, logs.Bytes())["trace_id"] != remote.TraceID().String() {
		t.Fatal("span unsampled kehilangan korelasi log")
	}
	span.End()
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatal("span unsampled tetap diekspor")
	}
}

func TestTracerProviderRejectsMissingOutputOrService(t *testing.T) {
	for _, test := range []struct {
		output  io.Writer
		service string
	}{{nil, "server"}, {io.Discard, "  "}} {
		if provider, err := telemetry.NewTracerProvider(test.output, test.service); err == nil || provider != nil {
			t.Fatal("konfigurasi tracing tidak valid diterima")
		}
	}
}
