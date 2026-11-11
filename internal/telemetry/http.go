package telemetry

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// HTTPHandler membuat satu server span dan satu log penyelesaian untuk tiap request.
// Nama route berasal dari router agar nilai path dan query pengguna tetap terlindungi.
func HTTPHandler(next http.Handler) http.Handler {
	tracer := otel.Tracer("github.com/wafhr/IF3110_K01-13/internal/telemetry/http")
	propagator := otel.GetTextMapPropagator()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		method := requestMethod(r.Method)
		ctx := context.WithValue(r.Context(), requestActorKey{}, &requestActor{id: "anonymous"})
		ctx = propagator.Extract(ctx, propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, method, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", method)))
		// Pembungkus mencegah SDK merekam nilai panic mentah saat span diakhiri.
		defer func() { span.End() }()
		r = r.WithContext(ctx)
		if span.SpanContext().IsValid() {
			w.Header().Set("X-Trace-ID", span.SpanContext().TraceID().String())
		}
		response := &httpResponse{}
		defer func() {
			recovered := recover()
			route := r.Pattern
			if _, path, found := strings.Cut(route, " "); found {
				route = path
			}
			if route != "" {
				span.SetName(method + " " + route)
				span.SetAttributes(attribute.String("http.route", route))
			}
			status := "success"
			level := slog.LevelInfo
			errorType := ""
			switch {
			case recovered != nil:
				errorType = "panic"
				if recovered == http.ErrAbortHandler {
					errorType = "aborted"
				}
			case r.Context().Err() != nil:
				errorType = r.Context().Err().Error()
			}
			if response.status == 0 && recovered == nil && errorType == "" && !response.hijacked {
				response.status = http.StatusOK
			}
			attrs := []slog.Attr{
				slog.String("http_method", method), slog.String("http_route", route),
				slog.Float64("duration_ms", float64(time.Since(started))/float64(time.Millisecond)),
			}
			if response.status != 0 {
				attrs = append(attrs, slog.Int("http_status_code", response.status))
				span.SetAttributes(attribute.Int("http.response.status_code", response.status))
			}
			if response.status >= 400 {
				status, level = "failed", slog.LevelWarn
			}
			if response.status >= 500 && errorType == "" {
				errorType = strconv.Itoa(response.status)
			}
			if errorType != "" {
				status, level = "failed", slog.LevelError
				span.SetStatus(codes.Error, errorType)
				span.SetAttributes(attribute.String("error.type", errorType))
				attrs = append(attrs, slog.String("error_type", errorType))
			}
			// Nilai panic tidak dicatat karena dapat berisi credential atau isi request.
			if recovered != nil {
				span.RecordError(errors.New("HTTP handler " + errorType))
			}
			span.SetAttributes(spanAttrs(contextAttrs(ctx))...)
			attrs = append(attrs, slog.String("status", status))
			slog.LogAttrs(ctx, level, "http_request_completed", attrs...)
			if recovered != nil {
				// net/http menutup koneksi tanpa mencatat nilai panic asli.
				panic(http.ErrAbortHandler)
			}
		}()
		next.ServeHTTP(response.wrap(w), r)
	})
}

// Metode nonstandar dipetakan ke satu nilai agar nama span tidak mengikuti input bebas.
func requestMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	default:
		return "_OTHER"
	}
}

type httpResponse struct {
	status   int
	hijacked bool
}

// httpsnoop mempertahankan interface opsional writer, termasuk streaming dan hijack.
func (r *httpResponse) wrap(w http.ResponseWriter) http.ResponseWriter {
	implicitOK := func() {
		if r.status == 0 {
			r.status = http.StatusOK
		}
	}
	return httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(code int) {
				next(code)
				if r.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
					r.status = code
				}
			}
		},
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(data []byte) (int, error) {
				implicitOK()
				return next(data)
			}
		},
		WriteString: func(next httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
			return func(value string) (int, error) {
				implicitOK()
				return next(value)
			}
		},
		ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(source io.Reader) (int64, error) {
				implicitOK()
				return next(source)
			}
		},
		Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() {
				implicitOK()
				next()
			}
		},
		FlushError: func(next httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
			return func() error {
				implicitOK()
				return next()
			}
		},
		Hijack: func(next httpsnoop.HijackFunc) httpsnoop.HijackFunc {
			return func() (net.Conn, *bufio.ReadWriter, error) {
				conn, buffer, err := next()
				if err == nil {
					r.hijacked = true
				}
				return conn, buffer, err
			}
		},
	})
}
