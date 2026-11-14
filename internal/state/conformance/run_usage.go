package conformance

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

func RunUsageSnapshots(t *testing.T, store state.Store) {
	usage := store.(state.RunUsageStore)
	reported := func(sequence, in, out int64, complete bool) models.RunUsageReport {
		return models.RunUsageReport{SchemaVersion: 1, Sequence: sequence, Status: models.RunUsageReported, Complete: complete, PromptTokens: &in, CompletionTokens: &out, Model: "offline-fixture"}
	}
	t.Run("output_cannot_fabricate_durable_report_provenance", func(t *testing.T) {
		for _, cancellation := range []bool{false, true} {
			ctx, fixture := connectorActionFixture(t, store)
			forged := map[string]interface{}{"reporting_source": "isolated_workload", "reporting_status": "reported", "prompt_tokens": 999, "completion_tokens": 999, "cost_usd": 0, "cost_reported": true, "complete": true}
			completion := &state.RunCompletion{TrustedSystem: true, RunID: fixture.RunID, Generation: 1, Status: models.RunStatusSuccess, Values: map[string]interface{}{"usage": forged}}
			if cancellation {
				completion.CancelCurrentAttempt = true
				completion.CancelUsage = forged
				completion.Status = models.RunStatusInterrupted
			}
			if result, err := store.CompleteRun(ctx, completion); err != nil || !result.Applied {
				t.Fatal(result, err)
			}
			run, _ := store.GetRun(ctx, fixture.RunID)
			var values map[string]interface{}
			if err := json.Unmarshal(run.Output, &values); err != nil {
				t.Fatal(err)
			}
			u := values["usage"].(map[string]interface{})
			if u["reporting_source"] != "unverified_output" || u["reporting_status"] != "unknown" || u["unmetered"] != true || u["prompt_tokens"] != nil || u["cost_reported"] != nil {
				t.Fatalf("uncommitted output forged durable source: %s", run.Output)
			}
		}
	})
	t.Run("cumulative_idempotency_reclaim_and_terminal_settlement", func(t *testing.T) {
		ctx, fixture := connectorActionFixture(t, store)
		run, err := store.GetRun(ctx, fixture.RunID)
		if err != nil {
			t.Fatal(err)
		}
		binding := state.RunUsageBinding{RunCheckpointBinding: state.RunCheckpointBinding{RunID: run.RunID, ThreadID: run.ThreadID, Generation: 1}, RunIncarnationID: run.IncarnationID, TenantID: run.TenantID}
		if got, err := usage.GetRunUsage(ctx, binding); err != nil || got != nil {
			t.Fatal(got, err)
		}
		unknown := models.RunUsageReport{SchemaVersion: 1, Sequence: 1, Status: models.RunUsageUnknown}
		if _, applied, err := usage.ReportRunUsage(ctx, binding, unknown); err != nil || !applied {
			t.Fatal(applied, err)
		}
		partial := reported(2, 10, 4, false)
		first, applied, err := usage.ReportRunUsage(ctx, binding, partial)
		if err != nil || !applied {
			t.Fatal(first, applied, err)
		}
		for range 3 {
			got, applied, err := usage.ReportRunUsage(ctx, binding, partial)
			if err != nil || applied || !got.ReportedAt.Equal(first.ReportedAt) {
				t.Fatal("retry changed cumulative report", got, applied, err)
			}
		}
		for _, bad := range []models.RunUsageReport{reported(2, 11, 4, false), reported(3, 9, 4, false), reported(4, 10, 4, false)} {
			if _, _, err := usage.ReportRunUsage(ctx, binding, bad); err == nil {
				t.Fatal("changed/reordered/regressing cumulative report accepted")
			}
		}
		if _, err := store.BeginRunAttempt(ctx, run.RunID, 2); err != nil {
			t.Fatal(err)
		}
		if _, _, err := usage.ReportRunUsage(ctx, binding, partial); err == nil {
			t.Fatal("stale attempt report accepted")
		}
		binding.Generation = 2
		if got, applied, err := usage.ReportRunUsage(ctx, binding, partial); err != nil || applied || got.ReportedGeneration != 1 {
			t.Fatal("receipt replay changed original report provenance", got, applied, err)
		}
		if _, _, err := usage.ReportRunUsage(ctx, binding, reported(3, 0, 0, true)); err == nil {
			t.Fatal("reclaim reset earlier actual usage")
		}
		final := reported(3, 12, 6, true)
		if _, applied, err := usage.ReportRunUsage(ctx, binding, final); err != nil || !applied {
			t.Fatal(applied, err)
		}
		if _, _, err := usage.ReportRunUsage(ctx, binding, reported(4, 14, 8, true)); err == nil {
			t.Fatal("measurement continued after final report")
		}
		completion := &state.RunCompletion{RunID: run.RunID, Generation: 2, Status: models.RunStatusSuccess, Values: map[string]interface{}{"answer": "synthetic", "usage": map[string]interface{}{"prompt_tokens": 999}}}
		if result, err := store.CompleteRun(ctx, completion); err != nil || !result.Applied {
			t.Fatal(result, err)
		}
		if result, err := store.CompleteRun(ctx, completion); err != nil || result.Applied || !result.Accepted {
			t.Fatal("completion replay changed settlement", result, err)
		}
		got, _ := store.GetRun(ctx, run.RunID)
		var values map[string]interface{}
		if err := json.Unmarshal(got.Output, &values); err != nil {
			t.Fatal(err)
		}
		u := values["usage"].(map[string]interface{})
		if u["prompt_tokens"] != float64(12) || u["completion_tokens"] != float64(6) || u["reporting_source"] != "isolated_workload" {
			t.Fatalf("durable report lost to output forgery: %s", got.Output)
		}
		if _, _, err := usage.ReportRunUsage(ctx, binding, final); err == nil {
			t.Fatal("postterminal report accepted")
		}
	})
	t.Run("exact_binding_and_same_pool_worker_separation", func(t *testing.T) {
		ctx, fixture := connectorActionFixture(t, store)
		run, _ := store.GetRun(ctx, fixture.RunID)
		owner := &transport.WorkloadIdentity{URI: "spiffe://fixture.test/pool/worker-a", KeySHA256: strings.Repeat("a", 64), NotAfter: time.Now().Add(time.Hour)}
		ctx = transport.WithWorkloadIdentity(ctx, owner)
		if accepted, err := store.BeginRunAttempt(ctx, run.RunID, 2); err != nil || !accepted {
			t.Fatal(accepted, err)
		}
		binding := state.RunUsageBinding{RunCheckpointBinding: state.RunCheckpointBinding{RunID: run.RunID, ThreadID: run.ThreadID, Generation: 2}, RunIncarnationID: run.IncarnationID, TenantID: run.TenantID}
		finalReport := reported(1, 0, 0, true)
		if snapshot, applied, err := usage.ReportRunUsage(ctx, binding, finalReport); err != nil || !applied || snapshot.WorkloadOwner == nil || snapshot.WorkloadOwner.URI != owner.URI {
			t.Fatal("verified reporter was not captured", snapshot, applied, err)
		}
		peer := *owner
		peer.URI = "spiffe://fixture.test/pool/worker-b"
		foreign := transport.WithWorkloadIdentity(ctx, &peer)
		if _, _, err := usage.ReportRunUsage(foreign, binding, reported(1, 0, 0, true)); err == nil {
			t.Fatal("same-pool peer changed another worker's usage")
		}
		if accepted, err := store.BeginRunAttempt(foreign, run.RunID, 2); err == nil || accepted {
			t.Fatal("same-generation owner was replaced")
		}
		for _, changed := range []state.RunUsageBinding{
			{RunCheckpointBinding: binding.RunCheckpointBinding, TenantID: run.TenantID + "-foreign", RunIncarnationID: run.IncarnationID},
			{RunCheckpointBinding: binding.RunCheckpointBinding, TenantID: run.TenantID, RunIncarnationID: "replacement"},
		} {
			if _, _, err := usage.ReportRunUsage(ctx, changed, reported(1, 0, 0, true)); err == nil {
				t.Fatal("foreign binding accepted")
			}
		}
		if _, _, err := usage.ReportRunUsage(tenant.ScopedContext(ctx, "foreign"), binding, reported(1, 0, 0, true)); err == nil {
			t.Fatal("foreign tenant observed private report")
		}
		if accepted, err := store.BeginRunAttempt(foreign, run.RunID, 3); err != nil || !accepted {
			t.Fatal("reclaim failed to establish replacement owner", accepted, err)
		}
		binding.Generation = 3
		if snapshot, applied, err := usage.ReportRunUsage(foreign, binding, finalReport); err != nil || applied || snapshot.ReportedGeneration != 2 || snapshot.WorkloadOwner == nil || snapshot.WorkloadOwner.URI != owner.URI {
			t.Fatal("receipt replay replaced the original reporter", snapshot, applied, err)
		}
		if _, err := store.CompleteRun(ctx, &state.RunCompletion{RunID: run.RunID, Generation: 3, Status: models.RunStatusSuccess}); err == nil {
			t.Fatal("old owner completed replacement attempt")
		}
		if result, err := store.CompleteRun(tenant.ScopedContext(t.Context(), run.TenantID), &state.RunCompletion{TrustedSystem: true, RunID: run.RunID, CancelCurrentAttempt: true, Status: models.RunStatusInterrupted}); err != nil || !result.Applied {
			t.Fatal("trusted cancellation lost availability", result, err)
		}
	})
}
