package main

import (
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
)

func TestMatchPolicyEffects_ExactSet(t *testing.T) {
	got := []models.AuditEvent{
		{Action: policy.StageToolCall, Decision: policy.EffectAllow, Connector: "bank", Tool: "transfer"},
		{Action: policy.StageToolCall, Decision: policy.EffectPending, Connector: "bank", Tool: "transfer", ReasonCode: "predicate_amount"},
		{Action: policy.StageConnectorSession, Decision: policy.EffectDeny, Connector: "bank", Tool: ""},
	}
	want := []simPolicyEffect{{Connector: "bank", Tool: "transfer", Effect: policy.EffectPending}}
	if err := matchPolicyEffects(got, want); err != nil {
		t.Fatal(err)
	}
}

func TestMatchPolicyEffects_ReasonCodeOptional(t *testing.T) {
	got := []models.AuditEvent{
		{Action: policy.StageToolCall, Decision: policy.EffectPending, Connector: "bank", Tool: "transfer", ReasonCode: "predicate_amount"},
	}
	want := []simPolicyEffect{{Connector: "bank", Tool: "transfer", Effect: policy.EffectPending}}
	if err := matchPolicyEffects(got, want); err != nil {
		t.Fatal(err)
	}
	want = []simPolicyEffect{{Connector: "bank", Tool: "transfer", Effect: policy.EffectPending, ReasonCode: "predicate_amount"}}
	if err := matchPolicyEffects(got, want); err != nil {
		t.Fatal(err)
	}
	want = []simPolicyEffect{{Connector: "bank", Tool: "transfer", Effect: policy.EffectPending, ReasonCode: "other"}}
	if err := matchPolicyEffects(got, want); err == nil {
		t.Fatal("wrong reason_code should fail")
	}
}

func TestMatchPolicyEffects_ExtraPendingFails(t *testing.T) {
	got := []models.AuditEvent{
		{Action: policy.StageToolCall, Decision: policy.EffectPending, Connector: "bank", Tool: "transfer"},
		{Action: policy.StageToolCall, Decision: policy.EffectDeny, Connector: "crm", Tool: "delete"},
	}
	want := []simPolicyEffect{{Connector: "bank", Tool: "transfer", Effect: policy.EffectPending}}
	err := matchPolicyEffects(got, want)
	if err == nil {
		t.Fatal("extra deny should fail")
	}
	if !strings.Contains(err.Error(), "unexpected deny on crm/delete") {
		t.Fatalf("got %v", err)
	}
}

func TestMatchPolicyEffects_MissingIsAllow(t *testing.T) {
	got := []models.AuditEvent{
		{Action: policy.StageToolCall, Decision: policy.EffectAllow, Connector: "bank", Tool: "transfer"},
	}
	want := []simPolicyEffect{{Connector: "bank", Tool: "transfer", Effect: policy.EffectPending}}
	err := matchPolicyEffects(got, want)
	if err == nil {
		t.Fatal("expected miss")
	}
	if !strings.Contains(err.Error(), "expected pending on bank/transfer, got allow") {
		t.Fatalf("got %v", err)
	}
}

func TestMatchPolicyEffects_EmptyWantNoDeny(t *testing.T) {
	got := []models.AuditEvent{
		{Action: policy.StageToolCall, Decision: policy.EffectAllow, Connector: "bank", Tool: "transfer"},
	}
	if err := matchPolicyEffects(got, nil); err != nil {
		t.Fatal(err)
	}
	if err := matchPolicyEffects(got, []simPolicyEffect{}); err != nil {
		t.Fatal(err)
	}
}
