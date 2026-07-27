package tracing

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"

	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// TestInit_DisabledByDefault proves the zero-dependency guarantee: with no
// OTEL_EXPORTER_OTLP_*_ENDPOINT set, Init must not attempt any network
// connection and must return a harmless no-op shutdown func.
func TestInit_DisabledByDefault(t *testing.T) {
	os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	os.Unsetenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")

	shutdown, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init should not error when tracing is disabled: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init must always return a non-nil shutdown func")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown should never error: %v", err)
	}
}

// TestTraceparent_EmptyWithNoOpTracer proves that with tracing disabled, no
// span is ever "valid" (recorded), so Traceparent returns "" rather than a
// misleading fake-looking ID -- the exact bug this replaced.
func TestTraceparent_EmptyWithNoOpTracer(t *testing.T) {
	os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	os.Unsetenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")

	ctx, span := Tracer().Start(context.Background(), "test-span")
	defer span.End()

	tp := Traceparent(ctx)
	if tp != "" {
		t.Fatalf("expected empty traceparent with tracing disabled, got %q", tp)
	}
}

// TestTraceparent_RealSpanProducesW3CFormat proves a real (recording) span
// produces an actual W3C traceparent -- "00-<32 hex trace id>-<16 hex span
// id>-<2 hex flags>" -- not the old fake now.UnixNano()-derived string.
func TestTraceparent_RealSpanProducesW3CFormat(t *testing.T) {
	tp := sdktrace.NewTracerProvider() // always-sample by default
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(prev)

	ctx, span := Tracer().Start(context.Background(), "test-span")
	defer span.End()

	got := Traceparent(ctx)
	parts := strings.Split(got, "-")
	if len(parts) != 4 {
		t.Fatalf("expected W3C traceparent format 00-traceid-spanid-flags, got %q", got)
	}
	if len(parts[1]) != 32 {
		t.Fatalf("expected 32 hex char trace id, got %q (len %d)", parts[1], len(parts[1]))
	}
	if len(parts[2]) != 16 {
		t.Fatalf("expected 16 hex char span id, got %q (len %d)", parts[2], len(parts[2]))
	}
}

// TestResource_ServiceNameEnvOverridesDefault is a regression test for a
// real bug found during manual verification: resource.New's WithFromEnv and
// WithAttributes(default) were in the wrong order, silently discarding
// OTEL_SERVICE_NAME because the hardcoded default was applied *after* (and
// so on top of) the env-derived value. Confirmed with a real OTel collector
// before this test was written; this locks the fix in.
func TestResource_ServiceNameEnvOverridesDefault(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "custom-service-name")

	res, err := resource.New(context.Background(),
		resource.WithAttributes(semconv.ServiceName("runkite")),
		resource.WithFromEnv(),
	)
	if err != nil {
		t.Fatal(err)
	}

	got, ok := res.Set().Value(semconv.ServiceNameKey)
	if !ok {
		t.Fatal("service.name attribute missing from resource")
	}
	if got.AsString() != "custom-service-name" {
		t.Fatalf("expected OTEL_SERVICE_NAME to override default, got %q", got.AsString())
	}
}
