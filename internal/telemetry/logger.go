package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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

// traceHandler memperkaya log dari context tanpa menyimpan span pada logger.
type traceHandler struct {
	slog.Handler
	boundKeys []string
}

func (h traceHandler) Handle(ctx context.Context, record slog.Record) error {
	seen := make(map[string]bool, len(h.boundKeys)+record.NumAttrs())
	for _, key := range h.boundKeys {
		seen[key] = true
	}
	record.Attrs(func(attr slog.Attr) bool {
		seen[attr.Key] = true
		return true
	})
	attrs := contextAttrs(ctx)
	span := trace.SpanContextFromContext(ctx)
	if span.IsValid() {
		attrs = append(attrs, slog.String("trace_id", span.TraceID().String()), slog.String("span_id", span.SpanID().String()))
	}
	record = record.Clone()
	for _, attr := range attrs {
		if !seen[attr.Key] {
			record.AddAttrs(attr)
			seen[attr.Key] = true
		}
	}
	return h.Handler.Handle(ctx, record)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	keys := append([]string(nil), h.boundKeys...)
	for _, attr := range attrs {
		keys = append(keys, attr.Key)
	}
	return traceHandler{Handler: h.Handler.WithAttrs(attrs), boundKeys: keys}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{Handler: h.Handler.WithGroup(name), boundKeys: h.boundKeys}
}

type logAttrsKey struct{}
type requestActorKey struct{}

// Hanya identitas aktor HTTP yang dibagikan ke context turunannya.
// Atribut operasi lain disalin agar tidak bercampur antar-scope.
type requestActor struct {
	mu sync.RWMutex
	id string
}

// WithLogAttrs menambahkan metadata terstruktur tanpa mengubah context induk.
func WithLogAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	previous, _ := ctx.Value(logAttrsKey{}).([]slog.Attr)
	merged := append([]slog.Attr(nil), previous...)
	for _, attr := range attrs {
		if attr.Key == "" || (attr.Value.Kind() == slog.KindString && attr.Value.String() == "") {
			continue
		}
		replaced := false
		for index := range merged {
			if merged[index].Key == attr.Key {
				merged[index] = attr
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, attr)
		}
	}
	return context.WithValue(ctx, logAttrsKey{}, merged)
}

// WithActor dipanggil setelah identitas diverifikasi. Log penyelesaian HTTP
// ikut memperoleh aktor walaupun middleware autentikasi menyalin request.
func WithActor(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	if actor, ok := ctx.Value(requestActorKey{}).(*requestActor); ok {
		actor.mu.Lock()
		actor.id = id
		actor.mu.Unlock()
	}
	return WithLogAttrs(ctx, slog.String("actor", id))
}

func contextAttrs(ctx context.Context) []slog.Attr {
	scoped, _ := ctx.Value(logAttrsKey{}).([]slog.Attr)
	attrs := append([]slog.Attr(nil), scoped...)
	for _, attr := range attrs {
		if attr.Key == "actor" {
			return attrs
		}
	}
	id := "system"
	if actor, ok := ctx.Value(requestActorKey{}).(*requestActor); ok {
		actor.mu.RLock()
		id = actor.id
		actor.mu.RUnlock()
	}
	return append(attrs, slog.String("actor", id))
}

func spanAttrs(attrs []slog.Attr) []attribute.KeyValue {
	values := make([]attribute.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		switch attr.Value.Kind() {
		case slog.KindString:
			values = append(values, attribute.String(attr.Key, attr.Value.String()))
		case slog.KindInt64:
			values = append(values, attribute.Int64(attr.Key, attr.Value.Int64()))
		case slog.KindBool:
			values = append(values, attribute.Bool(attr.Key, attr.Value.Bool()))
		}
	}
	return values
}

// Activity mencatat hasil operasi domain beserta span dan durasinya.
type Activity struct {
	ctx     context.Context
	span    trace.Span
	event   string
	started time.Time
}

func StartActivity(ctx context.Context, event string, attrs ...slog.Attr) (context.Context, *Activity) {
	return startActivity(ctx, event, trace.SpanKindInternal, attrs...)
}

// StartProducerActivity mencakup penyimpanan pesan sebelum antrean mengembalikan ID.
func StartProducerActivity(ctx context.Context, event string, attrs ...slog.Attr) (context.Context, *Activity) {
	return startActivity(ctx, event, trace.SpanKindProducer, attrs...)
}

// StartConsumerActivity memulihkan parent pesan pada context proses worker.
func StartConsumerActivity(ctx context.Context, parent TraceContext, event string, attrs ...slog.Attr) (context.Context, *Activity) {
	return startActivity(RestoreTraceContext(ctx, parent), event, trace.SpanKindConsumer, attrs...)
}

func startActivity(ctx context.Context, event string, kind trace.SpanKind, attrs ...slog.Attr) (context.Context, *Activity) {
	ctx = WithLogAttrs(ctx, attrs...)
	ctx, span := otel.Tracer("github.com/wafhr/IF3110_K01-13/internal/telemetry").Start(ctx, event,
		trace.WithSpanKind(kind), trace.WithAttributes(spanAttrs(contextAttrs(ctx))...))
	activity := &Activity{ctx: ctx, span: span, event: event, started: time.Now()}
	slog.InfoContext(ctx, event+"_started", "status", "running")
	return ctx, activity
}

// SetAttributes menambahkan ID hasil operasi yang baru tersedia setelah penyimpanan.
func (a *Activity) SetAttributes(attrs ...slog.Attr) {
	a.ctx = WithLogAttrs(a.ctx, attrs...)
	a.span.SetAttributes(spanAttrs(contextAttrs(a.ctx))...)
}

// End wajib dipasang langsung dengan defer agar panic tercatat sebagai kegagalan.
// Pesan error dan nilai panic mentah tidak dimasukkan ke telemetry.
func (a *Activity) End(operationErr *error) {
	recovered := recover()
	defer func() { a.span.End() }()
	status, level := "success", slog.LevelInfo
	var failure error
	if operationErr != nil {
		failure = *operationErr
	}
	if recovered != nil {
		failure = errors.New("operation panicked")
	}
	attrs := []slog.Attr{slog.Float64("duration_ms", float64(time.Since(a.started))/float64(time.Millisecond))}
	if failure != nil {
		status, level = "failed", slog.LevelError
		category, state := sqlError(failure)
		if category == "driver_error" {
			category = "operation_failed"
		}
		if recovered != nil {
			category = "panic"
		}
		attrs = append(attrs, slog.String("error", category))
		if state != "" {
			attrs = append(attrs, slog.String("sqlstate", state))
		}
		a.span.SetStatus(codes.Error, category)
		a.span.RecordError(errors.New(category))
		a.span.SetAttributes(attribute.String("error.type", category))
	}
	a.span.SetAttributes(spanAttrs(contextAttrs(a.ctx))...)
	attrs = append(attrs, slog.String("status", status))
	slog.LogAttrs(a.ctx, level, a.event+"_completed", attrs...)
	if recovered != nil {
		panic(recovered)
	}
}
