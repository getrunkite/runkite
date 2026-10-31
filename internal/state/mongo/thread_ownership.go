package mongo

import (
	"context"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func (s *Store) lockThread(ctx context.Context, threadID string, claim bool) error {
	filter := bson.M{"thread_id": threadID, "tenant_id": tenant.FromContext(ctx)}
	var thread threadDoc
	err := s.col("threads").FindOne(ctx, filter).Decode(&thread)
	if err == mongo.ErrNoDocuments {
		return &state.ErrNotFound{Resource: "thread", ID: threadID}
	}
	if err != nil {
		return err
	}
	if claim && thread.Status == string(models.ThreadStatusBusy) {
		return &state.ErrConflict{Resource: "thread", ID: threadID, Reason: "is busy"}
	}
	// A read alone does not conflict with a concurrent delete in Mongo's
	// snapshot isolation. This private revision makes child insertion and
	// parent deletion serialize without changing the public thread version.
	update := bson.M{"$inc": bson.M{"_runkite_child_revision": 1}}
	if claim {
		update["$set"] = bson.M{"status": string(models.ThreadStatusBusy)}
	}
	result, err := s.col("threads").UpdateOne(ctx, filter, update)
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return &state.ErrNotFound{Resource: "thread", ID: threadID}
	}
	return nil
}

func (s *Store) withThread(ctx context.Context, threadID string, mutate func(context.Context) error) error {
	return s.withTransaction(ctx, func(ctx context.Context) error {
		if err := s.lockThread(ctx, threadID, false); err != nil {
			return err
		}
		return mutate(ctx)
	})
}
