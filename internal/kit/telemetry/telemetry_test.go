package telemetry_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/telemetry"
)

func TestInitMakesRealSpansAndContinuesTraceparent(t *testing.T) {
	shutdown := telemetry.Init("widgets-api")
	defer func() { _ = shutdown(context.Background()) }()
	carrier := propagation.MapCarrier{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	_, span := otel.Tracer("t").Start(ctx, "x")
	defer span.End()
	sc := span.SpanContext()
	if sc.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || sc.SpanID().String() == "00f067aa0ba902b7" || !sc.IsValid() {
		t.Errorf("span %v", sc)
	}
	_, root := otel.Tracer("t").Start(context.Background(), "root")
	defer root.End()
	if !root.SpanContext().IsValid() {
		t.Error("a root span has no IDs")
	}
}
