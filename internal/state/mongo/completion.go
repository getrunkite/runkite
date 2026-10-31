package mongo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func (s *Store) BeginRunAttempt(ctx context.Context, id string, generation int64) (bool, error) {
	accepted := false
	err := s.withTransaction(ctx, func(ctx context.Context) error {
		accepted = false
		run, err := s.GetRun(ctx, id)
		if err != nil {
			return err
		}
		if state.TerminalRunStatus(run.Status) || state.MetadataGeneration(run.Metadata, state.AttemptGenerationKey) > generation {
			return nil
		}
		if run.Metadata == nil {
			run.Metadata = map[string]interface{}{}
		}
		run.Metadata[state.AttemptGenerationKey] = generation
		_, err = s.col("runs").UpdateOne(ctx, tenantFilter(ctx, bson.M{"run_id": id}), bson.M{"$set": bson.M{
			"metadata": run.Metadata, "status": string(models.RunStatusRunning), "updated_at": time.Now().UTC(),
		}})
		accepted = err == nil
		return err
	})
	return accepted, err
}

func (s *Store) CompleteRun(ctx context.Context, c *state.RunCompletion) (state.CompletionResult, error) {
	var result state.CompletionResult
	err := s.withTransaction(ctx, func(ctx context.Context) error {
		var err error
		result, err = s.completeRunInTransaction(ctx, c)
		return err
	})
	return result, err
}

func (s *Store) completeRunInTransaction(ctx context.Context, c *state.RunCompletion) (state.CompletionResult, error) {
	var result state.CompletionResult
	err := func() error {
		run, err := s.GetRun(ctx, c.RunID)
		if err != nil {
			return err
		}
		resolved := state.ResolveCompletion(run.Metadata, c)
		c := resolved
		result = state.CheckCompletion(run.Metadata, run.Status, c)
		if !result.Applied {
			return nil
		}
		if run.Metadata == nil {
			run.Metadata = map[string]interface{}{}
		}
		run.Metadata[state.CompletionGenerationKey] = c.Generation
		if c.Values != nil && !c.CancelCurrentAttempt {
			thread, err := s.GetThread(ctx, run.ThreadID)
			if err != nil {
				return err
			}
			merged := make(map[string]interface{}, len(thread.Values)+len(c.Values))
			for key, value := range thread.Values {
				merged[key] = value
			}
			for key, value := range c.Values {
				merged[key] = value
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			cp := &models.ThreadState{Values: merged, Metadata: map[string]interface{}{}, Next: []string{}, Tasks: []interface{}{}, Interrupts: []interface{}{}, CreatedAt: &now, Checkpoint: models.ThreadCheckpoint{ThreadID: run.ThreadID, CheckpointID: "completion:" + c.RunID}}
			previous, err := s.GetLatestCheckpoint(ctx, run.ThreadID)
			if err == nil {
				cp.ParentCheckpoint = &previous.Checkpoint
			} else if _, ok := err.(*state.ErrNotFound); !ok {
				return err
			}
			if err := s.SaveCheckpoint(ctx, run.ThreadID, cp); err != nil {
				return err
			}
			if _, err := s.UpdateThread(ctx, run.ThreadID, &models.ThreadPatch{Values: c.Values}); err != nil {
				return err
			}
		}
		var output []byte
		if c.Values != nil {
			output, err = json.Marshal(c.Values)
			if err != nil {
				return err
			}
		}
		if err := s.UpdateRunStatus(ctx, c.RunID, c.Status, output, c.Error); err != nil {
			return err
		}
		if _, err := s.col("runs").UpdateOne(ctx, tenantFilter(ctx, bson.M{"run_id": c.RunID}), bson.M{"$set": bson.M{"metadata": run.Metadata}}); err != nil {
			return err
		}
		active, err := s.col("runs").CountDocuments(ctx, tenantFilter(ctx, bson.M{"thread_id": run.ThreadID, "status": bson.M{"$in": []string{"pending", "running"}}, "run_id": bson.M{"$ne": c.RunID}}))
		if err != nil {
			return err
		}
		if active == 0 {
			threadStatus := models.ThreadStatusIdle
			if c.Status == models.RunStatusInterrupted {
				threadStatus = models.ThreadStatusInterrupted
			}
			_, err = s.col("threads").UpdateOne(ctx, tenantFilter(ctx, bson.M{"thread_id": run.ThreadID, "status": "busy"}), bson.M{"$set": bson.M{"status": string(threadStatus), "updated_at": time.Now().UTC()}})
			if err != nil {
				return err
			}
		}
		return nil
	}()
	return result, err
}
