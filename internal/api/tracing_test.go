package api_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/transport"
)

// withInMemoryTracing installs a real (always-sampling) TracerProvider
// backed by an in-memory exporter for the duration of the test, and
// restores whatever was globally installed before (the no-op default,
// in every other test in this package -- confirming Init's disabled-by-
// default behavior is also what every other test in this package runs
// under, without needing to say so explicitly).
func withInMemoryTracing(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return exp
}

// TestRunSpan_StartedOnCreateEndedOnTerminal proves createRun starts a real
// OTel span (with run/thread/graph attributes and a genuine W3C traceparent
// propagated to the runner) and that it's closed out with the run's final
// status once the run reaches a terminal state via StatusCallback.
func TestRunSpan_StartedOnCreateEndedOnTerminal(t *testing.T) {
	exp := withInMemoryTracing(t)
	env := newTestEnv(t)
	ctx := context.Background()

	resp, _ := postJSON(env.srv.URL+"/threads/otel-thread/runs", map[string]interface{}{"agent_id": "trace_agent"})
	expectStatus(t, resp, 200)
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)

	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected queued job: %v", err)
	}
	if assignment.TraceContext == nil || assignment.TraceContext.Traceparent == "" {
		t.Fatal("expected a real (non-empty) W3C traceparent on the assignment with tracing enabled")
	}

	// No span exported yet -- run hasn't reached a terminal state.
	if len(exp.GetSpans()) != 0 {
		t.Fatalf("expected 0 exported spans before run completes, got %d", len(exp.GetSpans()))
	}

	env.broker.Publish(ctx, run.RunID, &transport.RunEvent{
		EventID: "evt_1", Seq: 1, Method: "end",
		Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
	})
	env.apiServer.StatusCallback()(run.RunID, "success", "")

	spans := exp.GetSpans()
	var runSpan *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "run" {
			runSpan = &spans[i]
			break
		}
	}
	if runSpan == nil {
		t.Fatalf("expected a 'run' span to be exported after StatusCallback, got spans: %+v", spans)
	}

	attrs := map[string]string{}
	for _, a := range runSpan.Attributes {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	if attrs["run.id"] != run.RunID {
		t.Errorf("expected run.id=%s, got %+v", run.RunID, attrs)
	}
	if attrs["thread.id"] != "otel-thread" {
		t.Errorf("expected thread.id=otel-thread, got %+v", attrs)
	}
	if attrs["graph.id"] != "trace_agent" {
		t.Errorf("expected graph.id=trace_agent, got %+v", attrs)
	}
	if attrs["run.status"] != "success" {
		t.Errorf("expected run.status=success, got %+v", attrs)
	}
	if runSpan.EndTime.IsZero() {
		t.Error("expected span to be ended (non-zero EndTime)")
	}
}

// TestRunSpan_EndedOnCancel proves the "run.cancel" path also closes out
// the span, even if the runner's own StatusCallback never arrives.
func TestRunSpan_EndedOnCancel(t *testing.T) {
	exp := withInMemoryTracing(t)
	env := newTestEnv(t)
	ctx := context.Background()

	resp, _ := postJSON(env.srv.URL+"/threads/otel-cancel-thread/runs", map[string]interface{}{"agent_id": "trace_agent"})
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)

	cancelResp, _ := postJSON(env.srv.URL+"/runs/"+run.RunID+"/cancel", nil)
	expectStatus(t, cancelResp, 200)

	spans := exp.GetSpans()
	var found bool
	for _, s := range spans {
		if s.Name == "run" {
			found = true
			if s.EndTime.IsZero() {
				t.Error("expected cancelled run's span to be ended")
			}
		}
	}
	if !found {
		t.Fatal("expected a 'run' span to be exported after cancel")
	}
}
