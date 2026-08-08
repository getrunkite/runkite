package policy_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/getrunkite/runkite/internal/policy"
)

type spyExporter struct {
	mu    sync.Mutex
	calls int
	last  policy.PolicyDecision
	in    policy.PolicyInput
}

func (s *spyExporter) ExportPolicyDecision(_ context.Context, in policy.PolicyInput, dec policy.PolicyDecision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.last = dec
	s.in = in
}

func TestDecide_EmitsOTelSpanEvent(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})

	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "sf",
		}},
	})
	ctx, span := tp.Tracer("test").Start(context.Background(), "http")
	dec := eng.Decide(ctx, policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales",
		RunID: "run-1", Connector: "sf", Tool: "query",
	})
	span.End()
	if dec.Effect != policy.EffectAllow {
		t.Fatalf("effect = %q, want allow", dec.Effect)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	evs := spans[0].Events
	if len(evs) != 1 || evs[0].Name != "policy.decide" {
		t.Fatalf("events = %+v, want one policy.decide", evs)
	}
	attrs := map[string]string{}
	for _, a := range evs[0].Attributes {
		attrs[string(a.Key)] = a.Value.Emit()
	}
	if attrs["policy.effect"] != "allow" || attrs["policy.stage"] != policy.StageToolCall ||
		attrs["run.id"] != "run-1" || attrs["connector"] != "sf" || attrs["tool"] != "query" {
		t.Fatalf("event attrs = %#v", attrs)
	}
}

func TestDecide_CallsExporterOnce(t *testing.T) {
	spy := &spyExporter{}
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "sf",
		}},
		Exporter: spy,
	})
	dec := eng.Decide(context.Background(), policy.PolicyInput{
		Stage: policy.StageConnectorSession, TenantID: "acme", AgentID: "sales",
		Connector: "sf",
	})
	if dec.Effect != policy.EffectAllow {
		t.Fatalf("effect = %q", dec.Effect)
	}
	// Give nothing async — exporter is sync in this spy.
	time.Sleep(10 * time.Millisecond)
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if spy.calls != 1 {
		t.Fatalf("exporter calls = %d, want 1", spy.calls)
	}
	if spy.last.Effect != policy.EffectAllow || spy.in.Connector != "sf" {
		t.Fatalf("exporter saw effect=%q connector=%q", spy.last.Effect, spy.in.Connector)
	}
}

func TestDecide_NoSpanEventWhenTracingOff(t *testing.T) {
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "sf",
		}},
	})
	// Default global provider is no-op; must not panic.
	_ = eng.Decide(context.Background(), policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales", Connector: "sf",
	})
}
