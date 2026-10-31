package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

type toolHookSubscriptionBroker struct {
	transport.EventBroker
	mu       sync.Mutex
	ready    map[string]bool
	contexts chan context.Context
}

func (b *toolHookSubscriptionBroker) Subscribe(ctx context.Context, runID string) (<-chan *transport.RunEvent, error) {
	ch, err := b.EventBroker.Subscribe(ctx, runID)
	if err == nil {
		b.mu.Lock()
		b.ready[runID] = true
		b.mu.Unlock()
		b.contexts <- ctx
	}
	return ch, err
}

type immediateToolHookQueue struct {
	transport.JobQueue
	broker *toolHookSubscriptionBroker
}

func (q *immediateToolHookQueue) Enqueue(ctx context.Context, job *transport.RunAssignment) error {
	q.broker.mu.Lock()
	ready := q.broker.ready[job.RunID]
	q.broker.mu.Unlock()
	if !ready {
		return errors.New("runner dispatched before the tool-hook subscription was ready")
	}
	for _, event := range []*transport.RunEvent{
		{Generation: 1, Seq: 1, Method: "tool_call", Data: json.RawMessage(`{"tool":"search"}`)},
		{Generation: 1, Seq: 2, Method: "end", Data: json.RawMessage(`{"status":"success"}`)},
	} {
		if err := q.broker.Publish(ctx, job.RunID, event); err != nil {
			return err
		}
	}
	return nil
}

type subscriptionHookSink struct{ events chan hooks.Event }

func (s *subscriptionHookSink) Handle(_ context.Context, event hooks.Event) { s.events <- event }

func TestHooks_ToolCallSubscriptionReadyBeforeDispatch(t *testing.T) {
	// Model a fast same-process runner without giving an asynchronously
	// scheduled subscription a chance to hide the missing ordering guarantee.
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	ctx := context.Background()
	store, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "instant-tool-agent", Name: "instant-tool-agent"}); err != nil {
		t.Fatal(err)
	}
	broker := &toolHookSubscriptionBroker{
		EventBroker: inprocess.NewBroker(), ready: make(map[string]bool), contexts: make(chan context.Context, 4),
	}
	queue := &immediateToolHookQueue{JobQueue: inprocess.NewQueue(), broker: broker}
	plane := api.NewServer(store, queue, broker, inprocess.NewCancelBus())
	sink := &subscriptionHookSink{events: make(chan hooks.Event, 4)}
	dispatcher := hooks.NewDispatcher()
	dispatcher.Register(sink, hooks.ToolCall)
	plane.SetHookDispatcher(dispatcher)
	t.Cleanup(func() {
		deadline, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := dispatcher.Close(deadline); err != nil {
			t.Error(err)
		}
	})
	for attempt := 0; attempt < 4; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/runs", strings.NewReader(`{"agent_id":"instant-tool-agent"}`))
		response := httptest.NewRecorder()
		plane.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("immediate dispatch failed: status=%d body=%s", response.Code, response.Body.String())
		}
		var run models.Run
		if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		select {
		case event := <-sink.events:
			if event.RunID != run.RunID || event.Type != hooks.ToolCall || event.Data["tool"] != "search" {
				t.Fatalf("wrong immediate tool hook: %+v", event)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("immediate tool event was missed")
		}
		subscription := <-broker.contexts
		if _, bounded := subscription.Deadline(); !bounded {
			t.Fatal("tool-hook subscription has no orphan deadline")
		}
		select {
		case <-subscription.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("finished tool-hook watcher did not cancel its broker subscription")
		}
	}
}
