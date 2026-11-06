// Package telemetry sets up OpenTelemetry tracing with no exporter (docs/logging-design.md,
// "Tracing"): every request's span exists so its log lines carry traceId and spanId, and an
// exporter can be added later without touching services.
package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Init installs the global tracer provider (parent-based, always on, no exporter) and the W3C
// trace-context and baggage propagators. Call it once in main before serving; call shutdown on
// the way out.
func Init(service string) (shutdown func(context.Context) error) {
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", service))),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp.Shutdown
}
