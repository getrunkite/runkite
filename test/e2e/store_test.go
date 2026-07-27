package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestStoreAgent_CrossThreadCounter proves store dual mode end-to-end:
// the Python runner's RunkiteStore (direct Postgres when POSTGRES_DSN is
// set) is injected into a real LangGraph graph via get_store(), and two
// runs on different threads increment the SAME cross-thread counter.
// Also verifies the value is visible via the control plane's HTTP
// /store/* API (one store, not two competing systems).
func TestStoreAgent_CrossThreadCounter(t *testing.T) {
	count1 := runStoreAgentOnce(t)
	count2 := runStoreAgentOnce(t)
	if count2 != count1+1 {
		t.Fatalf("expected cross-thread counter to increment by 1, got count1=%d count2=%d", count1, count2)
	}

	resp, err := http.Get(baseURL + "/store/items?namespace=store_agent&key=visit_count")
	if err != nil {
		t.Fatalf("GET /store/items: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /store/items status %d", resp.StatusCode)
	}
	var item map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		t.Fatalf("decode store item: %v", err)
	}
	value, _ := item["value"].(map[string]interface{})
	httpCount, err := toInt(value["count"])
	if err != nil {
		t.Fatalf("parse HTTP count: %v (value=%v)", err, value)
	}
	if httpCount != count2 {
		t.Fatalf("HTTP store read got count=%d, want %d (runner and HTTP must share one store)", httpCount, count2)
	}
}

func runStoreAgentOnce(t *testing.T) int {
	t.Helper()
	resp := postJSON(t, "/threads", map[string]interface{}{})
	var thread map[string]interface{}
	decodeJSON(t, resp, &thread)
	threadID := thread["thread_id"].(string)

	resp = postJSON(t, "/threads/"+threadID+"/runs/wait", map[string]interface{}{
		"agent_id": "store_agent",
		"input":    map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "ping"}}},
	})
	var result map[string]interface{}
	decodeJSON(t, resp, &result)

	run, _ := result["run"].(map[string]interface{})
	if run == nil || run["status"] != "success" {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var runs []map[string]interface{}
			getJSON(t, "/threads/"+threadID+"/runs", &runs)
			if len(runs) == 1 && runs[0]["status"] == "success" {
				run = runs[0]
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if run == nil || run["status"] != "success" {
		t.Fatalf("store_agent run did not succeed: %v", result)
	}

	values, _ := result["values"].(map[string]interface{})
	msgs, _ := values["messages"].([]interface{})
	if len(msgs) == 0 {
		var state map[string]interface{}
		getJSON(t, "/threads/"+threadID+"/state", &state)
		if inner, ok := state["values"].(map[string]interface{}); ok {
			msgs, _ = inner["messages"].([]interface{})
		} else {
			msgs, _ = state["messages"].([]interface{})
		}
	}
	if len(msgs) == 0 {
		t.Fatalf("no messages in store_agent result: %v", result)
	}
	last, _ := msgs[len(msgs)-1].(map[string]interface{})
	content, _ := last["content"].(string)
	prefix := "visit_count="
	if !strings.HasPrefix(content, prefix) {
		t.Fatalf("expected final message %q..., got %q", prefix, content)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(content, prefix))
	if err != nil {
		t.Fatalf("parse visit_count from %q: %v", content, err)
	}
	return n
}

func toInt(v interface{}) (int, error) {
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	case json.Number:
		i, err := n.Int64()
		return int(i), err
	case string:
		return strconv.Atoi(n)
	default:
		return 0, fmt.Errorf("unsupported type %T", v)
	}
}
