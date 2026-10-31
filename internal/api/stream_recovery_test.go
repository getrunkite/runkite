package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/getrunkite/runkite/internal/api"
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

// Dedicated paired environment variables opt into the supported backend pair.
// UUID identities isolate each test without flushing Redis or deleting another
// test's state. Ordinary API tests continue to use SQLite and in-process fanout.
func streamRecoveryFixture(t *testing.T) (state.Store, transport.JobQueue, transport.EventBroker, transport.CancelBroker, string, string) {
	t.Helper()
	ctx := context.Background()
	dsn, redisURL := os.Getenv("STREAM_RECOVERY_POSTGRES_DSN"), os.Getenv("STREAM_RECOVERY_REDIS_URL")
	if (dsn == "") != (redisURL == "") {
		t.Fatal("set both STREAM_RECOVERY_POSTGRES_DSN and STREAM_RECOVERY_REDIS_URL, or neither")
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
	runID, threadID := "stream-recovery-run-"+suffix, "stream-recovery-thread-"+suffix
	now := time.Now().UTC()
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: threadID, Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, &models.Run{RunID: runID, ThreadID: threadID, AgentID: "stream-recovery-agent-" + suffix, Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = broker.Close(runID)
		_ = store.DeleteThread(context.Background(), threadID)
	})
	return store, queue, broker, cancels, runID, threadID
}

type recoverySubscribeBroker struct {
	transport.EventBroker
	subscribed chan struct{}
	replayed   chan struct{}
	once       sync.Once
	replayOnce sync.Once
}

func (b *recoverySubscribeBroker) Subscribe(ctx context.Context, runID string) (<-chan *transport.RunEvent, error) {
	events, err := b.EventBroker.Subscribe(ctx, runID)
	if err == nil {
		b.once.Do(func() { close(b.subscribed) })
	}
	return events, err
}

func (b *recoverySubscribeBroker) Replay(ctx context.Context, runID string, sinceSeq int64) ([]*transport.RunEvent, error) {
	events, err := b.EventBroker.Replay(ctx, runID, sinceSeq)
	if err == nil {
		b.replayOnce.Do(func() { close(b.replayed) })
	}
	return events, err
}

func recoveryPublish(t *testing.T, broker transport.EventBroker, runID string, generation, seq int64, id, method, data string) {
	t.Helper()
	if err := broker.Publish(context.Background(), runID, &transport.RunEvent{
		Generation: generation, EventID: id, Seq: seq, Method: method,
		Namespace: []string{}, Data: json.RawMessage(data), Ts: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
}

func recoveryPublishCurrent(t *testing.T, broker transport.EventBroker, runID string) {
	t.Helper()
	recoveryPublish(t, broker, runID, 2, 1, "current-message", "messages", `[{"type":"ai","content":"current answer"},{}]`)
	recoveryPublish(t, broker, runID, 2, 2, "current-values", "values", `{"answer":"current answer"}`)
	recoveryPublish(t, broker, runID, 2, 3, "current-terminal", "end", `{"status":"success"}`)
}

func recoveryWaitSubscribed(t *testing.T, ctx context.Context, broker *recoverySubscribeBroker) {
	t.Helper()
	select {
	case <-broker.subscribed:
	case <-ctx.Done():
		t.Fatal("stream never subscribed")
	}
	select {
	case <-broker.replayed:
	case <-ctx.Done():
		t.Fatal("stream never replayed its initial history")
	}
}

// The direct broker publish models a runner paused after a successful queue
// fence but before publication. Generation 2 is durably installed first, then
// the delayed generation-1 terminal arrives. No scheduling sleep creates the race.
func TestStreamRecoveryRunAndThreadSurviveSupersededTerminal(t *testing.T) {
	for _, replay := range []bool{false, true} {
		for _, thread := range []bool{false, true} {
			for _, terminal := range []string{"end", "error"} {
				for _, staleGeneration := range []int64{0, 1} {
					t.Run(fmt.Sprintf("replay_%t_thread_%t_%s_generation_%d", replay, thread, terminal, staleGeneration), func(t *testing.T) {
						store, queue, rawBroker, cancels, runID, threadID := streamRecoveryFixture(t)
						broker := &recoverySubscribeBroker{EventBroker: rawBroker, subscribed: make(chan struct{}), replayed: make(chan struct{})}
						plane := api.NewServer(store, queue, broker, cancels)
						ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
						defer cancel()
						if accepted, err := store.BeginRunAttempt(ctx, runID, 1); err != nil || !accepted {
							t.Fatalf("begin first attempt: accepted=%v err=%v", accepted, err)
						}
						if accepted, err := store.BeginRunAttempt(ctx, runID, 2); err != nil || !accepted {
							t.Fatalf("begin replacement: accepted=%v err=%v", accepted, err)
						}
						if replay {
							recoveryPublish(t, rawBroker, runID, staleGeneration, 80, "stale-terminal", terminal, `{"status":"error","message":"superseded"}`)
						}
						method, path, body := http.MethodGet, "/runs/"+runID+"/stream", ""
						if thread {
							method, path, body = http.MethodPost, "/threads/"+threadID+"/stream", `{}`
						}
						request := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
						response := httptest.NewRecorder()
						done := make(chan struct{})
						go func() {
							plane.Handler().ServeHTTP(response, request)
							close(done)
						}()
						recoveryWaitSubscribed(t, ctx, broker)
						if !replay {
							recoveryPublish(t, rawBroker, runID, staleGeneration, 80, "stale-terminal", terminal, `{"status":"error","message":"superseded"}`)
						}
						recoveryPublishCurrent(t, rawBroker, runID)
						select {
						case <-done:
						case <-ctx.Done():
							t.Fatal("stream did not finish after the replacement terminal")
						}
						got := response.Body.String()
						if response.Code != http.StatusOK || !strings.Contains(got, "current-message") || !strings.Contains(got, "current-values") || !strings.Contains(got, "current-terminal") {
							t.Fatalf("replacement output missing: status=%d body=%s", response.Code, got)
						}
						if strings.Contains(got, "stale-terminal") || strings.Count(got, "event: end\n") != 1 {
							t.Fatalf("superseded terminal reached client: %s", got)
						}
						saved, err := store.GetRun(ctx, runID)
						if err != nil || saved.Status != models.RunStatusSuccess || state.MetadataGeneration(saved.Metadata, state.CompletionGenerationKey) != 2 {
							t.Fatalf("stream ended without replacement completion: run=%+v err=%v", saved, err)
						}
						var output map[string]interface{}
						if err := json.Unmarshal(saved.Output, &output); err != nil || output["answer"] != "current answer" {
							t.Fatalf("durable replacement output=%s err=%v", saved.Output, err)
						}
					})
				}
			}
		}
	}
}

func TestStreamRecoveryWebSocketSurvivesSupersededTerminal(t *testing.T) {
	store, queue, rawBroker, cancels, _, _ := streamRecoveryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	agentID, threadID := "stream-recovery-agent-"+uuid.NewString(), "stream-recovery-ws-"+uuid.NewString()
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: agentID, Name: agentID}); err != nil {
		t.Fatal(err)
	}
	broker := &recoverySubscribeBroker{EventBroker: rawBroker, subscribed: make(chan struct{}), replayed: make(chan struct{})}
	plane := api.NewServer(store, queue, broker, cancels)
	server := httptest.NewServer(plane.Handler())
	defer server.Close()
	response, err := postJSON(server.URL+"/threads", map[string]interface{}{"thread_id": threadID})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("create thread status=%d", response.StatusCode)
	}
	t.Cleanup(func() { _ = store.DeleteThread(context.Background(), threadID) })
	conn, _, err := websocket.Dial(ctx, wsURL(server.URL)+"/threads/"+threadID+"/websocket", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := wsjson.Write(ctx, conn, models.StreamingCommand{
		ID: 1, Method: "run.start", Params: map[string]interface{}{"agent_id": agentID},
	}); err != nil {
		t.Fatal(err)
	}
	var ack models.StreamingCommandResponse
	if err := wsjson.Read(ctx, conn, &ack); err != nil || ack.Type != "success" {
		t.Fatalf("run.start ack=%+v err=%v", ack, err)
	}
	runID, _ := ack.Result["run_id"].(string)
	if runID == "" {
		t.Fatal("run.start missing run ID")
	}
	t.Cleanup(func() { _ = queue.Cancel(context.Background(), runID) })
	recoveryWaitSubscribed(t, ctx, broker)
	for _, generation := range []int64{1, 2} {
		if accepted, err := store.BeginRunAttempt(ctx, runID, generation); err != nil || !accepted {
			t.Fatalf("begin attempt %d: accepted=%v err=%v", generation, accepted, err)
		}
	}
	recoveryPublish(t, rawBroker, runID, 1, 80, "stale-terminal", "end", `{"status":"error"}`)
	recoveryPublishCurrent(t, rawBroker, runID)
	var methods []string
	for {
		var event models.StreamingEvent
		if err := wsjson.Read(ctx, conn, &event); err != nil {
			t.Fatalf("replacement WebSocket output missing: methods=%v err=%v", methods, err)
		}
		if event.EventID == "stale-terminal" {
			t.Fatalf("superseded terminal reached WebSocket: %+v", event)
		}
		methods = append(methods, event.Method)
		if event.Method == "end" {
			if event.EventID != "current-terminal" {
				t.Fatalf("wrong terminal=%+v", event)
			}
			break
		}
	}
	if strings.Join(methods, ",") != "messages,values,end" {
		t.Fatalf("replacement WebSocket event order=%v", methods)
	}
	saved, err := store.GetRun(ctx, runID)
	if err != nil || saved.Status != models.RunStatusSuccess || state.MetadataGeneration(saved.Metadata, state.CompletionGenerationKey) != 2 {
		t.Fatalf("WebSocket ended without replacement completion: run=%+v err=%v", saved, err)
	}
}

func TestStreamRecoveryThreadSinceDoesNotHideReplacementSequence(t *testing.T) {
	store, queue, rawBroker, cancels, runID, threadID := streamRecoveryFixture(t)
	broker := &recoverySubscribeBroker{EventBroker: rawBroker, subscribed: make(chan struct{}), replayed: make(chan struct{})}
	plane := api.NewServer(store, queue, broker, cancels)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if accepted, err := store.BeginRunAttempt(ctx, runID, 1); err != nil || !accepted {
		t.Fatalf("begin first attempt: accepted=%v err=%v", accepted, err)
	}
	recoveryPublish(t, rawBroker, runID, 1, 1, "already-seen", "messages", `[{"type":"ai","content":"already seen"},{}]`)
	request := httptest.NewRequest(http.MethodPost, "/threads/"+threadID+"/stream", strings.NewReader(`{"since":1}`)).WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		plane.Handler().ServeHTTP(response, request)
		close(done)
	}()
	recoveryWaitSubscribed(t, ctx, broker)
	if accepted, err := store.BeginRunAttempt(ctx, runID, 2); err != nil || !accepted {
		t.Fatalf("begin replacement: accepted=%v err=%v", accepted, err)
	}
	recoveryPublish(t, rawBroker, runID, 1, 2, "stale-terminal", "end", `{"status":"error"}`)
	recoveryPublishCurrent(t, rawBroker, runID)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("cursor stream did not finish after replacement")
	}
	got := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(got, "current-message") || !strings.Contains(got, "current-values") || !strings.Contains(got, "current-terminal") {
		t.Fatalf("replacement sequence hidden by prior attempt cursor: status=%d body=%s", response.Code, got)
	}
	if strings.Contains(got, "already-seen") || strings.Contains(got, "stale-terminal") {
		t.Fatalf("cursor replayed prior attempt: %s", got)
	}
}

type recoveryTransientCompletionStore struct {
	state.Store
	writes atomic.Int32
}

func (s *recoveryTransientCompletionStore) CompleteRun(ctx context.Context, completion *state.RunCompletion) (state.CompletionResult, error) {
	if s.writes.Add(1) == 1 {
		return state.CompletionResult{}, errors.New("injected completion transaction outage")
	}
	return s.Store.CompleteRun(ctx, completion)
}

func TestStreamRecoveryRetriesTerminalAfterTransientCompletionFailure(t *testing.T) {
	for _, thread := range []bool{false, true} {
		t.Run(fmt.Sprintf("thread_%t", thread), func(t *testing.T) {
			store, queue, rawBroker, cancels, runID, threadID := streamRecoveryFixture(t)
			fault := &recoveryTransientCompletionStore{Store: store}
			broker := &recoverySubscribeBroker{EventBroker: rawBroker, subscribed: make(chan struct{}), replayed: make(chan struct{})}
			plane := api.NewServer(fault, queue, broker, cancels)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if accepted, err := store.BeginRunAttempt(ctx, runID, 2); err != nil || !accepted {
				t.Fatalf("begin attempt: accepted=%v err=%v", accepted, err)
			}
			method, path, body := http.MethodGet, "/runs/"+runID+"/stream", ""
			if thread {
				method, path, body = http.MethodPost, "/threads/"+threadID+"/stream", `{}`
			}
			request := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				plane.Handler().ServeHTTP(response, request)
				close(done)
			}()
			recoveryWaitSubscribed(t, ctx, broker)
			recoveryPublishCurrent(t, rawBroker, runID)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("stream lost the terminal after its first completion attempt failed")
			}
			got := response.Body.String()
			saved, err := store.GetRun(ctx, runID)
			if response.Code != http.StatusOK || fault.writes.Load() < 2 || err != nil || saved.Status != models.RunStatusSuccess {
				t.Fatalf("completion was not retried before stream finished: status=%d writes=%d run=%+v err=%v body=%s", response.Code, fault.writes.Load(), saved, err, got)
			}
			if strings.Count(got, "id: current-terminal\n") != 1 || strings.Contains(got, "injected completion transaction outage") {
				t.Fatalf("terminal delivery after transient completion failure: %s", got)
			}
		})
	}
}

func TestStreamRecoveryFinishesDurableCancellationWithoutRunnerTerminal(t *testing.T) {
	for _, thread := range []bool{false, true} {
		t.Run(fmt.Sprintf("thread_%t", thread), func(t *testing.T) {
			store, queue, rawBroker, cancels, runID, threadID := streamRecoveryFixture(t)
			broker := &recoverySubscribeBroker{EventBroker: rawBroker, subscribed: make(chan struct{}), replayed: make(chan struct{})}
			plane := api.NewServer(store, queue, broker, cancels)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if accepted, err := store.BeginRunAttempt(ctx, runID, 2); err != nil || !accepted {
				t.Fatalf("begin attempt: accepted=%v err=%v", accepted, err)
			}
			method, path, body := http.MethodGet, "/runs/"+runID+"/stream", ""
			if thread {
				method, path, body = http.MethodPost, "/threads/"+threadID+"/stream", `{}`
			}
			request := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				plane.Handler().ServeHTTP(response, request)
				close(done)
			}()
			recoveryWaitSubscribed(t, ctx, broker)
			cancelResponse := httptest.NewRecorder()
			plane.Handler().ServeHTTP(cancelResponse, httptest.NewRequest(http.MethodPost, "/runs/"+runID+"/cancel", nil).WithContext(ctx))
			if cancelResponse.Code != http.StatusOK {
				t.Fatalf("cancel status=%d body=%s", cancelResponse.Code, cancelResponse.Body.String())
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("stream did not finish from persisted cancellation")
			}
			got := response.Body.String()
			if response.Code != http.StatusOK || !strings.Contains(got, `"status":"interrupted"`) || strings.Count(got, "event: end\n") != 1 {
				t.Fatalf("durable cancellation terminal missing: status=%d body=%s", response.Code, got)
			}
		})
	}
}

func TestStreamRecoveryDisconnectStopsWaitingWithoutCancellingRun(t *testing.T) {
	for _, thread := range []bool{false, true} {
		t.Run(fmt.Sprintf("thread_%t", thread), func(t *testing.T) {
			store, queue, rawBroker, cancels, runID, threadID := streamRecoveryFixture(t)
			broker := &recoverySubscribeBroker{EventBroker: rawBroker, subscribed: make(chan struct{}), replayed: make(chan struct{})}
			plane := api.NewServer(store, queue, broker, cancels)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if accepted, err := store.BeginRunAttempt(ctx, runID, 2); err != nil || !accepted {
				t.Fatalf("begin attempt: accepted=%v err=%v", accepted, err)
			}
			method, path, body := http.MethodGet, "/runs/"+runID+"/stream", ""
			if thread {
				method, path, body = http.MethodPost, "/threads/"+threadID+"/stream", `{}`
			}
			request := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				plane.Handler().ServeHTTP(response, request)
				close(done)
			}()
			waitCtx, stopWaiting := context.WithTimeout(context.Background(), 3*time.Second)
			defer stopWaiting()
			recoveryWaitSubscribed(t, waitCtx, broker)
			cancel()
			select {
			case <-done:
			case <-waitCtx.Done():
				_ = rawBroker.Close(runID)
				<-done
				t.Fatal("stream ignored its disconnected request context")
			}
			saved, err := store.GetRun(context.Background(), runID)
			if err != nil || saved.Status != models.RunStatusRunning {
				t.Fatalf("disconnect changed run status: run=%+v err=%v", saved, err)
			}
			if strings.Contains(response.Body.String(), "event: end\n") {
				t.Fatalf("disconnect invented a terminal: %s", response.Body.String())
			}
		})
	}
}

func TestStreamRecoveryCompletedReplaySkipsSupersededTerminal(t *testing.T) {
	for _, terminal := range []string{"end", "error"} {
		t.Run(terminal, func(t *testing.T) {
			store, queue, rawBroker, cancels, runID, _ := streamRecoveryFixture(t)
			plane := api.NewServer(store, queue, rawBroker, cancels)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if accepted, err := store.BeginRunAttempt(ctx, runID, 2); err != nil || !accepted {
				t.Fatalf("begin replacement: accepted=%v err=%v", accepted, err)
			}
			recoveryPublish(t, rawBroker, runID, 1, 80, "stale-terminal", terminal, `{"status":"error","message":"superseded"}`)
			recoveryPublishCurrent(t, rawBroker, runID)
			if accepted, err := plane.CompleteRun(ctx, &state.RunCompletion{RunID: runID, Generation: 2, Status: models.RunStatusSuccess}); err != nil || !accepted {
				t.Fatalf("complete replacement: accepted=%v err=%v", accepted, err)
			}
			request := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/stream", nil).WithContext(ctx)
			response := httptest.NewRecorder()
			plane.Handler().ServeHTTP(response, request)
			got := response.Body.String()
			if response.Code != http.StatusOK || !strings.Contains(got, "current-message") || !strings.Contains(got, "current-values") || !strings.Contains(got, "current-terminal") {
				t.Fatalf("completed replay lost current output: status=%d body=%s", response.Code, got)
			}
			if strings.Contains(got, "stale-terminal") || strings.Count(got, "event: end\n") != 1 {
				t.Fatalf("completed replay included superseded terminal: %s", got)
			}
		})
	}
}

func TestStreamRecoveryRetainsControlPlaneToolAuthEvents(t *testing.T) {
	for _, thread := range []bool{false, true} {
		for _, replay := range []bool{false, true} {
			for _, controlGeneration := range []int64{0, 2} {
				t.Run(fmt.Sprintf("thread_%t_replay_%t_control_generation_%d", thread, replay, controlGeneration), func(t *testing.T) {
					store, queue, rawBroker, cancels, runID, threadID := streamRecoveryFixture(t)
					broker := &recoverySubscribeBroker{EventBroker: rawBroker, subscribed: make(chan struct{}), replayed: make(chan struct{})}
					plane := api.NewServer(store, queue, broker, cancels)
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					if accepted, err := store.BeginRunAttempt(ctx, runID, 2); err != nil || !accepted {
						t.Fatalf("begin attempt: accepted=%v err=%v", accepted, err)
					}
					controlID := runID + "_tool_auth_fixed"
					publish := func() {
						recoveryPublish(t, rawBroker, runID, 2, 1, "current-message", "messages", `[{"type":"ai","content":"current answer"},{}]`)
						// Policy assigns its own sequence from replay, so it can
						// collide with the next event from the runner. Its stable
						// control-plane ID must deduplicate independently.
						recoveryPublish(t, rawBroker, runID, controlGeneration, 2, controlID, "tool_auth", `{"decision":"deny","reason":"operator policy"}`)
						recoveryPublish(t, rawBroker, runID, 2, 2, "current-values", "values", `{"answer":"current answer"}`)
						recoveryPublish(t, rawBroker, runID, 2, 3, "current-terminal", "end", `{"status":"success"}`)
					}
					if replay {
						publish()
					}
					method, path, body := http.MethodGet, "/runs/"+runID+"/stream", ""
					if thread {
						method, path, body = http.MethodPost, "/threads/"+threadID+"/stream", `{"channels":["messages","values"]}`
					}
					request := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
					response := httptest.NewRecorder()
					done := make(chan struct{})
					go func() {
						plane.Handler().ServeHTTP(response, request)
						close(done)
					}()
					recoveryWaitSubscribed(t, ctx, broker)
					if !replay {
						publish()
					}
					select {
					case <-done:
					case <-ctx.Done():
						t.Fatal("control-event stream did not finish")
					}
					got := response.Body.String()
					if response.Code != http.StatusOK || !strings.Contains(got, controlID) || !strings.Contains(got, "event: tool_auth\n") || !strings.Contains(got, "current-message") || !strings.Contains(got, "current-values") || !strings.Contains(got, "current-terminal") {
						t.Fatalf("control event or colliding runner event lost: status=%d body=%s", response.Code, got)
					}
					if strings.Count(got, "id: "+controlID+"\n") != 1 || strings.Count(got, "id: current-values\n") != 1 {
						t.Fatalf("control/runner event duplicated across replay and live tail: %s", got)
					}
				})
			}
		}
	}
}
