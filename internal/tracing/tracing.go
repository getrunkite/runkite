// Package tracing wires the control plane into OpenTelemetry, giving the
// master plan's "OTel observability fan-out (Langfuse, Phoenix, Jaeger,
// Datadog, any OTLP backend)" for free: this package contains almost no
// custom exporter logic because otlptracegrpc/otlptracehttp already read
// every standard OTEL_EXPORTER_OTLP_* env var (endpoint, headers for hosted
// backends' auth tokens, TLS, compression, timeout) per the OTel spec. Any
// backend that speaks OTLP works without a single line of runkite-specific
// config.
//
// Disabled by default (zero-dependency spirit): if OTEL_EXPORTER_OTLP_ENDPOINT
// / OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is unset, Init is a no-op and every
// Tracer() call returns the OTel API's built-in no-op tracer -- no
// background connection attempts, no overhead.
package tracing

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/runkite/runkite"

// Init configures the global TracerProvider from standard OTEL_* env vars.
// Returns a shutdown func to flush/close on exit (always non-nil, always
// safe to call/defer even when tracing was never enabled).
func Init(ctx context.Context) (shutdown func(context.Context) error, err error) {
	endpoint := firstNonEmpty(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"), os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := newExporter(ctx)
	if err != nil {
		return nil, err
	}

	// Order matters: resource.New applies options in sequence with later
	// ones overriding earlier ones on conflicting keys. The default must
	// come first so WithFromEnv's OTEL_SERVICE_NAME (if set) actually wins
	// instead of being silently clobbered by our own default afterwards.
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("runkite")), // default if OTEL_SERVICE_NAME unset
		resource.WithFromEnv(), // OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{}) // W3C traceparent/tracestate

	return tp.Shutdown, nil
}

// newExporter picks gRPC (default) or HTTP based on OTEL_EXPORTER_OTLP_PROTOCOL,
// per the OTel spec's standard values ("grpc", "http/protobuf", "http/json").
func newExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	protocol := strings.ToLower(firstNonEmpty(
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"),
		os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"),
		"grpc",
	))
	var e *otlptrace.Exporter
	var err error
	if strings.HasPrefix(protocol, "http") {
		e, err = otlptracehttp.New(ctx)
	} else {
		e, err = otlptracegrpc.New(ctx)
	}
	return e, err
}

// Tracer returns the runkite tracer from whatever TracerProvider is
// currently installed globally -- the OTel API's no-op provider if Init
// was never called or found no endpoint configured, so every call site
// using this is safe and free with tracing disabled.
func Tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// Traceparent extracts a run's W3C traceparent header value from a span's
// context, for embedding in the RunAssignment sent to a runner (see
// transport.TraceContext) -- real trace propagation instead of a fake
// hand-rolled ID, so a runner's own spans (e.g. LangChain's OTel callback
// handler) become children of this run's span in whatever backend is
// configured, not an orphaned, disconnected trace.
func Traceparent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
