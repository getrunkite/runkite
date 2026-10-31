package state

import (
	"context"
	"fmt"

	"github.com/getrunkite/runkite/internal/jsonvalue"
	"github.com/getrunkite/runkite/internal/models"
)

// CachedRunCreator is an optional synchronous creation boundary. A cache hit
// inserts its admitted run, final output, merged thread state and completion
// checkpoint in one transaction. The transaction must reject a busy or foreign
// thread. A rolled-back transaction leaves neither a run nor a changed thread.
// A lost commit acknowledgement can leave the complete result durable; a retry
// with the same run ID must reconcile it rather than assume the commit failed.
// Stores without this boundary use ordinary dispatch instead of cache hits.
type CachedRunCreator interface {
	CreateCachedRun(context.Context, *models.Run, *RunAdmissionCaps) error
}

// PrepareCachedRun reuses the ordinary generation-zero completion invariant
// inside cached creation's transaction. It leaves the caller's result and
// metadata untouched; the prepared nonterminal row is never visible outside a
// committed transaction containing its matching terminal writes.
func PrepareCachedRun(run *models.Run) (*models.Run, *RunCompletion, error) {
	if run == nil || run.Status != models.RunStatusSuccess {
		return nil, nil, fmt.Errorf("cached creation requires a successful result")
	}
	var values map[string]interface{}
	if err := jsonvalue.Unmarshal(run.Output, &values); err != nil {
		return nil, nil, fmt.Errorf("decode cached result: %w", err)
	}
	if values == nil {
		return nil, nil, fmt.Errorf("cached result must be a JSON object")
	}
	pending := *run
	pending.Status = models.RunStatusPending
	pending.Output = nil
	pending.Metadata = make(map[string]interface{}, len(run.Metadata))
	for key, value := range run.Metadata {
		if key != AttemptGenerationKey && key != CompletionGenerationKey {
			pending.Metadata[key] = value
		}
	}
	return &pending, &RunCompletion{RunID: run.RunID, Generation: 0, Status: models.RunStatusSuccess, Values: values}, nil
}
