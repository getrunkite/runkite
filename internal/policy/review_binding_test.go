package policy

import (
	"encoding/json"
	"testing"
)

func TestReviewArgumentDigestsRetainJSONTypes(t *testing.T) {
	for _, pair := range [][2]string{
		{`{"amount":1}`, `"{\"amount\":1}"`},
		{`[1,2]`, `"[1,2]"`},
		{`1`, `"1"`},
		{`true`, `"true"`},
		{`null`, `"null"`},
		{`{}`, `""`},
	} {
		_, left, _, err := BindArgsStrict(json.RawMessage(pair[0]))
		if err != nil {
			t.Fatal(err)
		}
		_, right, _, err := BindArgsStrict(json.RawMessage(pair[1]))
		if err != nil {
			t.Fatal(err)
		}
		if left == "" || right == "" || left == right {
			t.Fatalf("different JSON types share an approval binding: %s and %s", pair[0], pair[1])
		}
	}
	parsed, _, _, err := BindArgsStrict([]byte(`"{\"amount\":1}"`))
	if err != nil || parsed != `{"amount":1}` {
		t.Fatalf("literal JSON string was reinterpreted: parsed=%#v err=%v", parsed, err)
	}
}

func TestReviewEquivalentArgumentSpellingsKeepTheirBinding(t *testing.T) {
	for _, pair := range [][2]string{
		{`{"n":1}`, `{"n":1.000e0}`},
		{`{"n":-0}`, `{"n":0.000}`},
		{`{"text":"😀"}`, `{"text":"\ud83d\ude00"}`},
		{`{"a":1,"b":2}`, `{"b":2,"\u0061":1}`},
		{`null`, `{}`},
		{"", `{}`},
	} {
		_, left, _, err := BindArgsStrict(json.RawMessage(pair[0]))
		if err != nil {
			t.Fatal(err)
		}
		_, right, _, err := BindArgsStrict(json.RawMessage(pair[1]))
		if err != nil {
			t.Fatal(err)
		}
		if left == "" || left != right {
			t.Fatalf("equivalent arguments lost their binding: %s and %s", pair[0], pair[1])
		}
	}
}
