package state

import (
	"encoding/json"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
)

func TestPrepareCachedRunPreservesExactValuesWithoutMutatingCaller(t *testing.T) {
	run := &models.Run{RunID: "cached", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{"cache_hit": true, AttemptGenerationKey: 9, CompletionGenerationKey: 8}, Output: json.RawMessage(`{"id":9007199254740993}`)}
	pending, completion, err := PrepareCachedRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != models.RunStatusPending || pending.Output != nil || completion.Generation != 0 || completion.Values["id"] != json.Number("9007199254740993") {
		t.Fatalf("incorrect prepared result: %+v %+v", pending, completion)
	}
	if _, exists := pending.Metadata[AttemptGenerationKey]; exists {
		t.Fatal("cached synthetic attempt retained foreign dispatch generation")
	}
	if _, exists := pending.Metadata[CompletionGenerationKey]; exists {
		t.Fatal("cached synthetic attempt retained foreign completion generation")
	}
	pending.Metadata["cache_hit"] = false
	if run.Status != models.RunStatusSuccess || run.Metadata[AttemptGenerationKey] != 9 || run.Metadata["cache_hit"] != true || len(run.Output) == 0 {
		t.Fatalf("preparation mutated caller: %+v", run)
	}
}

func TestPrepareCachedRunRejectsNonobjectOrNonterminalResult(t *testing.T) {
	if _, _, err := PrepareCachedRun(nil); err == nil {
		t.Fatal("accepted absent cached result")
	}
	for _, raw := range []string{"null", "[]", "true", `"result"`, "{", "{} {}"} {
		if _, _, err := PrepareCachedRun(&models.Run{Status: models.RunStatusSuccess, Output: json.RawMessage(raw)}); err == nil {
			t.Errorf("accepted unsupported cached output %q", raw)
		}
	}
	if _, _, err := PrepareCachedRun(&models.Run{Status: models.RunStatusPending, Output: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("accepted nonterminal cached result")
	}
}
