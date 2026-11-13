package models

import (
	"strings"
	"testing"
	"time"
)

func TestApprovalRevisionValidationAndPlanNamespace(t *testing.T) {
	policyA := "policy-v1-" + strings.Repeat("a", 64)
	policyB := "policy-v1-" + strings.Repeat("b", 64)
	toolA := "tool-v1-" + strings.Repeat("a", 64)
	if !ValidPolicyRevision(policyA) || !ValidToolRevision(toolA) || !ValidToolRevision("") {
		t.Fatal("valid revision refused")
	}
	for _, bad := range []string{"", policyA + "a", "policy-v2-" + strings.Repeat("a", 64), "policy-v1-" + strings.Repeat("A", 64)} {
		if ValidPolicyRevision(bad) {
			t.Fatal("invalid policy revision accepted")
		}
	}
	for _, bad := range []string{toolA + "a", "tool-v2-" + strings.Repeat("a", 64), "tool-v1-" + strings.Repeat("A", 64)} {
		if ValidToolRevision(bad) {
			t.Fatal("invalid tool revision accepted")
		}
	}
	base := PendingAction{ID: "p", TenantID: "t", AgentID: "a", RunID: "r", Connector: "c", Tool: "write", ArgsDigest: "digest", TargetRevision: "target-v1-" + strings.Repeat("a", 64), PolicyRevision: policyA, RequiredApprovals: 1, RequesterIdentity: "requester"}
	original := PendingActionKey(&base)
	variations := []func(*PendingAction){func(a *PendingAction) { a.PolicyRevision = policyB }, func(a *PendingAction) { a.ToolRevision = toolA }, func(a *PendingAction) { a.RequiredApprovals = 2 }, func(a *PendingAction) { a.ExcludeRequester = true }, func(a *PendingAction) { a.RequesterIdentity = "Requester" }}
	for _, change := range variations {
		changed := base
		change(&changed)
		if PendingActionKey(&changed) == original {
			t.Fatal("different authority plans share pending key")
		}
	}
	implicit := base
	implicit.RequiredApprovals = 0
	if PendingActionKey(&implicit) != original {
		t.Fatal("default-one plan namespace differs")
	}
	expiry := time.Now().Add(time.Hour)
	base.ExpiresAt = &expiry
	embedded := PendingActionKey(&base)
	base.TargetRevision = "arbitrary"
	base.PolicyRevision = "arbitrary"
	base.ToolRevision = "arbitrary"
	if PendingActionKey(&base) != embedded {
		t.Fatal("standalone namespace depends on managed revisions")
	}
	base.RequiredApprovals = 2
	if PendingActionKey(&base) == embedded {
		t.Fatal("standalone quorum plans share pending key")
	}
}
