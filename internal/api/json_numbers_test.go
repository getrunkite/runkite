package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/finops"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	pgstore "github.com/getrunkite/runkite/internal/state/postgres"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
	redistransport "github.com/getrunkite/runkite/internal/transport/redis"
	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

type numberFixture struct {
	store                    state.Store
	queue                    transport.JobQueue
	broker                   transport.EventBroker
	plane                    *api.Server
	runID, threadID, agentID string
}

func jsonNumbersFixture(t *testing.T) *numberFixture {
	t.Helper()
	ctx := context.Background()
	dsn, redisURL := os.Getenv("JSON_NUMBERS_POSTGRES_DSN"), os.Getenv("JSON_NUMBERS_REDIS_URL")
	if (dsn == "") != (redisURL == "") {
		t.Fatal("set both JSON_NUMBERS_POSTGRES_DSN and JSON_NUMBERS_REDIS_URL, or neither")
	}
	var store state.Store
	var queue transport.JobQueue
	var broker transport.EventBroker
	var cancels transport.CancelBroker
	if dsn == "" {
		local, err := sqlite.New("")
		if err != nil {
			t.Fatal(err)
		}
		store, queue, broker, cancels = local, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus()
	} else {
		postgres, err := pgstore.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		store = postgres
		options, err := goredis.ParseURL(redisURL)
		if err != nil {
			store.Close()
			t.Fatal(err)
		}
		client := goredis.NewClient(options)
		t.Cleanup(func() { client.Close() })
		if err := client.Ping(ctx).Err(); err != nil {
			store.Close()
			t.Fatal(err)
		}
		queue, broker, cancels = redistransport.NewQueue(client), redistransport.NewBroker(client), redistransport.NewCancelBus(client)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := uuid.NewString()
	f := &numberFixture{store: store, queue: queue, broker: broker, runID: "number-run-" + suffix, threadID: "number-thread-" + suffix, agentID: "number-agent-" + suffix}
	now := time.Now().UTC()
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: f.agentID, Name: f.agentID, Metadata: map[string]interface{}{"cache_ttl_seconds": 3600}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: f.threadID, Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{"number_test": f.threadID}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, &models.Run{RunID: f.runID, ThreadID: f.threadID, AgentID: f.agentID, Status: models.RunStatusPending, Input: json.RawMessage(`{"identity":9007199254740993}`), Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	f.plane = api.NewServer(store, queue, broker, cancels)
	f.plane.SetFinOps(&finops.Config{Pricebook: finops.Pricebook{"number-test": {InputPer1k: 1, OutputPer1k: 2}}})
	if accepted, err := store.BeginRunAttempt(ctx, f.runID, 1); err != nil || !accepted {
		t.Fatalf("begin: %v %v", accepted, err)
	}
	t.Cleanup(func() { _ = broker.Close(f.runID); _ = store.DeleteThread(context.Background(), f.threadID) })
	return f
}

const numberResult = `{"large":9007199254740993,"min":-9223372036854775808,"max":18446744073709551615,"nested":[9007199254740993,{"decimal":0.12345678901234567890123456789,"exponent":1.234567890123456789e25}],"zero":0,"false":false,"nil":null}`

func numberJSON(t *testing.T, raw []byte) any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return value
}

func numberEquivalent(a, b any) bool {
	left, numeric := a.(json.Number)
	if numeric {
		right, ok := b.(json.Number)
		if !ok {
			return false
		}
		lr, lok := new(big.Rat).SetString(string(left))
		rr, rok := new(big.Rat).SetString(string(right))
		return lok && rok && lr.Cmp(rr) == 0
	}
	switch left := a.(type) {
	case map[string]any:
		right, ok := b.(map[string]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for key, value := range left {
			if other, ok := right[key]; !ok || !numberEquivalent(value, other) {
				return false
			}
		}
		return true
	case []any:
		right, ok := b.([]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for i, value := range left {
			if !numberEquivalent(value, right[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

func assertNumberResult(t *testing.T, value any) {
	t.Helper()
	want := numberJSON(t, []byte(numberResult))
	if !numberEquivalent(value, want) {
		raw, _ := json.Marshal(value)
		t.Errorf("numeric result changed: got=%s want=%s", raw, numberResult)
	}
}

func numberRequest(t *testing.T, f *numberFixture, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	f.plane.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	return w
}

func completeNumbers(t *testing.T, f *numberFixture, runID, result string) {
	t.Helper()
	raw := fmt.Sprintf(`{"result":%s,"usage":{"prompt_tokens":10,"completion_tokens":4,"model":"number-test"}}`, result)
	if err := f.broker.Publish(context.Background(), runID, &transport.RunEvent{Generation: 1, EventID: runID + "_values", Seq: 1, Method: "values", Data: json.RawMessage(raw)}); err != nil {
		t.Fatal(err)
	}
	if accepted, err := f.plane.CompleteRun(context.Background(), &state.RunCompletion{RunID: runID, Generation: 1, Status: models.RunStatusSuccess}); err != nil || !accepted {
		t.Fatalf("complete: %v %v", accepted, err)
	}
}

func TestJSONNumbersCompletionPreservesResults(t *testing.T) {
	f := jsonNumbersFixture(t)
	completeNumbers(t, f, f.runID, numberResult)
	run, err := f.store.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	assertNumberResult(t, numberJSON(t, run.Output).(map[string]any)["result"])
	for _, path := range []string{
		"/runs/" + f.runID,
		"/threads/" + f.threadID + "/runs/" + f.runID,
		"/admin-api/runs/" + f.runID,
	} {
		t.Run(path, func(t *testing.T) {
			response := numberRequest(t, f, http.MethodGet, path, "")
			value := numberJSON(t, response.Body.Bytes()).(map[string]any)
			assertNumberResult(t, value["output"].(map[string]any)["result"])
		})
	}
	for _, path := range []string{"/runs/" + f.runID + "/wait", "/threads/" + f.threadID + "/runs/" + f.runID + "/wait"} {
		t.Run(path, func(t *testing.T) {
			response := numberRequest(t, f, http.MethodGet, path, "")
			value := numberJSON(t, response.Body.Bytes()).(map[string]any)
			assertNumberResult(t, value["values"].(map[string]any)["result"])
		})
	}
	for _, path := range []string{"/threads/" + f.threadID, "/threads/" + f.threadID + "/state"} {
		response := numberRequest(t, f, http.MethodGet, path, "")
		value := numberJSON(t, response.Body.Bytes()).(map[string]any)
		assertNumberResult(t, value["values"].(map[string]any)["result"])
	}
	response := numberRequest(t, f, http.MethodPost, "/threads/search", fmt.Sprintf(`{"metadata":{"number_test":%q}}`, f.threadID))
	threads := numberJSON(t, response.Body.Bytes()).([]any)
	if len(threads) != 1 {
		t.Fatalf("search returned %d matching threads", len(threads))
	}
	assertNumberResult(t, threads[0].(map[string]any)["values"].(map[string]any)["result"])
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		body := ""
		if method == http.MethodPost {
			body = `{}`
		}
		response := numberRequest(t, f, method, "/threads/"+f.threadID+"/history", body)
		history := numberJSON(t, response.Body.Bytes()).([]any)
		if len(history) != 1 {
			t.Fatalf("history returned %d states", len(history))
		}
		assertNumberResult(t, history[0].(map[string]any)["values"].(map[string]any)["result"])
	}
}

func TestJSONNumbersThreadWritesAndSecondCompletionKeepPriorValues(t *testing.T) {
	f := jsonNumbersFixture(t)
	completeNumbers(t, f, f.runID, numberResult)
	patch := numberRequest(t, f, http.MethodPatch, "/threads/"+f.threadID, `{"values":{"preserved":`+numberResult+`}}`)
	assertNumberResult(t, numberJSON(t, patch.Body.Bytes()).(map[string]any)["values"].(map[string]any)["preserved"])
	numberRequest(t, f, http.MethodPost, "/threads/"+f.threadID+"/state", `{"values":{"checkpointed":`+numberResult+`}}`)
	ctx := context.Background()
	if claimed, err := f.store.TryClaimThread(ctx, f.threadID); err != nil || !claimed {
		t.Fatalf("claim second turn: %v %v", claimed, err)
	}
	nextID := "number-next-" + uuid.NewString()
	now := time.Now().UTC()
	if err := f.store.CreateRun(ctx, &models.Run{RunID: nextID, ThreadID: f.threadID, AgentID: f.agentID, Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if accepted, err := f.store.BeginRunAttempt(ctx, nextID, 1); err != nil || !accepted {
		t.Fatalf("begin second: %v %v", accepted, err)
	}
	completeNumbers(t, f, nextID, `{"later":1}`)
	response := numberRequest(t, f, http.MethodGet, "/threads/"+f.threadID, "")
	values := numberJSON(t, response.Body.Bytes()).(map[string]any)["values"].(map[string]any)
	assertNumberResult(t, values["preserved"])
	assertNumberResult(t, values["checkpointed"])
	history := numberRequest(t, f, http.MethodPost, "/threads/"+f.threadID+"/history", `{}`)
	found := false
	for _, item := range numberJSON(t, history.Body.Bytes()).([]any) {
		values := item.(map[string]any)["values"].(map[string]any)
		if result, ok := values["checkpointed"]; ok {
			assertNumberResult(t, result)
			found = true
		}
	}
	if !found {
		t.Fatal("state update checkpoint was lost")
	}
	usageStore := f.store.(interface {
		SumUsage(context.Context, string, string, time.Time, time.Time) (int64, int64, float64, error)
	})
	in, out, cost, err := usageStore.SumUsage(ctx, "default", f.agentID, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || in != 20 || out != 8 || math.Abs(cost-0.036) > 1e-9 {
		t.Fatalf("usage changed: in=%d out=%d usd=%v err=%v", in, out, cost, err)
	}
	if accepted, err := f.plane.CompleteRun(ctx, &state.RunCompletion{RunID: nextID, Generation: 1, Status: models.RunStatusSuccess}); err != nil || !accepted {
		t.Fatalf("replay completion: %v %v", accepted, err)
	}
	in, out, cost, err = usageStore.SumUsage(ctx, "default", f.agentID, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || in != 20 || out != 8 || math.Abs(cost-0.036) > 1e-9 {
		t.Fatalf("usage was counted twice: in=%d out=%d usd=%v err=%v", in, out, cost, err)
	}
}

func TestJSONNumbersRunningWaitReturnsExactValues(t *testing.T) {
	f := jsonNumbersFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/runs/"+f.runID+"/wait", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { f.plane.Handler().ServeHTTP(response, request); close(done) }()
	if err := f.broker.Publish(ctx, f.runID, &transport.RunEvent{Generation: 1, EventID: f.runID + "_values", Seq: 1, Method: "values", Data: json.RawMessage(`{"result":` + numberResult + `}`)}); err != nil {
		t.Fatal(err)
	}
	if err := f.broker.Publish(ctx, f.runID, &transport.RunEvent{Generation: 1, EventID: f.runID + "_end", Seq: 2, Method: "end", Data: json.RawMessage(`{"status":"success"}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("wait did not complete")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("wait: %d %s", response.Code, response.Body.String())
	}
	assertNumberResult(t, numberJSON(t, response.Body.Bytes()).(map[string]any)["values"].(map[string]any)["result"])
}

func TestJSONNumbersCacheAndRetryIdentity(t *testing.T) {
	f := jsonNumbersFixture(t)
	requestBody := func(id, raw string) string {
		return fmt.Sprintf(`{"run_id":%q,"agent_id":%q,"input":%s}`, id, f.agentID, raw)
	}
	firstThread, firstRun := "number-cache-"+uuid.NewString(), uuid.NewString()
	t.Cleanup(func() { _ = f.store.DeleteThread(context.Background(), firstThread) })
	t.Cleanup(func() { _ = f.queue.Cancel(context.Background(), firstRun) })
	input := `{"identity":9007199254740993,"one":1}`
	first := numberRequest(t, f, http.MethodPost, "/threads/"+firstThread+"/runs", requestBody(firstRun, input))
	if numberJSON(t, first.Body.Bytes()).(map[string]any)["run_id"] != firstRun {
		t.Fatal("run identity changed")
	}
	// Equivalent JSON numbers preserve retry semantics, while adjacent large
	// integer inputs must not alias to one dispatch operation.
	numberRequest(t, f, http.MethodPost, "/threads/"+firstThread+"/runs", requestBody(firstRun, `{"one":1e0,"identity":9007199254740993}`))
	wrong := httptest.NewRecorder()
	f.plane.Handler().ServeHTTP(wrong, httptest.NewRequest(http.MethodPost, "/threads/"+firstThread+"/runs", strings.NewReader(requestBody(firstRun, `{"identity":9007199254740992,"one":1}`))))
	if wrong.Code != http.StatusConflict {
		t.Errorf("different large input reused run: %d %s", wrong.Code, wrong.Body.String())
	}
	ctx := context.Background()
	job, err := f.queue.Dequeue(ctx, "python-langgraph", time.Second)
	if err != nil || job == nil || job.RunID != firstRun {
		t.Fatalf("cache miss dispatch: %v %v", job, err)
	}
	if accepted, err := f.store.BeginRunAttempt(ctx, firstRun, 1); err != nil || !accepted {
		t.Fatalf("begin cached run: %v %v", accepted, err)
	}
	completeNumbers(t, f, firstRun, numberResult)
	secondThread := "number-cache-hit-" + uuid.NewString()
	t.Cleanup(func() { _ = f.store.DeleteThread(context.Background(), secondThread) })
	second := numberRequest(t, f, http.MethodPost, "/threads/"+secondThread+"/runs", requestBody(uuid.NewString(), input))
	hit := numberJSON(t, second.Body.Bytes()).(map[string]any)
	if hit["metadata"].(map[string]any)["cache_hit"] != true {
		t.Fatalf("same raw request missed cache: %s", second.Body.String())
	}
	assertNumberResult(t, hit["output"].(map[string]any)["result"])
	stateResponse := numberRequest(t, f, http.MethodGet, "/threads/"+secondThread+"/state", "")
	assertNumberResult(t, numberJSON(t, stateResponse.Body.Bytes()).(map[string]any)["values"].(map[string]any)["result"])
	waitHit := numberRequest(t, f, http.MethodPost, "/runs/wait", fmt.Sprintf(`{"agent_id":%q,"input":%s}`, f.agentID, input))
	waitValue := numberJSON(t, waitHit.Body.Bytes()).(map[string]any)
	assertNumberResult(t, waitValue["values"].(map[string]any)["result"])
	if run, ok := waitValue["run"].(map[string]any); ok {
		t.Cleanup(func() { _ = f.store.DeleteThread(context.Background(), run["thread_id"].(string)) })
	}
	thirdThread, thirdRun := "number-cache-neighbor-"+uuid.NewString(), uuid.NewString()
	t.Cleanup(func() { _ = f.store.DeleteThread(context.Background(), thirdThread) })
	t.Cleanup(func() { _ = f.queue.Cancel(context.Background(), thirdRun) })
	third := numberRequest(t, f, http.MethodPost, "/threads/"+thirdThread+"/runs", requestBody(thirdRun, `{"identity":9007199254740992,"one":1}`))
	if numberJSON(t, third.Body.Bytes()).(map[string]any)["metadata"].(map[string]any)["cache_hit"] == true {
		t.Fatalf("neighboring large input hit wrong cache: %s", third.Body.String())
	}
	job, err = f.queue.Dequeue(ctx, "python-langgraph", time.Second)
	if err != nil || job == nil || job.RunID != thirdRun {
		t.Fatalf("neighbor should dispatch: %v %v", job, err)
	}
}

func TestJSONNumbersStorePutGetAndFilter(t *testing.T) {
	f := jsonNumbersFixture(t)
	namespace := "number-store-" + uuid.NewString()
	for _, key := range []string{"wanted", "neighbor"} {
		key := key
		t.Cleanup(func() { _ = f.store.DeleteItem(context.Background(), []string{namespace}, key) })
	}
	for _, item := range []struct{ key, path, value string }{
		{"wanted", "/store/items", numberResult},
		{"neighbor", "/internal/store/items", `{"large":9007199254740992}`},
	} {
		request := httptest.NewRequest(http.MethodPut, item.path, strings.NewReader(fmt.Sprintf(`{"namespace":[%q],"key":%q,"value":%s}`, namespace, item.key, item.value)))
		response := httptest.NewRecorder()
		f.plane.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("store put: %d %s", response.Code, response.Body.String())
		}
	}
	for _, path := range []string{"/store/items", "/internal/store/items"} {
		response := numberRequest(t, f, http.MethodGet, path+"?namespace="+namespace+"&key=wanted", "")
		assertNumberResult(t, numberJSON(t, response.Body.Bytes()).(map[string]any)["value"])
	}
	all := numberRequest(t, f, http.MethodPost, "/store/items/search", fmt.Sprintf(`{"namespace_prefix":[%q]}`, namespace))
	items := numberJSON(t, all.Body.Bytes()).(map[string]any)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("unfiltered store search returned %d items", len(items))
	}
	for _, item := range items {
		value := item.(map[string]any)
		if value["key"] == "wanted" {
			assertNumberResult(t, value["value"])
		}
	}
	for _, item := range []struct{ key, number string }{{"wanted", "9007199254740993"}, {"neighbor", "9007199254740992"}} {
		response := numberRequest(t, f, http.MethodPost, "/store/items/search", fmt.Sprintf(`{"namespace_prefix":[%q],"filter":{"large":%s}}`, namespace, item.number))
		items := numberJSON(t, response.Body.Bytes()).(map[string]any)["items"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["key"] != item.key {
			t.Errorf("numeric filter %s matched wrong store item: %s", item.number, response.Body.String())
		}
	}
}

func TestJSONNumbersCommandsPreserveInputAndResume(t *testing.T) {
	for _, mode := range []string{"http", "websocket"} {
		t.Run(mode, func(t *testing.T) {
			f := jsonNumbersFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			threadID := "number-command-" + uuid.NewString()
			now := time.Now().UTC()
			if err := f.store.CreateThread(ctx, &models.Thread{ThreadID: threadID, Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.store.DeleteThread(context.Background(), threadID) })
			var server *httptest.Server
			if mode == "websocket" {
				server = httptest.NewServer(f.plane.Handler())
				defer server.Close()
			}
			send := func(id int, method, params string) string {
				t.Helper()
				raw := fmt.Sprintf(`{"id":%d,"method":%q,"params":%s}`, id, method, params)
				var ack models.StreamingCommandResponse
				if mode == "http" {
					response := numberRequest(t, f, http.MethodPost, "/threads/"+threadID+"/commands", raw)
					if err := json.Unmarshal(response.Body.Bytes(), &ack); err != nil {
						t.Fatal(err)
					}
				} else {
					conn, _, err := websocket.Dial(ctx, wsURL(server.URL)+"/threads/"+threadID+"/websocket", nil)
					if err != nil {
						t.Fatal(err)
					}
					defer conn.CloseNow()
					if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
						t.Fatal(err)
					}
					if err := wsjson.Read(ctx, conn, &ack); err != nil {
						t.Fatal(err)
					}
				}
				if ack.ID != id || ack.Type != "success" {
					t.Fatalf("command acknowledgement changed: %+v", ack)
				}
				runID, _ := ack.Result["run_id"].(string)
				if runID == "" {
					t.Fatal("command did not return a run ID")
				}
				t.Cleanup(func() {
					_ = f.queue.Cancel(context.Background(), runID)
					_ = f.broker.Close(runID)
				})
				return runID
			}
			runID := send(27, "run.start", fmt.Sprintf(`{"agent_id":%q,"input":%s}`, f.agentID, numberResult))
			job, err := f.queue.Dequeue(ctx, "python-langgraph", time.Second)
			if err != nil || job == nil || job.RunID != runID {
				t.Fatalf("command dispatch: job=%+v err=%v", job, err)
			}
			assertNumberResult(t, numberJSON(t, job.Input))
			run, err := f.store.GetRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			assertNumberResult(t, numberJSON(t, run.Input))
			if accepted, err := f.store.BeginRunAttempt(ctx, runID, job.Generation); err != nil || !accepted {
				t.Fatalf("begin command run: accepted=%v err=%v", accepted, err)
			}
			numberRequest(t, f, http.MethodPost, "/runs/"+runID+"/cancel", `{}`)
			resumed := send(28, "input.respond", `{"response":`+numberResult+`}`)
			resumeJob, err := f.queue.Dequeue(ctx, "python-langgraph", time.Second)
			if err != nil || resumeJob == nil || resumeJob.RunID != resumed {
				t.Fatalf("resume dispatch: job=%+v err=%v", resumeJob, err)
			}
			assertNumberResult(t, numberJSON(t, resumeJob.ResumeCommand))
		})
	}
}
