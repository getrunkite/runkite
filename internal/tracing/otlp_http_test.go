package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

const (
	otlpHTTPTracePath = "/api/public/otel/v1/traces"
	otlpHTTPParent    = "00-11111111111111111111111111111111-2222222222222222-01"
	otlpHTTPAuth      = "Basic cGstc3ludGhldGljOnNrLXN5bnRoZXRpYw=="
)

type otlpHTTPReceipt struct {
	SchemaVersion int    `json:"schema_version"`
	TraceID       string `json:"trace_id"`
	SpanID        string `json:"span_id"`
	Traceparent   string `json:"traceparent"`
	Service       string `json:"service"`
	SpanName      string `json:"span_name"`
	Protocol      string `json:"protocol"`
}

type otlpHTTPWireRequest struct {
	path   string
	header http.Header
	body   []byte
	err    error
}

// The child uses production Init, batching, Tracer and shutdown. A fresh test
// process keeps its global provider out of the existing no-op tracing tests.
func TestOTLPHTTPWire(t *testing.T) {
	for _, signalSpecific := range []bool{false, true} {
		name := "generic_base"
		if signalSpecific {
			name = "trace_specific_overrides"
		}
		t.Run(name, func(t *testing.T) {
			requests := make(chan otlpHTTPWireRequest, 4)
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := &httptest.Server{Listener: listener, Config: &http.Server{
				ReadHeaderTimeout: 2 * time.Second,
				ReadTimeout:       5 * time.Second,
				WriteTimeout:      2 * time.Second,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(io.LimitReader(r.Body, 128*1024+1))
					if len(body) > 128*1024 {
						err = fmt.Errorf("OTLP request exceeds fixture limit")
					}
					if r.Method != http.MethodPost {
						err = fmt.Errorf("unexpected HTTP method %s", r.Method)
					}
					select {
					case requests <- otlpHTTPWireRequest{r.URL.Path, r.Header.Clone(), body, err}:
					default:
						http.Error(w, "too many fixture requests", http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "application/x-protobuf")
					w.WriteHeader(http.StatusOK) // Empty ExportTraceServiceResponse.
				}),
			}}
			server.Start()
			defer server.Close()

			directory := t.TempDir()
			receiptPath := filepath.Join(directory, "receipt.json")
			env := []string{
				"RUNKITE_TEST_OTLP_WIRE_CHILD=1",
				"RUNKITE_TEST_OTLP_RECEIPT=" + receiptPath,
				"RUNKITE_TEST_OTLP_PARENT=" + otlpHTTPParent,
				"OTEL_SERVICE_NAME=synthetic-go-service",
				"OTEL_RESOURCE_ATTRIBUTES=service.name=wrong-resource-service,fixture.kind=wire",
				"OTEL_EXPORTER_OTLP_TIMEOUT=2000",
				"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT=2000",
				"OTEL_BSP_SCHEDULE_DELAY=60000",
				"OTEL_BSP_EXPORT_TIMEOUT=4000",
				"TMPDIR=" + directory,
			}
			if signalSpecific {
				// Trace-specific values must win over conflicting general values.
				env = append(env,
					"OTEL_EXPORTER_OTLP_ENDPOINT="+server.URL+"/wrong-base",
					"OTEL_EXPORTER_OTLP_PROTOCOL=grpc",
					"OTEL_EXPORTER_OTLP_HEADERS=Authorization=wrong-general,x-general-only=absent",
					"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT="+server.URL+otlpHTTPTracePath,
					"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf",
					"OTEL_EXPORTER_OTLP_TRACES_HEADERS=Authorization=Basic%20cGstc3ludGhldGljOnNrLXN5bnRoZXRpYw%3D%3D,x-langfuse-ingestion-version=4",
				)
			} else {
				env = append(env,
					"OTEL_EXPORTER_OTLP_ENDPOINT="+server.URL+"/api/public/otel",
					"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf",
					"OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic%20cGstc3ludGhldGljOnNrLXN5bnRoZXRpYw%3D%3D,x-langfuse-ingestion-version=4",
				)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOTLPHTTPEmitHelper$", "-test.count=1")
			command.Env = env // No ambient OTEL settings, proxy, credentials or resource data.
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("fresh exporter process failed: %v\n%s", err, output)
			}
			var request otlpHTTPWireRequest
			select {
			case request = <-requests:
			case <-ctx.Done():
				t.Fatal("shutdown did not flush an OTLP request")
			}
			if request.err != nil {
				t.Fatal(request.err)
			}
			if request.path != otlpHTTPTracePath || request.header.Get("Content-Type") != "application/x-protobuf" {
				t.Fatalf("wrong OTLP destination or encoding: path=%s content-type=%s", request.path, request.header.Get("Content-Type"))
			}
			if request.header.Get("Authorization") != otlpHTTPAuth || request.header.Get("X-Langfuse-Ingestion-Version") != "4" {
				t.Fatal("exporter did not send decoded configured headers")
			}
			if signalSpecific && request.header.Get("X-General-Only") != "" {
				t.Fatal("Go trace-specific headers must replace the general header map")
			}
			var export collectorpb.ExportTraceServiceRequest
			if err := proto.Unmarshal(request.body, &export); err != nil {
				t.Fatalf("real exporter did not send OTLP protobuf: %v", err)
			}
			if len(export.ResourceSpans) != 1 || len(export.ResourceSpans[0].ScopeSpans) != 1 || len(export.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
				t.Fatal("expected exactly one resource, instrumentation scope and span")
			}
			resource := export.ResourceSpans[0]
			if otlpHTTPString(resource.Resource.Attributes, "service.name") != "synthetic-go-service" || otlpHTTPString(resource.Resource.Attributes, "fixture.kind") != "wire" {
				t.Fatal("service override or configured resource metadata was lost")
			}
			if resource.ScopeSpans[0].Scope.Name != tracerName {
				t.Fatal("span did not use the production tracer")
			}
			span := resource.ScopeSpans[0].Spans[0]
			if span.Name != "run" || span.EndTimeUnixNano <= span.StartTimeUnixNano || len(span.SpanId) != 8 || bytes.Equal(span.SpanId, make([]byte, 8)) {
				t.Fatal("missing completed production run span identity")
			}
			if hex.EncodeToString(span.TraceId) != strings.Repeat("11", 16) || hex.EncodeToString(span.ParentSpanId) != strings.Repeat("22", 8) || span.TraceState != "fixture=wire" {
				t.Fatal("W3C parent identity or tracestate was not propagated")
			}
			for key, value := range map[string]string{"run.id": "synthetic-run", "thread.id": "synthetic-thread", "graph.id": "synthetic-agent", "run.status": "success"} {
				if otlpHTTPString(span.Attributes, key) != value {
					t.Fatalf("missing metadata attribute %s", key)
				}
			}
			if len(span.Attributes) != 4 || len(span.Events) != 0 {
				t.Fatal("metadata fixture unexpectedly exported model/content attributes or events")
			}
			data, err := os.ReadFile(receiptPath)
			if err != nil {
				t.Fatal(err)
			}
			var receipt otlpHTTPReceipt
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.SchemaVersion != 1 || receipt.TraceID != hex.EncodeToString(span.TraceId) || receipt.SpanID != hex.EncodeToString(span.SpanId) || receipt.Traceparent != "00-"+receipt.TraceID+"-"+receipt.SpanID+"-01" || receipt.Service != "synthetic-go-service" || receipt.SpanName != "run" || receipt.Protocol != "http/protobuf" {
				t.Fatal("receipt does not describe the exported span")
			}
			info, err := os.Stat(receiptPath)
			if err != nil || info.Mode().Perm()&0o077 != 0 {
				t.Fatal("receipt must not be group/world accessible")
			}
			select {
			case <-requests:
				t.Fatal("unexpected extra export")
			default:
			}
		})
	}
}

func otlpHTTPString(attributes []*commonpb.KeyValue, key string) string {
	for _, item := range attributes {
		if item.Key == key {
			return item.Value.GetStringValue()
		}
	}
	return ""
}

// TestOTLPHTTPEmitHelper is opt-in native-backend evidence at the SDK/span
// boundary. Its identity receipt does not assert backend ingestion or dispatch.
func TestOTLPHTTPEmitHelper(t *testing.T) {
	wireChild := os.Getenv("RUNKITE_TEST_OTLP_WIRE_CHILD") == "1"
	if !wireChild && os.Getenv("RUNKITE_TEST_OTLP_EMIT") != "1" {
		t.Skip("set RUNKITE_TEST_OTLP_EMIT=1 for a local native backend")
	}
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if endpoint == "" && wireChild {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if err := validateOTLPHTTPLoopback(endpoint); err != nil {
		t.Fatal(err)
	}
	protocol := firstNonEmpty(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"), os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	if protocol != "http/protobuf" {
		t.Fatal("fixture requires explicit http/protobuf protocol")
	}
	if !wireChild && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS") == "" {
		t.Fatal("native fixture requires explicit trace headers")
	}
	service := os.Getenv("OTEL_SERVICE_NAME")
	if service == "" || len(service) > 128 {
		t.Fatal("fixture requires a service name of 1 to 128 bytes")
	}
	receiptPath := os.Getenv("RUNKITE_TEST_OTLP_RECEIPT")
	if receiptPath == "" {
		t.Fatal("fixture requires a new receipt path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	carrier := propagation.MapCarrier{}
	if parent := os.Getenv("RUNKITE_TEST_OTLP_PARENT"); parent != "" {
		carrier.Set("traceparent", parent)
		if wireChild {
			carrier.Set("tracestate", "fixture=wire")
		}
		ctx = propagation.TraceContext{}.Extract(ctx, carrier)
		parentContext := trace.SpanContextFromContext(ctx)
		if !parentContext.IsValid() || !parentContext.IsSampled() {
			t.Fatal("fixture parent must be a valid sampled W3C traceparent")
		}
	}
	shutdown, err := Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	spanContext, span := Tracer().Start(ctx, "run", trace.WithAttributes(
		attribute.String("run.id", "synthetic-run"),
		attribute.String("thread.id", "synthetic-thread"),
		attribute.String("graph.id", "synthetic-agent"),
	))
	span.SetAttributes(attribute.String("run.status", "success"))
	if !span.IsRecording() || !span.SpanContext().IsValid() {
		span.End()
		_ = shutdown(ctx)
		t.Fatal("fixture did not create a recording span")
	}
	receipt := otlpHTTPReceipt{
		SchemaVersion: 1,
		TraceID:       span.SpanContext().TraceID().String(),
		SpanID:        span.SpanContext().SpanID().String(),
		Traceparent:   Traceparent(spanContext),
		Service:       service,
		SpanName:      "run",
		Protocol:      "http/protobuf",
	}
	span.End()
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := writeOTLPHTTPReceipt(receiptPath, receipt); err != nil {
		t.Fatal(err)
	}
}

func writeOTLPHTTPReceipt(path string, receipt otlpHTTPReceipt) error {
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func validateOTLPHTTPLoopback(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(endpoint, "#") {
		return fmt.Errorf("fixture endpoint must be HTTP on literal 127.0.0.1 without credentials, query or fragment")
	}
	return nil
}

func TestOTLPHTTPFixtureEndpointBoundary(t *testing.T) {
	for _, endpoint := range []string{"", "http://localhost:3000/path", "https://127.0.0.1/path", "http://127.0.0.1.example/path", "http://[::1]/path", "http://synthetic-user@127.0.0.1/path", "http://127.0.0.1/path?key=value", "http://127.0.0.1/path?", "http://127.0.0.1/path#", "http://127.0.0.1/path#fragment"} {
		if validateOTLPHTTPLoopback(endpoint) == nil {
			t.Fatal("fixture accepted an unsafe endpoint")
		}
	}
	if err := validateOTLPHTTPLoopback("http://127.0.0.1:3000" + otlpHTTPTracePath); err != nil {
		t.Fatal(err)
	}
}

func TestOTLPHTTPReceiptRefusesReplacement(t *testing.T) {
	directory := t.TempDir()
	existing := filepath.Join(directory, "existing.json")
	if err := os.WriteFile(existing, []byte("preserve existing receipt"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "symlink.json")
	if err := os.Symlink(existing, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{existing, link} {
		if err := writeOTLPHTTPReceipt(path, otlpHTTPReceipt{}); !os.IsExist(err) {
			t.Fatal("receipt writer did not reject an existing file or symlink")
		}
	}
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "preserve existing receipt" {
		t.Fatal("existing receipt was changed")
	}
}
