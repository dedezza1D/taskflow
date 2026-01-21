package observability

import (
	"context"
	"errors"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

type OTelConfig struct {
	ServiceName string
	Endpoint    string // OTLP HTTP endpoint, e.g. http://otel-collector:4318
	Env         string
}

// InitTracing configures OpenTelemetry tracing. It returns a shutdown func you should call on exit.
func InitTracing(ctx context.Context, cfg OTelConfig) (func(context.Context) error, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = "taskflow"
	}

	// Allow disabling tracing entirely.
	if os.Getenv("OTEL_TRACES_EXPORTER") == "none" || cfg.Endpoint == "" {
		otel.SetTextMapPropagator(propagation.TraceContext{})
		tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
		otel.SetTracerProvider(tp)
		return tp.Shutdown, nil
	}

	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
		otlptracehttp.WithTimeout(5*time.Second),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.DeploymentEnvironment(cfg.Env),
		),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1))),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}
