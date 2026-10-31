package state

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/getrunkite/runkite/internal/models"
)

// These reserved metadata fields are maintained only by the dispatch/completion
// store transactions. Run creation removes client-supplied copies.
const (
	AttemptGenerationKey    = "_runkite_attempt_generation"
	CompletionGenerationKey = "_runkite_completion_generation"
)

type RunCompletion struct {
	// RunnerKind is the authenticated bridge identity; ReplayOnly permits only
	// acknowledgement of an already committed result after the lease is gone.
	// CancelCurrentAttempt is for trusted control-plane cancellation/timeout only.
	// It is resolved under the store transaction lock. Only CancelUsage from
	// the captured Generation may survive; arbitrary values are discarded.
	CancelCurrentAttempt bool
	CancelUsage          map[string]interface{}
	RunnerKind           string
	ReplayOnly           bool
	RunID                string
	Generation           int64
	Status               models.RunStatus
	Error                string
	Values               map[string]interface{}
}

type CompletionResult struct {
	Accepted bool
	Applied  bool
}

// RunCompletionStore advances attempts monotonically before dispatch and commits
// output, checkpoint, run status and thread release in one durable transaction.
// Retrying a committed completion is accepted without applying its writes twice.
type RunCompletionStore interface {
	BeginRunAttempt(context.Context, string, int64) (bool, error)
	CompleteRun(context.Context, *RunCompletion) (CompletionResult, error)
}

func TerminalRunStatus(status models.RunStatus) bool {
	switch status {
	case models.RunStatusSuccess, models.RunStatusError, models.RunStatusInterrupted, models.RunStatusTimeout:
		return true
	}
	return false
}

func MetadataGeneration(metadata map[string]interface{}, key string) int64 {
	switch n := metadata[key].(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case json.Number:
		v, _ := n.Int64()
		return v
	case string:
		v, _ := strconv.ParseInt(n, 10, 64)
		return v
	}
	return 0
}

// CheckCompletion must execute under the same transaction lock as its writes.
func CheckCompletion(metadata map[string]interface{}, status models.RunStatus, c *RunCompletion) CompletionResult {
	if !TerminalRunStatus(c.Status) {
		return CompletionResult{}
	}
	current := MetadataGeneration(metadata, AttemptGenerationKey)
	if c.Generation != current {
		return CompletionResult{}
	}
	if _, exists := metadata[CompletionGenerationKey]; exists {
		return CompletionResult{Accepted: MetadataGeneration(metadata, CompletionGenerationKey) == c.Generation && status == c.Status}
	}
	if TerminalRunStatus(status) {
		return CompletionResult{}
	}
	return CompletionResult{Accepted: true, Applied: true}
}

// ResolveCompletion binds trusted cancellation/timeout to the current attempt.
// External completions must always present their own dispatch generation.
func ResolveCompletion(metadata map[string]interface{}, c *RunCompletion) *RunCompletion {
	if !c.CancelCurrentAttempt || (c.Status != models.RunStatusInterrupted && c.Status != models.RunStatusTimeout) {
		return c
	}
	copy := *c
	copy.Generation = MetadataGeneration(metadata, AttemptGenerationKey)
	copy.Values = nil
	if c.Generation == copy.Generation && c.CancelUsage != nil {
		copy.Values = map[string]interface{}{"usage": c.CancelUsage}
	}
	return &copy
}
