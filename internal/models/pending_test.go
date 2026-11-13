package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

func TestPendingArgsKeepExactNumericDisplay(t *testing.T) {
	raw := []byte(`{"id":9007199254740993,"nested":[18446744073709551615,0.12345678901234567890123456789]}`)
	args := UnmarshalPendingArgs(raw)
	if args["id"] != json.Number("9007199254740993") {
		t.Fatalf("pending argument changed: %#v", args["id"])
	}
	if encoded := string(MarshalPendingArgs(args)); encoded != string(raw) {
		t.Fatalf("pending preview changed numeric values: %s", encoded)
	}
	for _, invalid := range []string{`{"id":1} trailing`, `[]`, `null`, `{}`} {
		if got := UnmarshalPendingArgs([]byte(invalid)); got != nil {
			t.Fatalf("invalid/empty args %q decoded: %#v", invalid, got)
		}
	}
}

func TestPendingActionKeyPreservesNamespacesAndPinsRunIncarnation(t *testing.T) {
	a := &PendingAction{TenantID: "tenant", AgentID: "agent", RunID: "operation", Generation: 1, Connector: "files", Tool: "write", ArgsDigest: "digest"}
	legacy, _ := json.Marshal([]any{a.TenantID, a.AgentID, false, a.RunID, a.Generation, a.Connector, a.Tool, a.ArgsDigest})
	sum := sha256.Sum256(legacy)
	if PendingActionKey(a) != hex.EncodeToString(sum[:]) {
		t.Fatal("unbound historical key changed")
	}
	unbound := PendingActionKey(a)
	a.RunIncarnationID = "first"
	first := PendingActionKey(a)
	a.RunIncarnationID = "second"
	if first == unbound || PendingActionKey(a) == first {
		t.Fatal("managed review key did not distinguish run incarnations")
	}
	expires := time.Now().Add(time.Minute)
	a.ExpiresAt = &expires
	embedded := PendingActionKey(a)
	a.RunIncarnationID = ""
	if PendingActionKey(a) != embedded || embedded == unbound {
		t.Fatal("embedded decision namespace changed or collided with managed")
	}
}
