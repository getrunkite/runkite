package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/execution"
	"github.com/getrunkite/runkite/internal/models"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestPoolAdmissionRejectsWrongAgentAndUnknownPoolBeforeRun(t *testing.T) {
	for _, tc := range []struct {
		agent, kind string
		want        int
	}{{"finance", "pool-finance", 200}, {"foreign", "pool-finance", 403}, {"finance", "pool-unknown", 403}, {"finance", "python-langgraph", 403}} {
		t.Run(tc.agent+tc.kind, func(t *testing.T) {
			store, err := sqlitestore.New("")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.Init(context.Background()); err != nil {
				t.Fatal(err)
			}
			queue := inprocess.NewQueue()
			server := api.NewServer(store, queue, inprocess.NewBroker(), inprocess.NewCancelBus())
			catalog, err := execution.NewCatalog(map[string]execution.Pool{"finance": {RunnerKind: "python-langgraph", Tenants: []string{"default"}, Agents: []string{"finance"}}})
			if err != nil {
				t.Fatal(err)
			}
			server.SetExecutionPools(catalog)
			if err := store.UpsertAgent(context.Background(), &models.Agent{AgentID: tc.agent, Name: tc.agent, Metadata: map[string]interface{}{"runner_kind": tc.kind}}); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]any{"agent_id": tc.agent, "input": map[string]any{"synthetic": true}, "metadata": map[string]any{"run_incarnation_id": "caller-supplied-nonce"}})
			request := httptest.NewRequest("POST", "/runs", strings.NewReader(string(body)))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != tc.want {
				var body any
				json.NewDecoder(response.Body).Decode(&body)
				t.Fatalf("status %d: %v", response.Code, body)
			}
			if tc.want == http.StatusOK {
				job, err := queue.Dequeue(context.Background(), tc.kind, time.Second)
				if err != nil || job == nil || job.GraphID != tc.agent {
					t.Fatalf("job=%v err=%v", job, err)
				}
				stored, err := store.GetRun(context.Background(), job.RunID)
				if err != nil || job.RunIncarnationID == "" || job.RunIncarnationID == "caller-supplied-nonce" || job.RunIncarnationID != stored.IncarnationID {
					t.Fatalf("assignment did not capture original durable incarnation: assignment=%q stored=%v err=%v", job.RunIncarnationID, stored, err)
				}
			} else {
				job, err := queue.Dequeue(context.Background(), tc.kind, time.Millisecond)
				if err != nil || job != nil {
					t.Fatalf("unauthorized assignment queued: %v %v", job, err)
				}
			}
		})
	}
}
