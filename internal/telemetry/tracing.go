package telemetry

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// NewTracerProvider mengekspor span sebagai JSON melalui antrean batch.
// Pemanggil memasang provider dan wajib memanggil Shutdown dengan context baru.
func NewTracerProvider(output io.Writer, service string, secrets ...string) (*sdktrace.TracerProvider, error) {
	if output == nil || strings.TrimSpace(service) == "" {
		return nil, errors.New("tracing requires an output writer and service name")
	}
	exporter, err := stdouttrace.New(stdouttrace.WithWriter(output))
	if err != nil {
		return nil, errors.New("initialize console trace exporter")
	}

	return tracerProvider(exporter, service, secrets...), nil
}

// NewRuntimeTracerProvider memakai OTLP/HTTP saat endpoint Collector diisi.
func NewRuntimeTracerProvider(ctx context.Context, output io.Writer, service, endpoint string, secrets ...string) (*sdktrace.TracerProvider, error) {
	if endpoint == "" {
		return NewTracerProvider(output, service, secrets...)
	}
	target, err := url.Parse(endpoint)
	if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") ||
		target.User != nil || target.Fragment != "" || target.RawQuery != "" || target.ForceQuery {
		return nil, errors.New("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT must be an HTTP(S) URL without credentials, query, or fragment")
	}
	if strings.TrimSpace(service) == "" {
		return nil, errors.New("tracing requires a service name")
	}
	if target.Path == "" || target.Path == "/" {
		target.Path = "/v1/traces"
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(target.String()))
	if err != nil {
		return nil, errors.New("initialize OTLP trace exporter")
	}
	return tracerProvider(exporter, service, secrets...), nil
}

func tracerProvider(exporter sdktrace.SpanExporter, service string, secrets ...string) *sdktrace.TracerProvider {
	redactor := NewRedactor(secrets...)
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", service))),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithBatcher(redactingExporter{SpanExporter: exporter, redactor: redactor}),
	)
}

// Redaction dilakukan pada salinan tampilan span sebelum diteruskan ke exporter.
// Snapshot asli tetap dapat dipakai oleh processor lain tanpa berubah.
type redactingExporter struct {
	sdktrace.SpanExporter
	redactor *Redactor
}

func (e redactingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	sanitized := make([]sdktrace.ReadOnlySpan, len(spans))
	for index, span := range spans {
		sanitized[index] = redactedSpan{ReadOnlySpan: span, redactor: e.redactor}
	}
	return e.SpanExporter.ExportSpans(ctx, sanitized)
}

type redactedSpan struct {
	sdktrace.ReadOnlySpan
	redactor *Redactor
}

func (s redactedSpan) Name() string { return s.redactor.String(s.ReadOnlySpan.Name()) }

func (s redactedSpan) Attributes() []attribute.KeyValue {
	return s.redactor.traceAttributes(s.ReadOnlySpan.Attributes())
}

func (s redactedSpan) Events() []sdktrace.Event {
	events := append([]sdktrace.Event(nil), s.ReadOnlySpan.Events()...)
	for index := range events {
		events[index].Name = s.redactor.String(events[index].Name)
		events[index].Attributes = s.redactor.traceAttributes(events[index].Attributes)
	}
	return events
}

func (s redactedSpan) Links() []sdktrace.Link {
	links := append([]sdktrace.Link(nil), s.ReadOnlySpan.Links()...)
	for index := range links {
		links[index].Attributes = s.redactor.traceAttributes(links[index].Attributes)
	}
	return links
}

func (s redactedSpan) Status() sdktrace.Status {
	status := s.ReadOnlySpan.Status()
	status.Description = s.redactor.String(status.Description)
	return status
}

func (s redactedSpan) Resource() *resource.Resource {
	original := s.ReadOnlySpan.Resource()
	return resource.NewWithAttributes(original.SchemaURL(), s.redactor.traceAttributes(original.Attributes())...)
}

func (s redactedSpan) InstrumentationScope() instrumentation.Scope {
	scope := s.ReadOnlySpan.InstrumentationScope()
	scope.Name = s.redactor.String(scope.Name)
	scope.Version = s.redactor.String(scope.Version)
	scope.SchemaURL = s.redactor.String(scope.SchemaURL)
	scope.Attributes = attribute.NewSet(s.redactor.traceAttributes(scope.Attributes.ToSlice())...)
	return scope
}

// TraceContext adalah metadata W3C yang dapat disimpan pada payload antrean.
// Identitas aktor diperiksa dari snapshot bisnis yang disimpan secara persisten.
type TraceContext struct {
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
}

func CaptureTraceContext(ctx context.Context) TraceContext {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return TraceContext{TraceParent: carrier.Get("traceparent"), TraceState: carrier.Get("tracestate")}
}

// RestoreTraceContext mempertahankan pembatalan/deadline worker. Parent lokal
// dibersihkan agar payload lama atau rusak memulai trace tersendiri.
func RestoreTraceContext(ctx context.Context, parent TraceContext) context.Context {
	ctx = trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{
		"traceparent": parent.TraceParent,
		"tracestate":  parent.TraceState,
	})
}
