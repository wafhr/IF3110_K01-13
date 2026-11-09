package telemetry

import (
	"context"
	"errors"
	"io"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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
	redactor := NewRedactor(secrets...)
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", service))),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithBatcher(redactingExporter{SpanExporter: exporter, redactor: redactor}),
	), nil
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
