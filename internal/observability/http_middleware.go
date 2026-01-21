package observability

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

type ctxKey string

const (
	requestIDKey ctxKey = "request_id"
)

func RequestIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(requestIDKey).(string)
	return v, ok
}

// RequestIDMiddleware ensures every request has an X-Request-Id and stores it in context.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-Id")
		if rid == "" {
			rid = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", rid)
		ctx := context.WithValue(r.Context(), requestIDKey, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AccessLogMiddleware logs one line per request (JSON zap) with request_id and trace_id.
func AccessLogMiddleware(logger *zap.Logger, routeName func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(rec, r)

			rid, _ := RequestIDFromContext(r.Context())
			span := trace.SpanFromContext(r.Context())
			sc := span.SpanContext()

			logger.Info("http_request",
				zap.String("request_id", rid),
				zap.String("trace_id", sc.TraceID().String()),
				zap.String("span_id", sc.SpanID().String()),
				zap.String("route", routeName(r)),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Int("status", rec.status),
				zap.Duration("duration", time.Since(start)),
			)
		})
	}
}

// TracingMiddleware starts a span per HTTP request and attaches useful attributes.
func TracingMiddleware(routeName func(*http.Request) string) func(http.Handler) http.Handler {
	tr := otel.Tracer("taskflow/http")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name := routeName(r)
			ctx, span := tr.Start(r.Context(), name)
			defer span.End()

			span.SetAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.route", name),
				attribute.String("http.target", r.URL.Path),
			)

			rec := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(rec, r.WithContext(ctx))

			span.SetAttributes(attribute.Int("http.status_code", rec.status))
			if rec.status >= 500 {
				span.SetStatus(codes.Error, "server_error")
			}
		})
	}
}
