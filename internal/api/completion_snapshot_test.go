package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

func TestJSONNumbersCompletionStateAndHistoryRetainMergedValues(t *testing.T) {
	f := jsonNumbersFixture(t)
	ctx := context.Background()
	if _, err := f.store.UpdateThread(ctx, f.threadID, &models.ThreadPatch{Values: map[string]interface{}{"prior_id": json.Number("9007199254740993")}}); err != nil {
		t.Fatal(err)
	}
	if result, err := f.store.CompleteRun(ctx, &state.RunCompletion{RunID: f.runID, Generation: 1, Status: models.RunStatusSuccess, Values: map[string]interface{}{"answer": "current"}}); err != nil || !result.Applied {
		t.Fatal(result, err)
	}
	for _, suffix := range []string{"", "/state", "/history"} {
		response := numberRequest(t, f, http.MethodGet, "/threads/"+f.threadID+suffix, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", suffix, response.Code, response.Body.String())
		}
		value := numberJSON(t, response.Body.Bytes())
		var object map[string]interface{}
		if suffix == "/history" {
			rows := value.([]interface{})
			if len(rows) != 1 {
				t.Fatalf("history count=%d", len(rows))
			}
			object = rows[0].(map[string]interface{})
		} else {
			object = value.(map[string]interface{})
		}
		values := object["values"].(map[string]interface{})
		if values["prior_id"] != json.Number("9007199254740993") || values["answer"] != "current" {
			t.Fatalf("GET %s lost merged state: %#v", suffix, values)
		}
	}
	response := numberRequest(t, f, http.MethodGet, "/threads/"+f.threadID+"/runs/"+f.runID, "")
	run := numberJSON(t, response.Body.Bytes()).(map[string]interface{})
	output := run["output"].(map[string]interface{})
	if len(output) != 1 || output["answer"] != "current" {
		t.Fatalf("run output must contain only its own result: %#v", output)
	}
}
