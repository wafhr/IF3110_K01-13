package telemetry

import (
	"io"
	"log/slog"
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
	return slog.New(handler).With("service", service)
}
