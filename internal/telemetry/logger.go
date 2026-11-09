package telemetry

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// NewLogger menghasilkan satu objek JSON per baris dengan level minimum INFO.
// Secret opsional dipakai untuk menyamarkan nilai yang muncul pada teks bebas.
func NewLogger(output io.Writer, service string, secrets ...string) *slog.Logger {
	redactor := NewRedactor(secrets...)
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			switch attr.Key {
			case slog.TimeKey:
				if len(groups) == 0 && attr.Value.Kind() == slog.KindTime {
					attr.Value = slog.TimeValue(attr.Value.Time().UTC())
				}
			case slog.MessageKey:
				if len(groups) == 0 {
					attr.Key = "event"
				}
			}
			return redactor.ReplaceAttr(groups, attr)
		},
	})
	return slog.New(traceHandler{Handler: handler}).With("service", service)
}

// traceHandler menambahkan korelasi dari context tanpa menyimpan span pada logger.
type traceHandler struct {
	slog.Handler
}

func (h traceHandler) Handle(ctx context.Context, record slog.Record) error {
	span := trace.SpanContextFromContext(ctx)
	if span.IsValid() {
		record = record.Clone()
		record.AddAttrs(slog.String("trace_id", span.TraceID().String()), slog.String("span_id", span.SpanID().String()))
	}
	return h.Handler.Handle(ctx, record)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{Handler: h.Handler.WithGroup(name)}
}
