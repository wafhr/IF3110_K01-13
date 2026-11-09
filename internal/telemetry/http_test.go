package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/wafhr/IF3110_K01-13/internal/app"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

// Global provider dan logger dikembalikan setelah test. Test ini berjalan berurutan.
func setupHTTPTracing(t *testing.T) (*tracetest.SpanRecorder, *bytes.Buffer) {
	t.Helper()
	provider, err := telemetry.NewTracerProvider(io.Discard, "server")
	if err != nil {
		t.Fatal(err)
	}
	recorder := tracetest.NewSpanRecorder()
	provider.RegisterSpanProcessor(recorder)
	oldProvider, oldPropagator, oldLogger := otel.GetTracerProvider(), otel.GetTextMapPropagator(), slog.Default()
	var logs bytes.Buffer
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	slog.SetDefault(telemetry.NewLogger(&logs, "server"))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(oldProvider)
		otel.SetTextMapPropagator(oldPropagator)
		slog.SetDefault(oldLogger)
	})
	return recorder, &logs
}

func TestHTTPHandlerPropagatesAndCorrelatesRequest(t *testing.T) {
	recorder, logs := setupHTTPTracing(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /databases/{databaseID}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("databaseID") != "fixture-private-id" || r.URL.Query().Get("token") != "fixture-query-token" || r.Header.Get("Authorization") != "Bearer fixture-header-token" {
			t.Fatal("middleware mengubah request")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "fixture-request-secret" {
			t.Fatal("body request berubah")
		}
		_, child := otel.Tracer("test").Start(r.Context(), "handler_operation")
		child.End()
		slog.InfoContext(r.Context(), "handler_checked")
		w.Header().Set("X-Handler", "preserved")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("fixture-response-secret"))
	})
	request := httptest.NewRequest(http.MethodPost, "/databases/fixture-private-id?token=fixture-query-token", strings.NewReader("fixture-request-secret"))
	request.Header.Set("Authorization", "Bearer fixture-header-token")
	request.Header.Set("Cookie", "session=fixture-cookie-secret")
	request.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	response := httptest.NewRecorder()
	telemetry.HTTPHandler(mux).ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Body.String() != "fixture-response-secret" || response.Header().Get("X-Handler") != "preserved" {
		t.Fatal("middleware mengubah respons handler")
	}
	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("jumlah span = %d", len(spans))
	}
	child, server := spans[0], spans[1]
	if server.Name() != "POST /databases/{databaseID}" || server.SpanKind() != trace.SpanKindServer || !server.Parent().IsRemote() || server.Parent().SpanID().String() != "0123456789abcdef" {
		t.Fatal("nama, kind, atau parent span HTTP tidak sesuai")
	}
	if server.SpanContext().TraceID().String() != "0123456789abcdef0123456789abcdef" || child.Parent().SpanID() != server.SpanContext().SpanID() || child.SpanContext().TraceID() != server.SpanContext().TraceID() {
		t.Fatal("trace context tidak diteruskan ke handler")
	}
	if response.Header().Get("X-Trace-ID") != server.SpanContext().TraceID().String() {
		t.Fatal("header trace ID tidak sesuai span")
	}
	rawLogs := logs.String()
	decoder := json.NewDecoder(logs)
	for _, event := range []string{"handler_checked", "http_request_completed"} {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record["event"] != event || record["trace_id"] != server.SpanContext().TraceID().String() || record["span_id"] != server.SpanContext().SpanID().String() {
			t.Fatal("log request kehilangan korelasi span")
		}
		if event == "http_request_completed" && (record["http_status_code"] != float64(http.StatusCreated) || record["status"] != "success" || record["http_route"] != "/databases/{databaseID}" || record["duration_ms"].(float64) < 0) {
			t.Fatal("atribut penyelesaian request tidak sesuai")
		}
	}
	encoded, err := json.Marshal(tracetest.SpanStubsFromReadOnlySpans(spans))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fixture-private-id", "fixture-query-token", "fixture-header-token", "fixture-cookie-secret", "fixture-request-secret", "fixture-response-secret"} {
		if strings.Contains(string(encoded), secret) || strings.Contains(rawLogs, secret) {
			t.Fatal("data request atau response dicatat pada span")
		}
	}
}

func TestHTTPHandlerRecordsStatusAndMissingRoutes(t *testing.T) {
	for _, test := range []struct {
		path   string
		code   int
		level  string
		status codes.Code
	}{
		{"/ok", http.StatusOK, "INFO", codes.Unset},
		{"/denied", http.StatusForbidden, "WARN", codes.Unset},
		{"/failed", http.StatusInternalServerError, "ERROR", codes.Error},
		{"/custom", 599, "ERROR", codes.Error},
		{"/missing-private-path?secret=private-query", http.StatusNotFound, "WARN", codes.Unset},
	} {
		t.Run(test.path, func(t *testing.T) {
			recorder, logs := setupHTTPTracing(t)
			mux := http.NewServeMux()
			if test.code != http.StatusNotFound {
				mux.HandleFunc("GET "+test.path, func(w http.ResponseWriter, r *http.Request) {
					if test.code != http.StatusOK {
						w.WriteHeader(test.code)
					}
				})
			}
			response := httptest.NewRecorder()
			telemetry.HTTPHandler(mux).ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			record := decodeLog(t, logs.Bytes())
			spans := recorder.Ended()
			if response.Code != test.code || len(spans) != 1 || spans[0].Status().Code != test.status || record["http_status_code"] != float64(test.code) || record["level"] != test.level {
				t.Fatal("status respons, log, atau span tidak sesuai")
			}
			if test.code == http.StatusNotFound && (record["http_route"] != "" || spans[0].Name() != "GET") {
				t.Fatal("request tanpa route memakai path mentah")
			}
		})
	}
}

func TestHTTPHandlerStartsTraceWithoutValidIncomingParent(t *testing.T) {
	for _, header := range []string{"", "invalid", "00-00000000000000000000000000000000-0000000000000000-01"} {
		t.Run(header, func(t *testing.T) {
			recorder, _ := setupHTTPTracing(t)
			request := httptest.NewRequest(http.MethodGet, "/health", nil)
			request.Header.Set("traceparent", header)
			response := httptest.NewRecorder()
			telemetry.HTTPHandler(app.Routes()).ServeHTTP(response, request)
			spans := recorder.Ended()
			if response.Code != http.StatusOK || len(spans) != 1 || !spans[0].SpanContext().IsValid() || spans[0].Parent().IsValid() {
				t.Fatal("request tanpa parent valid tidak memperoleh root span")
			}
		})
	}
}

func TestHTTPHandlerRecordsPanicAndPreservesPropagation(t *testing.T) {
	for _, test := range []struct {
		name      string
		value     any
		errorType string
	}{
		{"panic", "fixture-sensitive-panic", "panic"},
		{"abort", http.ErrAbortHandler, "aborted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder, logs := setupHTTPTracing(t)
			handler := telemetry.HTTPHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(test.value) }))
			func() {
				defer func() {
					if got := recover(); got != test.value {
						t.Fatal("nilai panic berubah atau ditelan")
					}
				}()
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
			}()
			record := decodeLog(t, logs.Bytes())
			spans := recorder.Ended()
			if record["status"] != "failed" || record["error_type"] != test.errorType || len(spans) != 1 || spans[0].Status().Code != codes.Error {
				t.Fatal("kegagalan panic tidak tercatat")
			}
			if _, exists := record["http_status_code"]; exists {
				t.Fatal("status respons dibuat sebelum ada respons")
			}
			encoded, _ := json.Marshal(tracetest.SpanStubsFromReadOnlySpans(spans))
			if strings.Contains(logs.String(), "fixture-sensitive-panic") || strings.Contains(string(encoded), "fixture-sensitive-panic") {
				t.Fatal("isi panic sensitif ikut dicatat")
			}
		})
	}
}

func TestHTTPHandlerPreservesFlushingAndCommittedStatus(t *testing.T) {
	_, logs := setupHTTPTracing(t)
	handler := telemetry.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Fatal("kemampuan Flush hilang")
		}
		if _, ok := w.(http.Hijacker); ok {
			t.Fatal("wrapper menambahkan kemampuan Hijack yang tidak tersedia")
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "streamed")
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	record := decodeLog(t, logs.Bytes())
	if response.Code != http.StatusOK || !response.Flushed || response.Body.String() != "streamed" || record["http_status_code"] != float64(http.StatusOK) {
		t.Fatal("Flush atau status respons pertama berubah")
	}
}

func TestHTTPHandlerCorrelatesUnsampledRequests(t *testing.T) {
	recorder, logs := setupHTTPTracing(t)
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	request.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-00")
	response := httptest.NewRecorder()
	telemetry.HTTPHandler(app.Routes()).ServeHTTP(response, request)
	record := decodeLog(t, logs.Bytes())
	if response.Code != http.StatusOK || len(recorder.Ended()) != 0 || record["trace_id"] != "0123456789abcdef0123456789abcdef" || record["span_id"] == "0123456789abcdef" {
		t.Fatal("request unsampled kehilangan korelasi atau mengubah keputusan sampling")
	}
}

func TestHTTPHandlerHandlesCancellationAndUnknownMethods(t *testing.T) {
	recorder, logs := setupHTTPTracing(t)
	ctx, cancel := context.WithCancel(context.Background())
	handler := telemetry.HTTPHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { cancel() }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PRIVATE-METHOD", "/", nil).WithContext(ctx))
	record := decodeLog(t, logs.Bytes())
	span := recorder.Ended()[0]
	if record["http_method"] != "_OTHER" || record["status"] != "failed" || record["error_type"] != context.Canceled.Error() || span.Name() != "_OTHER" || span.Status().Code != codes.Error {
		t.Fatal("pembatalan request atau normalisasi metode tidak sesuai")
	}
}

func TestHTTPCompletionUsesVerifiedActorAcrossRequestCopies(t *testing.T) {
	_, logs := setupHTTPTracing(t)
	handler := telemetry.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(telemetry.WithLogAttrs(r.Context(), slog.String("scope", "copied")))
		r = r.WithContext(telemetry.WithActor(r.Context(), "verified-actor"))
		slog.InfoContext(r.Context(), "authenticated_handler")
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	for _, record := range logRecords(t, logs.String()) {
		if record["actor"] != "verified-actor" {
			t.Fatal("log HTTP kehilangan aktor pada request salinan")
		}
	}
}
