package state

import (
	"testing"

	"github.com/getrunkite/runkite/internal/models"
)

func TestFailUnstartedRequiresOriginalPendingUnattemptedRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   models.RunStatus
		metadata map[string]interface{}
		change   func(*RunCompletion)
		want     CompletionResult
	}{
		{name: "pending", status: models.RunStatusPending, want: CompletionResult{Accepted: true, Applied: true}},
		{name: "active generation", status: models.RunStatusRunning, metadata: map[string]interface{}{AttemptGenerationKey: int64(1)}},
		{name: "legacy active zero", status: models.RunStatusRunning, metadata: map[string]interface{}{AttemptGenerationKey: int64(0)}},
		{name: "pending attempt zero", status: models.RunStatusPending, metadata: map[string]interface{}{AttemptGenerationKey: int64(0)}},
		{name: "running without fence", status: models.RunStatusRunning},
		{name: "success", status: models.RunStatusSuccess, metadata: map[string]interface{}{CompletionGenerationKey: int64(0)}},
		{name: "previous rollback", status: models.RunStatusError, metadata: map[string]interface{}{CompletionGenerationKey: int64(0)}, want: CompletionResult{Accepted: true}},
		{name: "untrusted", status: models.RunStatusPending, change: func(c *RunCompletion) { c.TrustedSystem = false }},
		{name: "no row identity", status: models.RunStatusPending, change: func(c *RunCompletion) { c.ExpectedIncarnationID = "" }},
		{name: "worker generation", status: models.RunStatusPending, change: func(c *RunCompletion) { c.Generation = 1 }},
		{name: "success request", status: models.RunStatusPending, change: func(c *RunCompletion) { c.Status = models.RunStatusSuccess }},
		{name: "arbitrary output", status: models.RunStatusPending, change: func(c *RunCompletion) { c.Values = map[string]interface{}{"unsafe": true} }},
		{name: "cancel mode", status: models.RunStatusPending, change: func(c *RunCompletion) { c.CancelCurrentAttempt = true }},
		{name: "replay mode", status: models.RunStatusPending, change: func(c *RunCompletion) { c.ReplayOnly = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			completion := &RunCompletion{ExpectedIncarnationID: "original", TrustedSystem: true, FailUnstarted: true, Status: models.RunStatusError}
			if tc.change != nil {
				tc.change(completion)
			}
			if got := CheckCompletion(tc.metadata, tc.status, completion); got != tc.want {
				t.Fatalf("completion result=%+v want=%+v", got, tc.want)
			}
		})
	}
}
