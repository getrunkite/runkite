package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/getrunkite/runkite/internal/policy"
)

type requiredAuditStub struct {
	err   error
	calls int
	last  policy.PolicyDecision
}

func (a *requiredAuditStub) WritePolicyDecision(_ context.Context, _ policy.PolicyInput, dec policy.PolicyDecision) error {
	a.calls++
	a.last = dec
	return a.err
}

func requiredAuditInput(stage string) policy.PolicyInput {
	return policy.PolicyInput{
		Stage: stage, TenantID: "acme", AgentID: "support", Principal: "operator",
		RunID: "run-audit", Generation: 3, Connector: "crm", Tool: "update",
	}
}

func TestRequiredAudit_UnavailableBlocksEveryStage(t *testing.T) {
	for _, stage := range []string{policy.StageConnectorSession, policy.StageToolCall, policy.StageRunCreate} {
		for _, missing := range []bool{false, true} {
			name := stage + "/failed"
			if missing {
				name = stage + "/missing"
			}
			t.Run(name, func(t *testing.T) {
				var auditor policy.Auditor
				if !missing {
					auditor = &requiredAuditStub{err: errors.New("synthetic-internal-storage-detail")}
				}
				eng := policy.New(policy.Config{
					DefaultEffect: policy.EffectAllow, AuditRequired: true, Auditor: auditor,
				})
				if !eng.Enabled() || !eng.AuditRequired() {
					t.Fatal("required auditing must enable a fail-closed engine")
				}
				dec := eng.Decide(context.Background(), requiredAuditInput(stage))
				if dec.Effect != policy.EffectDeny || dec.ReasonCode != policy.ReasonPolicyAuditFailed || !dec.AuditFailed {
					t.Fatalf("decision = %#v", dec)
				}
				if strings.Contains(dec.Reason, "synthetic-internal-storage-detail") {
					t.Fatal("storage details leaked into returned denial")
				}
			})
		}
	}
}

func TestRequiredAudit_PendingCannotSurviveFailedWrite(t *testing.T) {
	auditor := &requiredAuditStub{err: errors.New("audit unavailable")}
	eng := policy.New(policy.Config{
		AuditRequired: true, Auditor: auditor, DefaultEffect: policy.EffectAllow,
		MandatoryHITL: []policy.MandatoryHITLRule{{ID: "review", TenantID: "acme", Connector: "crm"}},
	})
	dec := eng.Decide(context.Background(), requiredAuditInput(policy.StageToolCall))
	if auditor.last.Effect != policy.EffectPending {
		t.Fatalf("pre-audit decision = %#v, want pending", auditor.last)
	}
	if dec.Effect != policy.EffectDeny || dec.ReasonCode != policy.ReasonPolicyAuditFailed || !dec.AuditFailed {
		t.Fatalf("decision = %#v", dec)
	}
}

func TestRequiredAudit_PreservesExistingDenyAndMarksFailure(t *testing.T) {
	auditor := &requiredAuditStub{err: errors.New("audit unavailable")}
	eng := policy.New(policy.Config{
		AuditRequired: true, Auditor: auditor,
		Grants: []policy.Grant{{
			ID: "deny-update", TenantID: "acme", AgentID: "support", Connector: "crm",
			Tools: &policy.ToolFilter{Deny: []string{"update"}},
		}},
	})
	dec := eng.Decide(context.Background(), requiredAuditInput(policy.StageToolCall))
	if dec.Effect != policy.EffectDeny || dec.ReasonCode != policy.ReasonPolicyToolDenied || dec.RuleID != "deny-update" || !dec.AuditFailed {
		t.Fatalf("authoritative denial was changed or made bypassable: %#v", dec)
	}
	raw, err := json.Marshal(dec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "AuditFailed") {
		t.Fatal("internal persistence marker must not enter generic JSON responses")
	}
}

func TestRequiredAudit_CacheReauditsAndRecovers(t *testing.T) {
	auditor := &requiredAuditStub{}
	exporter := &spyExporter{}
	eng := policy.New(policy.Config{
		ForceEnable: true, DefaultEffect: policy.EffectAllow, AuditRequired: true,
		Auditor: auditor, Exporter: exporter, CacheTTL: time.Hour,
	})
	in := requiredAuditInput(policy.StageToolCall)
	for i, fail := range []bool{false, true, false} {
		auditor.err = nil
		if fail {
			auditor.err = errors.New("audit unavailable")
		}
		dec := eng.Decide(context.Background(), in)
		wantEffect := policy.EffectAllow
		if fail {
			wantEffect = policy.EffectDeny
		}
		if dec.Effect != wantEffect || dec.AuditFailed != fail {
			t.Fatalf("call %d: %#v", i+1, dec)
		}
		if exporter.last != dec || exporter.calls != i+1 {
			t.Fatalf("export did not observe final decision on call %d: %#v", i+1, exporter.last)
		}
	}
	if auditor.calls != 3 || eng.CacheLen() != 1 {
		t.Fatalf("audit calls = %d, cache entries = %d", auditor.calls, eng.CacheLen())
	}
}

func TestRequiredAudit_TraceRecordsFinalDenial(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("audit-required").Start(context.Background(), "tool")
	eng := policy.New(policy.Config{DefaultEffect: policy.EffectAllow, AuditRequired: true})
	eng.Decide(ctx, requiredAuditInput(policy.StageToolCall))
	span.End()
	spans := exp.GetSpans()
	if len(spans) != 1 || len(spans[0].Events) != 1 {
		t.Fatalf("spans = %#v", spans)
	}
	attrs := map[string]string{}
	auditFailed := false
	for _, attr := range spans[0].Events[0].Attributes {
		if string(attr.Key) == "policy.audit_failed" {
			auditFailed = attr.Value.AsBool()
			continue
		}
		attrs[string(attr.Key)] = attr.Value.AsString()
	}
	if attrs["policy.effect"] != policy.EffectDeny || attrs["policy.reason_code"] != policy.ReasonPolicyAuditFailed || !auditFailed {
		t.Fatalf("trace did not report final audit denial: %#v", attrs)
	}
}

func TestOptionalAudit_FailurePreservesCompatibility(t *testing.T) {
	eng := policy.New(policy.Config{
		ForceEnable: true, DefaultEffect: policy.EffectAllow,
		Auditor: &requiredAuditStub{err: errors.New("audit unavailable")},
	})
	if eng.AuditRequired() {
		t.Fatal("auditing must remain optional unless requested")
	}
	dec := eng.Decide(context.Background(), requiredAuditInput(policy.StageToolCall))
	if dec.Effect != policy.EffectAllow || dec.AuditFailed {
		t.Fatalf("optional audit changed policy result: %#v", dec)
	}
	var nilEngine *policy.Engine
	if nilEngine.AuditRequired() {
		t.Fatal("nil engine cannot require auditing")
	}
}
