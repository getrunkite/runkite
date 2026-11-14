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
	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func TestDurableUsageReportsSettleUnknownZeroAndCancelledPartialExactlyOnce(t *testing.T) {
	for _, scenario := range []struct {
		name, body, status string
		in, out            int64
		unknown            bool
	}{
		{"unknown", `{"schema_version":1,"sequence":1,"status":"unknown","complete":false}`, "success", 0, 0, true},
		{"reported-zero", `{"schema_version":1,"sequence":1,"status":"reported","complete":true,"prompt_tokens":0,"completion_tokens":0,"cost_usd":0}`, "success", 0, 0, false},
		{"cancelled-partial", `{"schema_version":1,"sequence":1,"status":"reported","complete":false,"prompt_tokens":10,"completion_tokens":4}`, "interrupted", 10, 4, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store, queue, broker, cancel := completionFixture(t, map[string]interface{}{"run_manifest": map[string]interface{}{"runner_kind": "python-isolated"}})
			plane := api.NewServer(store, queue, broker, cancel)
			ctx := tenant.ScopedContext(t.Context(), "default")
			if _, err := store.BeginRunAttempt(ctx, "run", 1); err != nil {
				t.Fatal(err)
			}
			run, _ := store.GetRun(ctx, "run")
			bound := auth.WithRunBinding(ctx, &auth.RunBinding{RunID: "run", RunIncarnationID: run.IncarnationID, ThreadID: "thread", TenantID: "default", AgentID: "agent", Generation: 1})
			for range 2 {
				response := httptest.NewRecorder()
				plane.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/internal/runs/run/usage", strings.NewReader(scenario.body)).WithContext(bound))
				if response.Code != http.StatusOK {
					t.Fatalf("report: %d %s", response.Code, response.Body.String())
				}
			}
			for range 2 {
				accepted, err := plane.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, RunID: "run", Generation: 1, Status: models.RunStatus(scenario.status), CancelCurrentAttempt: scenario.status == "interrupted"})
				if err != nil || !accepted {
					t.Fatal(accepted, err)
				}
			}
			got, _ := store.GetRun(ctx, "run")
			var result map[string]interface{}
			if err := json.Unmarshal(got.Output, &result); err != nil {
				t.Fatal(err)
			}
			u := result["usage"].(map[string]interface{})
			if u["reporting_source"] != "isolated_workload" || u["unmetered"] != scenario.unknown {
				t.Fatalf("provenance/unknown status lost: %s", got.Output)
			}
			in, out, _, err := store.SumUsage(ctx, "default", "agent", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
			if err != nil || in != scenario.in || out != scenario.out {
				t.Fatal("lost/double usage", in, out, err)
			}
			rows, err := store.SearchUsageSummary(ctx, &models.UsageSummaryRequest{TenantID: "default"})
			if err != nil || len(rows) != 1 || rows[0].RunCount != 1 {
				t.Fatal("unknown/zero report was silently omitted or duplicated", rows, err)
			}
		})
	}
}

func TestUsageReportEndpointRejectsAmbiguityAndCallerAuthority(t *testing.T) {
	store, queue, broker, cancel := completionFixture(t, map[string]interface{}{"run_manifest": map[string]interface{}{"runner_kind": "python-isolated"}})
	plane := api.NewServer(store, queue, broker, cancel)
	ctx := context.Background()
	if _, err := store.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	run, _ := store.GetRun(ctx, "run")
	bound := auth.WithRunBinding(ctx, &auth.RunBinding{RunID: "run", RunIncarnationID: run.IncarnationID, ThreadID: "thread", TenantID: "default", AgentID: "agent", Generation: 1})
	for _, body := range []string{
		`{"schema_version":1,"sequence":1,"sequence":2,"status":"unknown","complete":false}`,
		`{"schema_version":1,"sequence":1,"status":"unknown","complete":false,"run_id":"foreign"}`,
		`{"schema_version":1,"sequence":1,"status":"unknown","complete":false,"prompt_tokens":null}`,
		`{"schema_version":1,"sequence":1,"status":"unknown","complete":false,"model":""}`,
		`{"schema_version":1,"sequence":1,"status":"reported","complete":false,"prompt_tokens":true,"completion_tokens":0}`,
		`{"schema_version":1,"sequence":1,"status":"reported","complete":false,"prompt_tokens":-1,"completion_tokens":0}`,
		`{"schema_version":1,"sequence":1,"status":"reported","complete":false,"prompt_tokens":0,"completion_tokens":0,"cost_usd":1e309}`,
		`{"schema_version":1,"sequence":1,"status":"unknown","complete":null}`,
		`{"schema_version":1,"sequence":1,"status":"unknown","complete":false} {}`,
	} {
		response := httptest.NewRecorder()
		plane.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/internal/runs/run/usage", strings.NewReader(body)).WithContext(bound))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("ambiguous report accepted: %d %s", response.Code, response.Body.String())
		}
	}
	for _, candidate := range []context.Context{ctx, auth.WithRunBinding(ctx, &auth.RunBinding{RunID: "foreign", RunIncarnationID: run.IncarnationID})} {
		response := httptest.NewRecorder()
		plane.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/internal/runs/run/usage", nil).WithContext(candidate))
		if response.Code != http.StatusForbidden {
			t.Fatal("unbound caller read usage", response.Code)
		}
	}
	if got, err := store.GetRunUsage(ctx, state.RunUsageBinding{RunCheckpointBinding: state.RunCheckpointBinding{RunID: "run", ThreadID: "thread", Generation: 1}, RunIncarnationID: run.IncarnationID, TenantID: "default"}); err != nil || got != nil {
		t.Fatal("invalid bodies changed private snapshot", got, err)
	}
}

func TestUsageReportRequiresFrozenIsolatedProfile(t *testing.T) {
	store, queue, broker, cancel := completionFixture(t)
	plane := api.NewServer(store, queue, broker, cancel)
	ctx := context.Background()
	if _, err := store.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	run, _ := store.GetRun(ctx, "run")
	bound := auth.WithRunBinding(ctx, &auth.RunBinding{RunID: "run", RunIncarnationID: run.IncarnationID, ThreadID: "thread", TenantID: "default", AgentID: "agent", Generation: 1})
	response := httptest.NewRecorder()
	plane.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/internal/runs/run/usage", strings.NewReader(`{"schema_version":1,"sequence":1,"status":"unknown","complete":false}`)).WithContext(bound))
	if response.Code != http.StatusForbidden {
		t.Fatal("ordinary runner relabeled as isolated reporting profile", response.Code)
	}
}
