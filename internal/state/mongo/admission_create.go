package mongo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// CreateRunAdmitted serializes via a lock document write inside a
// multi-document transaction, then COUNT + INSERT in that same txn so
// concurrent creators for the same scope cannot all observe a stale zero.
func (s *Store) CreateRunAdmitted(ctx context.Context, run *models.Run, caps *state.RunAdmissionCaps) error {
	if !caps.Enabled() {
		return s.CreateRun(ctx, run)
	}

	tid := tenant.FromContext(ctx)
	return s.withTransaction(ctx, func(sessCtx context.Context) error {
		// Touch lock docs first (upsert $inc) so concurrent transactions
		// on the same scope conflict and retry with a fresh snapshot.
		if caps.TenantConcurrent > 0 || caps.TenantDaily > 0 {
			if err := touchAdmissionLock(sessCtx, s, "t:"+tid); err != nil {
				return err
			}
		}
		if caps.AgentConcurrent > 0 || caps.AgentDaily > 0 {
			if err := touchAdmissionLock(sessCtx, s, "a:"+tid+":"+run.AgentID); err != nil {
				return err
			}
		}

		countActive := func(agentID string) (int, error) {
			filter := bson.M{
				"tenant_id": tid,
				"status":    bson.M{"$in": []string{"pending", "running"}},
			}
			if agentID != "" {
				filter["agent_id"] = agentID
			}
			n, err := s.col("runs").CountDocuments(sessCtx, filter)
			return int(n), err
		}
		countSince := func(since time.Time, agentID string) (int, error) {
			filter := bson.M{
				"tenant_id":  tid,
				"created_at": bson.M{"$gte": since.UTC()},
			}
			if agentID != "" {
				filter["agent_id"] = agentID
			}
			n, err := s.col("runs").CountDocuments(sessCtx, filter)
			return int(n), err
		}
		if err := state.EvaluateRunAdmission(caps, run.AgentID, countActive, countSince); err != nil {
			return err
		}

		doc := runDoc{
			TenantID: tid, RunID: run.RunID, ThreadID: run.ThreadID, AgentID: run.AgentID,
			Status: string(run.Status), Metadata: run.Metadata, Input: jsonToBSON(run.Input), Config: jsonToBSON(run.Config),
			CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, Depth: run.Depth,
		}
		if run.ParentRunID != nil {
			doc.ParentRunID = *run.ParentRunID
		}
		if run.RootRunID != nil {
			doc.RootRunID = *run.RootRunID
		}
		_, err := s.col("runs").InsertOne(sessCtx, doc)
		if mongo.IsDuplicateKeyError(err) {
			return &state.ErrConflict{Resource: "run", ID: run.RunID}
		}
		return err
	})
}

func touchAdmissionLock(ctx context.Context, s *Store, id string) error {
	_, err := s.col("admission_locks").UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{
			"$inc":         bson.M{"n": 1},
			"$setOnInsert": bson.M{"_id": id},
		},
		options.UpdateOne().SetUpsert(true),
	)
	return err
}
