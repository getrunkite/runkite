package state

import "testing"

func TestWithoutDurableUsageReservesProvenanceWithoutMutatingNativeOutput(t *testing.T) {
	native := &RunCompletion{Values: map[string]interface{}{"usage": map[string]interface{}{"prompt_tokens": 12}, "text": "retained"}}
	if WithoutDurableUsage(native) != native {
		t.Fatal("legacy usage changed")
	}
	for _, field := range []string{"reporting_source", "reporting_status", "reported_generation", "reported_workload", "cost_reported"} {
		claim := map[string]interface{}{"prompt_tokens": 12, field: true}
		original := &RunCompletion{Values: map[string]interface{}{"usage": claim, "text": "retained"}}
		got := WithoutDurableUsage(original)
		usage := got.Values["usage"].(map[string]interface{})
		if usage["reporting_source"] != "unverified_output" || usage["reporting_status"] != "unknown" || usage["unmetered"] != true || usage["prompt_tokens"] != nil || got.Values["text"] != "retained" {
			t.Fatal("unsupported provenance retained measurement", got)
		}
		if original.Values["usage"].(map[string]interface{})["prompt_tokens"] != 12 {
			t.Fatal("caller-owned output was mutated")
		}
	}
}
