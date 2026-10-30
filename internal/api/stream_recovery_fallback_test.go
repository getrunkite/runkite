package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/transport"
)

type unavailableStreamBroker struct {
	transport.EventBroker
	failSubscribe bool
}

func (b *unavailableStreamBroker) Subscribe(ctx context.Context, runID string) (<-chan *transport.RunEvent, error) {
	if b.failSubscribe {
		return nil, errors.New("synthetic subscription outage")
	}
	return b.EventBroker.Subscribe(ctx, runID)
}

func (b *unavailableStreamBroker) Replay(context.Context, string, int64) ([]*transport.RunEvent, error) {
	return nil, errors.New("synthetic replay outage")
}

func TestStreamRecoveryCompletedRunSurvivesBrokerOutage(t *testing.T) {
	for _, failSubscribe := range []bool{false, true} {
		name := "replay"
		if failSubscribe {
			name = "subscribe_and_replay"
		}
		t.Run(name, func(t *testing.T) {
			store, queue, broker, cancels, runID, _ := streamRecoveryFixture(t)
			plane := api.NewServer(store, queue, broker, cancels)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if accepted, err := store.BeginRunAttempt(ctx, runID, 1); err != nil || !accepted {
				t.Fatalf("begin attempt: accepted=%v err=%v", accepted, err)
			}
			recoveryPublish(t, broker, runID, 1, 1, "durable-values", "values", `{"answer":"durable"}`)
			if accepted, err := plane.CompleteRun(ctx, &state.RunCompletion{RunID: runID, Generation: 1, Status: models.RunStatusSuccess}); err != nil || !accepted {
				t.Fatalf("completion: accepted=%v err=%v", accepted, err)
			}
			plane = api.NewServer(store, queue, &unavailableStreamBroker{EventBroker: broker, failSubscribe: failSubscribe}, cancels)
			response := httptest.NewRecorder()
			plane.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/stream", nil).WithContext(ctx))
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"success"`) || strings.Count(response.Body.String(), "event: end\n") != 1 {
				t.Fatalf("durable stream depended on broker: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

type streamContextBroker struct {
	transport.EventBroker
	contexts chan context.Context
}

func (b *streamContextBroker) Subscribe(ctx context.Context, runID string) (<-chan *transport.RunEvent, error) {
	ch, err := b.EventBroker.Subscribe(ctx, runID)
	if err == nil {
		b.contexts <- ctx
	}
	return ch, err
}

type failedStreamWriter struct{ header http.Header }

func (w *failedStreamWriter) Header() http.Header { return w.header }
func (w *failedStreamWriter) WriteHeader(int)     {}
func (w *failedStreamWriter) Flush()              {}
func (w *failedStreamWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic disconnected client")
}

func TestStreamRecoveryWriteFailureStopsSubscriptionWithoutCompletingRun(t *testing.T) {
	store, queue, rawBroker, cancels, runID, _ := streamRecoveryFixture(t)
	broker := &streamContextBroker{EventBroker: rawBroker, contexts: make(chan context.Context, 1)}
	plane := api.NewServer(store, queue, broker, cancels)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if accepted, err := store.BeginRunAttempt(ctx, runID, 1); err != nil || !accepted {
		t.Fatalf("begin attempt: accepted=%v err=%v", accepted, err)
	}
	done := make(chan struct{})
	go func() {
		plane.Handler().ServeHTTP(&failedStreamWriter{header: make(http.Header)}, httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/stream", nil).WithContext(ctx))
		close(done)
	}()
	var subscription context.Context
	select {
	case subscription = <-broker.contexts:
	case <-ctx.Done():
		t.Fatal("stream did not subscribe")
	}
	recoveryPublish(t, rawBroker, runID, 1, 1, "client-message", "messages", `[{"type":"ai","content":"synthetic"},{}]`)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("stream continued after client write failure")
	}
	if subscription.Err() == nil {
		t.Fatal("stream left its subscription context alive")
	}
	run, err := store.GetRun(ctx, runID)
	if err != nil || state.TerminalRunStatus(run.Status) {
		t.Fatalf("client disconnect changed run outcome: run=%+v err=%v", run, err)
	}
}
