package models

import (
	"strings"
	"testing"
	"time"
)

func TestPendingTargetRevisionValidationAndNamespace(t *testing.T) {
	a := "target-v1-" + strings.Repeat("a", 64)
	b := "target-v1-" + strings.Repeat("b", 64)
	if !ValidConnectorTargetRevision(a) || !ValidConnectorTargetRevision(b) {
		t.Fatal("valid target revision refused")
	}
	for _, value := range []string{"", "target-v1-" + strings.Repeat("A", 64), "target-v1-" + strings.Repeat("a", 63), a + "a", "target-v2-" + strings.Repeat("a", 64), "target-v1-" + strings.Repeat("g", 64)} {
		if ValidConnectorTargetRevision(value) {
			t.Fatalf("invalid target revision accepted: %q", value)
		}
	}
	row := &PendingAction{TenantID: "tenant", AgentID: "agent", RunID: "run", Generation: 1, Connector: "files", Tool: "write", ArgsDigest: "digest", RunIncarnationID: "incarnation", TargetRevision: a}
	first := PendingActionKey(row)
	row.TargetRevision = b
	if PendingActionKey(row) == first {
		t.Fatal("managed pending key merges different targets")
	}
	old := *row
	old.RunIncarnationID = b
	old.TargetRevision = ""
	unbound := *row
	unbound.RunIncarnationID = ""
	unbound.TargetRevision = b
	if PendingActionKey(&old) == PendingActionKey(&unbound) {
		t.Fatal("target suffix collided with historical incarnation namespace")
	}
	expiry := time.Now().Add(time.Hour)
	row.ExpiresAt = &expiry
	embedded := PendingActionKey(row)
	row.TargetRevision = a
	if PendingActionKey(row) != embedded {
		t.Fatal("embedded operation namespace changed with managed target")
	}
	row.TargetRevision = ""
	if PendingActionKey(row) != embedded {
		t.Fatal("legacy embedded operation namespace changed")
	}
}
