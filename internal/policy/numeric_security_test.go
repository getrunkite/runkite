package policy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

func TestBindArgsDistinctWideNumbers(t *testing.T) {
	_, a, _ := BindArgs([]byte(`{"id":9007199254740992}`))
	_, b, _ := BindArgs([]byte(`{"id":9007199254740993}`))
	if a == b {
		t.Fatal("different tool arguments share a decision/approval digest")
	}
}

func TestBindArgsStrictRejectsAmbiguityAndVersionsDigest(t *testing.T) {
	for _, raw := range []string{`{"x":1,"x":2}`, `{"x":{"y":1,"y":2}}`, `{"x":"\ud800"}`} {
		if _, _, _, err := BindArgsStrict([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	_, a, _ := BindArgs([]byte(`{"n":1000}`))
	_, b, _ := BindArgs([]byte(`{"n":1e3}`))
	if a != b {
		t.Fatal("equivalent numeric spellings differ")
	}
	old := sha256.Sum256([]byte(`{"n":1000}`))
	if a == fmt.Sprintf("%x", old) {
		t.Fatal("old lossy-parser approval digest still usable")
	}
}

func TestPredicatesDistinctWideNumbers(t *testing.T) {
	in, _, _ := BindArgs([]byte(`{"amount":9007199254740993}`))
	p := Predicate{Path: "amount", Op: "gt", Value: json.Number("9007199254740992")}
	if matched, _ := evalPredicate(&p, PolicyInput{Args: in}); !matched {
		t.Fatal("strictly larger amount escaped the configured predicate")
	}
}
